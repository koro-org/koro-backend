package notify_test

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/notify/pushtest"
	"koro/internal/platform/clock"
	"koro/internal/platform/problem"
)

// webPushEnv is a PushTransport over the web push double (and optionally
// the FCM double) with a memory store.
type webPushEnv struct {
	wp       *pushtest.WebPush
	store    notify.Store
	sessions *pushtest.Sessions // testSession is open
	pt       *notify.PushTransport
	key      string // VAPID public key of the adapter
}

func newWebPushEnv(t *testing.T, fcm *pushtest.FCM) webPushEnv {
	t.Helper()
	e := webPushEnv{wp: pushtest.NewWebPush(t), store: notify.NewMemStore(), sessions: pushtest.NewSessions(testSession)}
	clk := clock.NewFake(t0)
	w, err := notify.NewWebPush(notify.WebPushConfig{PrivateKey: pushtest.NewVAPIDKey(t), Subject: "mailto:push@koro.test",
		Clock: clk, HTTPClient: e.wp.Client()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := notify.PushTransportConfig{Tokens: e.store, Subscriptions: e.store, Sessions: e.sessions, WebPush: w, Clock: clk, Log: slog.New(slog.DiscardHandler)}
	if fcm != nil {
		if cfg.FCM, err = notify.NewFCM(notify.FCMConfig{ProjectID: fcm.ProjectID, CredentialsJSON: fcm.Credentials,
			BaseURL: fcm.Server.URL, Clock: clk, HTTPClient: fcm.Client()}); err != nil {
			t.Fatal(err)
		}
	}
	if e.pt, err = notify.NewPushTransport(cfg); err != nil {
		t.Fatal(err)
	}
	e.key = w.PublicKey()
	return e
}

// subscribe stores a browser subscription of the double for user.
func (e webPushEnv) subscribe(t *testing.T, user uuid.UUID) pushtest.WebPushSubscription {
	t.Helper()
	s := e.wp.Subscribe(t)
	storeSubscription(t, e.store, user, s.Endpoint, s.P256DH, s.Auth)
	return s
}

func storeSubscription(t *testing.T, store notify.Store, user uuid.UUID, endpoint string, p256dh, auth []byte) notify.WebPushSubscription {
	t.Helper()
	sum := sha256.Sum256([]byte(endpoint))
	got, err := store.UpsertWebPushSubscription(t.Context(), notify.WebPushSubscription{ID: uuid.NewV7(), UserID: user, SessionID: &testSession,
		Endpoint: endpoint, EndpointSHA256: sum[:], P256DH: p256dh, Auth: auth, CreatedAt: t0, LastSeenAt: t0}, notify.MaxWebPushSubscriptions)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPushTransport_WebPush_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	e := newWebPushEnv(t, nil)
	pushSenderContract(t, e.pt, pushFixture{
		Register:  func(t *testing.T, user uuid.UUID) string { return e.subscribe(t, user).ID },
		Delivered: func() int { return len(e.wp.Messages()) },
		Kill:      e.wp.Kill,
		Down:      func() { e.wp.SetDown(true) },
	})
}

// TestWebPush_EncryptedAndSignedForTheBrowser (RFC 8291, 8292): the
// browser decrypts the fixed text, the link and the opaque IDs; the VAPID
// token is signed by the configured key for the push service's origin; the
// delivery ID is the topic (collapse key); the endpoint never appears in
// the result.
func TestWebPush_EncryptedAndSignedForTheBrowser(t *testing.T) {
	t.Parallel()
	e := newWebPushEnv(t, nil)
	user := uuid.NewV7()
	sub := e.subscribe(t, user)
	m := pushMessage(user)
	id, err := e.pt.SendPush(t.Context(), m)
	if err != nil || id != "0:msg-1" {
		t.Fatalf("SendPush = %q, %v", id, err)
	}
	got := e.wp.Messages()
	if len(got) != 1 {
		t.Fatalf("messages = %d", len(got))
	}
	msg := got[0]
	if msg.DeviceToken != sub.ID || msg.Title != m.Title || msg.Body != m.Body || msg.Data["link"] != "/notifications" ||
		msg.Data["kind"] != "events.reminder" || msg.Data["delivery_id"] != m.DeliveryID.String() ||
		msg.Data["event_id"] != m.Data["event_id"] || msg.CollapseID != base64.RawURLEncoding.EncodeToString(m.DeliveryID[:]) {
		t.Fatalf("message = %+v", msg)
	}
	if keys := e.wp.VAPIDKeys(); len(keys) != 1 || keys[0] != e.key {
		t.Fatalf("VAPID k = %v, want the adapter's public key", keys)
	}
	// The cached token serves the next push to the same origin.
	if _, err := e.pt.SendPush(t.Context(), pushMessage(user)); err != nil || e.wp.Refused() != 0 {
		t.Fatalf("second push = %v (refused %d)", err, e.wp.Refused())
	}
}

// TestWebPush_GoneSubscriptionDeleted: 410 (and 404) delete the
// subscription at once; it is never tried again and the send succeeds
// (a retry cannot help).
func TestWebPush_GoneSubscriptionDeleted(t *testing.T) {
	t.Parallel()
	e := newWebPushEnv(t, nil)
	user := uuid.NewV7()
	dead, alive := e.subscribe(t, user), e.subscribe(t, user)
	e.wp.Kill(dead.ID)
	storeSubscription(t, e.store, user, pushtest.WebPushEndpointBase+"never-subscribed", alive.P256DH, alive.Auth) // 404
	if _, err := e.pt.SendPush(t.Context(), pushMessage(user)); err != nil {
		t.Fatal(err)
	}
	left, _ := e.store.WebPushSubscriptionsOf(t.Context(), user)
	if len(left) != 1 || left[0].Endpoint != alive.Endpoint {
		t.Fatalf("subscriptions left = %+v, want only the live one (410 and 404 deleted)", left)
	}
	if len(e.wp.Messages()) != 1 {
		t.Fatalf("messages = %d", len(e.wp.Messages()))
	}
}

// TestWebPush_UnknownPushServiceNeverContacted (SSRF guard): a stored
// subscription whose endpoint is not a known push service is dropped
// without a request.
func TestWebPush_UnknownPushServiceNeverContacted(t *testing.T) {
	t.Parallel()
	e := newWebPushEnv(t, nil)
	user := uuid.NewV7()
	ok := e.wp.Subscribe(t)
	storeSubscription(t, e.store, user, "https://169.254.169.254/latest/meta-data", ok.P256DH, ok.Auth)
	if id, err := e.pt.SendPush(t.Context(), pushMessage(user)); err != nil || id != "" {
		t.Fatalf("SendPush = %q, %v", id, err)
	}
	if left, _ := e.store.WebPushSubscriptionsOf(t.Context(), user); len(left) != 0 || len(e.wp.Messages()) != 0 {
		t.Fatalf("left = %d, messages = %d", len(left), len(e.wp.Messages()))
	}
}

// TestPushTransport_PreferenceChannels (BE-173): ChannelPush reaches the
// mobile devices, ChannelWebPush the browsers (web push subscriptions and
// FCM web tokens).
func TestPushTransport_PreferenceChannels(t *testing.T) {
	t.Parallel()
	fcm := pushtest.NewFCM(t)
	e := newWebPushEnv(t, fcm)
	user := uuid.NewV7()
	devices(t, e.store, user, notify.PushAndroid, "android-token")
	devices(t, e.store, user, notify.PushWeb, "fcm-web-token")
	e.subscribe(t, user)
	for _, tc := range []struct {
		name               string
		channels           []notify.Channel
		wantFCM, wantWebPN int
	}{
		{"mobile only", []notify.Channel{notify.ChannelPush}, 1, 0},
		{"browsers only", []notify.Channel{notify.ChannelWebPush}, 1, 1},
		{"both", nil, 2, 1},
	} {
		beforeFCM, beforeWeb := len(fcm.Messages()), len(e.wp.Messages())
		m := pushMessage(user)
		m.Channels = tc.channels
		if _, err := e.pt.SendPush(t.Context(), m); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		gotFCM, gotWeb := len(fcm.Messages())-beforeFCM, len(e.wp.Messages())-beforeWeb
		if gotFCM != tc.wantFCM || gotWeb != tc.wantWebPN {
			t.Errorf("%s: fcm %d, web push %d; want %d, %d", tc.name, gotFCM, gotWeb, tc.wantFCM, tc.wantWebPN)
		}
		if tc.name == "mobile only" && fcm.Messages()[len(fcm.Messages())-1].DeviceToken != "android-token" {
			t.Errorf("mobile only reached %q", fcm.Messages()[len(fcm.Messages())-1].DeviceToken)
		}
	}
}

func TestValidWebPushEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		endpoint string
		want     bool
	}{
		{"https://fcm.googleapis.com/fcm/send/abc:APA91b", true},
		{"https://updates.push.services.mozilla.com/wpush/v2/gAAAAAB", true},
		{"https://web.push.apple.com/QGuQyavXutnMH", true},
		{"https://wns2-par02p.notify.windows.com/w/?token=BQYAAAB", true},
		{"https://fcm.googleapis.com:443/fcm/send/x", true},
		{"http://fcm.googleapis.com/fcm/send/x", false},
		{"https://fcm.googleapis.com:8443/fcm/send/x", false},
		{"https://user:pw@fcm.googleapis.com/fcm/send/x", false},
		{"https://fcm.googleapis.com.evil.example/x", false},
		{"https://evilfcm.googleapis.com/x", false},
		{"https://notify.windows.com/x", false},
		{"https://FCM.googleapis.com/x", false},
		{"https://169.254.169.254/latest/meta-data", false},
		{"https://[::1]/x", false},
		{"https://localhost/x", false},
		{"https://fcm.googleapis.com/x#frag", false},
		{"https://fcm.googleapis.com/x y", false},
		{"https://fcm.googleapis.com/" + strings.Repeat("a", notify.MaxWebPushEndpoint), false},
		{"", false},
	} {
		if got := notify.ValidWebPushEndpoint(tc.endpoint); got != tc.want {
			t.Errorf("ValidWebPushEndpoint(%q) = %v, want %v", tc.endpoint, got, tc.want)
		}
	}
}

