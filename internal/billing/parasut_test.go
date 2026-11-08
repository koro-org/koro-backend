package billing_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/billing"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/ratelimit"
)

func civilDate(t *testing.T, s string) civil.Date {
	t.Helper()
	d, err := civil.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// contractParties are the contract suite's two parties.
var contractParties = [2]billing.PartySnapshot{
	{Snapshot: billing.Snapshot{Title: "Sözleşme Test A.Ş.", TaxNo: "1000000001", TaxOffice: "Kadıköy",
		Address: billing.SnapshotAddress{Line1: "Test Cad. 1", District: "Kadıköy", City: "İstanbul"}}, LegalType: "company"},
	{Snapshot: billing.Snapshot{Title: "Sözleşme Test Vakfı", TaxNo: "1000000002"}, LegalType: "foundation"},
}

// TestParasut_ContractSuite runs the InvoiceProvider contract suite
// (BE-042) against ParasutProvider over the httptest double of Paraşüt,
// with the real token source: jobs stay running for two polls.
func TestParasut_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the provider's state and run in order
	t.Parallel()
	r := newParasutRig(t, nil, nil)
	providerContract(t, r.provider, providerFixture{
		Series: "KRT", FirstNo: 1, IssueDate: civilDate(t, "2026-10-14"), MaxPolls: 5,
		Contact: contractParties[0], Other: contractParties[1],
	})
	if r.d.sawPDFAuth() {
		t.Fatal("the PDF download carried the bearer token to the link's host")
	}
}

// draftOf is a valid draft of contact with the series and number.
func draftOf(contact, series string, no int) billing.DraftInvoice {
	return billing.DraftInvoice{InvoiceID: uuid.NewV7().String(), ContactID: contact, Series: series, No: no,
		IssueDate: civil.Date{Year: 2026, Month: 10, Day: 14}, Currency: billing.Currency, TotalGross: 2_400_000,
		Recipient: billing.Snapshot{Title: "Alıcı Test A.Ş.", TaxNo: "2222222222"},
		Lines:     []billing.DraftLine{{Description: "Platform hizmet bedeli", Quantity: 2, UnitPrice: 1_000_000, VATRateBP: 2000}}}
}

