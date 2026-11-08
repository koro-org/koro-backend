package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/money"
	"koro/internal/platform/obs"
	"koro/internal/platform/ratelimit"
)

// ParasutProvider is the InvoiceProvider over Paraşüt's API v4 (BE-053).
// Sources, read 2026-11-08 (the adapter is proven against the httptest
// double of its tests; the check against a live Paraşüt account is a
// separate step): devhub.parasut.com's openapi.json (changelog 2026-10-23),
// its sales invoice guide ("fatura-irsaliye-rehberi") and its AI rules file
// ("parasut-skill.md"):
//
//	GET  {base}/v4/{company}/contacts?filter[tax_number]=…                       UpsertContact: lookup
//	POST {base}/v4/{company}/contacts                                            UpsertContact: create
//	GET  {base}/v4/{company}/sales_invoices?filter[invoice_series]=…&filter[invoice_id]=…   CreateDraft: query first
//	GET  {base}/v4/{company}/sales_invoices/find_by_external_reference_key?external_reference_key=<uuid>
//	POST {base}/v4/{company}/sales_invoices                                      CreateDraft: create (201)
//	GET  {base}/v4/{company}/sales_invoices/{id}?include=active_e_document,contact   Issue, PollJob, FetchPDF
//	GET  {base}/v4/{company}/e_invoice_inboxes?filter[vkn]=…                     Issue: is the recipient an e-Fatura user
//	POST {base}/v4/{company}/e_invoices | e_archives                             Issue: 202 + trackable job
//	GET  {base}/v4/{company}/trackable_jobs/{id}                                 PollJob: running | done | error
//	GET  {base}/v4/{company}/e_invoices/{id}/pdf | e_archives/{id}/pdf           FetchPDF: 204 not ready, 200 {url, expires_at}
//
// Rules:
//
//   - JSON:API bodies (application/vnd.api+json); amounts leave as decimal
//     numbers (money.Decimal), Paraşüt's currency code of the lira is TRL.
//     The bearer token and the company ID come from TokenSource
//     (ProviderTokens over integration_credentials); a 401 renews the token
//     once and repeats the call once.
//   - Idempotency: Paraşüt has no idempotency key for most calls, but a
//     sales invoice takes an external_reference_key (a UUID, unique per
//     company; a second create answers 422 with meta.sales_invoice_id).
//     CreateDraft queries by Koro's reserved series and number first, then
//     by the key (Koro's invoice ID), and creates with the key; a lost
//     answer is found by the next call. A draft whose totals (net_total =
//     "Genel Toplam", total_vat) differ from Koro's is ErrProviderInvalid.
//   - Issue reads the draft first: an e-document already linked is done
//     (no second issue); an issue started in this process returns its job
//     (memo). DocUnknown asks e_invoice_inboxes by the recipient's tax
//     number: an inbox → e-Fatura to its first address (scenario
//     ParasutConfig.Scenario, basic by default), else e-Arşiv (no
//     internet_sale block: PR-03 open). The job ID is opaque:
//     "<sales invoice id>:<trackable job id>".
//   - PollJob: running → pending; error → failed with Paraşüt's texts
//     (scrubbed); done → the draft's active e-document gives the official
//     number (the e-Arşiv's invoice_number, else the sales invoice's
//     invoice_no) and the UUID (ETTN). A trackable job is kept 15 minutes
//     after its last change: a 404 then falls back to the draft; no
//     e-document yet is pending (the provider flow gives up after 24 h).
//   - FetchPDF: 204 is ErrProviderNotReady; the PDF link of a 200 is fetched
//     without the bearer token (another host), up to 10 MiB, and must be a
//     PDF.
//   - Errors: 400 and 422 are ErrProviderInvalid, 404 ErrProviderNotFound;
//     401 (after the renewal), 402 (Paraşüt usage period ended), 403 (role
//     or package), 429 (rate limit) and 5xx are transient. Every error
//     carries the operation, the HTTP status and Paraşüt's error titles and
//     pointers (scrubbed, bounded), never a token, a tax number or an
//     address. One debug log line per call (operation, status).
//   - Rate limit: Paraşüt's package sets the calls per 10 seconds; the
//     shared limiter (Redis, every worker) spends RequestsPer10s (default
//     5) before a call; a spent budget is a transient error without a call.
type ParasutProvider struct {
	base     string
	tokens   TokenSource
	limiter  ratelimit.Limiter
	rule     ratelimit.Rule
	scenario string
	http     *http.Client
	log      *slog.Logger

	mu      sync.Mutex
	issuing map[string]string // sales invoice ID → job ID of an issue started by this process
}

