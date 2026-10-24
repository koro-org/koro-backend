package outreach_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"uuid"

	"koro/internal/consent"
	"koro/internal/notify"
	"koro/internal/outreach"
)

// heldConsents is consent's filter with the İYS send-time hold on
// (CONSENT_IYS_SYNC_MAX_AGE, BE-103): while held, commercial e-mail answers
// consent.ErrIYSSyncStale; every other purpose answers as usual.
type heldConsents struct {
	*fakeConsents
	mu   sync.Mutex
	held bool
}

func (h *heldConsents) set(held bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = held
}

func (h *heldConsents) FilterActive(ctx context.Context, ids []uuid.UUID, p consent.Purpose) ([]uuid.UUID, error) {
	h.mu.Lock()
	held := h.held
	h.mu.Unlock()
	if held && p == consent.PurposeCommercialEmail {
		return nil, fmt.Errorf("%w (email: never reconciled)", consent.ErrIYSSyncStale)
	}
	return h.fakeConsents.FilterActive(ctx, ids, p)
}

// TestRunDispatch_CommercialHeldWhileIYSSyncStale (BE-103, T-312): while
// consent holds commercial e-mail because the İYS reconciliation is late, a
// company's dispatch sends nothing and goes back to scheduled (the task is
// retried, the resume job picks it up); once the reconciliation is fresh
// the retry reaches every consenting student once.
func TestRunDispatch_CommercialHeldWhileIYSSyncStale(t *testing.T) {
	t.Parallel()
	held := &heldConsents{held: true}
	e := newEnv(t, func(e *env, d *outreach.Deps) {
		held.fakeConsents = e.consents
		d.Consents = held
	})
	e.grantDispatches(t, companyA, 1)
	ids := e.audience.add(12, true, uniKey(uniA))
	e.users.add(false, ids...)
	e.consents.grant(consent.PurposeCommercialEmail, ids...)
	d := e.schedule(t, "company_member", e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)), outreach.ChannelEmail)

	for range 2 {
		if err := e.deliver(t); !errors.Is(err, consent.ErrIYSSyncStale) {
			t.Fatalf("run while held = %v, want ErrIYSSyncStale", err)
		}
		if got := e.dispatch(t, d); got.Status != outreach.DispatchScheduled || e.sender.attempts != 0 {
			t.Fatalf("held dispatch is %s after %d notify calls, want scheduled and none", got.Status, e.sender.attempts)
		}
	}

	held.set(false)
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	if got := e.sender.to(notify.ChannelEmail); len(got) != len(ids) || e.sender.attempts != len(ids) {
		t.Fatalf("after the hold: %d e-mails in %d attempts, want %d", len(got), e.sender.attempts, len(ids))
	}
	if got := e.dispatch(t, d); got.Status != outreach.DispatchCompleted {
		t.Fatalf("dispatch %s, want completed", got.Status)
	}
}