// TestParasut_CreateDraft_QueryBeforeCreate_NoDuplicate: a draft is created
// once per reserved series and number whatever got lost: an answer lost
// after Paraşüt stored the draft (the retry finds it by series and
// number), a list filter that misses it (found by the external reference
// key), both missing (the duplicate key's 422 names it); a draft whose
// totals differ from Koro's is refused, also when found again.
func TestParasut_CreateDraft_QueryBeforeCreate_NoDuplicate(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*parasutRig, string) {
		t.Helper()
		r := newParasutRig(t, nil, nil)
		contact, err := r.provider.UpsertContact(t.Context(), contractParties[0])
		if err != nil {
			t.Fatal(err)
		}
		return r, contact
	}

	t.Run("lost answer", func(t *testing.T) {
		t.Parallel()
		r, contact := setup(t)
		ctx := t.Context()
		in := draftOf(contact, "KRA", 1)
		r.d.set(func(d *parasutDouble) { d.loseCreate = true })
		if _, err := r.provider.CreateDraft(ctx, in); err == nil || errors.Is(err, billing.ErrProviderInvalid) {
			t.Fatalf("CreateDraft with a lost answer = %v, want a transient error", err)
		}
		posts := r.d.count("POST /sales_invoices")
		ref, err := r.provider.CreateDraft(ctx, in)
		if err != nil || ref.InvoiceID == "" {
			t.Fatalf("retry = %+v, %v", ref, err)
		}
		if r.d.count("POST /sales_invoices") != posts || r.d.draftsOf("KRA", 1) != 1 {
			t.Fatalf("posts %d → %d, drafts %d; want the stored draft found, not created again",
				posts, r.d.count("POST /sales_invoices"), r.d.draftsOf("KRA", 1))
		}
	})

	t.Run("found by the external reference key", func(t *testing.T) {
		t.Parallel()
		r, contact := setup(t)
		ctx := t.Context()
		in := draftOf(contact, "KRB", 1)
		first, err := r.provider.CreateDraft(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		r.d.set(func(d *parasutDouble) { d.blindList = true })
		posts, finds := r.d.count("POST /sales_invoices"), r.d.count("GET /sales_invoices/find_by_external_reference_key")
		again, err := r.provider.CreateDraft(ctx, in)
		if err != nil || again.InvoiceID != first.InvoiceID || r.d.count("POST /sales_invoices") != posts ||
			r.d.count("GET /sales_invoices/find_by_external_reference_key") != finds+1 {
			t.Fatalf("again = %+v, %v; posts %d; want the first draft by its key", again, err, r.d.count("POST /sales_invoices"))
		}
	})

	t.Run("duplicate key names the draft", func(t *testing.T) {
		t.Parallel()
		r, contact := setup(t)
		ctx := t.Context()
		in := draftOf(contact, "KRC", 1)
		first, err := r.provider.CreateDraft(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		r.d.set(func(d *parasutDouble) { d.blindList, d.blindKey = true, true })
		again, err := r.provider.CreateDraft(ctx, in)
		if err != nil || again.InvoiceID != first.InvoiceID || r.d.draftsOf("KRC", 1) != 1 {
			t.Fatalf("again = %+v, %v; drafts %d; want the draft the 422 names", again, err, r.d.draftsOf("KRC", 1))
		}
	})

	t.Run("totals that differ are refused", func(t *testing.T) {
		t.Parallel()
		r, contact := setup(t)
		ctx := t.Context()
		in := draftOf(contact, "KRD", 1)
		r.d.set(func(d *parasutDouble) { d.totalsOff = 1 })
		if _, err := r.provider.CreateDraft(ctx, in); !errors.Is(err, billing.ErrProviderInvalid) {
			t.Fatalf("CreateDraft(Paraşüt total off by 1 kuruş) = %v, want ErrProviderInvalid", err)
		}
		if _, err := r.provider.CreateDraft(ctx, in); !errors.Is(err, billing.ErrProviderInvalid) || r.d.draftsOf("KRD", 1) != 1 {
			t.Fatalf("again = %v, drafts %d; want refused again without a second draft", err, r.d.draftsOf("KRD", 1))
		}
	})

	t.Run("a local check makes no call", func(t *testing.T) {
		t.Parallel()
		r, contact := setup(t)
		posts := r.d.count("POST /sales_invoices")
		bad := draftOf(contact, "KRE", 1)
		bad.Lines[0].Quantity = 0
		if _, err := r.provider.CreateDraft(t.Context(), bad); !errors.Is(err, billing.ErrProviderInvalid) ||
			r.d.count("POST /sales_invoices") != posts {
			t.Fatalf("CreateDraft(no quantity) = %v", err)
		}
	})
}

// TestParasut_IssueLeftToProvider_EFaturaForInboxUser: a recipient whose
// tax number has an e-Fatura inbox gets an e-Fatura to its address (basic
// scenario); its number is the draft's invoice_no.
func TestParasut_IssueLeftToProvider_EFaturaForInboxUser(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newParasutRig(t, nil, nil)
	r.d.set(func(d *parasutDouble) {
		d.inboxes["2222222222"], d.runningPolls = "urn:mail:defaultpk@alici.example.test", 0
	})
	contact, err := r.provider.UpsertContact(ctx, billing.PartySnapshot{Snapshot: billing.Snapshot{Title: "Alıcı Test A.Ş.",
		TaxNo: "2222222222"}, EInvoice: true})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.provider.CreateDraft(ctx, draftOf(contact, "KRF", 1))
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.provider.Issue(ctx, ref, billing.DocUnknown)
	if err != nil || r.d.count("POST /e_invoices") != 1 || r.d.count("POST /e_archives") != 0 {
		t.Fatalf("Issue = %q, %v; e_invoices %d", job, err, r.d.count("POST /e_invoices"))
	}
	st, err := r.provider.PollJob(ctx, job)
	if err != nil || st.State != billing.JobDone || st.Doc != billing.DocEFatura || st.InvoiceNo == "" || st.ETTN == "" ||
		!strings.HasPrefix(st.Ref.EDocID, "e_invoices/") {
		t.Fatalf("PollJob = %+v, %v", st, err)
	}
	// An explicit e-Fatura to a recipient without an inbox is refused.
	other, err := r.provider.UpsertContact(ctx, contractParties[1])
	if err != nil {
		t.Fatal(err)
	}
	in := draftOf(other, "KRF", 2)
	in.Recipient = contractParties[1].Snapshot
	ref2, err := r.provider.CreateDraft(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.provider.Issue(ctx, ref2, billing.DocEFatura); !errors.Is(err, billing.ErrProviderInvalid) {
		t.Fatalf("Issue(e-Fatura, no inbox) = %v, want ErrProviderInvalid", err)
	}
}

// TestParasut_JobsAndDocuments: a job's error ends it failed with Paraşüt's
// texts scrubbed; an expired job (404) falls back to the draft; a PDF not
// ready (204) is ErrProviderNotReady; a malformed job ID is not found.
func TestParasut_JobsAndDocuments(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newParasutRig(t, nil, nil)
	contact, err := r.provider.UpsertContact(ctx, contractParties[0])
	if err != nil {
		t.Fatal(err)
	}
	r.d.set(func(d *parasutDouble) {
		d.runningPolls, d.jobErrors = 0, []string{"Alıcı VKN 1000000001 hatalı", "GİB reddetti"}
	})
	ref, err := r.provider.CreateDraft(ctx, draftOf(contact, "KRG", 1))
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.provider.Issue(ctx, ref, billing.DocEArsiv)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.provider.PollJob(ctx, job)
	if err != nil || st.State != billing.JobFailed || !strings.Contains(st.Error, "GİB reddetti") || strings.Contains(st.Error, "1000000001") {
		t.Fatalf("failed job = %+v, %v; want failed with the scrubbed texts", st, err)
	}

	ref2, err := r.provider.CreateDraft(ctx, draftOf(contact, "KRG", 2))
	if err != nil {
		t.Fatal(err)
	}
	job2, err := r.provider.Issue(ctx, ref2, billing.DocEArsiv)
	if err != nil {
		t.Fatal(err)
	}
	_, tj, _ := strings.Cut(job2, ":")
	r.d.set(func(d *parasutDouble) { d.pdfNotReady = 1 })
	if st, err := r.provider.PollJob(ctx, job2); err != nil || st.State != billing.JobDone {
		t.Fatalf("PollJob = %+v, %v", st, err)
	}
	r.d.set(func(d *parasutDouble) { delete(d.jobs, tj) }) // Paraşüt keeps a job 15 minutes
	st, err = r.provider.PollJob(ctx, job2)
	if err != nil || st.State != billing.JobDone || st.InvoiceNo == "" {
		t.Fatalf("PollJob(expired job) = %+v, %v; want done from the draft", st, err)
	}
	if _, err := r.provider.FetchPDF(ctx, st.Ref); !errors.Is(err, billing.ErrProviderNotReady) {
		t.Fatalf("FetchPDF(204) = %v, want ErrProviderNotReady", err)
	}
	if pdf, err := r.provider.FetchPDF(ctx, st.Ref); err != nil || !strings.HasPrefix(string(pdf), "%PDF-") {
		t.Fatalf("FetchPDF = %q, %v", pdf, err)
	}
	for _, bad := range []string{"", "no-colon", "../x:1", "1:../../x"} {
		if _, err := r.provider.PollJob(ctx, bad); !errors.Is(err, billing.ErrProviderNotFound) {
			t.Errorf("PollJob(%q) = %v, want ErrProviderNotFound", bad, err)
		}
	}
}

// TestParasut_ErrorClasses: 422 and 400 are ErrProviderInvalid, 404
// ErrProviderNotFound; 402, 403, 429 and 5xx are transient; Paraşüt's error
// details (which repeat the request's data) never reach the error.
func TestParasut_ErrorClasses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newParasutRig(t, nil, nil)
	party := billing.PartySnapshot{Snapshot: billing.Snapshot{Title: "Sınıf Test A.Ş.", TaxNo: "1000000009"}}
	for _, c := range []struct {
		status    int
		body      string
		invalid   bool
		notFound  bool
		transient bool
	}{
		{http.StatusUnprocessableEntity, `{"errors":[{"title":"Invalid","detail":"VKN 1000000009 fatura@alici.example.test","source":{"pointer":"/data/attributes/tax_number"}}]}`, true, false, false},
		{http.StatusBadRequest, `{"error":"Invalid JSON"}`, true, false, false},
		{http.StatusNotFound, ``, false, true, false},
		{http.StatusPaymentRequired, `{"errors":["Kullanım süreniz doldu"]}`, false, false, true},
		{http.StatusForbidden, `{"errors":[{"title":"Forbidden"}]}`, false, false, true},
		{http.StatusTooManyRequests, `{"errors":[{"title":"Too many requests","detail":"retry in 7 s"}]}`, false, false, true},
		{http.StatusServiceUnavailable, `oops`, false, false, true},
	} {
		r.d.answerNext(dAnswer{status: c.status, body: c.body})
		_, err := r.provider.UpsertContact(ctx, party)
		switch {
		case err == nil:
			t.Errorf("HTTP %d: no error", c.status)
		case errors.Is(err, billing.ErrProviderInvalid) != c.invalid, errors.Is(err, billing.ErrProviderNotFound) != c.notFound:
			t.Errorf("HTTP %d: %v; want invalid %v, not found %v", c.status, err, c.invalid, c.notFound)
		case strings.Contains(err.Error(), "1000000009") || strings.Contains(err.Error(), "fatura@"):
			t.Errorf("HTTP %d: the error repeats Paraşüt's details: %v", c.status, err)
		}
	}
	// A spent call budget is transient and makes no call.
	lim := ratelimit.NewMem(clock.NewFake(t0))
	p, err := billing.NewParasutProvider(billing.ParasutConfig{BaseURL: r.d.srv.URL, Tokens: r.tokens, HTTPClient: r.d.srv.Client(),
		Limiter: lim, RequestsPer10s: 2}) // the first upsert lists and creates
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpsertContact(ctx, party); err != nil {
		t.Fatalf("first call: %v", err)
	}
	lists := r.d.count("GET /contacts")
	if _, err := p.UpsertContact(ctx, party); err == nil || errors.Is(err, billing.ErrProviderInvalid) || r.d.count("GET /contacts") != lists {
		t.Fatalf("over budget = %v (calls %d → %d); want a transient error without a call", err, lists, r.d.count("GET /contacts"))
	}
}

// TestParasut_401RenewsOnce: a refused token is renewed once and the call
// repeated once; a token refused again ends the call (no loop).
func TestParasut_401RenewsOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newParasutRig(t, nil, nil)
	r.d.revoke()
	if _, err := r.provider.UpsertContact(ctx, contractParties[0]); err != nil {
		t.Fatalf("after a revoked token: %v", err)
	}
	if n := r.d.tokenCount(); n != 1 {
		t.Fatalf("token calls = %d, want 1", n)
	}
	r.d.answerNext(dAnswer{status: http.StatusUnauthorized, body: `{"errors":[{"title":"Unauthorized"}]}`},
		dAnswer{status: http.StatusUnauthorized, body: `{"errors":[{"title":"Unauthorized"}]}`})
	if _, err := r.provider.UpsertContact(ctx, contractParties[1]); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("refused twice = %v, want the 401", err)
	}
	if n := r.d.tokenCount(); n != 2 {
		t.Fatalf("token calls = %d, want 2 (one renewal per call)", n)
	}
}

