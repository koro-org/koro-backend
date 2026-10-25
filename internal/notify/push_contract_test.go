package notify_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"uuid"

	"koro/internal/notify"
	"koro/internal/notify/pushtest"
	"koro/internal/platform/clock"
)

// pushFixture is what the PushSender contract suite needs from the
// adapter's environment.
type pushFixture struct {
	// Register gives user one device and returns its device token.
	Register func(t *testing.T, user uuid.UUID) string
	// Delivered is the number of pushes the provider has accepted; nil for
	// an adapter that reaches no device (noop).
	Delivered func() int
	// Kill makes the provider report token unregistered; nil skips the step.
	Kill func(token string)
	// Down takes the provider down for good (the last step); nil skips it.
	Down func()
}

// pushSenderContract is the contract suite of notify.PushSender (BE-113):
// NoopPush and PushTransport over the FCM and APNs doubles of pushtest pass
// it. A new push provider is a new adapter file, one line in
// cmd/koro/wire_notify.go pushProviders and a Test<Adapter>_ContractSuite
// that runs this suite. The steps run in order and share the provider.
func pushSenderContract(t *testing.T, s notify.PushSender, fx pushFixture) {
	t.Helper()
	ctx := t.Context()
	if name := s.Name(); name == "" || name != strings.ToLower(name) || s.Name() != name {
		t.Fatalf("Name() = %q: a stable lower-case name is stored on every delivery", name)
	}
	delivered := func() int {
		if fx.Delivered == nil {
			return 0
		}
		return fx.Delivered()
	}
	noToken := func(t *testing.T, token, id string, err error) {
		t.Helper()
		if token != "" && (strings.Contains(id, token) || (err != nil && strings.Contains(err.Error(), token))) {
			t.Fatalf("result leaks the device token: %q, %v", id, err)
		}
	}

	t.Run("a user without devices is a no-op", func(t *testing.T) {
		before := delivered()
		id, err := s.SendPush(ctx, pushMessage(uuid.NewV7()))
		if err != nil || id != "" || delivered() != before {
			t.Fatalf("SendPush(no devices) = %q, %v; delivered %d → %d", id, err, before, delivered())
		}
	})

	user := uuid.NewV7()
	token := fx.Register(t, user)
	t.Run("the device of the user gets the push", func(t *testing.T) {
		before := delivered()
		id, err := s.SendPush(ctx, pushMessage(user))
		noToken(t, token, id, err)
		if err != nil {
			t.Fatalf("SendPush = %q, %v", id, err)
		}
		if fx.Delivered != nil && delivered() != before+1 {
			t.Fatalf("provider accepted %d pushes, want %d", delivered(), before+1)
		}
	})

	t.Run("concurrent pushes are all handled", func(t *testing.T) {
		before := delivered()
		var wg sync.WaitGroup
		errs := make([]error, 6)
		for i := range errs {
			wg.Go(func() { _, errs[i] = s.SendPush(ctx, pushMessage(user)) })
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		if fx.Delivered != nil && delivered() != before+len(errs) {
			t.Fatalf("provider accepted %d pushes, want %d", delivered()-before, len(errs))
		}
	})

	if fx.Kill != nil {
		t.Run("an unregistered device is dropped without an error", func(t *testing.T) {
			lone := uuid.NewV7()
			dead := fx.Register(t, lone)
			fx.Kill(dead)
			for range 2 {
				id, err := s.SendPush(ctx, pushMessage(lone))
				noToken(t, dead, id, err)
				if err != nil {
					t.Fatalf("SendPush(dead device) = %v, want nil (the token is revoked, a retry cannot help)", err)
				}
			}
		})
	}

	if fx.Down != nil {
		t.Run("a provider outage is a transient error", func(t *testing.T) {
			fx.Down()
			id, err := s.SendPush(ctx, pushMessage(user))
			noToken(t, token, id, err)
			if err == nil || errors.Is(err, notify.ErrPermanent) {
				t.Fatalf("SendPush while down = %v, want a transient error (the worker retries)", err)
			}
		})
	}
}

func TestNoopPush_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	pushSenderContract(t, notify.NoopPush{}, pushFixture{
		Register: func(*testing.T, uuid.UUID) string { return "" },
	})
}

// registerer registers devices of one platform in a memory store.
func registerer(store notify.Store, platform notify.PushPlatform, token func(i int) string) func(*testing.T, uuid.UUID) string {
	var mu sync.Mutex
	n := 0
	return func(t *testing.T, user uuid.UUID) string {
		t.Helper()
		mu.Lock()
		n++
		raw := token(n)
		mu.Unlock()
		sum := sha256.Sum256([]byte(raw))
		if _, err := store.UpsertPushToken(t.Context(), notify.PushToken{ID: uuid.NewV7(), UserID: user, Platform: platform,
			Token: raw, TokenSHA256: sum[:], CreatedAt: t0, LastSeenAt: t0}); err != nil {
			t.Fatal(err)
		}
		return raw
	}
}

func TestPushTransport_FCM_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	fcm := pushtest.NewFCM(t)
	store := notify.NewMemStore()
	f, err := notify.NewFCM(notify.FCMConfig{ProjectID: fcm.ProjectID, CredentialsJSON: fcm.Credentials,
		BaseURL: fcm.Server.URL, Clock: clock.NewFake(t0), HTTPClient: fcm.Client()})
	if err != nil {
		t.Fatal(err)
	}
	pt, err := notify.NewPushTransport(notify.PushTransportConfig{Tokens: store, FCM: f, Clock: clock.NewFake(t0),
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	pushSenderContract(t, pt, pushFixture{
		Register:  registerer(store, notify.PushAndroid, func(i int) string { return fmt.Sprintf("contract-android-token-%d", i) }),
		Delivered: func() int { return len(fcm.Messages()) },
		Kill:      fcm.Kill,
		Down:      func() { fcm.SetDown(true) },
	})
}

func TestPushTransport_APNs_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	apns := pushtest.NewAPNs(t)
	store := notify.NewMemStore()
	a, err := notify.NewAPNs(notify.APNsConfig{KeyPEM: apns.KeyPEM, KeyID: apns.KeyID, TeamID: apns.TeamID, Topic: apns.Topic,
		BaseURL: apns.Server.URL, Clock: clock.NewFake(t0), HTTPClient: apns.Client()})
	if err != nil {
		t.Fatal(err)
	}
	pt, err := notify.NewPushTransport(notify.PushTransportConfig{Tokens: store, APNs: a, Clock: clock.NewFake(t0),
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	pushSenderContract(t, pt, pushFixture{
		// APNs device tokens are 64 hex digits.
		Register:  registerer(store, notify.PushIOS, func(i int) string { return fmt.Sprintf("%064x", 0xc0ffee00+i) }),
		Delivered: func() int { return len(apns.Messages()) },
		Kill:      apns.Kill,
		Down:      apns.Server.Close,
	})
}