func TestNewWebPush_Validates(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	key := pushtest.NewVAPIDKey(t)
	for _, tc := range []struct {
		name string
		c    notify.WebPushConfig
	}{
		{"no clock", notify.WebPushConfig{PrivateKey: key, Subject: "mailto:a@koro.test"}},
		{"not base64url", notify.WebPushConfig{PrivateKey: "not a key!", Subject: "mailto:a@koro.test", Clock: clk}},
		{"short key", notify.WebPushConfig{PrivateKey: "AAAA", Subject: "mailto:a@koro.test", Clock: clk}},
		{"http subject", notify.WebPushConfig{PrivateKey: key, Subject: "http://koro.test", Clock: clk}},
		{"bare address", notify.WebPushConfig{PrivateKey: key, Subject: "a@koro.test", Clock: clk}},
	} {
		if _, err := notify.NewWebPush(tc.c); err == nil {
			t.Errorf("%s: NewWebPush accepted it", tc.name)
		} else if strings.Contains(err.Error(), key) {
			t.Errorf("%s: the error repeats the key", tc.name)
		}
	}
	w, err := notify.NewWebPush(notify.WebPushConfig{PrivateKey: key, Subject: "https://koro.test/contact", Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(w.PublicKey()); err != nil || len(raw) != 65 || raw[0] != 4 {
		t.Fatalf("PublicKey() = %q", w.PublicKey())
	}
}

// ---------------------------------------------------------------- service

func webPushFixture(t *testing.T, key string) fixture {
	t.Helper()
	return newFixture(t, func(d *notify.Deps) { d.WebPushKey = key })
}

func subscriptionInput(t *testing.T, endpoint string) notify.WebPushSubscriptionInput {
	t.Helper()
	s := pushtest.NewWebPush(t).Subscribe(t)
	if endpoint == "" {
		endpoint = s.Endpoint
	}
	return notify.WebPushSubscriptionInput{Endpoint: endpoint, P256DH: s.P256DH, Auth: s.Auth}
}

// TestWebPushSubscriptions_RegisterAndDelete: the owner registers and
// deletes; registering the endpoint again (another account) moves the one
// row; another user's subscription is 404; every change is audited without
// the endpoint.
func TestWebPushSubscriptions_RegisterAndDelete(t *testing.T) {
	t.Parallel()
	f := webPushFixture(t, "BKey")
	student, other := userActor(newID()), userActor(newID())
	in := subscriptionInput(t, "")
	id, err := f.svc.RegisterWebPushSubscription(t.Context(), student, in)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := f.svc.RegisterWebPushSubscription(t.Context(), student, in); err != nil || again != id {
		t.Fatalf("re-register = %v, %v; want the same ID", again, err)
	}
	if err := f.svc.DeleteWebPushSubscription(t.Context(), other, id); !errors.Is(err, notify.ErrWebPushSubscriptionNotFound) {
		t.Fatalf("delete by another user = %v, want 404", err)
	}
	moved, err := f.svc.RegisterWebPushSubscription(t.Context(), other, in)
	if err != nil || moved != id {
		t.Fatalf("register by another account = %v, %v; want the row moved", moved, err)
	}
	if mine, _ := f.store.WebPushSubscriptionsOf(t.Context(), student.UserID); len(mine) != 0 {
		t.Fatalf("the first account keeps %d subscriptions", len(mine))
	}
	if got, _ := f.store.WebPushSubscriptionsOf(t.Context(), other.UserID); len(got) != 1 || got[0].SessionID == nil ||
		*got[0].SessionID != other.SessionID {
		t.Fatalf("subscription = %+v, want bound to the session", got)
	}
	if err := f.svc.DeleteWebPushSubscription(t.Context(), other, id); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range auditRows(t, f) {
		if r.ResourceType == authz.TypeWebPushSubscription {
			n++
			if strings.Contains(string(r.Metadata), "fcm.googleapis.com") {
				t.Fatalf("audit record carries the endpoint: %s", r.Metadata)
			}
		}
	}
	if n != 4 {
		t.Fatalf("audit records = %d, want 4 (3 registrations, 1 deletion)", n)
	}
}

// TestWebPushSubscriptions_Validation: only known push services (SSRF),
// a P-256 point and a 16-byte secret; web push off is 503.
func TestWebPushSubscriptions_Validation(t *testing.T) {
	t.Parallel()
	f := webPushFixture(t, "BKey")
	a := userActor(newID())
	good := subscriptionInput(t, "")
	for _, tc := range []struct {
		name  string
		in    notify.WebPushSubscriptionInput
		field string
	}{
		{"metadata service", notify.WebPushSubscriptionInput{Endpoint: "https://169.254.169.254/x", P256DH: good.P256DH, Auth: good.Auth}, "body.endpoint"},
		{"own host", notify.WebPushSubscriptionInput{Endpoint: "https://api.koro.test/push", P256DH: good.P256DH, Auth: good.Auth}, "body.endpoint"},
		{"not a point", notify.WebPushSubscriptionInput{Endpoint: good.Endpoint, P256DH: make([]byte, 65), Auth: good.Auth}, "body.keys.p256dh"},
		{"missing key", notify.WebPushSubscriptionInput{Endpoint: good.Endpoint, Auth: good.Auth}, "body.keys.p256dh"},
		{"short secret", notify.WebPushSubscriptionInput{Endpoint: good.Endpoint, P256DH: good.P256DH, Auth: good.Auth[:8]}, "body.keys.auth"},
	} {
		_, err := f.svc.RegisterWebPushSubscription(t.Context(), a, tc.in)
		var pe *problem.Error
		if !errors.Is(err, problem.ErrValidation) || !errors.As(err, &pe) || len(pe.Fields()) == 0 || pe.Fields()[0].Location != tc.field {
			t.Errorf("%s: %v, want 422 on %s", tc.name, err, tc.field)
		}
	}
	off := webPushFixture(t, "")
	if _, err := off.svc.RegisterWebPushSubscription(t.Context(), a, good); !errors.Is(err, notify.ErrWebPushUnavailable) {
		t.Fatalf("web push off = %v, want 503", err)
	}
	st, err := off.svc.MyWebPushSettings(t.Context(), a)
	if err != nil || st.Enabled || st.VAPIDPublicKey != "" {
		t.Fatalf("settings (off) = %+v, %v", st, err)
	}
	st, err = f.svc.MyWebPushSettings(t.Context(), a)
	if err != nil || !st.Enabled || st.VAPIDPublicKey != "BKey" {
		t.Fatalf("settings = %+v, %v", st, err)
	}
}

// TestWebPushSubscriptions_CapDropsLeastRecentlySeen: at most
// MaxWebPushSubscriptions browsers per user; the next one replaces the
// least recently seen.
func TestWebPushSubscriptions_CapDropsLeastRecentlySeen(t *testing.T) {
	t.Parallel()
	f := webPushFixture(t, "BKey")
	a := userActor(newID())
	var first uuid.UUID
	for i := range notify.MaxWebPushSubscriptions + 1 {
		id, err := f.svc.RegisterWebPushSubscription(t.Context(), a, subscriptionInput(t, ""))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = id
		}
		f.clk.Advance(time.Minute)
	}
	subs, _ := f.store.WebPushSubscriptionsOf(t.Context(), a.UserID)
	if len(subs) != notify.MaxWebPushSubscriptions {
		t.Fatalf("subscriptions = %d, want %d", len(subs), notify.MaxWebPushSubscriptions)
	}
	for _, s := range subs {
		if s.ID == first {
			t.Fatal("the least recently seen subscription survived")
		}
	}
}

// TestSend_PushFollowsBothSwitches (BE-173): a transactional push is
// skipped only when both the mobile (push) and browser (web_push) switches
// of its category are off; otherwise it reaches the surfaces still on.
func TestSend_PushFollowsBothSwitches(t *testing.T) {
	t.Parallel()
	rec := &recordingPush{}
	f := newFixture(t, func(d *notify.Deps) {
		d.Templates = loadTemplates(t, withPromo(baseTemplates(t)))
		d.Push = rec
	})
	a := userActor(newID())
	uid := a.UserID
	send := func(key string) notify.Delivery {
		t.Helper()
		id := mustSend(t, f, notify.Message{UserID: &uid, Channel: notify.ChannelPush, Category: notify.CategoryTransactional,
			Template: "notify.test_notice", DedupeKey: key})
		return mustGet(t, f.store, id)
	}
	last := func() notify.PushMessage {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return rec.sent[len(rec.sent)-1]
	}
	if d := send("k0"); d.Status != notify.StatusSent || len(last().Channels) != 2 {
		t.Fatalf("defaults: %s, channels %v; want both surfaces", d.Status, last().Channels)
	}
	if _, err := f.svc.PutMyPreferences(t.Context(), a, []notify.Preference{
		{Category: notify.PrefEventUpdates, Channel: notify.ChannelPush, Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if d := send("k1"); d.Status != notify.StatusSent || len(last().Channels) != 1 || last().Channels[0] != notify.ChannelWebPush {
		t.Fatalf("mobile off: %s, channels %v; want sent to browsers only", d.Status, last().Channels)
	}
	if _, err := f.svc.PutMyPreferences(t.Context(), a, []notify.Preference{
		{Category: notify.PrefEventUpdates, Channel: notify.ChannelWebPush, Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if d := send("k2"); d.Status != notify.StatusSkippedPreference || len(rec.sent) != 2 {
		t.Fatalf("both off: %s (%d pushes), want skipped_preference", d.Status, len(rec.sent))
	}
}

// TestPreferences_WebPushChannel: 7 categories × 4 channels; web_push is on
// by default and can be switched off; security stays locked on it too.
func TestPreferences_WebPushChannel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	a := userActor(newID())
	prefs, err := f.svc.PutMyPreferences(t.Context(), a, []notify.Preference{
		{Category: notify.PrefFeedFollows, Channel: notify.ChannelWebPush, Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prefs) != 28 {
		t.Fatalf("pairs = %d, want 28", len(prefs))
	}
	for _, p := range prefs {
		if p.Channel == notify.ChannelWebPush && p.Enabled != (p.Category != notify.PrefFeedFollows) {
			t.Errorf("%s/web_push = %v", p.Category, p.Enabled)
		}
	}
	if _, err := f.svc.PutMyPreferences(t.Context(), a, []notify.Preference{
		{Category: notify.PrefSecurity, Channel: notify.ChannelWebPush, Enabled: false}}); !errors.Is(err, notify.ErrSecurityLocked) {
		t.Fatalf("security off on web_push = %v", err)
	}
}

// TestPrivacy_WebPushSubscriptions: the export names the subscription and
// its push service, never the endpoint or the keys; erasure deletes it.
func TestPrivacy_WebPushSubscriptions(t *testing.T) {
	t.Parallel()
	f := webPushFixture(t, "BKey")
	a := userActor(newID())
	in := subscriptionInput(t, "")
	id, err := f.svc.RegisterWebPushSubscription(t.Context(), a, in)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := f.mod.Privacy.Export(t.Context(), a.UserID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range sec.Items {
		if it["kind"] == "web_push_subscription" {
			found = it["web_push_subscription_id"] == id.String() && it["push_service"] == "fcm.googleapis.com"
		}
		for _, v := range it {
			if s, ok := v.(string); ok && strings.Contains(s, "/fcm/send/") {
				t.Fatalf("export leaks the endpoint: %v", it)
			}
		}
	}
	if !found {
		t.Fatalf("export = %+v", sec.Items)
	}
	if _, err := f.mod.Privacy.Erase(t.Context(), a.UserID); err != nil {
		t.Fatal(err)
	}
	if left, _ := f.store.WebPushSubscriptionsOf(t.Context(), a.UserID); len(left) != 0 {
		t.Fatalf("erasure left %d subscriptions", len(left))
	}
}