var _ InvoiceProvider = (*ParasutProvider)(nil)

// TokenSource gives a provider adapter its access token and account
// (ProviderTokens).
type TokenSource interface {
	// Token returns the current access token.
	Token(ctx context.Context) (ProviderToken, error)
	// Refresh renews the token the provider refused (stale).
	Refresh(ctx context.Context, stale string) (ProviderToken, error)
}

// Defaults of the Paraşüt adapter.
const (
	// DefaultParasutBaseURL is Paraşüt's API (the paths add /v4/{company}).
	DefaultParasutBaseURL = "https://api.parasut.com"
	// DefaultParasutRequestsPer10s is the share of Paraşüt's per-10-second
	// limit the workers spend together.
	DefaultParasutRequestsPer10s = 5

	parasutCurrency = "TRL"
	maxAnswer       = 2 << 20  // a page of 25 resources with their includes
	maxPDF          = 10 << 20 // files' invoice_pdf limit
)

// e-Fatura scenarios (EInvoiceFormAttributes.scenario).
const (
	ScenarioBasic      = "basic"
	ScenarioCommercial = "commercial"
)

// ParasutConfig configures ParasutProvider.
type ParasutConfig struct {
	BaseURL        string            // DefaultParasutBaseURL when empty
	Tokens         TokenSource       // required
	Limiter        ratelimit.Limiter // the shared call budget (nil: none)
	RequestsPer10s int               // DefaultParasutRequestsPer10s when 0
	Scenario       string            // the e-Fatura scenario: basic (default) or commercial
	Timeout        time.Duration     // DefaultParasutTimeout when ≤ 0
	HTTPClient     *http.Client      // replaces the client built from Timeout (tests)
	Log            *slog.Logger      // nil discards
}

// NewParasutProvider checks c.
func NewParasutProvider(c ParasutConfig) (*ParasutProvider, error) {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = DefaultParasutBaseURL
	}
	switch {
	case checkAPIURL(base) != nil:
		return nil, errors.New("billing parasut: BILLING_PARASUT_BASE_URL must be an http(s) URL without credentials, query or fragment")
	case c.Tokens == nil:
		return nil, errors.New("billing parasut: a token source is required")
	case c.RequestsPer10s < 0 || c.RequestsPer10s > 1000:
		return nil, errors.New("billing parasut: BILLING_PARASUT_REQUESTS_PER_10S must be between 0 (default) and 1000")
	}
	if c.RequestsPer10s == 0 {
		c.RequestsPer10s = DefaultParasutRequestsPer10s
	}
	switch c.Scenario {
	case "":
		c.Scenario = ScenarioBasic
	case ScenarioBasic, ScenarioCommercial:
	default:
		return nil, errors.New("billing parasut: BILLING_PARASUT_SCENARIO must be basic or commercial")
	}
	if c.Log == nil {
		c.Log = obs.Discard()
	}
	return &ParasutProvider{base: base, tokens: c.Tokens, limiter: c.Limiter, scenario: c.Scenario,
		rule: ratelimit.Rule{Name: "parasut_api", Rate: c.RequestsPer10s, Per: 10 * time.Second, Key: ratelimit.KeyProvider},
		http: httpClient(c.HTTPClient, c.Timeout), log: c.Log, issuing: map[string]string{}}, nil
}

