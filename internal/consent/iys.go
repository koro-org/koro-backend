package consent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"
)

// IYSClient delivers consent changes of commercial purposes to İYS (İleti
// Yönetim Sistemi; 6563, H-14). The real HTTP adapter is IYSHTTP (BE-055);
// it resolves the recipient address of UserID itself through IYSRecipients
// (consent holds no contact data, T-159).
//
// Submit must be safe to repeat for the same consent: a retry after a lost
// answer sends the same state again (İYS keeps the latest state per
// recipient, channel and type).
type IYSClient interface {
	// Name identifies the client in logs ("noop", "fake", "iys").
	Name() string
	// Real reports whether the client reaches İYS. CONSENT_IYS_REQUIRED
	// refuses to start without one.
	Real() bool
	// Submit sends one consent change. An error leaves the submission
	// failed; it is retried with back-off and by consent.iys_reconcile,
	// except an error wrapping ErrIYSPermanent, which raises the alarm at
	// once and is not retried by the task (reconcile still tries daily).
	Submit(ctx context.Context, r IYSRecord) (IYSReceipt, error)
}

// ErrIYSPermanent marks an İYS submission a retry cannot fix: İYS refused
// the record (validation) or the recipient address is gone (T-159). The
// DPO handles it (alarm).
var ErrIYSPermanent = errors.New("consent: İYS submission refused permanently")

// ErrIYSRecipientGone is returned by IYSRecipients when the user has no
// address for the channel any more (account erased before the delivery).
var ErrIYSRecipientGone = errors.New("consent: İYS recipient address is not available")

// IYSRecipients resolves the İYS recipient (e-mail address or phone number)
// of a user for a channel. identity provides it (late binding in
// cmd/koro/wire_consent.go); the address is used for the request only and
// never stored or logged.
type IYSRecipients interface {
	IYSRecipient(ctx context.Context, userID uuid.UUID, ch IYSChannel) (string, error)
}

// IYSRecord is one consent change for İYS. It carries IDs and codes only.
type IYSRecord struct {
	SubmissionID uuid.UUID
	ConsentID    uuid.UUID
	UserID       uuid.UUID
	Channel      IYSChannel
	Action       Action  // granted → ONAY, withdrawn → RET
	Source       Channel // where the consent act happened → İYS "source"
	OccurredAt   time.Time
}

// IYSReceipt is İYS's answer.
type IYSReceipt struct {
	TransactionID string
}

// NoopIYS is the client while no real İYS adapter is configured (dev, until
// BE-055): submissions end as "skipped".
type NoopIYS struct{}

// Name implements IYSClient.
func (NoopIYS) Name() string { return "noop" }

// Real implements IYSClient.
func (NoopIYS) Real() bool { return false }

// Submit implements IYSClient; it is never called for a client that is not
// real (the submission is skipped instead).
func (NoopIYS) Submit(context.Context, IYSRecord) (IYSReceipt, error) {
	return IYSReceipt{}, errors.New("consent: no İYS client configured")
}

// FakeIYS is the recording İYS client of tests (05 §13.2). It counts as a
// real client; Fail makes the next submissions fail. It also stands for
// İYS's consent state (IYSStatusReader, BE-103): an accepted submission
// becomes the recipient's state unless a newer one is held, SetStatus
// records a state given elsewhere (a refusal in İYS itself) and
// FailStatuses makes the reads fail.
type FakeIYS struct {
	mu          sync.Mutex
	sent        []IYSRecord
	fail        error
	states      map[fakeIYSKey]IYSState
	statusFail  error
	statusCalls int
}

type fakeIYSKey struct {
	user uuid.UUID
	ch   IYSChannel
}

var _ IYSStatusReader = (*FakeIYS)(nil)

// Name implements IYSClient.
func (*FakeIYS) Name() string { return "fake" }

// Real implements IYSClient.
func (*FakeIYS) Real() bool { return true }

// Fail makes every following Submit return err (nil: succeed again).
func (f *FakeIYS) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

// Submit implements IYSClient.
func (f *FakeIYS) Submit(_ context.Context, r IYSRecord) (IYSReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return IYSReceipt{}, f.fail
	}
	f.sent = append(f.sent, r)
	status := IYSApproved
	if r.Action == Withdrawn {
		status = IYSRefused
	}
	f.apply(r.UserID, r.Channel, IYSState{Status: status, At: r.OccurredAt.Truncate(time.Second), Source: iysSource(r.Source)})
	return IYSReceipt{TransactionID: "fake-" + r.ConsentID.String()}, nil
}

// apply keeps st unless a newer state is held (İYS keeps the latest
// record per recipient and channel). Call with f.mu held.
func (f *FakeIYS) apply(userID uuid.UUID, ch IYSChannel, st IYSState) {
	if f.states == nil {
		f.states = map[fakeIYSKey]IYSState{}
	}
	k := fakeIYSKey{user: userID, ch: ch}
	if cur, ok := f.states[k]; ok && cur.At.After(st.At) {
		return
	}
	f.states[k] = st
}

// SetStatus makes st İYS's state of userID on ch, whatever was held.
func (f *FakeIYS) SetStatus(userID uuid.UUID, ch IYSChannel, st IYSState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.states == nil {
		f.states = map[fakeIYSKey]IYSState{}
	}
	f.states[fakeIYSKey{user: userID, ch: ch}] = st
}

// FailStatuses makes every following Statuses return err (nil: answer again).
func (f *FakeIYS) FailStatuses(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusFail = err
}

// StatusCalls counts the Statuses calls.
func (f *FakeIYS) StatusCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusCalls
}

// Statuses implements IYSStatusReader.
func (f *FakeIYS) Statuses(_ context.Context, ch IYSChannel, userIDs []uuid.UUID) (map[uuid.UUID]IYSState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	switch {
	case f.statusFail != nil:
		return nil, f.statusFail
	case len(userIDs) > IYSStatusBatch:
		return nil, fmt.Errorf("consent: fake İYS: %d users in one status read (at most %d)", len(userIDs), IYSStatusBatch)
	}
	out := make(map[uuid.UUID]IYSState, len(userIDs))
	for _, id := range userIDs {
		if st, ok := f.states[fakeIYSKey{user: id, ch: ch}]; ok {
			out[id] = st
		}
	}
	return out, nil
}

// Sent returns the accepted submissions in order.
func (f *FakeIYS) Sent() []IYSRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}
