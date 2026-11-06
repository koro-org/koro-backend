package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/notify/pushtest"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/privacy"
)

// matrixResendSecret is the svix secret of the wiring and matrix tests
// ("whsec_" + base64 of 32 bytes); bodies/notify/resend_bounce.json is
// signed with it at the matrix clock (testdata/authz/notify.yaml headers).
const matrixResendSecret = "whsec_a29yby1tYXRyaXgtd2ViaG9vay1zZWNyZXQtMzJieXQ="

// useMemStore installs the in-memory notify stores used by service,
// wiring and authz matrix tests (05 §13.2). Owner: BE-005, BE-015.
// Only tickets of the notify module edit this file (BE-001 R4).
func (w *notifyWiring) useMemStore() {
	w.store = notify.NewMemStore()
	w.tx = db.NopTx{}
	w.transport = notify.NewMemTransport()
	x, err := crypto.NewIndexer([]byte("koro-wiring-test-blind-index-key"))
	if err != nil {
		panic(err)
	}
	w.indexer = blindIndexer{x: x}
	w.cursor = &memCursor{}
	// Web push is announced (BE-173: the matrix subscribes browsers) with a
	// VAPID public key made at run time; no web push sender is built.
	vapid, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	w.webPushKey = base64.RawURLEncoding.EncodeToString(vapid.PublicKey().Bytes())
	// Test containers carry no loaded configuration: give the sender identity
	// and the webhook secret.
	w.cfg = &config.Notify{EmailProvider: config.EmailProviderMem, PushProvider: config.PushProviderAuto,
		SMTPFrom: "noreply@koro.test", SMTPFromName: "Koro",
		EmailWebhookResendSecret: config.NewSecret(matrixResendSecret)}
}

func sampleMessage(key string) notify.Message {
	return notify.Message{
		ToEmail: "ayse.yilmaz@ogrenci.edu.tr", Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: "notify.test_message", Locale: "tr", Data: map[string]any{"name": "Ayşe"}, DedupeKey: key,
	}
}

func buildNotify(t *testing.T, mod func(w *notifyWiring)) (*container, *notify.MemTransport) {
	t.Helper()
	c := newTestContainer(t)
	if mod != nil {
		mod(&c.notify)
	}
	if err := c.build(); err != nil {
		t.Fatalf("build() error = %v", err)
	}
	return c, c.notify.transport.(*notify.MemTransport)
}

func workerContext(t *testing.T) context.Context {
	t.Helper()
	return obs.WithTaskID(t.Context(), "task-1")
}

func TestWire_Notify_SendsThroughTheWiredServices(t *testing.T) {
	t.Parallel()
	c, tr := buildNotify(t, nil)
	if c.notify.svc == nil {
		t.Fatal("notify.svc is not set by build")
	}
	id, err := c.notify.svc.Send(workerContext(t), sampleMessage("k1"))
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	again, err := c.notify.svc.Send(workerContext(t), sampleMessage("k1"))
	if err != nil || again != id {
		t.Fatalf("repeated Send() = (%s, %v), want the previous delivery %s", again, err, id)
	}
	sent := tr.Sent()
	if len(sent) != 1 || sent[0].FromAddress != "noreply@koro.test" || sent[0].FromName != "Koro" {
		t.Fatalf("sent = %+v, want exactly one e-mail from the configured sender", sent)
	}
}

// Commercial messages go through consent.Checker.FilterActive of the wired
// consent module: a user without a grant is skipped, never mailed.
func TestWire_Notify_CommercialIsGatedByTheConsentModule(t *testing.T) {
	t.Parallel()
	recorded := &recordingConsent{}
	c, tr := buildNotify(t, func(w *notifyWiring) { w.consent = recorded })
	uid := uuid.NewV7()
	m := sampleMessage("promo")
	m.UserID, m.Category, m.UnsubscribeURL = &uid, notify.CategoryCommercial, "https://koro.test/u/1"
	// test_message is transactional: the template category guard holds.
	if _, err := c.notify.svc.Send(workerContext(t), m); !errors.Is(err, notify.ErrInvalidMessage) {
		t.Fatalf("Send(commercial on a transactional template) error = %v, want ErrInvalidMessage", err)
	}
	if len(tr.Sent()) != 0 {
		t.Fatal("an e-mail went out")
	}

	// Without the preset, the consent module itself is the checker.
	plain, _ := buildNotify(t, nil)
	if _, ok := plain.notify.deps.Consent.(consentFilter); !ok {
		t.Fatalf("consent checker = %T, want the consent module adapter", plain.notify.deps.Consent)
	}
	active, err := plain.notify.deps.Consent.FilterActive(t.Context(), []uuid.UUID{uid}, notify.PurposeCommercialEmail)
	if err != nil || len(active) != 0 {
		t.Fatalf("FilterActive() = (%v, %v), want nobody without a grant", active, err)
	}
}

