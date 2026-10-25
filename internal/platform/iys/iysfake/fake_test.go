package iysfake_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"uuid"

	"koro/internal/platform/iys"
	"koro/internal/platform/iys/iyscontract"
	"koro/internal/platform/iys/iysfake"
)

var errDown = errors.New("fake İYS: 503 service unavailable")

func harness(f *iysfake.Fake, adapter iyscontract.Adapter) iyscontract.Harness {
	return iyscontract.Harness{
		Adapter: adapter, Lag: iysfake.DefaultLag,
		Act: func(_ *testing.T, address string, typ iys.Type, st iys.Status) {
			f.Set(address, typ, iys.State{Status: st, Source: "IYS_WEB"})
		},
		Held: func(_ *testing.T, address string, typ iys.Type) (iys.State, bool) { return f.Held(address, typ) },
		SetDown: func(down bool) {
			var err error
			if down {
				err = errDown
			}
			f.FailSubmits(err)
			f.FailReads(err)
		},
	}
}

// TestFake_Contract: the in-memory İYS behaves like every adapter.
func TestFake_Contract(t *testing.T) {
	t.Parallel()
	iyscontract.Run(t, func(_ *testing.T, env iyscontract.Env) iyscontract.Harness {
		f := iysfake.New(iysfake.Options{Clock: env.Clock, Recipients: env.Recipients})
		return harness(f, f)
	})
}

// submitOnly is an adapter that only registers consents (an integrator
// without reads).
type submitOnly struct{ f *iysfake.Fake }

func (s submitOnly) Name() string { return "submit-only" }
func (s submitOnly) Submit(ctx context.Context, c iys.Consent) (iys.Receipt, error) {
	return s.f.Submit(ctx, c)
}

// TestFake_Contract_SubmitOnly: an adapter without reads passes the cases
// of the capabilities it has.
func TestFake_Contract_SubmitOnly(t *testing.T) {
	t.Parallel()
	iyscontract.Run(t, func(_ *testing.T, env iyscontract.Env) iyscontract.Harness {
		f := iysfake.New(iysfake.Options{Clock: env.Clock, Recipients: env.Recipients})
		return harness(f, submitOnly{f: f})
	})
}

// TestAddresses_RoundTrip: the fake's own directory maps users to
// synthetic addresses and back; anything else is no user.
func TestAddresses_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var d iysfake.Addresses
	u := uuid.NewV7()
	a, _ := d.Recipient(ctx, u, iys.Email)
	if back, err := d.User(ctx, iys.Email, a); err != nil || back != u || !strings.HasSuffix(a, "@iys-fake.test") {
		t.Fatalf("round trip %q → %v, %v", a, back, err)
	}
	for _, a := range []string{"", "ada@example.test", "u00@iys-fake.test", "uzz" + "@iys-fake.test"} {
		if _, err := d.User(ctx, iys.Email, a); !errors.Is(err, iys.ErrRecipientGone) {
			t.Errorf("User(%q) = %v, want ErrRecipientGone", a, err)
		}
	}
}
