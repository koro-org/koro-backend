package billing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/billing"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/money"
)

// Test credentials of the double (no real account; not Paraşüt's formats).
const (
	dblClientID     = "koro-double-client"
	dblClientSecret = "double-client-secret-not-real"
	dblCompany      = "4242"
)

// parasutDouble is Paraşüt over httptest (BE-053): the OAuth token endpoint
// and the API v4 routes ParasutProvider calls, answering as the
// documentation read on 2026-11-08 says (devhub.parasut.com openapi.json,
// the sales invoice guide, the AI rules file). Where the documentation is
// silent the double makes the assumption the adapter makes (noted at the
// route). Safe for concurrent use.
type parasutDouble struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	access     string   // the valid access token
	refresh    string   // the valid refresh token
	issued     []string // every token the double handed out (the log checks)
	tokenCalls int
	calls      map[string]int // "METHOD /route" → count
	pdfAuth    bool           // a PDF download carried an Authorization header
	seq        int

	contacts map[string]map[string]any // id → attributes
	invoices map[string]*dInvoice
	keys     map[string]string // external_reference_key → invoice ID
	jobs     map[string]*dJob
	edocs    map[string]*dEDoc // "type/id" → document
	inboxes  map[string]string // VKN → e-Fatura address

	// Knobs.
	runningPolls int       // polls a job answers running before it is done
	pdfNotReady  int       // 204 answers before a PDF link
	jobErrors    []string  // the next job ends in error with these texts
	next         []dAnswer // raw answers of the next API calls (after the auth check)
	loseCreate   bool      // the next sales invoice is stored but answered 500
	blindList    bool      // the sales invoice list filter finds nothing
	blindKey     bool      // find_by_external_reference_key finds nothing
	totalsOff    int64     // kuruş added to net_total of new drafts
}

type dAnswer struct {
	status int
	body   string
}

type dInvoice struct {
	id       string
	attrs    map[string]any
	contact  string
	edoc     string // "type/id" once issued
	key      string
	details  int
	netTotal int64
	vat      int64
}

type dJob struct {
	id, invoice, typ string
	polls            int
	errs             []string
}

type dEDoc struct {
	typ, id, invoice string
	number, uuid     string
	pdfPolls         int
}

func newParasutDouble(t *testing.T) *parasutDouble {
	t.Helper()
	d := &parasutDouble{t: t, access: "dbl-access-0", refresh: "dbl-refresh-0", calls: map[string]int{},
		contacts: map[string]map[string]any{}, invoices: map[string]*dInvoice{}, keys: map[string]string{},
		jobs: map[string]*dJob{}, edocs: map[string]*dEDoc{}, inboxes: map[string]string{}, runningPolls: 2}
	d.issued = append(d.issued, d.access, d.refresh)
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *parasutDouble) tokenURL() string { return d.srv.URL + "/oauth/token" }

// count returns how often a route was called.
func (d *parasutDouble) count(route string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[route]
}

// tokenCount returns how often the token endpoint was called.
func (d *parasutDouble) tokenCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tokenCalls
}

// sawPDFAuth reports whether a PDF download carried an Authorization header.
func (d *parasutDouble) sawPDFAuth() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pdfAuth
}

// revoke invalidates the current access token (the next call answers 401).
func (d *parasutDouble) revoke() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.access = "dbl-access-revoked-" + strconv.Itoa(d.seq)
	d.seq++
}

// answerNext queues raw answers for the next API calls.
func (d *parasutDouble) answerNext(a ...dAnswer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next = append(d.next, a...)
}

func (d *parasutDouble) set(fn func(d *parasutDouble)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d)
}

// secrets returns every token and the client secret the double knows.
func (d *parasutDouble) secrets() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append(append([]string(nil), d.issued...), dblClientSecret)
}

// draftsOf returns the stored drafts with the series and number.
func (d *parasutDouble) draftsOf(series string, no int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, inv := range d.invoices {
		if inv.attrs["invoice_series"] == series && inv.attrs["invoice_id"] == float64(no) {
			n++
		}
	}
	return n
}

