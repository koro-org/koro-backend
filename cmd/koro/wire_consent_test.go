package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/consent/iystest"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// testUnsubscribeBase is CONSENT_UNSUBSCRIBE_BASE_URL of wiring and matrix
// tests: the API's own endpoint (BE-062).
const testUnsubscribeBase = "https://api.koro.test/api/v1/unsubscribe-links"

// matrixReauthPassword is the password the test re-authentication verifier
// accepts (bodies/consent/*.json carry it in "reauth").
const matrixReauthPassword = "matrix-reauth-password"

// errTestReauth is the answer of the test verifier to a wrong or missing
// proof. identity's own codes (identity.reauth_required/_failed) are
// declared by identity (BE-014), so the fake uses the generic 403.
var errTestReauth = problem.ErrForbidden.Wrap(errors.New("test reauth: wrong or missing proof"))

// testReauth is the re-authentication verifier of wiring and matrix tests.
func testReauth(_ context.Context, _ uuid.UUID, password, _ string) (string, error) {
	if subtle.ConstantTimeCompare([]byte(password), []byte(matrixReauthPassword)) != 1 {
		return "", errTestReauth
	}
	return "password", nil
}

// useMemStore installs the in-memory consent store used by wiring and authz
// matrix tests (05 §13.2), a recording İYS client and the test
// re-authentication verifier. First owner: BE-012. Only tickets of the
// consent module edit this file (BE-001 R4).
func (w *consentWiring) useMemStore() {
	w.store = consent.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
	w.iys = &consent.FakeIYS{}
	w.cfg = &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase}
	w.deriver = testDeriver() // the shared test token key (identity's)
	w.reauth = testReauth
}

// TestWire_ConsentTasksAndPrivacy pins what consent registers (01 §6.2,
// §7): the İYS subscription on the critical queue, the reconcile job, the
// İYS status reconciliation (BE-103; the test client reads İYS states) and
// the KVKK provider.
func TestWire_ConsentTasksAndPrivacy(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "consent.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"consent.iys_reconcile 30 5 * * * default", "consent.iys_status_sync 20 * * * * default"}; !slices.Equal(jobs, want) {
		t.Fatalf("consent jobs %q, want %q", jobs, want)
	}
	// consent's own subscription; other modules may consume the topic too
	// (students.clear_contact_shares, BE-089).
	subs := slices.DeleteFunc(c.infra.subs.For(domainevent.TopicConsentRecorded), func(s outbox.Subscription) bool {
		return !strings.HasPrefix(s.Name, "consent.")
	})
	if len(subs) != 1 || subs[0].Name != consent.SubmitIYSTaskName || subs[0].Queue != "critical" {
		t.Fatalf("consent.recorded subscriptions %+v", subs)
	}
	mux, err := c.workerMux(obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{consent.SubmitIYSTaskName, consent.ReconcileJobName, consent.StatusSyncJobName} {
		if !slices.Contains(mux.Types(), name) {
			t.Errorf("worker does not route %s", name)
		}
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "consent" }) {
		t.Fatal("consent privacy provider not registered")
	}
}

// TestWire_ConsentRefusesIYSRequiredWithoutClient: CONSENT_IYS_REQUIRED=true
// with the no-op İYS client stops the process at start.
func TestWire_ConsentRefusesIYSRequiredWithoutClient(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.consent.iys = nil
	c.consent.cfg = &config.Consent{IYSRequired: true}
	if err := c.build(); err == nil || !strings.Contains(err.Error(), "CONSENT_IYS_REQUIRED") {
		t.Fatalf("build() error = %v", err)
	}
}

// TestWire_ConsentReauthIsLateBound: without a verifier every R operation
// fails closed; a verifier bound after build (identity, BE-014) is used.
func TestWire_ConsentReauthIsLateBound(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.consent.reauth = nil
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.consent.reauth == nil {
		t.Fatal("identity did not bind consent's re-authentication verifier (BE-014)")
	}
	c.consent.reauth = nil // unbound: fail closed
	dpo, _ := authztest.Get("dpo")
	proof := consent.ReauthProof{Password: matrixReauthPassword}
	if _, err := c.consent.svc.UpdatePurpose(t.Context(), dpo.Actor, consent.PurposeCommercialPush,
		consent.PurposePatch{Active: new(false)}, proof); !errors.Is(err, consent.ErrReauthUnavailable) {
		t.Fatalf("UpdatePurpose without a verifier = %v, want %v", err, consent.ErrReauthUnavailable)
	}
	c.consent.reauth = testReauth // what wire_identity.go does in BE-014
	p, err := c.consent.svc.UpdatePurpose(t.Context(), dpo.Actor, consent.PurposeCommercialPush,
		consent.PurposePatch{Active: new(false)}, proof)
	if err != nil || p.Active {
		t.Fatalf("UpdatePurpose after binding = %+v, %v", p, err)
	}
}

