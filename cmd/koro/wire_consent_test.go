package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/iys"
	"koro/internal/platform/iys/iystest"
	"koro/internal/platform/iys/netgsm"
	"koro/internal/platform/iys/netgsm/netgsmtest"
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
// matrix tests (05 §13.2), a recording İYS client, a Netgsm webhook
// receiver over an in-memory store (receiveIYSWebhook of the matrix; its
// addresses are no Koro user's) and the test re-authentication verifier.
// First owner: BE-012. Only tickets of the consent module edit this file
// (BE-001 R4).
func (w *consentWiring) useMemStore() {
	w.store = consent.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
	w.iys = &consent.FakeIYS{}
	w.netgsmStore = netgsm.NewMemStore()
	push, err := netgsm.New(netgsm.Config{BaseURL: "https://netgsm.invalid", Username: netgsmtest.Username,
		Password: netgsmtest.Password, IYSCode: netgsmtest.IYSCode, BrandCode: netgsmtest.BrandCode,
		WebhookToken: netgsmtest.WebhookToken, Recipients: noIYSRecipients{}, Store: w.netgsmStore, Clock: clock.Real()})
	if err != nil {
		panic(err) // a test configuration bug
	}
	w.push = push
	w.cfg = &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase}
	w.deriver = testDeriver() // the shared test token key (identity's)
	w.reauth = testReauth
}

// noIYSRecipients knows no İYS recipient (the matrix's webhook items are
// strangers' and stored as anonymous markers).
type noIYSRecipients struct{}

func (noIYSRecipients) Recipient(context.Context, uuid.UUID, iys.Type) (string, error) {
	return "", iys.ErrRecipientGone
}

func (noIYSRecipients) User(context.Context, iys.Type, string) (uuid.UUID, error) {
	return uuid.Nil(), iys.ErrRecipientGone
}

// TestWire_ConsentTasksAndPrivacy pins what consent registers (01 §6.2,
// §7): the İYS subscription on the critical queue, the reconcile job, the
// İYS status reconciliation (BE-103; the test client reads İYS states), the
// retention of the Netgsm webhook items (BE-110) and
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
	if want := []string{"consent.iys_reconcile 30 5 * * * default", "consent.iys_status_sync 20 * * * * default",
		"consent.iys_netgsm_purge 40 4 * * * low"}; !slices.Equal(jobs, want) {
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
	for _, name := range []string{consent.SubmitIYSTaskName, consent.ReconcileJobName, consent.StatusSyncJobName, netgsm.PurgeJobName} {
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
// recipient gave İYS itself then withdraws the consent (channel iys).
// Without an İYS adapter the hold refuses to start.
func TestWire_ConsentIYSStatusSync(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.consent.cfg = &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase, IYSSyncMaxAge: 6 * time.Hour}
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	uid, purpose := uuid.NewV7(), consent.PurposeCommercialEmail
	grantCommercial(t, c, uid, purpose)
	filter := consentFilter{c: c.consent.svc} // notify's port; outreach calls the same method
	if _, err := filter.FilterActive(ctx, []uuid.UUID{uid}, string(purpose)); !errors.Is(err, consent.ErrIYSSyncStale) {
		t.Fatalf("FilterActive before any reconciliation = %v, want ErrIYSSyncStale", err)
	}
	sync := scheduledJob(t, c, consent.StatusSyncJobName)
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

	none := newTestContainer(t)
	none.consent.iys = nil
	none.consent.cfg = &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase, IYSSyncMaxAge: 6 * time.Hour}
	if err := none.build(); err == nil || !strings.Contains(err.Error(), "CONSENT_IYS_SYNC_MAX_AGE") {
		t.Fatalf("build with the hold and no İYS adapter = %v", err)
	}
}

// officialIYS is the consent configuration of the official İYS adapter
// over the double (CONSENT_IYS_PROVIDER=official).
func officialIYS(srv *iystest.Server, maxAge time.Duration) *config.Consent {
	return &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase, IYSSyncMaxAge: maxAge,
		IYSProvider: config.IYSProviderOfficial, IYSBaseURL: srv.URL, IYSUsername: iystest.Username,
		IYSPassword: config.NewSecret(iystest.Password), IYSCode: iystest.IYSCode, IYSBrandCode: iystest.BrandCode,
		IYSRecipientType: "BIREYSEL", IYSTimeout: 5 * time.Second}
}