// TestParasut_NoTokensInLogs: through a whole issue, a renewal after a
// revoked token, a refused refresh token and refused calls, no access or
// refresh token and no client secret reaches a log line (debug included),
// an error or an audit record; neither do Paraşüt's error details.
func TestParasut_NoTokensInLogs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newParasutRig(t, nil, nil)
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	contact, err := r.provider.UpsertContact(ctx, contractParties[0])
	if err != nil {
		t.Fatal(err)
	}
	r.d.revoke()
	ref, err := r.provider.CreateDraft(ctx, draftOf(contact, "KRN", 1))
	if err != nil {
		t.Fatal(err)
	}
	r.d.set(func(d *parasutDouble) { d.runningPolls = 0 })
	job, err := r.provider.Issue(ctx, ref, billing.DocUnknown)
	if err != nil {
		t.Fatal(err)
	}
	st, err := r.provider.PollJob(ctx, job)
	keep(err)
	_, err = r.provider.FetchPDF(ctx, st.Ref)
	keep(err)
	r.d.answerNext(dAnswer{status: http.StatusUnprocessableEntity,
		body: `{"errors":[{"title":"Invalid","detail":"token dbl-access-1 for 1000000001"}]}`})
	_, err = r.provider.UpsertContact(ctx, contractParties[1])
	keep(err)
	// The job renews after an hour of age; then the refresh token is refused.
	r.clk.Advance(90 * time.Minute)
	keep(r.tokens.RefreshDue(ctx))
	r.d.set(func(d *parasutDouble) { d.refresh = "dbl-refresh-elsewhere" })
	r.clk.Advance(2 * time.Hour)
	keep(r.tokens.RefreshDue(ctx))
	_, err = r.provider.UpsertContact(ctx, contractParties[1])
	keep(err)
	if len(errs) < 2 {
		t.Fatalf("errors = %v; want the refused call and the refused refresh", errs)
	}
	if !errors.Is(errs[len(errs)-1], billing.ErrRefreshRefused) {
		t.Fatalf("last error = %v, want ErrRefreshRefused", errs[len(errs)-1])
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "parasut call") || !strings.Contains(logs, "integration token not renewed") {
		t.Fatalf("logs miss the call and refusal lines:\n%s", logs)
	}
	var texts []string
	for _, e := range errs {
		texts = append(texts, e.Error())
	}
	for _, ev := range r.audit.All() {
		texts = append(texts, ev.Action)
		for k, v := range ev.Metadata {
			texts = append(texts, k, toString(v))
		}
	}
	all := logs + "\n" + strings.Join(texts, "\n")
	for _, secret := range r.d.secrets() {
		if strings.Contains(all, secret) {
			t.Errorf("%q leaked into the logs, errors or audit", secret)
		}
	}
	for _, pii := range []string{"1000000001", "fatura@alici.example.test"} {
		if strings.Contains(all, pii) {
			t.Errorf("%q leaked into the logs or errors", pii)
		}
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestParasutOAuth_Answers: a refused refresh token or client is
// ErrRefreshRefused with the OAuth code only; a rotated pair comes back;
// the configuration is checked.
func TestParasutOAuth_Answers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	d := newParasutDouble(t)
	o, err := billing.NewParasutOAuth(billing.ParasutOAuthConfig{TokenURL: d.tokenURL(), ClientID: dblClientID,
		ClientSecret: dblClientSecret, HTTPClient: d.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	g, err := o.Refresh(ctx, "dbl-refresh-0")
	if err != nil || g.AccessToken == "" || g.RefreshToken == "" || g.ExpiresIn != 2*time.Hour {
		t.Fatalf("Refresh = %v, %v", g, err)
	}
	_, err = o.Refresh(ctx, "dbl-refresh-0") // rotated: the old one is refused
	if !errors.Is(err, billing.ErrRefreshRefused) || !strings.Contains(err.Error(), "invalid_grant") ||
		strings.Contains(err.Error(), "dbl-refresh-0") {
		t.Fatalf("Refresh(old) = %v", err)
	}
	bad, _ := billing.NewParasutOAuth(billing.ParasutOAuthConfig{TokenURL: d.tokenURL(), ClientID: dblClientID,
		ClientSecret: "wrong-secret-not-real", HTTPClient: d.srv.Client()})
	if _, err := bad.Refresh(ctx, g.RefreshToken); !errors.Is(err, billing.ErrRefreshRefused) ||
		strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("Refresh(wrong client) = %v", err)
	}
	if s := g.String(); strings.Contains(s, g.AccessToken) {
		t.Fatalf("TokenGrant.String = %q", s)
	}
	for _, c := range []billing.ParasutOAuthConfig{{ClientID: ""}, {ClientID: "x", TokenURL: "ftp://id.parasut.com"},
		{ClientID: "x", TokenURL: "https://u:p@id.parasut.com/t"}} {
		if _, err := billing.NewParasutOAuth(c); err == nil {
			t.Errorf("NewParasutOAuth(%+v) = nil error", c)
		}
	}
	for _, c := range []billing.ParasutConfig{{}, {Tokens: &billing.ProviderTokens{}, BaseURL: "https://api.parasut.com?x=1"},
		{Tokens: &billing.ProviderTokens{}, Scenario: "export"}, {Tokens: &billing.ProviderTokens{}, RequestsPer10s: -1}} {
		if _, err := billing.NewParasutProvider(c); err == nil {
			t.Errorf("NewParasutProvider(%+v) = nil error", c)
		}
	}
}
