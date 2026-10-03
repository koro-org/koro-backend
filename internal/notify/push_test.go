package notify_test

import (
	"crypto/sha256"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/notify"
	"koro/internal/notify/pushtest"
	"koro/internal/platform/clock"
)

// devices registers tokens for one user in a memory store.
func devices(t *testing.T, store notify.Store, user uuid.UUID, platform notify.PushPlatform, tokens ...string) map[string]uuid.UUID {
	t.Helper()
	ids := map[string]uuid.UUID{}
	for _, raw := range tokens {
		sum := sha256.Sum256([]byte(raw))
		got, err := store.UpsertPushToken(t.Context(), notify.PushToken{ID: uuid.NewV7(), UserID: user, Platform: platform,
			Token: raw, TokenSHA256: sum[:], CreatedAt: t0, LastSeenAt: t0})
		if err != nil {
			t.Fatal(err)
		}
		ids[raw] = got.ID
	}
	return ids
}

func pushMessage(user uuid.UUID) notify.PushMessage {
	return notify.PushMessage{DeliveryID: uuid.NewV7(), UserID: user, Kind: "events.reminder",
		Title: "Yeni bir bildirimin var", Body: "Ayrıntılar uygulamada.", LinkPath: "/notifications",
		Data: map[string]string{"event_id": "0199a4f0-0000-7000-8000-00000000000e"}}
}