// TestWire_ConsentOneClickUnsubscribe (BE-062, RFC 8058) end to end on the
// wired application: consent mints the recipient's own link, notify puts
// it in the commercial e-mail (List-Unsubscribe headers and footer), and
// the mailbox provider's one-click POST — form body, no cookie, no Origin —
// withdraws the consent; repeating it changes nothing. A tampered link is
// 422; a cross-site browser form post is stopped by CrossOriginProtection
// (the web page posts from a trusted origin instead).
func TestWire_ConsentOneClickUnsubscribe(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.consent.links == nil {
		t.Fatal("consent links are not wired with CONSENT_UNSUBSCRIBE_BASE_URL")
	}
	ctx := t.Context()
	uid := uuid.NewV7()
	purpose, now := consent.PurposeCommercialEmail, time.Now().UTC().Truncate(time.Microsecond)
	body := "Ticari elektronik ileti izni (wiring testi)\n"
	text := consent.Notice{ID: uuid.NewV7(), Kind: consent.KindConsentText, PurposeCode: &purpose, Audience: consent.AudienceAll,
		Locale: consent.LocaleTR, Version: "2026.10.01", Title: "İzin", BodyMD: body, BodySHA256: consent.BodyDigest(body), CreatedAt: now}
	if err := c.consent.store.InsertNotice(ctx, text); err != nil {
		t.Fatal(err)
	}
	if err := c.consent.store.Publish(ctx, text.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := c.consent.svc.Record(ctx, consent.RecordInput{UserID: uid, Purpose: purpose, Action: consent.Granted,
		NoticeVersionID: &text.ID, TextSHA256: text.BodySHA256, Channel: consent.ChannelWeb}); err != nil {
		t.Fatal(err)
	}

	link, err := c.consent.links.UnsubscribeURL(uid, purpose)
	if err != nil || !strings.HasPrefix(link, testUnsubscribeBase+"/") {
		t.Fatalf("UnsubscribeURL = %q, %v", link, err)
	}
	m := notify.Message{UserID: &uid, ToEmail: "alici@example.test", Channel: notify.ChannelEmail, Category: notify.CategoryCommercial,
		Template: "outreach.commercial", Locale: "tr", DedupeKey: "be062:" + uid.String(), UnsubscribeURL: link,
		Data: map[string]any{"title": "Yaz stajı", "org_name": "Örnek A.Ş.", "url": "https://app.koro.test/posts/1"}}
	if _, err := c.notify.svc.Send(workerContext(t), m); err != nil {
		t.Fatalf("notify.Send(commercial) = %v", err)
	}
	sent := c.notify.transport.(*notify.MemTransport).Sent()
	if len(sent) != 1 || sent[0].UnsubscribeURL != link || !strings.Contains(sent[0].Text, link) {
		t.Fatalf("commercial e-mail does not carry the recipient's link (header and footer): %+v", sent)
	}

	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors})
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	path := strings.TrimPrefix(link, "https://api.koro.test")
	do := func(method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := do(http.MethodGet, path, nil, "")
	var page map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page) != 1 || page["purpose_code"] != "commercial_email" {
		t.Fatalf("GET = %d %s, want only the purpose", rec.Code, rec.Body)
	}
	active := func() bool {
		ok, err := c.consent.svc.HasActive(ctx, uid, purpose, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !active() {
		t.Fatal("GET withdrew the consent")
	}
	oneClick := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "User-Agent": "Mailbox-OneClick/1.0"}
	for i := range 2 {
		if rec := do(http.MethodPost, path, oneClick, "List-Unsubscribe=One-Click"); rec.Code != http.StatusNoContent {
			t.Fatalf("one-click POST #%d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if active() {
		t.Fatal("one-click POST left the consent in force")
	}
	rows, err := c.consent.store.AllOf(ctx, uid)
	if err != nil || len(rows) != 2 || rows[1].Action != consent.Withdrawn || rows[1].Channel != consent.ChannelEmailLink ||
		rows[1].UserAgent != "Mailbox-OneClick/1.0" {
		t.Fatalf("ledger after two clicks = %+v, %v", rows, err)
	}
	multipart := map[string]string{"Content-Type": "multipart/form-data; boundary=x"}
	if rec := do(http.MethodPost, path, multipart, "--x\r\nContent-Disposition: form-data; name=\"List-Unsubscribe\"\r\n\r\nOne-Click\r\n--x--\r\n"); rec.Code != http.StatusNoContent {
		t.Fatalf("multipart one-click = %d %s", rec.Code, rec.Body)
	}

	i, swap := len(path)-10, byte('A') // a character of the MAC
	if path[i] == 'A' {
		swap = 'B'
	}
	bad := path[:i] + string(swap) + path[i+1:]
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := do(method, bad, oneClick, "List-Unsubscribe=One-Click")
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"code":"consent.invalid_unsubscribe_link"`) {
			t.Fatalf("%s tampered link = %d %s", method, rec.Code, rec.Body)
		}
	}
	crossSite := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "cross-site",
		"Origin": "https://mail.example.test"}
	if rec := do(http.MethodPost, path, crossSite, "List-Unsubscribe=One-Click"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site browser POST = %d %s, want 403 (CrossOriginProtection)", rec.Code, rec.Body)
	}

	// Without CONSENT_UNSUBSCRIBE_BASE_URL no link is minted.
	off := newTestContainer(t)
	off.consent.cfg = &config.Consent{}
	if err := off.build(); err != nil {
		t.Fatal(err)
	}
	if off.consent.links != nil {
		t.Fatal("links wired without a base URL")
	}
}

// TestWire_ConsentIYSStatusSync (BE-103, T-312) on the wired application:
// with CONSENT_IYS_SYNC_MAX_AGE the commercial e-mail filter that outreach
// and notify use holds until consent.iys_status_sync ran; a refusal the
// recipient gave İYS itself then withdraws the consent (channel iys). The
// HTTP adapter cannot read İYS states yet (T-354): with it the job is not
// scheduled and the hold refuses to start.
func TestWire_ConsentIYSStatusSync(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.consent.cfg = &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase, IYSSyncMaxAge: 6 * time.Hour}
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	uid := uuid.NewV7()
	purpose, now := consent.PurposeCommercialEmail, time.Now().UTC().Truncate(time.Microsecond)
	body := "Ticari elektronik ileti izni (İYS mutabakatı)\n"
	text := consent.Notice{ID: uuid.NewV7(), Kind: consent.KindConsentText, PurposeCode: &purpose, Audience: consent.AudienceAll,
		Locale: consent.LocaleTR, Version: "2026.10.01", Title: "İzin", BodyMD: body, BodySHA256: consent.BodyDigest(body), CreatedAt: now}
	if err := c.consent.store.InsertNotice(ctx, text); err != nil {
		t.Fatal(err)
	}
	if err := c.consent.store.Publish(ctx, text.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := c.consent.svc.Record(ctx, consent.RecordInput{UserID: uid, Purpose: purpose, Action: consent.Granted,
		NoticeVersionID: &text.ID, TextSHA256: text.BodySHA256, Channel: consent.ChannelWeb}); err != nil {
		t.Fatal(err)
	}
	filter := consentFilter{c: c.consent.svc} // notify's port; outreach calls the same method
	if _, err := filter.FilterActive(ctx, []uuid.UUID{uid}, string(purpose)); !errors.Is(err, consent.ErrIYSSyncStale) {
		t.Fatalf("FilterActive before any reconciliation = %v, want ErrIYSSyncStale", err)
	}
	var sync func(context.Context) error
	for _, j := range c.infra.jobs.Jobs() {
		if j.Name == consent.StatusSyncJobName {
			sync = j.Handle
		}
	}
	if sync == nil {
		t.Fatal("consent.iys_status_sync is not scheduled")
	}
	if err := sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := filter.FilterActive(ctx, []uuid.UUID{uid}, string(purpose)); err != nil || !slices.Equal(got, []uuid.UUID{uid}) {
		t.Fatalf("FilterActive after the reconciliation = %v, %v", got, err)
	}
	fake, ok := c.consent.iys.(*consent.FakeIYS)
	if !ok {
		t.Fatalf("test İYS client %T", c.consent.iys)
	}
	fake.SetStatus(uid, consent.IYSEmail, consent.IYSState{Status: consent.IYSRefused, At: time.Now().Truncate(time.Second), Source: "IYS_WEB"})
	if err := sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := filter.FilterActive(ctx, []uuid.UUID{uid}, string(purpose)); err != nil || len(got) != 0 {
		t.Fatalf("FilterActive after the İYS refusal = %v, %v", got, err)
	}
	rows, err := c.consent.store.AllOf(ctx, uid)
	if err != nil || len(rows) != 2 || rows[1].Action != consent.Withdrawn || rows[1].Channel != consent.ChannelIYS {
		t.Fatalf("ledger = %+v, %v", rows, err)
	}

	srv := iystest.New(t)
	httpClient := func(t *testing.T) consent.IYSClient {
		t.Helper()
		client, err := consent.NewIYSHTTP(consent.IYSHTTPConfig{BaseURL: srv.URL, Username: iystest.Username,
			Password: iystest.Password, IYSCode: iystest.IYSCode, BrandCode: iystest.BrandCode,
			Recipients: staticRecipients{}, Clock: clock.Real()})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	plain := newTestContainer(t)
	plain.consent.iys = httpClient(t)
	if err := plain.build(); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(plain.infra.jobs.Jobs(), func(j queue.Job) bool { return j.Name == consent.StatusSyncJobName }) {
		t.Fatal("consent.iys_status_sync scheduled with a client that cannot read İYS states")
	}
	held := newTestContainer(t)
	held.consent.iys = httpClient(t)
	held.consent.cfg = &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase, IYSSyncMaxAge: 6 * time.Hour}
	if err := held.build(); err == nil || !strings.Contains(err.Error(), "CONSENT_IYS_SYNC_MAX_AGE") {
		t.Fatalf("build with the hold and the HTTP adapter = %v", err)
	}
}
