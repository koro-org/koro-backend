package consent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
	"uuid"
)

// IYSClient delivers consent changes of commercial purposes to İYS (İleti
// Yönetim Sistemi; 6563, H-14). The real HTTP adapter is BE-055; it resolves
// the recipient address of UserID itself (consent holds no contact data).
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
	// failed; it is retried with back-off and by consent.iys_reconcile.
	Submit(ctx context.Context, r IYSRecord) (IYSReceipt, error)
}

// IYSRecord is one consent change for İYS. It carries IDs and codes only.
type IYSRecord struct {
	SubmissionID uuid.UUID
	ConsentID    uuid.UUID
	UserID       uuid.UUID
	Channel      IYSChannel
	Action       Action // granted → ONAY, withdrawn → RET
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
// real client; Fail makes the next submissions fail.
type FakeIYS struct {
	mu   sync.Mutex
	sent []IYSRecord
	fail error
}

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
	return IYSReceipt{TransactionID: "fake-" + r.ConsentID.String()}, nil
}

// Sent returns the accepted submissions in order.
func (f *FakeIYS) Sent() []IYSRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}