func (d *parasutDouble) id() string {
	d.seq++
	return strconv.Itoa(100 + d.seq)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, title, pointer string, meta map[string]any) {
	e := map[string]any{"title": title, "detail": "detail may repeat 1234567890 and fatura@alici.example.test"}
	if pointer != "" {
		e["source"] = map[string]any{"pointer": pointer}
	}
	if meta != nil {
		e["meta"] = meta
	}
	writeJSON(w, status, map[string]any{"errors": []any{e}})
}

func (d *parasutDouble) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/oauth/token":
		d.token(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/files/"):
		d.mu.Lock()
		d.pdfAuth = d.pdfAuth || r.Header.Get("Authorization") != ""
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = io.WriteString(w, "%PDF-1.7\n% double invoice "+strings.TrimPrefix(r.URL.Path, "/files/")+"\n%%EOF\n")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	prefix := "/v4/" + dblCompany
	if r.Header.Get("Authorization") != "Bearer "+d.access {
		jsonErr(w, http.StatusUnauthorized, "Unauthorized", "", nil)
		return
	}
	if !strings.HasPrefix(r.URL.Path, prefix+"/") {
		jsonErr(w, http.StatusUnauthorized, "No access to this company", "", nil)
		return
	}
	if len(d.next) > 0 {
		a := d.next[0]
		d.next = d.next[1:]
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	route := r.Method + " /" + parts[0]
	if len(parts) > 1 {
		route += "/" + map[bool]string{true: parts[1], false: "{id}"}[parts[1] == "find_by_external_reference_key"]
	}
	if len(parts) > 2 {
		route += "/" + parts[2]
	}
	d.calls[route]++
	var body map[string]any
	if r.Body != nil && r.Method == http.MethodPost {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid JSON"})
			return
		}
	}
	q := r.URL.Query()
	switch route {
	case "GET /contacts":
		d.listContacts(w, q)
	case "POST /contacts":
		d.createContact(w, body)
	case "GET /sales_invoices":
		d.listInvoices(w, q)
	case "GET /sales_invoices/find_by_external_reference_key":
		id, ok := d.keys[q.Get("external_reference_key")]
		if !ok || d.blindKey {
			w.WriteHeader(http.StatusNotFound) // "404 with no body"
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": d.invoiceResource(d.invoices[id])})
	case "POST /sales_invoices":
		d.createInvoice(w, body)
	case "GET /sales_invoices/{id}":
		d.getInvoice(w, parts[1])
	case "GET /e_invoice_inboxes":
		var data []any
		if addr, ok := d.inboxes[q.Get("filter[vkn]")]; ok {
			data = append(data, map[string]any{"id": d.id(), "type": "e_invoice_inboxes",
				"attributes": map[string]any{"vkn": q.Get("filter[vkn]"), "e_invoice_address": addr, "inbox_type": "PK"}})
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data})
	case "POST /e_archives":
		d.createEDoc(w, body, "e_archives", "sales_invoice")
	case "POST /e_invoices":
		d.createEDoc(w, body, "e_invoices", "invoice")
	case "GET /trackable_jobs/{id}":
		d.pollJob(w, parts[1])
	case "GET /e_archives/{id}/pdf", "GET /e_invoices/{id}/pdf":
		d.pdf(w, parts[0]+"/"+parts[1])
	default:
		jsonErr(w, http.StatusNotFound, "Not Found", "", nil)
	}
}

// token is the OAuth refresh grant: the refresh token rotates and the old
// one stops working (the documentation's "usually"; the stricter case).
func (d *parasutDouble) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Method != http.MethodPost ||
		r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tokenCalls++
	f := r.PostForm
	switch {
	case f.Get("client_id") != dblClientID || f.Get("client_secret") != dblClientSecret:
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client", "error_description": "secret " + f.Get("client_secret")})
		return
	case f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != d.refresh:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant",
			"error_description": "Token is not active: " + f.Get("refresh_token")})
		return
	}
	d.seq++
	d.access, d.refresh = fmt.Sprintf("dbl-access-%d", d.seq), fmt.Sprintf("dbl-refresh-%d", d.seq)
	d.issued = append(d.issued, d.access, d.refresh)
	writeJSON(w, http.StatusOK, map[string]any{"access_token": d.access, "token_type": "Bearer", "expires_in": 7200,
		"refresh_token": d.refresh, "refresh_expires_in": 2592000, "scope": "openid"})
}

