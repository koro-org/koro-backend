package notify_test

import (
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/notify"
	"koro/internal/notify/pushtest"
)

// sessionEnv is a transport over the FCM and web push doubles with a session
// double of its own.
func sessionEnv(t *testing.T) (webPushEnv, *pushtest.FCM) {
	t.Helper()
	fcm := pushtest.NewFCM(t)
	return newWebPushEnv(t, fcm), fcm
}

func boundToken(t *testing.T, store notify.Store, user uuid.UUID, session *uuid.UUID, raw string) notify.PushToken {
	t.Helper()
	sum := sha256.Sum256([]byte(raw))
	got, err := store.UpsertPushToken(t.Context(), notify.PushToken{ID: uuid.NewV7(), UserID: user, SessionID: session,
		Platform: notify.PushAndroid, Token: raw, TokenSHA256: sum[:], CreatedAt: t0, LastSeenAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e webPushEnv) boundSubscription(t *testing.T, user uuid.UUID, session *uuid.UUID) pushtest.WebPushSubscription {
	t.Helper()
	s := e.wp.Subscribe(t)
	sum := sha256.Sum256([]byte(s.Endpoint))
	if _, err := e.store.UpsertWebPushSubscription(t.Context(), notify.WebPushSubscription{ID: uuid.NewV7(), UserID: user, SessionID: session,
		Endpoint: s.Endpoint, EndpointSHA256: sum[:], P256DH: s.P256DH, Auth: s.Auth, CreatedAt: t0, LastSeenAt: t0},
		notify.MaxWebPushSubscriptions); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPushTransport_OnlyOpenSessionsReceive (BE-182; 02 §6.3): device tokens
// and browser subscriptions are sent to only while their session is open.
// Those of a closed session and those without a session row are dropped
// without a provider call; the send is a success.
func TestPushTransport_OnlyOpenSessionsReceive(t *testing.T) {
	t.Parallel()
	e, fcm := sessionEnv(t)
	user := uuid.NewV7()
	closed := uuid.NewV7()
	boundToken(t, e.store, user, &testSession, "open-device")
	boundToken(t, e.store, user, &closed, "closed-device")
	boundToken(t, e.store, user, nil, "unbound-device")
	open := e.boundSubscription(t, user, &testSession)
	e.boundSubscription(t, user, &closed)
	e.boundSubscription(t, user, nil)

	if _, err := e.pt.SendPush(t.Context(), pushMessage(user)); err != nil {
		t.Fatal(err)
	}
	if got := fcm.Messages(); len(got) != 1 || got[0].DeviceToken != "open-device" {
		t.Fatalf("FCM messages = %+v, want only the open session's device", got)
	}
	for _, raw := range []string{"closed-device", "unbound-device"} {
		if n := fcm.Attempts(raw); n != 0 {
			t.Fatalf("%s tried %d times", raw, n)
		}
	}
	if got := e.wp.Messages(); len(got) != 1 || got[0].DeviceToken != open.ID {
		t.Fatalf("web push messages = %+v, want only the open session's browser", got)
	}
	active, _ := e.store.ActivePushTokens(t.Context(), user)
	if len(active) != 1 || active[0].Token != "open-device" {
		t.Fatalf("active tokens = %+v", active)
	}
	all, _ := e.store.PushTokensOf(t.Context(), user)
	if revoked := slices.IndexFunc(all, func(x notify.PushToken) bool { return x.RevokedAt == nil && x.Token != "open-device" }); revoked >= 0 {
		t.Fatalf("token %+v not revoked", all[revoked])
	}
	subs, _ := e.store.WebPushSubscriptionsOf(t.Context(), user)
	if len(subs) != 1 || subs[0].Endpoint != open.Endpoint {
		t.Fatalf("subscriptions = %+v, want only the open session's", subs)
	}
}

// TestPushTransport_SessionCloses_RegisterAgainRestores: a surface stops with
// its session, for good, until a new session registers it again.
func TestPushTransport_SessionCloses_RegisterAgainRestores(t *testing.T) {
	t.Parallel()
	e, fcm := sessionEnv(t)
	user, first, second := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	e.sessions.Open(first)
	token := boundToken(t, e.store, user, &first, "device-a")
	browser := e.boundSubscription(t, user, &first)
	send := func() {
		t.Helper()
		if _, err := e.pt.SendPush(t.Context(), pushMessage(user)); err != nil {
			t.Fatal(err)
		}
	}
	send()
	if len(fcm.Messages()) != 1 || len(e.wp.Messages()) != 1 {
		t.Fatalf("open: %d FCM, %d web push", len(fcm.Messages()), len(e.wp.Messages()))
	}

	e.sessions.Close(first)
	send()
	send()
	if len(fcm.Messages()) != 1 || len(e.wp.Messages()) != 1 {
		t.Fatalf("closed: %d FCM, %d web push", len(fcm.Messages()), len(e.wp.Messages()))
	}

	e.sessions.Open(second)
	again := boundToken(t, e.store, user, &second, "device-a")
	if again.ID != token.ID || again.RevokedAt != nil {
		t.Fatalf("registered again = %+v, want the same row, not revoked", again)
	}
	sum := sha256.Sum256([]byte(browser.Endpoint))
	if _, err := e.store.UpsertWebPushSubscription(t.Context(), notify.WebPushSubscription{ID: uuid.NewV7(), UserID: user, SessionID: &second,
		Endpoint: browser.Endpoint, EndpointSHA256: sum[:], P256DH: browser.P256DH, Auth: browser.Auth,
		CreatedAt: t0.Add(time.Hour), LastSeenAt: t0.Add(time.Hour)}, notify.MaxWebPushSubscriptions); err != nil {
		t.Fatal(err)
	}
	send()
	if len(fcm.Messages()) != 2 || len(e.wp.Messages()) != 2 {
		t.Fatalf("registered again: %d FCM, %d web push", len(fcm.Messages()), len(e.wp.Messages()))
	}
}

// TestPushTransport_SessionsUnreadable_NothingSentOrDropped: an unknown answer
// neither reaches a surface nor loses one; the delivery is retried.
func TestPushTransport_SessionsUnreadable_NothingSentOrDropped(t *testing.T) {
	t.Parallel()
	e, fcm := sessionEnv(t)
	user := uuid.NewV7()
	boundToken(t, e.store, user, &testSession, "device-b")
	e.boundSubscription(t, user, &testSession)
	boom := errors.New("sessions unreadable")
	e.sessions.Fail(boom)

	if _, err := e.pt.SendPush(t.Context(), pushMessage(user)); !errors.Is(err, boom) || errors.Is(err, notify.ErrPermanent) {
		t.Fatalf("SendPush() = %v, want a retryable error", err)
	}
	if len(fcm.Messages()) != 0 || len(e.wp.Messages()) != 0 {
		t.Fatalf("sent %d FCM, %d web push", len(fcm.Messages()), len(e.wp.Messages()))
	}
	if active, _ := e.store.ActivePushTokens(t.Context(), user); len(active) != 1 {
		t.Fatalf("active tokens = %+v", active)
	}
	if subs, _ := e.store.WebPushSubscriptionsOf(t.Context(), user); len(subs) != 1 {
		t.Fatalf("subscriptions = %+v", subs)
	}

	e.sessions.Fail(nil)
	if _, err := e.pt.SendPush(t.Context(), pushMessage(user)); err != nil || len(fcm.Messages()) != 1 || len(e.wp.Messages()) != 1 {
		t.Fatalf("after healing: %v, %d FCM, %d web push", err, len(fcm.Messages()), len(e.wp.Messages()))
	}
}

// TestPushTransport_NoSurface_NoSessionLookup: most users have no surface;
// their pushes cost no identity read.
func TestPushTransport_NoSurface_NoSessionLookup(t *testing.T) {
	t.Parallel()
	e, _ := sessionEnv(t)
	if id, err := e.pt.SendPush(t.Context(), pushMessage(uuid.NewV7())); err != nil || id != "" {
		t.Fatalf("SendPush() = %q, %v", id, err)
	}
	if n := e.sessions.Calls(); n != 0 {
		t.Fatalf("session lookups = %d", n)
	}
}