type recordingConsent struct{}

func (recordingConsent) FilterActive(context.Context, []uuid.UUID, string) ([]uuid.UUID, error) {
	return nil, nil
}

func TestWire_Notify_WithoutAStoreTheBuildFails(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.notify.store = nil // and no database: never an in-memory fallback (05 §12.2)
	if err := c.build(); err == nil {
		t.Fatal("build() without a store succeeded")
	}
}

func TestWire_Notify_SMTPTransportBuiltFromConfig(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.notify.transport, c.notify.cfg = nil, nil // production state: SMTP from the loaded SMTP_* settings
	c.infra.cfg.Notify = config.Notify{
		EmailProvider: config.EmailProviderSMTP, PushProvider: config.PushProviderAuto, SMTPHost: "localhost", SMTPPort: 1025, SMTPTLS: config.SMTPTLSNone,
		SMTPFrom: "noreply@koro.test", SMTPFromName: "Koro", SMTPTimeout: time.Second,
	}
	if err := c.build(); err != nil {
		t.Fatalf("build() error = %v", err)
	}
	if _, isMem := c.notify.transport.(*notify.MemTransport); isMem {
		t.Fatal("test transport used instead of SMTP")
	}

	// An empty configuration (a section that was never loaded) must not start.
	bad := newTestContainer(t)
	bad.notify.transport, bad.notify.cfg = nil, nil
	if err := bad.build(); err == nil {
		t.Error("build() with an empty SMTP configuration succeeded")
	}
}

func TestWire_Notify_BlindIndexKeyRequired(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.notify.indexer = nil // production: CRYPTO_BLIND_INDEX_KEY; none loaded here
	if err := c.build(); err == nil {
		t.Fatal("build() without a blind index key succeeded")
	}
}

// TestWire_Notify_TasksWebhookAndPrivacy pins what notify registers: the
// audit alert job (every minute, default), the inbox processor of provider
// "email" and the KVKK provider.
func TestWire_Notify_TasksWebhookAndPrivacy(t *testing.T) {
	t.Parallel()
	c, _ := buildNotify(t, nil)
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if j.Name == notify.AuditAlertJobName {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"notify.send_audit_alerts * * * * * default"}; !slices.Equal(jobs, want) {
		t.Fatalf("notify jobs = %q, want %q", jobs, want)
	}
	if err := c.infra.inbox.Register(c.notify.mod.Webhook); err == nil {
		t.Fatal("the email inbox processor is not registered (a second registration succeeded)")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "notify" }) {
		t.Fatal("notify privacy provider is not registered")
	}
}