func (d *parasutDouble) listContacts(w http.ResponseWriter, q url.Values) {
	var data []any
	for id, a := range d.contacts {
		if (q.Has("filter[tax_number]") && a["tax_number"] == q.Get("filter[tax_number]")) ||
			(q.Has("filter[name]") && strings.Contains(a["name"].(string), q.Get("filter[name]"))) {
			data = append(data, map[string]any{"id": id, "type": "contacts", "attributes": a})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{"total_count": len(data)}})
}

func (d *parasutDouble) createContact(w http.ResponseWriter, body map[string]any) {
	data, _ := body["data"].(map[string]any)
	a, _ := data["attributes"].(map[string]any)
	name, _ := a["name"].(string)
	if data["type"] != "contacts" || name == "" || (a["account_type"] != "customer" && a["account_type"] != "supplier") {
		jsonErr(w, http.StatusUnprocessableEntity, "Invalid contact", "/data/attributes/name", nil)
		return
	}
	id := d.id()
	d.contacts[id] = a
	writeJSON(w, http.StatusCreated, map[string]any{"data": map[string]any{"id": id, "type": "contacts", "attributes": a}})
}

func (d *parasutDouble) listInvoices(w http.ResponseWriter, q url.Values) {
	var data []any
	for _, inv := range d.invoices {
		if d.blindList {
			break
		}
		if inv.attrs["invoice_series"] == q.Get("filter[invoice_series]") &&
			strconv.FormatFloat(inv.attrs["invoice_id"].(float64), 'f', -1, 64) == q.Get("filter[invoice_id]") {
			data = append(data, d.invoiceResource(inv))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// amount reads a JSON number of a detail into kuruş.
func amount(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		f, isF := v.(float64)
		if !isF {
			return 0, false
		}
		n = json.Number(strconv.FormatFloat(f, 'f', 2, 64))
	}
	s := n.String()
	if w, frac, ok := strings.Cut(s, "."); ok && len(frac) > 2 {
		s = w + "." + frac[:2]
	}
	k, err := money.ParseDecimal(s)
	return k, err == nil
}

func (d *parasutDouble) createInvoice(w http.ResponseWriter, body map[string]any) {
	data, _ := body["data"].(map[string]any)
	a, _ := data["attributes"].(map[string]any)
	rels, _ := data["relationships"].(map[string]any)
	if data["type"] != "sales_invoices" || a["item_type"] != "invoice" || a["issue_date"] == nil || a["currency"] != "TRL" {
		jsonErr(w, http.StatusUnprocessableEntity, "Invalid sales invoice", "/data/attributes", nil)
		return
	}
	contact := ""
	if c, ok := rels["contact"].(map[string]any); ok {
		if cd, ok := c["data"].(map[string]any); ok {
			contact, _ = cd["id"].(string)
		}
	}
	if _, ok := d.contacts[contact]; !ok {
		jsonErr(w, http.StatusUnprocessableEntity, "Contact not found", "/data/relationships/contact", nil)
		return
	}
	key := ""
	if k, ok := rels["external_reference_key"].(map[string]any); ok {
		kd, _ := k["data"].(map[string]any)
		ka, _ := kd["attributes"].(map[string]any)
		key, _ = ka["key"].(string)
		if _, err := uuid.Parse(key); err != nil {
			jsonErr(w, http.StatusUnprocessableEntity, "Gönderilen key geçerli bir UUID formatında değil.", "", nil)
			return
		}
		if prev, dup := d.keys[key]; dup {
			id, _ := strconv.Atoi(prev)
			jsonErr(w, http.StatusUnprocessableEntity, "Bu key ile daha önce bir fatura oluşturulmuş.", "",
				map[string]any{"external_reference_key": key, "sales_invoice_id": id})
			return
		}
	}
	details, _ := rels["details"].(map[string]any)
	lines, _ := details["data"].([]any)
	var total, vat int64
	for _, l := range lines {
		la, _ := l.(map[string]any)["attributes"].(map[string]any)
		qty, _ := la["quantity"].(float64)
		price, okP := amount(la["unit_price"])
		rate, okR := amount(la["vat_rate"]) // percent ×100
		if qty <= 0 || !okP || !okR {
			jsonErr(w, http.StatusUnprocessableEntity, "Invalid detail", "/data/relationships/details", nil)
			return
		}
		net, _ := money.Mul(price, int64(qty))
		v, _ := money.VAT(net, rate)
		total, vat = total+net+v, vat+v
	}
	id := d.id()
	inv := &dInvoice{id: id, attrs: a, contact: contact, key: key, details: len(lines), netTotal: total + d.totalsOff, vat: vat}
	d.invoices[id] = inv
	if key != "" {
		d.keys[key] = id
	}
	if d.loseCreate {
		d.loseCreate = false
		jsonErr(w, http.StatusInternalServerError, "Internal Server Error", "", nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": d.invoiceResource(inv)})
}

// invoiceResource renders a draft: net_total as a JSON number, total_vat as
// a string (the adapter reads both forms).
func (d *parasutDouble) invoiceResource(inv *dInvoice) map[string]any {
	attrs := map[string]any{}
	for k, v := range inv.attrs {
		attrs[k] = v
	}
	attrs["net_total"] = json.Number(money.Decimal(inv.netTotal) + "0")
	attrs["total_vat"] = money.Decimal(inv.vat)
	if e, ok := d.edocs[inv.edoc]; ok && e.typ == "e_invoices" {
		attrs["invoice_no"] = e.number // an e-Fatura has no invoice_number attribute: the draft carries it (assumption)
	}
	rels := map[string]any{"contact": map[string]any{"data": map[string]any{"type": "contacts", "id": inv.contact}},
		"active_e_document": map[string]any{"data": nil}}
	if e, ok := d.edocs[inv.edoc]; ok {
		rels["active_e_document"] = map[string]any{"data": map[string]any{"type": e.typ, "id": e.id}}
	}
	return map[string]any{"id": inv.id, "type": "sales_invoices", "attributes": attrs, "relationships": rels}
}

func (d *parasutDouble) getInvoice(w http.ResponseWriter, id string) {
	inv, ok := d.invoices[id]
	if !ok {
		jsonErr(w, http.StatusNotFound, "Not Found", "", nil)
		return
	}
	included := []any{map[string]any{"id": inv.contact, "type": "contacts", "attributes": d.contacts[inv.contact]}}
	if e, ok := d.edocs[inv.edoc]; ok {
		attrs := map[string]any{"uuid": e.uuid, "status": "legalized"}
		if e.typ == "e_archives" {
			attrs["invoice_number"] = e.number
		} else {
			attrs["status"] = "successful"
		}
		included = append(included, map[string]any{"id": e.id, "type": e.typ, "attributes": attrs})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": d.invoiceResource(inv), "included": included})
}

func (d *parasutDouble) createEDoc(w http.ResponseWriter, body map[string]any, typ, rel string) {
	data, _ := body["data"].(map[string]any)
	rels, _ := data["relationships"].(map[string]any)
	ref, _ := rels[rel].(map[string]any)
	rd, _ := ref["data"].(map[string]any)
	id, _ := rd["id"].(string)
	inv, ok := d.invoices[id]
	switch {
	case data["type"] != typ:
		jsonErr(w, http.StatusUnprocessableEntity, "Invalid type", "/data/type", nil)
		return
	case !ok:
		jsonErr(w, http.StatusNotFound, "Not Found", "", nil)
		return
	case inv.edoc != "":
		jsonErr(w, http.StatusUnprocessableEntity, "Invoice already has an e-document", "", nil)
		return
	}
	if typ == "e_invoices" {
		a, _ := data["attributes"].(map[string]any)
		if (a["scenario"] != "basic" && a["scenario"] != "commercial") || a["to"] == "" || a["to"] == nil {
			jsonErr(w, http.StatusUnprocessableEntity, "Invalid e-invoice", "/data/attributes/to", nil)
			return
		}
	}
	job := &dJob{id: d.id(), invoice: id, typ: typ, errs: d.jobErrors}
	d.jobErrors = nil
	d.jobs[job.id] = job
	writeJSON(w, http.StatusAccepted, map[string]any{"data": map[string]any{"id": job.id, "type": "trackable_jobs",
		"attributes": map[string]any{"status": "running", "errors": []string{}}}})
}

func (d *parasutDouble) pollJob(w http.ResponseWriter, id string) {
	job, ok := d.jobs[id]
	if !ok {
		jsonErr(w, http.StatusNotFound, "Not Found", "", nil)
		return
	}
	status, errs := "running", []string{}
	switch {
	case job.polls < d.runningPolls:
		job.polls++
	case len(job.errs) > 0:
		status, errs = "error", job.errs
	default:
		status = "done"
		inv := d.invoices[job.invoice]
		if inv.edoc == "" {
			e := &dEDoc{typ: job.typ, id: d.id(), invoice: inv.id, uuid: uuid.NewV7().String()}
			series, _ := inv.attrs["invoice_series"].(string)
			e.number = fmt.Sprintf("%s2026%09d", series[:min(3, len(series))], len(d.edocs)+1)
			d.edocs[e.typ+"/"+e.id] = e
			inv.edoc = e.typ + "/" + e.id
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"id": id, "type": "trackable_jobs",
		"attributes": map[string]any{"status": status, "errors": errs}}})
}

func (d *parasutDouble) pdf(w http.ResponseWriter, key string) {
	e, ok := d.edocs[key]
	if !ok {
		jsonErr(w, http.StatusNotFound, "Not Found", "", nil)
		return
	}
	if e.pdfPolls < d.pdfNotReady {
		e.pdfPolls++
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"id": e.id, "type": "e_document_pdfs",
		"attributes": map[string]any{"url": d.srv.URL + "/files/" + e.id + ".pdf?X-Signature=double",
			"expires_at": time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC).Format(time.RFC3339)}}})
}

// ---- the adapter over the double

// syncBuffer is a log sink safe for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// parasutRig is ParasutProvider over the double with the real token
// rotation over a store holding the double's first token pair.
type parasutRig struct {
	d        *parasutDouble
	clk      *clock.Fake
	store    billing.Store
	audit    *audittest.Recorder
	tokens   *billing.ProviderTokens
	provider *billing.ParasutProvider
	logs     *syncBuffer
	log      *slog.Logger
}

func newParasutRig(t *testing.T, tx db.TxManager, store billing.Store) *parasutRig {
	t.Helper()
	d := newParasutDouble(t)
	clk := clock.NewFake(t0)
	if store == nil {
		store = billing.NewMemStore(clk.Now)
	}
	if tx == nil {
		tx = db.NopTx{}
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := &parasutRig{d: d, clk: clk, store: store, audit: &audittest.Recorder{}, logs: logs, log: log}
	env := testEnvelope(t)
	r.putTokens(t, env, "dbl-access-0", "dbl-refresh-0", t0.Add(2*time.Hour))
	oauth, err := billing.NewParasutOAuth(billing.ParasutOAuthConfig{TokenURL: d.tokenURL(), ClientID: dblClientID,
		ClientSecret: dblClientSecret, HTTPClient: d.srv.Client(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if r.tokens, err = billing.NewProviderTokens(billing.TokenDeps{Provider: billing.ProviderParasut, Store: store, Tx: tx,
		Sealer: env, Refresher: oauth, Clock: clk, Log: log, Audit: r.audit}); err != nil {
		t.Fatal(err)
	}
	if r.provider, err = billing.NewParasutProvider(billing.ParasutConfig{BaseURL: d.srv.URL, Tokens: r.tokens,
		HTTPClient: d.srv.Client(), Log: log}); err != nil {
		t.Fatal(err)
	}
	return r
}

// putTokens stores a sealed token pair for the double's company.
func (r *parasutRig) putTokens(t *testing.T, env interface {
	Seal(context.Context, string, []byte) ([]byte, error)
}, access, refresh string, exp time.Time) {
	t.Helper()
	ctx := t.Context()
	a, err := env.Seal(ctx, billing.AccessTokenPurposePrefix+billing.ProviderParasut, []byte(access))
	if err != nil {
		t.Fatal(err)
	}
	f, err := env.Seal(ctx, billing.RefreshTokenPurposePrefix+billing.ProviderParasut, []byte(refresh))
	if err != nil {
		t.Fatal(err)
	}
	account, at := dblCompany, t0
	if err := (db.NopTx{}).WithTx(ctx, func(ctx context.Context) error {
		_, err := r.store.PutCredential(ctx, billing.Credential{Provider: billing.ProviderParasut, ExternalAccount: &account,
			AccessTokenEnc: a, RefreshTokenEnc: f, ExpiresAt: &exp, RotatedAt: &at})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