// TestFCM_InvalidToken_Revoked (02 §6.3): an FCM UNREGISTERED answer revokes
// the device token, which is never tried again; the other devices still
// get the message; the service account assertion is exchanged once and the
// payload carries no personal data.
func TestFCM_InvalidToken_Revoked(t *testing.T) {
	t.Parallel()
	fcm := pushtest.NewFCM(t)
	clk := clock.NewFake(t0)
	store := notify.NewMemStore()
	user := uuid.NewV7()
	ids := devices(t, store, user, notify.PushAndroid, "android-good-token", "android-dead-token")
	devices(t, store, user, notify.PushWeb, "web-good-token")
	fcm.Kill("android-dead-token")

	f, err := notify.NewFCM(notify.FCMConfig{ProjectID: fcm.ProjectID, CredentialsJSON: fcm.Credentials,
		BaseURL: fcm.Server.URL, Clock: clk, HTTPClient: fcm.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	pt, err := notify.NewPushTransport(notify.PushTransportConfig{Tokens: store, FCM: f, Clock: clk,
		Log: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	m := pushMessage(user)
	id, err := pt.SendPush(t.Context(), m)
	if err != nil || !strings.HasPrefix(id, "projects/koro-test-project/messages/") {
		t.Fatalf("SendPush = %q, %v", id, err)
	}
	active, _ := store.ActivePushTokens(t.Context(), user)
	if len(active) != 2 {
		t.Fatalf("active tokens = %d, want 2 (the dead one revoked)", len(active))
	}
	all, _ := store.PushTokensOf(t.Context(), user)
	for _, tok := range all {
		if tok.ID == ids["android-dead-token"] && (tok.RevokedAt == nil || !tok.RevokedAt.Equal(t0)) {
			t.Fatalf("dead token = %+v, want revoked at %v", tok, t0)
		}
	}
	got := fcm.Messages()
	if len(got) != 2 {
		t.Fatalf("FCM accepted %d messages, want 2", len(got))
	}
	for _, msg := range got {
		if msg.Title != m.Title || msg.Data["link"] != "/notifications" || msg.Data["delivery_id"] != m.DeliveryID.String() ||
			msg.Data["event_id"] == "" || msg.CollapseID != m.DeliveryID.String() || len(msg.Data) != 4 {
			t.Fatalf("payload = %+v", msg)
		}
	}

	// The next push does not try the revoked token; the access token is reused.
	if _, err := pt.SendPush(t.Context(), pushMessage(user)); err != nil {
		t.Fatal(err)
	}
	if n := fcm.Attempts("android-dead-token"); n != 1 {
		t.Fatalf("dead token tried %d times, want 1", n)
	}
	if n := fcm.TokenExchanges.Load(); n != 1 {
		t.Fatalf("token exchanges = %d, want 1 (cached)", n)
	}
	// Near the expiry the access token is refreshed.
	clk.Advance(56 * time.Minute)
	if _, err := pt.SendPush(t.Context(), pushMessage(user)); err != nil || fcm.TokenExchanges.Load() != 2 {
		t.Fatalf("refresh = %v, exchanges %d", err, fcm.TokenExchanges.Load())
	}

	// FCM down: no device reached → retryable error, nothing revoked.
	fcm.SetDown(true)
	if _, err := pt.SendPush(t.Context(), pushMessage(user)); err == nil || errors.Is(err, notify.ErrPermanent) {
		t.Fatalf("SendPush while FCM is down = %v, want a transient error", err)
	}
	if active, _ := store.ActivePushTokens(t.Context(), user); len(active) != 2 {
		t.Fatal("an outage revoked tokens")
	}
	// A user without devices is a no-op.
	if id, err := pt.SendPush(t.Context(), pushMessage(uuid.NewV7())); err != nil || id != "" {
		t.Fatalf("no devices = %q, %v", id, err)
	}
	for _, secret := range []string{"android-good-token", "android-dead-token", "ya29.fake-access", "PRIVATE KEY"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log contains %q: %s", secret, logs.String())
		}
	}
}

// TestAPNs_JWTRotation: the ES256 provider token is reused for up to 50
// minutes, then signed again; an ExpiredProviderToken answer rotates it at
// once and the push is retried; 410 Unregistered revokes the device token.
// Requests use HTTP/2.
func TestAPNs_JWTRotation(t *testing.T) {
	t.Parallel()
	apns := pushtest.NewAPNs(t)
	clk := clock.NewFake(t0)
	store := notify.NewMemStore()
	user := uuid.NewV7()
	good, dead := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	ids := devices(t, store, user, notify.PushIOS, good, dead)
	a, err := notify.NewAPNs(notify.APNsConfig{KeyPEM: apns.KeyPEM, KeyID: apns.KeyID, TeamID: apns.TeamID, Topic: apns.Topic,
		BaseURL: apns.Server.URL, Clock: clk, HTTPClient: apns.Client()})
	if err != nil {
		t.Fatal(err)
	}
	pt, _ := notify.NewPushTransport(notify.PushTransportConfig{Tokens: store, APNs: a, Clock: clk, Log: slog.New(slog.DiscardHandler)})
	ctx := t.Context()
	send := func() {
		t.Helper()
		if _, err := pt.SendPush(ctx, pushMessage(user)); err != nil {
			t.Fatalf("SendPush = %v", err)
		}
	}
	distinct := func() []string {
		var out []string
		for _, tok := range apns.ProviderTokens() {
			if len(out) == 0 || out[len(out)-1] != tok {
				out = append(out, tok)
			}
		}
		return out
	}

	apns.Kill(dead)
	send() // t0: token A; the dead device is revoked
	clk.Advance(30 * time.Minute)
	send() // reuses A
	if tokens := distinct(); len(tokens) != 1 || apns.IssuedAt(tokens[0]) != t0.Unix() {
		t.Fatalf("provider tokens within 30 min = %d, want 1 issued at t0", len(tokens))
	}
	clk.Advance(21 * time.Minute) // 51 min: past APNsTokenTTL
	send()
	tokens := distinct()
	if len(tokens) != 2 || apns.IssuedAt(tokens[1]) != t0.Add(51*time.Minute).Unix() {
		t.Fatalf("after 51 min: %d tokens, want a second one issued at +51m", len(tokens))
	}

	clk.Advance(5 * time.Minute)
	apns.ExpireNextToken()
	send() // 403 ExpiredProviderToken → new token C, retried with success
	tokens = distinct()
	if len(tokens) != 3 || apns.IssuedAt(tokens[2]) != t0.Add(56*time.Minute).Unix() {
		t.Fatalf("after ExpiredProviderToken: %d tokens, want a third issued at +56m", len(tokens))
	}
	if n := len(apns.Messages()); n != 4 {
		t.Fatalf("APNs accepted %d pushes, want 4", n)
	}
	if apns.NonHTTP2Requests() != 0 {
		t.Fatal("APNs requests did not use HTTP/2")
	}
	all, _ := store.PushTokensOf(ctx, user)
	for _, tok := range all {
		if tok.ID == ids[dead] && tok.RevokedAt == nil {
			t.Fatal("410 Unregistered did not revoke the token")
		}
	}
	// A malformed device token is revoked without a request.
	malformed := devices(t, store, user, notify.PushIOS, "not-hex")
	before := len(apns.ProviderTokens())
	send()
	if len(apns.ProviderTokens()) != before+1 { // only the good device
		t.Fatal("a malformed token reached APNs")
	}
	if act, _ := store.ActivePushTokens(ctx, user); len(act) != 1 || act[0].ID == malformed["not-hex"] {
		t.Fatalf("active after malformed = %+v", act)
	}
}

func TestPush_ConfigErrors(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	fcm := pushtest.NewFCM(t)
	for _, c := range []notify.FCMConfig{
		{ProjectID: "x", CredentialsJSON: fcm.Credentials, Clock: clk},
		{ProjectID: fcm.ProjectID, CredentialsJSON: []byte("{"), Clock: clk},
		{ProjectID: fcm.ProjectID, CredentialsJSON: []byte(`{"type":"authorized_user"}`), Clock: clk},
		{ProjectID: fcm.ProjectID, CredentialsJSON: []byte(`{"type":"service_account","client_email":"a@b","private_key":"junk"}`), Clock: clk},
		{ProjectID: fcm.ProjectID, CredentialsJSON: fcm.Credentials, Clock: clk, BaseURL: "http://fcm.local"},
	} {
		_, err := notify.NewFCM(c)
		if err == nil || strings.Contains(err.Error(), "BEGIN") {
			t.Errorf("NewFCM(%s) = %v", c.ProjectID, err)
		}
	}
	apns := pushtest.NewAPNs(t)
	for _, c := range []notify.APNsConfig{
		{KeyPEM: apns.KeyPEM, KeyID: "short", TeamID: apns.TeamID, Topic: apns.Topic, Clock: clk},
		{KeyPEM: apns.KeyPEM, KeyID: apns.KeyID, TeamID: apns.TeamID, Topic: "bad topic", Clock: clk},
		{KeyPEM: []byte(fcm.PrivateKey), KeyID: apns.KeyID, TeamID: apns.TeamID, Topic: apns.Topic, Clock: clk},
		{KeyPEM: apns.KeyPEM, KeyID: apns.KeyID, TeamID: apns.TeamID, Topic: apns.Topic},
	} {
		if _, err := notify.NewAPNs(c); err == nil {
			t.Errorf("NewAPNs accepted %+v", c.KeyID+"|"+c.Topic)
		}
	}
	if _, err := notify.NewPushTransport(notify.PushTransportConfig{Tokens: notify.NewMemStore(), Clock: clk, Log: slog.New(slog.DiscardHandler)}); err == nil {
		t.Fatal("NewPushTransport without providers succeeded")
	}
}