// TestWire_Notify_AuditAlerts runs the audit alert job over the wired audit
// module (06 H-40, H-43): a break-glass read notifies its subject, an admin
// entitlement grant notifies every platform admin, and a second pass sends
// nothing new.
func TestWire_Notify_AuditAlerts(t *testing.T) {
	t.Parallel()
	admin1, admin2, subject, support := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	c, _ := buildNotify(t, func(w *notifyWiring) {
		w.platformAdmins = func(context.Context) ([]uuid.UUID, error) { return []uuid.UUID{admin1, admin2}, nil }
	})
	ctx := workerContext(t)
	ent, org := uuid.NewV7(), uuid.NewV7()
	for _, e := range []audit.Event{
		{Action: string(authz.NotifyDeliveryList), Outcome: audit.Success, ActorUserID: &support, Reason: "Ticket 1: bounce check",
			Metadata: map[string]any{audit.MetaBreakGlass: true, audit.MetaSubjectUserID: subject.String()}},
		{Action: string(authz.EntitlementsEntitlementGrant), Outcome: audit.Success, ActorUserID: &admin1,
			ResourceType: authz.TypeEntitlement, ResourceID: &ent, OrgID: &org},
		{Action: string(authz.NotifyDeliveryList), Outcome: audit.Denied, ActorUserID: &support,
			Metadata: map[string]any{audit.MetaBreakGlass: true, audit.MetaSubjectUserID: subject.String()}},
	} {
		if err := c.audit.svc.Log(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	runJob(t, c, "audit.chain")
	runJob(t, c, notify.AuditAlertJobName)
	runJob(t, c, notify.AuditAlertJobName) // idempotent: the cursor and the dedupe keys

	want := map[uuid.UUID]string{subject: "notify.support_access", admin1: "notify.entitlement_granted", admin2: "notify.entitlement_granted"}
	for user, kind := range want {
		p, err := c.notify.svc.ListMine(t.Context(), authz.Actor{Kind: authz.KindUser, UserID: user}, notify.InAppQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) != 1 || p.Items[0].Kind != kind || p.Items[0].Data["audit_event_id"] == "" {
			t.Fatalf("user %s inbox = %+v, want one %s alert", user, p.Items, kind)
		}
	}
	if p, _ := c.notify.svc.ListMine(t.Context(), authz.Actor{Kind: authz.KindUser, UserID: support}, notify.InAppQuery{}); len(p.Items) != 0 {
		t.Fatalf("the support user got %d alerts, want none", len(p.Items))
	}
}

func runJob(t *testing.T, c *container, name string) {
	t.Helper()
	for _, j := range c.infra.jobs.Jobs() {
		if j.Name == name {
			if err := j.Handle(workerContext(t)); err != nil {
				t.Fatalf("job %s: %v", name, err)
			}
			return
		}
	}
	t.Fatalf("job %s is not scheduled", name)
}

// TestWire_Notify_WebhookAcceptsAnyContentType posts a signed delivery as
// SNS does (text/plain) and as JSON: the raw body reaches the signature
// check unparsed, and a forged one is 401.
func TestWire_Notify_WebhookAcceptsAnyContentType(t *testing.T) {
	t.Parallel()
	c, _ := buildNotify(t, nil)
	ready := httpx.NewReady()
	ready.Set(true)
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Version: apiVersion, Ready: ready})
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	secret := c.notify.deps.Webhooks.ResendSecret
	if len(secret) == 0 {
		t.Fatal("no resend secret wired")
	}
	body := []byte(`{"type":"email.delivered","data":{"email_id":"re_9","to":["x@koro.test"]}}`)
	ts := strconv.FormatInt(c.infra.clock.Now().Unix(), 10)
	for i, ct := range []string{"text/plain; charset=UTF-8", "application/json", ""} {
		id := "msg_ct_" + strconv.Itoa(i)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/webhooks/email/resend", bytes.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("svix-id", id)
		req.Header.Set("svix-timestamp", ts)
		req.Header.Set("svix-signature", notify.SvixSignature(secret, id, ts, body))
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("content type %q: status %d (%s), want 204", ct, rec.Code, rec.Body)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/webhooks/email/resend", bytes.NewReader(body))
	req.Header.Set("svix-id", "msg_forged")
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", notify.SvixSignature([]byte("forged-secret-forged-secret-1234"), "msg_forged", ts, body))
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"unauthenticated"`) {
		t.Fatalf("forged delivery: status %d (%s), want 401 unauthenticated", rec.Code, rec.Body)
	}
}

// TestWire_Push_FollowsTheIdentitySession (BE-182): a token registered through
// the real services reaches its device while identity holds the session open
// and is revoked on the first push after identity closes it. Registering from
// a new session brings it back.
func TestWire_Push_FollowsTheIdentitySession(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u := e.user(t, nil)
	actor := func() authz.Actor {
		t.Helper()
		raw := e.session(t, u, identity.ClientWeb)
		a, err := e.resolve(t.Context(), httpx.TransportCookie, raw)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	fcm := pushtest.NewFCM(t)
	clk := e.c.infra.clock
	f, err := notify.NewFCM(notify.FCMConfig{ProjectID: fcm.ProjectID, CredentialsJSON: fcm.Credentials, BaseURL: fcm.Server.URL,
		Clock: clk, HTTPClient: fcm.Client()})
	if err != nil {
		t.Fatal(err)
	}
	store := e.c.notify.store
	pt, err := notify.NewPushTransport(notify.PushTransportConfig{Tokens: store, Sessions: lateSessions{w: &e.c.notify}, FCM: f,
		Clock: clk, Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	const device = "wire-session-device"
	in := notify.PushTokenInput{Platform: notify.PushAndroid, Token: device}
	if _, err := e.c.notify.svc.RegisterPushToken(t.Context(), actor(), in); err != nil {
		t.Fatal(err)
	}
	msg := notify.PushMessage{DeliveryID: uuid.NewV7(), UserID: u.ID, Kind: "events.reminder", Title: "Yeni bir bildirimin var"}
	if _, err := pt.SendPush(t.Context(), msg); err != nil || len(fcm.Messages()) != 1 {
		t.Fatalf("push on an open session = %v, %d messages", err, len(fcm.Messages()))
	}
	if n, err := e.c.identity.svc.RevokeAll(t.Context(), u.ID, identity.RevokeAdmin); err != nil || n != 1 {
		t.Fatalf("RevokeAll() = %d, %v", n, err)
	}
	if _, err := pt.SendPush(t.Context(), msg); err != nil || len(fcm.Messages()) != 1 {
		t.Fatalf("push after the session closed = %v, %d messages", err, len(fcm.Messages()))
	}
	if active, _ := store.ActivePushTokens(t.Context(), u.ID); len(active) != 0 {
		t.Fatalf("active tokens = %+v, want the token revoked", active)
	}
	if _, err := e.c.notify.svc.RegisterPushToken(t.Context(), actor(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := pt.SendPush(t.Context(), msg); err != nil || len(fcm.Messages()) != 2 {
		t.Fatalf("push on the new session = %v, %d messages", err, len(fcm.Messages()))
	}
}

// TestWire_Push_SessionReaderNotWired_NothingIsSent: without the identity hook
// the lookup fails, so the delivery is retried instead of sent.
func TestWire_Push_SessionReaderNotWired_NothingIsSent(t *testing.T) {
	t.Parallel()
	if _, err := (lateSessions{w: &notifyWiring{}}).UnrevokedSessionIDs(t.Context(), uuid.NewV7()); err == nil {
		t.Fatal("an unwired session reader answered")
	}
	c, _ := buildNotify(t, nil)
	if c.notify.unrevokedSessions == nil {
		t.Fatal("identity did not bind the session reader")
	}
}

// TestWire_Notify_WebPushFromConfig (BE-173): PUSH_VAPID_* turn the
// webpush adapter on under NOTIFY_PUSH_PROVIDER=auto; the push transport
// serves its public key, which getMyWebPushConfig announces. A broken key
// stops the build without repeating it.
func TestWire_Notify_WebPushFromConfig(t *testing.T) {
	t.Parallel()
	key := pushtest.NewVAPIDKey(t)
	c, _ := buildNotify(t, func(w *notifyWiring) {
		cfg := *w.cfg
		cfg.PushVAPIDPrivateKey, cfg.PushVAPIDSubject = config.NewSecret(key), "mailto:push@koro.test"
		w.cfg, w.webPushKey = &cfg, ""
	})
	pt, ok := c.notify.deps.Push.(*notify.PushTransport)
	if !ok || pt.WebPushKey() == "" || c.notify.deps.WebPushKey != pt.WebPushKey() {
		t.Fatalf("push = %T, deps key %q", c.notify.deps.Push, c.notify.deps.WebPushKey)
	}
	st, err := c.notify.svc.MyWebPushSettings(t.Context(), authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(),
		StudentStatus: authz.StudentNone})
	if err != nil || !st.Enabled || st.VAPIDPublicKey != pt.WebPushKey() {
		t.Fatalf("settings = %+v, %v", st, err)
	}

	broken := newTestContainer(t)
	cfg := *broken.notify.cfg
	cfg.PushVAPIDPrivateKey, cfg.PushVAPIDSubject = config.NewSecret("not-a-vapid-key"), "mailto:push@koro.test"
	broken.notify.cfg = &cfg
	if err := broken.build(); err == nil || !strings.Contains(err.Error(), "PUSH_VAPID_PRIVATE_KEY") ||
		strings.Contains(err.Error(), "not-a-vapid-key") {
		t.Fatalf("build() with a broken key = %v", err)
	}
}