// reID is a Paraşüt resource ID as it may appear in a path.
var reID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ---- contacts

// contactAttrs are the attributes of a Paraşüt contact (ContactAttributes).
type contactAttrs struct {
	Name        string `json:"name"`
	AccountType string `json:"account_type,omitempty"`
	ContactType string `json:"contact_type,omitempty"`
	Email       string `json:"email,omitempty"`
	TaxNumber   string `json:"tax_number,omitempty"`
	TaxOffice   string `json:"tax_office,omitempty"`
	Address     string `json:"address,omitempty"`
	District    string `json:"district,omitempty"`
	City        string `json:"city,omitempty"`
	PostalCode  string `json:"postal_code,omitempty"`
	Country     string `json:"country,omitempty"`
}

// UpsertContact implements InvoiceProvider: the contact with the party's
// tax number (else its exact title) is reused as it is; otherwise a
// customer contact is created.
func (c *ParasutProvider) UpsertContact(ctx context.Context, p PartySnapshot) (string, error) {
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return "", fmt.Errorf("%w: contact title is required", ErrProviderInvalid)
	}
	q := url.Values{"page[size]": {"25"}}
	if p.TaxNo != "" {
		q.Set("filter[tax_number]", p.TaxNo)
	} else {
		q.Set("filter[name]", title)
	}
	var list jsonAPIList
	if _, err := c.call(ctx, "contacts.list", http.MethodGet, "/contacts", q, nil, &list); err != nil {
		return "", err
	}
	for _, r := range list.Data {
		var a contactAttrs
		if r.attrs(&a) != nil || !reID.MatchString(r.ID) {
			continue
		}
		if (p.TaxNo != "" && a.TaxNumber == p.TaxNo) || (p.TaxNo == "" && a.Name == title) {
			return r.ID, nil
		}
	}
	attrs := contactAttrs{Name: title, AccountType: "customer", ContactType: "company", Email: p.Email, TaxNumber: p.TaxNo,
		TaxOffice: p.TaxOffice, Address: p.Address.Line1, District: p.Address.District, City: p.Address.City,
		PostalCode: p.Address.PostalCode, Country: p.Address.Country}
	if p.LegalType == "individual" || p.LegalType == "sole_prop" {
		attrs.ContactType = "person"
	}
	var created jsonAPIOne
	body := jsonAPIBody{Data: jsonAPINew{Type: "contacts", Attributes: attrs}}
	if _, err := c.call(ctx, "contacts.create", http.MethodPost, "/contacts", nil, body, &created); err != nil {
		return "", err
	}
	if !reID.MatchString(created.Data.ID) {
		return "", errors.New("billing parasut: contacts.create: answer without a contact ID")
	}
	return created.Data.ID, nil
}

// ---- sales invoices (drafts)

// invoiceWrite are the attributes Koro sends with a sales invoice
// (SalesInvoiceAttributes; invoice_no and the totals are read only).
type invoiceWrite struct {
	ItemType       string `json:"item_type"`
	Description    string `json:"description,omitempty"`
	IssueDate      string `json:"issue_date"`
	DueDate        string `json:"due_date"`
	InvoiceSeries  string `json:"invoice_series"`
	InvoiceID      int    `json:"invoice_id"`
	Currency       string `json:"currency"`
	TaxNumber      string `json:"tax_number,omitempty"`
	TaxOffice      string `json:"tax_office,omitempty"`
	BillingAddress string `json:"billing_address,omitempty"`
	City           string `json:"city,omitempty"`
	District       string `json:"district,omitempty"`
}

