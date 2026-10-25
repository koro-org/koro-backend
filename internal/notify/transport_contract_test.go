package notify_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"koro/internal/notify"
)

// emailFixture is what the EmailTransport contract suite needs from the
// adapter's environment.
type emailFixture struct {
	// Delivered returns the Message-IDs (without angle brackets) the
	// provider has accepted so far, in any order.
	Delivered func(t *testing.T) []string
	// Refuse makes the provider refuse every following message: for good
	// (permanent) or for now. nil skips the refusal steps (a provider whose
	// sandbox cannot be told to refuse).
	Refuse func(permanent bool)
}

// emailTransportContract is the contract suite of notify.EmailTransport
// (BE-113): every adapter passes it — MemTransport and SMTPTransport here,
// against the fake SMTP server and Mailpit (integration). A new provider
// (SES, Resend …) is a new adapter file plus one line in
// cmd/koro/wire_notify.go emailTransports, and a Test<Adapter>_ContractSuite
// that runs this suite. The steps run in order and share the provider.
func emailTransportContract(t *testing.T, tr notify.EmailTransport, fx emailFixture) {
	t.Helper()
	ctx := t.Context()
	name := tr.Name()
	if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`).MatchString(name) || tr.Name() != name {
		t.Fatalf("Name() = %q: a stable lower-case provider name is stored on every delivery", name)
	}
	msg := func(id string) notify.Email {
		e := sampleEmail()
		e.MessageID = id + "@koro.test"
		return e
	}
	delivered := func(t *testing.T, id string) bool {
		return slices.Contains(fx.Delivered(t), id+"@koro.test")
	}

	t.Run("an e-mail is delivered and gets a provider message ID", func(t *testing.T) {
		id, err := tr.Send(ctx, msg("contract-1"))
		if err != nil || id == "" {
			t.Fatalf("Send() = %q, %v; want a provider message ID", id, err)
		}
		if !delivered(t, "contract-1") {
			t.Fatalf("provider did not receive contract-1: %v", fx.Delivered(t))
		}
	})

	t.Run("a cancelled context sends nothing and is not permanent", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := tr.Send(cctx, msg("contract-cancelled"))
		if err == nil || errors.Is(err, notify.ErrPermanent) {
			t.Fatalf("Send(cancelled) error = %v, want a transient error", err)
		}
		if delivered(t, "contract-cancelled") {
			t.Fatal("a cancelled send reached the provider")
		}
	})

	t.Run("concurrent sends are all delivered", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Go(func() {
				_, errs[i] = tr.Send(ctx, msg(fmt.Sprintf("contract-par-%d", i)))
			})
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		for i := range errs {
			if !delivered(t, fmt.Sprintf("contract-par-%d", i)) {
				t.Fatalf("contract-par-%d missing", i)
			}
		}
	})

	if fx.Refuse == nil {
		return
	}
	t.Run("a temporary refusal is transient", func(t *testing.T) {
		fx.Refuse(false)
		if _, err := tr.Send(ctx, msg("contract-deferred")); err == nil || errors.Is(err, notify.ErrPermanent) {
			t.Fatalf("Send() error = %v, want a transient error", err)
		}
	})
	t.Run("a refusal for good wraps ErrPermanent", func(t *testing.T) {
		fx.Refuse(true)
		if _, err := tr.Send(ctx, msg("contract-refused")); !errors.Is(err, notify.ErrPermanent) {
			t.Fatalf("Send() error = %v, want ErrPermanent", err)
		}
	})
}

func TestMemTransport_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	tr := notify.NewMemTransport()
	emailTransportContract(t, tr, emailFixture{
		Delivered: func(*testing.T) []string {
			var ids []string
			for _, e := range tr.Sent() {
				ids = append(ids, e.MessageID)
			}
			return ids
		},
		Refuse: func(permanent bool) {
			if permanent {
				tr.FailWith(notify.Permanent(errors.New("mem: refused")))
				return
			}
			tr.FailWith(errors.New("mem: deferred"))
		},
	})
}

func TestSMTPTransport_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	srv := startFakeSMTP(t, nil)
	tr := newTransport(t, srv, func(c *notify.SMTPConfig) { c.Timeout = 5 * time.Second })
	emailTransportContract(t, tr, emailFixture{
		Delivered: func(*testing.T) []string { return srv.messageIDs() },
		Refuse: func(permanent bool) {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if permanent {
				srv.rcptReply = "550 5.1.1 user unknown"
				return
			}
			srv.rcptReply = "451 4.3.0 try again later"
		},
	})
}

var reMessageID = regexp.MustCompile(`(?m)^Message-ID: <([^>]+)>`)

// messageIDs returns the Message-IDs of the messages the fake accepted.
func (s *fakeSMTP) messageIDs() []string {
	_, data := s.received()
	var ids []string
	for _, d := range data {
		if m := reMessageID.FindStringSubmatch(d); m != nil {
			ids = append(ids, m[1])
		}
	}
	return ids
}