// TestWire_ConsentOfficialIYSAdapter (BE-105): CONSENT_IYS_PROVIDER=official
// wires the official adapter (submit, change feed, single states) with
// identity as its recipient resolver; end to end over the İYS double: the
// grant is registered as ONAY with the account's address, the first
// reconciliation reads it back, a refusal the recipient gives İYS itself
// is listed an hour later and withdraws the consent (channel iys); the
// hold follows the feed's horizon.
func TestWire_ConsentOfficialIYSAdapter(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	clk := c.infra.clock.(*clock.Fake)
	srv := iystest.New(t, iystest.WithClock(clk))
	c.consent.iys = nil
	c.consent.cfg = officialIYS(srv, 3*time.Hour)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.consent.iys.(*consent.IYSAdapterClient); !ok || c.consent.iys.Name() != "iys" {
		t.Fatalf("İYS client %T, want the official adapter", c.consent.iys)
	}
	ctx := t.Context()
	const address = "ada.wire@ogrenci.example.test"
	purpose := consent.PurposeCommercialEmail
	uid := iysWireUser(t, c, address)
	grantID := grantCommercial(t, c, uid, purpose)
	submitIYS(t, c, grantID, uid, purpose)
	if got := srv.Consents(); len(got) != 1 || got[0].Recipient != address || got[0].Status != "ONAY" || got[0].Type != "EPOSTA" {
		t.Fatalf("İYS records = %+v", got)
	}
	filter := consentFilter{c: c.consent.svc}
	if _, err := filter.FilterActive(ctx, []uuid.UUID{uid}, string(purpose)); !errors.Is(err, consent.ErrIYSSyncStale) {
		t.Fatalf("FilterActive before any reconciliation = %v, want held", err)
	}
	sync := scheduledJob(t, c, consent.StatusSyncJobName)
	if err := sync(ctx); err != nil {
		t.Fatal(err)
	}
	if srv.Calls("bulk") != 1 || srv.Calls("status") != 0 {
		t.Fatalf("first reconciliation: %d bulk, %d single queries; want the bulk approval read only", srv.Calls("bulk"), srv.Calls("status"))
	}
	if got, err := filter.FilterActive(ctx, []uuid.UUID{uid}, string(purpose)); err != nil || !slices.Equal(got, []uuid.UUID{uid}) {
		t.Fatalf("FilterActive after the reconciliation = %v, %v", got, err)
	}

	srv.Act(iystest.Action{Recipient: address, Source: "IYS_MOBIL"})
	srv.Act(iystest.Action{Recipient: "stranger@elsewhere.example.test"}) // no Koro account
	clk.Advance(time.Hour + time.Minute)
	if err := sync(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := c.consent.store.AllOf(ctx, uid)
	if err != nil || len(rows) != 2 || rows[1].Action != consent.Withdrawn || rows[1].Channel != consent.ChannelIYS {
		t.Fatalf("ledger = %+v, %v", rows, err)
	}
	if got, err := filter.FilterActive(ctx, []uuid.UUID{uid}, string(purpose)); err != nil || len(got) != 0 {
		t.Fatalf("FilterActive after the İYS refusal = %v, %v", got, err)
	}
	if srv.Calls("status") != 0 {
		t.Fatal("the change feed was not enough: single queries were made")
	}

	// The worker registers koro_consent_iys_alarms_total{reason}.
	m := obs.NewMetrics()
	if err := c.consent.metrics(m); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, obs.MetricsPath, nil))
	for _, reason := range []string{consent.IYSAlarmTooLate, consent.IYSAlarmConflict, consent.IYSAlarmSweepUnavailable,
		consent.IYSAlarmReadRejected} {
		if want := `koro_consent_iys_alarms_total{reason="` + reason + `"} 0`; !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

// iysWireUser inserts the active account of address: the İYS recipient
// the adapters resolve through identity.
func iysWireUser(t *testing.T, c *container, address string) uuid.UUID {
	t.Helper()
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	uid := uuid.NewV7()
	if err := c.identity.store.InsertUser(t.Context(), identity.User{
		ID: uid, Status: identity.StatusActive, Email: address, EmailNormalized: address, EmailVerifiedAt: &now,
		PasswordHash: testPasswordHash(), FirstName: "Ada", LastName: "Yılmaz", Persona: identity.PersonaPlatform,
		Locale: identity.LocaleTR, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return uid
}

// publishNotice publishes a notice of kind (with purpose for a consent
// text) in c's consent store.
func publishNotice(t *testing.T, c *container, kind consent.NoticeKind, purpose *consent.Purpose, body string) consent.Notice {
	t.Helper()
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	n := consent.Notice{ID: uuid.NewV7(), Kind: kind, PurposeCode: purpose, Audience: consent.AudienceAll,
		Locale: consent.LocaleTR, Version: "2026.10.01", Title: "Metin", BodyMD: body, BodySHA256: consent.BodyDigest(body), CreatedAt: now}
	if err := c.consent.store.InsertNotice(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if err := c.consent.store.Publish(t.Context(), n.ID, now); err != nil {
		t.Fatal(err)
	}
	return n
}

// grantCommercial publishes the consent text of purpose and records uid's
// grant on the web.
func grantCommercial(t *testing.T, c *container, uid uuid.UUID, purpose consent.Purpose) uuid.UUID {
	t.Helper()
	text := publishNotice(t, c, consent.KindConsentText, &purpose, "Ticari elektronik ileti izni ("+string(purpose)+")\n")
	id, err := c.consent.svc.Record(t.Context(), consent.RecordInput{UserID: uid, Purpose: purpose, Action: consent.Granted,
		NoticeVersionID: &text.ID, TextSHA256: text.BodySHA256, Channel: consent.ChannelWeb})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// submitIYS runs consent.submit_iys for the consent.recorded event of a
// grant (what the outbox relay does in the worker).
func submitIYS(t *testing.T, c *container, grantID, uid uuid.UUID, purpose consent.Purpose) {
	t.Helper()
	ev, err := domainevent.New(domainevent.ConsentRecorded{ConsentID: grantID, UserID: uid, Purpose: string(purpose),
		Action: string(consent.Granted)}, "consent", grantID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range c.infra.subs.For(domainevent.TopicConsentRecorded) {
		if sub.Name == consent.SubmitIYSTaskName {
			if err := sub.Handle(t.Context(), ev); err != nil {
				t.Fatalf("consent.submit_iys: %v", err)
			}
			return
		}
	}
	t.Fatal("consent.submit_iys is not subscribed")
}

// scheduledJob returns the handler of the scheduled job name.
func scheduledJob(t *testing.T, c *container, name string) func(context.Context) error {
	t.Helper()
	for _, j := range c.infra.jobs.Jobs() {
		if j.Name == name {
			return j.Handle
		}
	}
	t.Fatalf("%s is not scheduled", name)
	return nil
}

// TestWire_ConsentIYSAdapterByConfig (BE-105, T-364, BE-110):
// CONSENT_IYS_PROVIDER alone picks the İYS adapter. none: no adapter, no
// reconciliation, and a commercial consent is not collected (the
// registration creates the account without it); fake: the in-memory İYS
// with identity's addresses, delivery and reconciliation; netgsm: the
// Netgsm adapter with its reconciliation and backup check (its settings
// are required); an unknown name is refused.
func TestWire_ConsentIYSAdapterByConfig(t *testing.T) {
	t.Parallel()
	build := func(t *testing.T, provider string) (*container, error) {
		t.Helper()
		c := newTestContainer(t)
		c.consent.iys = nil
		c.consent.cfg = &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase, IYSProvider: provider,
			IYSRecipientType: "BIREYSEL", IYSTimeout: 5 * time.Second}
		return c, c.build()
	}

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		c, err := build(t, config.IYSProviderNone)
		if err != nil {
			t.Fatal(err)
		}
		if c.consent.iys != nil || slices.ContainsFunc(c.infra.jobs.Jobs(), func(j queue.Job) bool { return j.Name == consent.StatusSyncJobName }) {
			t.Fatalf("İYS client %T and the reconciliation without an adapter", c.consent.iys)
		}
		ctx := t.Context()
		uid := iysWireUser(t, c, "ada.none@ogrenci.example.test")
		purpose := consent.PurposeCommercialEmail
		text := publishNotice(t, c, consent.KindConsentText, &purpose, "Ticari elektronik ileti izni\n")
		if _, err := c.consent.svc.Record(ctx, consent.RecordInput{UserID: uid, Purpose: purpose, Action: consent.Granted,
			NoticeVersionID: &text.ID, TextSHA256: text.BodySHA256, Channel: consent.ChannelWeb}); !errors.Is(err, consent.ErrIYSUnavailable) {
			t.Fatalf("commercial grant without İYS = %v, want %v", err, consent.ErrIYSUnavailable)
		}
		// The registration page's commercial checkbox: the account is
		// created, the consent is not (identity skips a refused consent).
		privacyNotice := publishNotice(t, c, consent.KindPrivacyNotice, nil, "Aydınlatma metni\n")
		const address = "bea.none@sirket.example.test"
		if err := c.identity.svc.Register(ctx, authz.Anonymous(), identity.Registration{
			Email: address, Password: "Uzun-ve-tahmin-edilmez-parola-2026", FirstName: "Bea", LastName: "Kaya",
			Persona: identity.PersonaCompany, Locale: identity.LocaleTR, PrivacyNoticeID: privacyNotice.ID,
			Consents: []identity.ConsentGrant{{Purpose: purpose, NoticeVersionID: text.ID, TextSHA256: text.BodySHA256}},
		}); err != nil {
			t.Fatalf("registration with the commercial consent = %v", err)
		}
		id, found, err := c.identity.svc.FindByEmail(ctx, address)
		if err != nil || !found {
			t.Fatalf("account not created: %v", err)
		}
		if rows, err := c.consent.store.AllOf(ctx, id); err != nil || len(rows) != 0 {
			t.Fatalf("consents of the new account = %+v, %v; want none", rows, err)
		}
	})

	t.Run("fake", func(t *testing.T) {
		t.Parallel()
		c, err := build(t, config.IYSProviderFake)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.consent.iys.(*consent.IYSAdapterClient); !ok || c.consent.iys.Name() != "fake" {
			t.Fatalf("İYS client %T, want the fake adapter", c.consent.iys)
		}
		purpose := consent.PurposeCommercialEmail
		uid := iysWireUser(t, c, "ada.fake@ogrenci.example.test")
		grantID := grantCommercial(t, c, uid, purpose)
		submitIYS(t, c, grantID, uid, purpose)
		if sub, err := c.consent.store.SubmissionOf(t.Context(), grantID); err != nil || sub.Status != consent.SubmissionSent {
			t.Fatalf("submission = %+v, %v; want sent", sub, err)
		}
		if err := scheduledJob(t, c, consent.StatusSyncJobName)(t.Context()); err != nil {
			t.Fatalf("reconciliation over the fake: %v", err)
		}
	})

	t.Run("netgsm", func(t *testing.T) {
		t.Parallel()
		c := newTestContainer(t)
		c.consent.iys, c.consent.push, c.consent.netgsmStore = nil, nil, netgsm.NewMemStore()
		c.consent.cfg = netgsmConsent("https://api.netgsm.invalid", 0)
		if err := c.build(); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.consent.iys.(*consent.IYSAdapterClient); !ok || c.consent.iys.Name() != "netgsm" {
			t.Fatalf("İYS client %T, want the Netgsm adapter", c.consent.iys)
		}
		for _, job := range []string{consent.StatusSyncJobName, netgsm.RecheckJobName} {
			if !slices.ContainsFunc(c.infra.jobs.Jobs(), func(j queue.Job) bool { return j.Name == job }) {
				t.Errorf("%s is not scheduled", job)
			}
		}
		// Without its settings the process does not start.
		if _, err := build(t, config.IYSProviderNetgsm); err == nil || !strings.Contains(err.Error(), "CONSENT_IYS_NETGSM") {
			t.Fatalf("build with netgsm and no settings = %v", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		if _, err := build(t, "verimor"); err == nil ||
			!strings.Contains(err.Error(), `CONSENT_IYS_PROVIDER="verimor": no such adapter (wired: fake, netgsm, none, official)`) {
			t.Fatalf("build with an unknown adapter = %v", err)
		}
	})
}

// netgsmConsent is the consent configuration of the Netgsm adapter over
// the double at base (CONSENT_IYS_PROVIDER=netgsm).
func netgsmConsent(base string, maxAge time.Duration) *config.Consent {
	return &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase, IYSSyncMaxAge: maxAge, IYSProvider: config.IYSProviderNetgsm,
		IYSRecipientType: "BIREYSEL", IYSTimeout: 5 * time.Second,
		Netgsm: config.IYSNetgsm{BaseURL: base, Username: netgsmtest.Username, Password: config.NewSecret(netgsmtest.Password),
			IYSCode: netgsmtest.IYSCode, BrandCode: netgsmtest.BrandCode, WebhookToken: config.NewSecret(netgsmtest.WebhookToken),
			WebhookAllowedIPs: []string{"192.0.2.0/24"}, RefIDIn: "header", RequestsPerMinute: 10, StatusQueriesPerHour: 600,
			RecheckPerHour: 60, ResendAfter: time.Hour}}
}

// TestWire_ConsentNetgsmIYSAdapter (BE-110): CONSENT_IYS_PROVIDER=netgsm
// end to end over the Netgsm double, its webhook delivered through the
// API's own receiveIYSWebhook: the grant is uploaded with the account's
// address and becomes "sent" once İYS's answer arrived by webhook; the
// first reconciliation reads it back; a refusal the recipient gives İYS
// itself is pushed, listed once it settled and withdraws the consent
// (channel iys); the backup check reads the grants in force; a delivery
// with a wrong token or from another address is 401, another provider 404
// and a foreign body 422, and none of them stores anything.
func TestWire_ConsentNetgsmIYSAdapter(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	clk := c.infra.clock.(*clock.Fake)
	var app *httpx.App
	post := func(ctx context.Context, provider, token, remote string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/webhooks/iys/"+provider+"/"+token, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	srv := netgsmtest.New(t, netgsmtest.WithClock(clk), netgsmtest.WithWebhook(func(ctx context.Context, body []byte) error {
		if rec := post(ctx, "netgsm", netgsmtest.WebhookToken, "192.0.2.10:443", body); rec.Code != http.StatusOK {
			return fmt.Errorf("webhook answered %d %s", rec.Code, rec.Body)
		}
		return nil
	}))
	store := netgsm.NewMemStore()
	c.consent.iys, c.consent.push, c.consent.netgsmStore = nil, nil, store
	c.consent.cfg = netgsmConsent(srv.URL, 3*time.Hour)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var err error
	if app, err = httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors}); err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	ctx := t.Context()
	const address = "ada.netgsm@ogrenci.example.test"
	purpose := consent.PurposeCommercialEmail
	uid := iysWireUser(t, c, address)
	grantID := grantCommercial(t, c, uid, purpose)
	submitIYS(t, c, grantID, uid, purpose)
	if up := srv.Uploads(); len(up) != 1 || up[0].Recipient != address || up[0].Status != "ONAY" || up[0].Type != "EPOSTA" ||
		up[0].Source != "HS_WEB" || up[0].RefIn != "header" {
		t.Fatalf("uploads = %+v", up)
	}
	if sub, err := c.consent.store.SubmissionOf(ctx, grantID); err != nil || sub.Status != consent.SubmissionSent ||
		sub.IYSTransactionID == nil || len(*sub.IYSTransactionID) != 64 {
		t.Fatalf("submission = %+v, %v; want sent with İYS's transaction ID", sub, err)
	}
	if errs := srv.SinkErrors(); len(errs) != 0 {
		t.Fatalf("webhook deliveries failed: %v", errs)
	}

	sync := scheduledJob(t, c, consent.StatusSyncJobName)
	if err := sync(ctx); err != nil {
		t.Fatal(err)
	}
	filter := consentFilter{c: c.consent.svc}
	if got, err := filter.FilterActive(ctx, []uuid.UUID{uid}, string(purpose)); err != nil || !slices.Equal(got, []uuid.UUID{uid}) {
		t.Fatalf("FilterActive after the first reconciliation = %v, %v", got, err)
	}
	searches := srv.Calls("search")
	if err := scheduledJob(t, c, netgsm.RecheckJobName)(ctx); err != nil || srv.Calls("search") != searches+1 {
		t.Fatalf("backup check = %v (%d → %d searches), want the grant read once", err, searches, srv.Calls("search"))
	}

	srv.Act(ctx, netgsmtest.Action{Recipient: address, Source: "IYS_MOBIL"})
	srv.Act(ctx, netgsmtest.Action{Recipient: "stranger@elsewhere.example.test"}) // no Koro account
	if errs := srv.SinkErrors(); len(errs) != 0 {
		t.Fatalf("webhook deliveries failed: %v", errs)
	}
	clk.Advance(netgsm.Settle + time.Minute)
	if err := sync(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := c.consent.store.AllOf(ctx, uid)
	if err != nil || len(rows) != 2 || rows[1].Action != consent.Withdrawn || rows[1].Channel != consent.ChannelIYS {
		t.Fatalf("ledger = %+v, %v", rows, err)
	}
	if got, err := filter.FilterActive(ctx, []uuid.UUID{uid}, string(purpose)); err != nil || len(got) != 0 {
		t.Fatalf("FilterActive after the İYS refusal = %v, %v", got, err)
	}

	feed := "netgsm:" + netgsmtest.IYSCode + "/" + netgsmtest.BrandCode
	stored := len(store.All(feed))
	body := []byte(`[{"iyscode": 700123, "brandcode": 700124, "type": "EPOSTA", "source": "IYS_WEB", "status": "RET",
		"recipient": "` + address + `", "consentDate": "2026-10-03 12:00:00", "resultstatus": "success", "transactionid": "ff01"}]`)
	for _, tc := range []struct {
		provider, token, remote string
		body                    []byte
		want                    int
	}{
		{"netgsm", "wrong-token-of-the-wiring-test-0123456789", "192.0.2.10:443", body, http.StatusUnauthorized},
		{"netgsm", netgsmtest.WebhookToken, "198.51.100.9:443", body, http.StatusUnauthorized},
		{"verimor", netgsmtest.WebhookToken, "192.0.2.10:443", body, http.StatusNotFound},
		{"netgsm", netgsmtest.WebhookToken, "192.0.2.10:443", []byte(`{"html": "<b>"`), http.StatusUnprocessableEntity},
	} {
		if rec := post(ctx, tc.provider, tc.token, tc.remote, tc.body); rec.Code != tc.want || strings.Contains(rec.Body.String(), address) {
			t.Errorf("POST %s from %s = %d %s, want %d", tc.provider, tc.remote, rec.Code, rec.Body, tc.want)
		}
	}
	if n := len(store.All(feed)); n != stored {
		t.Fatalf("refused deliveries stored %d items", n-stored)
	}
}

// TestWire_ConsentNoticesCollectable (BE-114, T-367): the consent texts of
// listCurrentNotices carry collectable over the API, from the İYS gate of
// the configured adapter: with CONSENT_IYS_PROVIDER=none commercial_email
// is false (the client hides its checkbox) and commercial_push true; with
// an adapter (fake) commercial_email is true; a privacy notice has none.
func TestWire_ConsentNoticesCollectable(t *testing.T) {
	t.Parallel()
	collectable := func(t *testing.T, provider string) map[string]any {
		t.Helper()
		c := newTestContainer(t)
		c.consent.iys = nil
		c.consent.cfg = &config.Consent{UnsubscribeBaseURL: testUnsubscribeBase, IYSProvider: provider,
			IYSRecipientType: "BIREYSEL", IYSTimeout: 5 * time.Second}
		if err := c.build(); err != nil {
			t.Fatal(err)
		}
		for _, p := range []consent.Purpose{consent.PurposeCommercialEmail, consent.PurposeCommercialPush} {
			publishNotice(t, c, consent.KindConsentText, &p, "Ticari ileti izni ("+string(p)+")\n")
		}
		publishNotice(t, c, consent.KindPrivacyNotice, nil, "Aydınlatma metni\n")
		app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors})
		if err != nil {
			t.Fatal(err)
		}
		c.registerRoutes(app.V1)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/notices/current?audience=student", nil)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		var body struct {
			Items []map[string]any `json:"items"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("listCurrentNotices = %d %s", rec.Code, rec.Body)
		}
		out := map[string]any{}
		for _, it := range body.Items {
			v, has := it["collectable"]
			switch {
			case it["kind"] == string(consent.KindConsentText) && !has:
				t.Fatalf("consent text without collectable: %v", it)
			case it["kind"] != string(consent.KindConsentText) && has:
				t.Fatalf("%v notice with collectable", it["kind"])
			case has:
				out[it["purpose_code"].(string)] = v
			}
		}
		return out
	}
	if got := collectable(t, config.IYSProviderNone); got["commercial_email"] != false || got["commercial_push"] != true {
		t.Fatalf("none: collectable = %v", got)
	}
	if got := collectable(t, config.IYSProviderFake); got["commercial_email"] != true || got["commercial_push"] != true {
		t.Fatalf("fake: collectable = %v", got)
	}
}

// TestWire_ConsentNetgsmForgedSuccessIsNotSent (Q-16, BE-114): a "success"
// webhook item for Koro's upload that İYS's record does not confirm
// (/iys/search) never makes the submission "sent", even with the right
// token and source address; the same item from an address outside the
// allow list is refused (401) and not stored.
func TestWire_ConsentNetgsmForgedSuccessIsNotSent(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	clk := c.infra.clock.(*clock.Fake)
	var app *httpx.App
	post := func(ctx context.Context, remote string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/webhooks/iys/netgsm/"+netgsmtest.WebhookToken,
			strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	srv := netgsmtest.New(t, netgsmtest.WithClock(clk), netgsmtest.WithWebhook(func(ctx context.Context, body []byte) error {
		if rec := post(ctx, "192.0.2.10:443", body); rec.Code != http.StatusOK {
			return fmt.Errorf("webhook answered %d %s", rec.Code, rec.Body)
		}
		return nil
	}))
	store := netgsm.NewMemStore()
	c.consent.iys, c.consent.push, c.consent.netgsmStore = nil, nil, store
	c.consent.cfg = netgsmConsent(srv.URL, 0)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var err error
	if app, err = httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors}); err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	ctx := t.Context()
	const address = "ada.forged@ogrenci.example.test"
	purpose := consent.PurposeCommercialEmail
	uid := iysWireUser(t, c, address)
	grantID := grantCommercial(t, c, uid, purpose)
	sub, err := c.consent.store.SubmissionOf(ctx, grantID)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := domainevent.New(domainevent.ConsentRecorded{ConsentID: grantID, UserID: uid, Purpose: string(purpose),
		Action: string(consent.Granted)}, "consent", grantID)
	if err != nil {
		t.Fatal(err)
	}
	deliver := func() error {
		for _, s := range c.infra.subs.For(domainevent.TopicConsentRecorded) {
			if s.Name == consent.SubmitIYSTaskName {
				return s.Handle(ctx, ev)
			}
		}
		t.Fatal("consent.submit_iys is not subscribed")
		return nil
	}

	// İYS does not take the upload (an İYS error) and its answer is lost.
	srv.DropWebhooks(true)
	srv.FailNext("H103")
	if err := deliver(); err == nil {
		t.Fatal("delivery without İYS's answer succeeded")
	}
	consentRow, err := c.consent.store.ConsentByID(ctx, grantID)
	if err != nil {
		t.Fatal(err)
	}
	stamp := consentRow.OccurredAt.In(clock.Istanbul()).Format("2006-01-02 15:04:05")
	forged := []byte(`[{"iyscode": 700123, "brandcode": 700124, "submitid": "00000000-0000-4000-8000-999999999999",
		"resultstatus": "success", "creationdate": "` + stamp + `", "user_refcode": "` + sub.ID.String() + `",
		"transactionid": "ff00ff00", "type": "EPOSTA", "source": "HS_WEB", "recipient": "` + address + `", "status": "ONAY",
		"consentDate": "` + stamp + `", "recipientType": "BIREYSEL"}]`)
	feed := "netgsm:" + netgsmtest.IYSCode + "/" + netgsmtest.BrandCode
	if rec := post(ctx, "198.51.100.9:443", forged); rec.Code != http.StatusUnauthorized || len(store.All(feed)) != 0 {
		t.Fatalf("forged item from outside the allow list = %d, %d stored", rec.Code, len(store.All(feed)))
	}
	if rec := post(ctx, "192.0.2.10:443", forged); rec.Code != http.StatusOK {
		t.Fatalf("forged item with the token = %d %s (stored; checked on use)", rec.Code, rec.Body)
	}
	if err := deliver(); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("delivery over the forged success = %v, want unconfirmed", err)
	}
	got, err := c.consent.store.SubmissionOf(ctx, grantID)
	if err != nil || got.Status == consent.SubmissionSent || got.IYSTransactionID != nil || got.SentAt != nil {
		t.Fatalf("submission = %+v, %v; want it open (not sent)", got, err)
	}
	if _, _, _, held := srv.Held(address, "EPOSTA"); held {
		t.Fatal("the double holds a record: the test premise is wrong")
	}
}