// invoiceRead are the attributes Koro reads back.
type invoiceRead struct {
	ItemType      string     `json:"item_type"`
	InvoiceSeries string     `json:"invoice_series"`
	InvoiceID     jsonNumber `json:"invoice_id"`
	InvoiceNo     string     `json:"invoice_no"`
	NetTotal      jsonNumber `json:"net_total"` // "Genel Toplam": Σ net + VAT
	TotalVAT      jsonNumber `json:"total_vat"`
	TaxNumber     string     `json:"tax_number"`
}

// detailWrite is one line (SalesInvoiceDetailAttributes): unit price and
// VAT rate as decimal numbers.
type detailWrite struct {
	Quantity    int     `json:"quantity"`
	UnitPrice   decimal `json:"unit_price"`
	VATRate     decimal `json:"vat_rate"`
	Description string  `json:"description,omitempty"`
}

// CreateDraft implements InvoiceProvider (query before create).
func (c *ParasutProvider) CreateDraft(ctx context.Context, in DraftInvoice) (ProviderRef, error) {
	vat, err := checkDraft(in)
	if err != nil {
		return ProviderRef{}, err
	}
	key := ""
	if id, err := uuid.Parse(in.InvoiceID); err == nil {
		key = id.String()
	}
	found, ok, err := c.findDraft(ctx, in.Series, in.No, key)
	if err != nil {
		return ProviderRef{}, err
	}
	if ok {
		return checkTotals(found, in.TotalGross, vat)
	}
	details := make([]jsonAPINew, len(in.Lines))
	for i, l := range in.Lines {
		details[i] = jsonAPINew{Type: "sales_invoice_details", Attributes: detailWrite{Quantity: l.Quantity,
			UnitPrice: decimal(money.Decimal(l.UnitPrice)), VATRate: decimal(money.Decimal(int64(l.VATRateBP))),
			Description: l.Description}}
	}
	date := in.IssueDate.String()
	rels := map[string]jsonAPIRel{
		"contact": {Data: jsonAPIRef{Type: "contacts", ID: in.ContactID}},
		"details": {Data: details},
	}
	if key != "" {
		rels["external_reference_key"] = jsonAPIRel{Data: jsonAPINew{Type: "external_reference_keys",
			Attributes: map[string]string{"key": key}}}
	}
	r := in.Recipient
	body := jsonAPIBody{Data: jsonAPINew{Type: "sales_invoices", Attributes: invoiceWrite{
		ItemType: "invoice", Description: "Koro " + in.Series + "-" + strconv.Itoa(in.No), IssueDate: date, DueDate: date,
		InvoiceSeries: in.Series, InvoiceID: in.No, Currency: parasutCurrency, TaxNumber: r.TaxNo, TaxOffice: r.TaxOffice,
		BillingAddress: joinAddress(r.Address), City: r.Address.City, District: r.Address.District,
	}, Relationships: rels}}
	var created jsonAPIOne
	_, err = c.call(ctx, "sales_invoices.create", http.MethodPost, "/sales_invoices", nil, body, &created)
	if ae, isAPI := errors.AsType[*apiError](err); isAPI && ae.existing != "" {
		// The key was used before (an answer lost): Paraşüt names the draft.
		one, err := c.salesInvoice(ctx, ae.existing)
		if err != nil {
			return ProviderRef{}, err
		}
		return checkTotals(one.Data, in.TotalGross, vat)
	}
	if err != nil {
		return ProviderRef{}, err
	}
	return checkTotals(created.Data, in.TotalGross, vat)
}

