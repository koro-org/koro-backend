// Package smscontract is the behaviour every SMS adapter shows (BE-172):
// Run drives an adapter and the provider double behind it through the same
// cases, so that identity works with any of them unchanged. The in-memory
// adapter (smsfake) and the Netgsm adapter (over an httptest double) pass
// it; a new provider's adapter plugs into the same Run with its own double.
// Never import it outside tests.
package smscontract

import (
	"context"
	"errors"
	"strings"
	"testing"

	"koro/internal/platform/sms"
)

// Adapter is the port the consumers declare (identity.SMSSender).
type Adapter interface {
	Send(ctx context.Context, m sms.Message) (sms.Receipt, error)
}

// Outcome is how the provider double answers the next send.
type Outcome int

// Outcomes of the provider double.
const (
	// BadNumber: the provider refuses the number.
	BadNumber Outcome = iota + 1
	// Throttled: the provider's rate limit or a passing system error.
	Throttled
	// Refused: a permanent refusal (credentials, sender name, credit).
	Refused
	// Down: the provider cannot be reached or fails (HTTP 5xx).
	Down
)

// Harness is the adapter under test and the provider behind it.
type Harness struct {
	Adapter Adapter
	// Delivered returns what the provider accepted, oldest first, with the
	// recipient in E.164.
	Delivered func() []sms.Message
	// Next makes the provider answer the next send with o (once).
	Next func(o Outcome)
}

// Example values of the suite: a number of the example range the project
// uses in tests (never a real subscriber) and an OTP-like text.
const (
	Number = "+905550000001"
	Text   = "Koro dogrulama kodunuz: 482915. Kodu kimseyle paylasmayin."
	Ref    = "sms-code-0190f3a1"
)

// Run runs the contract against the adapters newHarness builds (one per
// case, cases in parallel).
func Run(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Helper()
	setup := func(t *testing.T) Harness {
		t.Helper()
		h := newHarness(t)
		if h.Adapter == nil || h.Delivered == nil || h.Next == nil {
			t.Fatal("smscontract: incomplete harness")
		}
		return h
	}
	good := sms.Message{To: Number, Text: Text, Ref: Ref}

	t.Run("delivers a message once", func(t *testing.T) {
		t.Parallel()
		h := setup(t)
		rc, err := h.Adapter.Send(t.Context(), good)
		if err != nil || rc.ID == "" {
			t.Fatalf("Send = %+v, %v; want a receipt", rc, err)
		}
		got := h.Delivered()
		if len(got) != 1 || got[0].To != Number || got[0].Text != Text {
			t.Fatalf("delivered %d messages (%v), want the one sent", len(got), got)
		}
	})

	t.Run("refuses an unusable message without a call", func(t *testing.T) {
		t.Parallel()
		h := setup(t)
		for _, c := range []struct {
			name   string
			m      sms.Message
			number bool
		}{
			{"landline", sms.Message{To: "+902120000001", Text: Text, Ref: Ref}, true},
			{"national form", sms.Message{To: "05550000001", Text: Text, Ref: Ref}, true},
			{"empty text", sms.Message{To: Number, Ref: Ref}, false},
			{"long text", sms.Message{To: Number, Text: strings.Repeat("a", sms.MaxText+1), Ref: Ref}, false},
			{"non-ASCII text", sms.Message{To: Number, Text: "Doğrulama kodu 482915", Ref: Ref}, false},
			{"no reference", sms.Message{To: Number, Text: Text}, false},
		} {
			_, err := h.Adapter.Send(t.Context(), c.m)
			if !errors.Is(err, sms.ErrRejected) || errors.Is(err, sms.ErrInvalidNumber) != c.number {
				t.Errorf("%s: err = %v, want ErrRejected (invalid number: %v)", c.name, err, c.number)
			}
			noSecret(t, c.name, err, c.m)
		}
		if n := len(h.Delivered()); n != 0 {
			t.Fatalf("%d messages reached the provider", n)
		}
	})

	t.Run("maps the provider's answers", func(t *testing.T) {
		t.Parallel()
		h := setup(t)
		for _, c := range []struct {
			name                        string
			o                           Outcome
			rejected, number, throttled bool
		}{
			{"bad number", BadNumber, true, true, false},
			{"throttled", Throttled, false, false, true},
			{"refused", Refused, true, false, false},
			{"down", Down, false, false, false},
		} {
			h.Next(c.o)
			_, err := h.Adapter.Send(t.Context(), good)
			switch {
			case err == nil:
				t.Errorf("%s: no error", c.name)
			case errors.Is(err, sms.ErrRejected) != c.rejected, errors.Is(err, sms.ErrInvalidNumber) != c.number,
				errors.Is(err, sms.ErrThrottled) != c.throttled:
				t.Errorf("%s: err = %v; want rejected %v, invalid number %v, throttled %v", c.name, err, c.rejected, c.number, c.throttled)
			}
			noSecret(t, c.name, err, good)
		}
		if n := len(h.Delivered()); n != 0 {
			t.Fatalf("%d refused messages counted as delivered", n)
		}
		if _, err := h.Adapter.Send(t.Context(), good); err != nil {
			t.Fatalf("a send after the failures: %v", err)
		}
	})

	t.Run("stops on a cancelled context", func(t *testing.T) {
		t.Parallel()
		h := setup(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := h.Adapter.Send(ctx, good); err == nil {
			t.Fatal("Send on a cancelled context succeeded")
		}
		if n := len(h.Delivered()); n != 0 {
			t.Fatalf("%d messages delivered after cancellation", n)
		}
	})
}

// noSecret fails when err repeats the number (in any form) or the text.
func noSecret(t *testing.T, name string, err error, m sms.Message) {
	t.Helper()
	if err == nil {
		return
	}
	s := err.Error()
	for _, x := range []string{m.To, sms.National(m.To), strings.TrimPrefix(m.To, "+"), m.Text, "482915"} {
		if x != "" && strings.Contains(s, x) {
			t.Errorf("%s: the error repeats the number or the text: %q", name, s)
		}
	}
}