// findDraft returns the draft with the series and number (not cancelled),
// else the one with the external reference key.
func (c *ParasutProvider) findDraft(ctx context.Context, series string, no int, key string) (jsonAPIResource, bool, error) {
	var list jsonAPIList
	q := url.Values{"filter[invoice_series]": {series}, "filter[invoice_id]": {strconv.Itoa(no)}, "page[size]": {"25"}}
	if _, err := c.call(ctx, "sales_invoices.find", http.MethodGet, "/sales_invoices", q, nil, &list); err != nil {
		return jsonAPIResource{}, false, err
	}
	for _, r := range list.Data {
		var a invoiceRead
		if r.attrs(&a) != nil || !reID.MatchString(r.ID) || a.ItemType == "cancelled" {
			continue
		}
		if a.InvoiceSeries == series && string(a.InvoiceID) == strconv.Itoa(no) {
			return r, true, nil
		}
	}
	if key == "" {
		return jsonAPIResource{}, false, nil
	}
	var one jsonAPIOne
	_, err := c.call(ctx, "sales_invoices.find_by_key", http.MethodGet, "/sales_invoices/find_by_external_reference_key",
		url.Values{"external_reference_key": {key}}, nil, &one)
	switch {
	case errors.Is(err, ErrProviderNotFound):
		return jsonAPIResource{}, false, nil
	case err != nil:
		return jsonAPIResource{}, false, err
	case !reID.MatchString(one.Data.ID):
		return jsonAPIResource{}, false, errors.New("billing parasut: sales_invoices.find_by_key: answer without an invoice ID")
	}
	return one.Data, true, nil
}

// checkTotals compares Paraşüt's totals of a draft with Koro's: a draft
// that differs is never issued (R-003 §4.6.2).
func checkTotals(r jsonAPIResource, gross, vat int64) (ProviderRef, error) {
	var a invoiceRead
	if err := r.attrs(&a); err != nil || !reID.MatchString(r.ID) {
		return ProviderRef{}, errors.New("billing parasut: malformed sales invoice")
	}
	total, okTotal := a.NetTotal.kurus()
	tax, okVAT := a.TotalVAT.kurus()
	if !okTotal || !okVAT || total != gross || tax != vat {
		return ProviderRef{}, fmt.Errorf("%w: Paraşüt's totals of draft %s differ from Koro's (%s / %s VAT expected)",
			ErrProviderInvalid, r.ID, money.Decimal(gross), money.Decimal(vat))
	}
	return ProviderRef{InvoiceID: r.ID}, nil
}

// joinAddress is the address line of an invoice ("line1, postal district").
func joinAddress(a SnapshotAddress) string {
	var parts []string
	for _, p := range []string{a.Line1, strings.TrimSpace(a.PostalCode + " " + a.District)} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

// salesInvoice reads a draft with its active e-document and contact.
func (c *ParasutProvider) salesInvoice(ctx context.Context, id string) (jsonAPIOne, error) {
	if !reID.MatchString(id) {
		return jsonAPIOne{}, fmt.Errorf("%w: invoice %q", ErrProviderNotFound, id)
	}
	var one jsonAPIOne
	if _, err := c.call(ctx, "sales_invoices.get", http.MethodGet, "/sales_invoices/"+id,
		url.Values{"include": {"active_e_document,contact"}}, nil, &one); err != nil {
		return jsonAPIOne{}, err
	}
	if one.Data.ID != id {
		return jsonAPIOne{}, errors.New("billing parasut: sales_invoices.get: answer for another invoice")
	}
	return one, nil
}

// eDocument is a draft's active e-document (e_archives or e_invoices).
type eDocument struct {
	doc DocumentType
	typ string
	id  string
	// EArchiveAttributes / EInvoiceAttributes (read only).
	InvoiceNumber string `json:"invoice_number"`
	UUID          string `json:"uuid"`
	Status        string `json:"status"`
	StatusCode    string `json:"status_code"`
}

// activeEDocument returns the draft's active e-document, if any.
func activeEDocument(one jsonAPIOne) (eDocument, bool) {
	ref, ok := one.Data.rel("active_e_document")
	if !ok || !reID.MatchString(ref.ID) {
		return eDocument{}, false
	}
	e := eDocument{typ: ref.Type, id: ref.ID}
	switch ref.Type {
	case "e_archives":
		e.doc = DocEArsiv
	case "e_invoices":
		e.doc = DocEFatura
	default:
		return eDocument{}, false // e_smms: not an invoice Koro issues
	}
	if inc, ok := one.included(ref); ok {
		_ = inc.attrs(&e)
	}
	return e, true
}

// recipientTaxNo is the draft's tax number, else its contact's.
func recipientTaxNo(one jsonAPIOne) string {
	var a invoiceRead
	if one.Data.attrs(&a) == nil && a.TaxNumber != "" {
		return a.TaxNumber
	}
	if ref, ok := one.Data.rel("contact"); ok {
		if inc, ok := one.included(ref); ok {
			var ca contactAttrs
			if inc.attrs(&ca) == nil {
				return ca.TaxNumber
			}
		}
	}
	return ""
}

// ---- issue

// Issue implements InvoiceProvider.
func (c *ParasutProvider) Issue(ctx context.Context, ref ProviderRef, doc DocumentType) (string, error) {
	if doc != DocEFatura && doc != DocEArsiv && doc != DocUnknown {
		return "", fmt.Errorf("%w: document type %q", ErrProviderInvalid, doc)
	}
	one, err := c.salesInvoice(ctx, ref.InvoiceID)
	if err != nil {
		return "", err
	}
	if _, ok := activeEDocument(one); ok {
		return ref.InvoiceID + ":", nil // already issued: PollJob reads the document
	}
	c.mu.Lock()
	job, started := c.issuing[ref.InvoiceID]
	c.mu.Unlock()
	if started {
		return job, nil
	}
	to := ""
	if doc != DocEArsiv {
		vkn := recipientTaxNo(one)
		if vkn != "" {
			if to, err = c.inbox(ctx, vkn); err != nil {
				return "", err
			}
		}
		switch {
		case doc == DocEFatura && to == "":
			return "", fmt.Errorf("%w: the recipient has no e-Fatura inbox", ErrProviderInvalid)
		case to != "":
			doc = DocEFatura
		default:
			doc = DocEArsiv
		}
	}
	draft := jsonAPIRel{Data: jsonAPIRef{Type: "sales_invoices", ID: ref.InvoiceID}}
	var body jsonAPIBody
	op, path := "e_archives.create", "/e_archives"
	if doc == DocEFatura {
		op, path = "e_invoices.create", "/e_invoices"
		body = jsonAPIBody{Data: jsonAPINew{Type: "e_invoices", Attributes: map[string]string{"scenario": c.scenario, "to": to},
			Relationships: map[string]jsonAPIRel{"invoice": draft}}}
	} else {
		body = jsonAPIBody{Data: jsonAPINew{Type: "e_archives", Attributes: map[string]string{},
			Relationships: map[string]jsonAPIRel{"sales_invoice": draft}}}
	}
	var tj jsonAPIOne
	if _, err := c.call(ctx, op, http.MethodPost, path, nil, body, &tj); err != nil {
		return "", err
	}
	if !reID.MatchString(tj.Data.ID) {
		return "", fmt.Errorf("billing parasut: %s: answer without a trackable job", op)
	}
	job = ref.InvoiceID + ":" + tj.Data.ID
	c.mu.Lock()
	c.issuing[ref.InvoiceID] = job
	c.mu.Unlock()
	c.log.LogAttrs(ctx, slog.LevelDebug, "parasut issue started", slog.String("document", string(doc)))
	return job, nil
}

// inbox returns the first e-Fatura address of a tax number ("" when the
// recipient is not an e-Fatura user).
func (c *ParasutProvider) inbox(ctx context.Context, vkn string) (string, error) {
	var list jsonAPIList
	if _, err := c.call(ctx, "e_invoice_inboxes.list", http.MethodGet, "/e_invoice_inboxes",
		url.Values{"filter[vkn]": {vkn}, "page[size]": {"25"}}, nil, &list); err != nil {
		return "", err
	}
	for _, r := range list.Data {
		var a struct {
			EInvoiceAddress string `json:"e_invoice_address"`
		}
		if r.attrs(&a) == nil && strings.TrimSpace(a.EInvoiceAddress) != "" {
			return strings.TrimSpace(a.EInvoiceAddress), nil
		}
	}
	return "", nil
}

// ---- jobs and documents

// PollJob implements InvoiceProvider.
func (c *ParasutProvider) PollJob(ctx context.Context, jobID string) (JobStatus, error) {
	invoiceID, tj, ok := strings.Cut(jobID, ":")
	if !ok || !reID.MatchString(invoiceID) || (tj != "" && !reID.MatchString(tj)) {
		return JobStatus{}, fmt.Errorf("%w: job %q", ErrProviderNotFound, jobID)
	}
	pending := JobStatus{State: JobPending, Ref: ProviderRef{InvoiceID: invoiceID}}
	if tj != "" {
		var job jsonAPIOne
		_, err := c.call(ctx, "trackable_jobs.get", http.MethodGet, "/trackable_jobs/"+tj, nil, nil, &job)
		switch {
		case errors.Is(err, ErrProviderNotFound): // kept 15 minutes after its last change: read the draft
		case err != nil:
			return JobStatus{}, err
		default:
			var a struct {
				Status string   `json:"status"`
				Errors []string `json:"errors"`
			}
			_ = job.Data.attrs(&a)
			switch a.Status {
			case "error":
				c.forget(invoiceID)
				msg := safeText(strings.Join(a.Errors, "; "))
				if msg == "" {
					msg = "Paraşüt reported the issue job as failed"
				}
				return JobStatus{State: JobFailed, Ref: pending.Ref, Error: msg}, nil
			case "done":
			default: // running (or a state Paraşüt adds later)
				return pending, nil
			}
		}
	}
	one, err := c.salesInvoice(ctx, invoiceID)
	if err != nil {
		return JobStatus{}, err
	}
	e, ok := activeEDocument(one)
	if !ok {
		return pending, nil
	}
	if e.doc == DocEFatura && e.Status == "failed" {
		c.forget(invoiceID)
		return JobStatus{State: JobFailed, Ref: pending.Ref,
			Error: safeText("the e-Fatura failed at GİB (status code " + e.StatusCode + ")")}, nil
	}
	no := strings.TrimSpace(e.InvoiceNumber)
	if no == "" {
		var a invoiceRead
		if one.Data.attrs(&a) == nil {
			no = strings.TrimSpace(a.InvoiceNo)
		}
	}
	if no == "" {
		return pending, nil // linked, not numbered yet
	}
	c.forget(invoiceID)
	return JobStatus{State: JobDone, Ref: ProviderRef{InvoiceID: invoiceID, EDocID: e.typ + "/" + e.id}, Doc: e.doc,
		InvoiceNo: no, ETTN: strings.TrimSpace(e.UUID)}, nil
}

func (c *ParasutProvider) forget(invoiceID string) {
	c.mu.Lock()
	delete(c.issuing, invoiceID)
	c.mu.Unlock()
}

// FetchPDF implements InvoiceProvider.
func (c *ParasutProvider) FetchPDF(ctx context.Context, ref ProviderRef) ([]byte, error) {
	one, err := c.salesInvoice(ctx, ref.InvoiceID)
	if err != nil {
		return nil, err
	}
	e, ok := activeEDocument(one)
	if !ok {
		return nil, ErrProviderNotReady
	}
	var link jsonAPIOne
	status, err := c.call(ctx, e.typ+".pdf", http.MethodGet, "/"+e.typ+"/"+e.id+"/pdf", nil, nil, &link)
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusNoContent:
		return nil, ErrProviderNotReady
	}
	var a struct {
		URL string `json:"url"`
	}
	if link.Data.attrs(&a) != nil || a.URL == "" {
		return nil, ErrProviderNotReady
	}
	return c.download(ctx, a.URL)
}
