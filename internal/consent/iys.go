package consent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/iys"
	"koro/internal/platform/iys/iysfake"
)

// The İYS port of consent (İleti Yönetim Sistemi; 6563, H-14; BE-055,
// BE-103, BE-105): small interfaces declared by their users here, one per
// capability. The consent.submit_iys and consent.iys_reconcile handlers
// deliver through IYSClient; consent.iys_status_sync reads through
// IYSFeedReader (the change feed) and IYSStateReader (single states). An
// İYS adapter (a package of internal/platform/iys: the official API, an
// integrator, the fake) implements IYSSubmitter and whichever readers it
// can; cmd/koro/wire_consent.go picks it by configuration and wraps its
// Submit with NewIYSClient. An integrator that pushes (Netgsm) also
// implements IYSPushReceiver: receiveIYSWebhook hands it the deliveries.
// No adapter type, İYS error code or URL reaches this package: adapters
// map them to the errors below.

// IYSClient delivers consent changes of commercial purposes to İYS. The
// adapter resolves the recipient address of UserID itself (consent holds
// no contact data, T-159).
//
// Submit must be safe to repeat for the same consent: a retry after a lost
// answer sends the same state again (İYS keeps the latest state per
// recipient, channel and type).
type IYSClient interface {
	// Name identifies the client in logs ("noop", "fake", "iys", "netgsm").
	Name() string
	// Real reports whether the client reaches İYS. CONSENT_IYS_REQUIRED
	// refuses to start without one; without one a grant of a requires_iys
	// purpose is refused (ErrIYSUnavailable, BE-105).
	Real() bool
	// Submit sends one consent change. An error leaves the submission
	// failed; it is retried with back-off and by consent.iys_reconcile,
	// except an error wrapping ErrIYSPermanent, which raises the alarm at
	// once and is not retried by the task (reconcile still tries daily).
	// A refusal İYS already held is a success.
	Submit(ctx context.Context, r IYSRecord) (IYSReceipt, error)
}

// Errors of the port (the same values as internal/platform/iys's, so an
// adapter's error matches with errors.Is).
var (
	// ErrIYSPermanent marks an İYS request a retry cannot fix: İYS refused
	// the record (validation, authorisation, subscription) or the
	// recipient address is gone (T-159). The DPO handles it (alarm).
	ErrIYSPermanent = iys.ErrRejected
	// ErrIYSRecipientGone: the user has no address for the channel any
	// more (account erased before the delivery).
	ErrIYSRecipientGone = iys.ErrRecipientGone
	// ErrIYSTooLate: İYS registers a consent taken outside İYS only within
	// three business days (Yönetmelik m.7/11–12); the grant is not valid
	// for commercial messages. Wrapped with ErrIYSPermanent; alarmed.
	ErrIYSTooLate = iys.ErrTooLate
	// ErrIYSThrottled: a rate limit of İYS or of the adapter; retried later.
	ErrIYSThrottled = iys.ErrThrottled
)

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
	// TransactionID is İYS's reference; "" when İYS held the state already.
	TransactionID string
}

// IYSStatus is İYS's consent status of a recipient on a channel.
type IYSStatus = iys.Status

// İYS statuses.
const (
	IYSApproved IYSStatus = iys.Approved
	IYSRefused  IYSStatus = iys.Refused
)

// IYSState is İYS's record of a recipient on a channel: Status, At (the
// consent date; İYS keeps seconds; zero when only the status is known) and
// Source (İYS's source code, e.g. "HS_WEB" for a brand channel, "IYS_WEB"
// for İYS's own; audit only, kept when it is a plain code).
type IYSState = iys.State

// IYSSubmitter is what consent needs of every İYS adapter: registering a
// consent change (iys.Consent: İYS's type, status, source and date).
type IYSSubmitter interface {
	// Name identifies the adapter in logs ("iys", "netgsm", "fake").
	Name() string
	// Submit registers one consent change; safe to repeat. A refusal İYS
	// already held is a success (iys.Receipt.AlreadyHeld).
	Submit(ctx context.Context, c iys.Consent) (iys.Receipt, error)
}

// IYSFeedReader is the change feed of an adapter (consent.iys_status_sync):
// the approvals and refusals recipients gave İYS itself, pulled from İYS
// or pushed to the adapter by an integrator. The adapter resolves addresses
// to users itself; an address is never returned.
type IYSFeedReader interface {
	// Feed names the feed (one cursor per feed) and how far back it reaches.
	Feed() iys.Feed
	// Changes returns the next page after cursor ("" = the oldest change
	// the feed holds).
	Changes(ctx context.Context, cursor string) (iys.ChangePage, error)
}

// IYSStateReader reads İYS's current state of single recipients
// (consent.iys_status_sync: the reconciliation after a gap in the feed and
// the check of grants İYS has not confirmed).
type IYSStateReader interface {
	// Statuses returns the state of the users' addresses for type t, keyed
	// by user; at most IYSStatusBatch users. A user İYS has no record of,
	// or whose address is gone, is absent. An error fails the batch.
	Statuses(ctx context.Context, t iys.Type, userIDs []uuid.UUID) (map[uuid.UUID]iys.State, error)
}

// IYSPushReceiver takes the webhook deliveries of an integrator that pushes
// İYS's answers and the changes recipients give İYS itself
// (receiveIYSWebhook; BE-110: Netgsm). The adapter authenticates and
// stores a delivery itself (its change feed and its answers to uploads);
// consent only routes it.
type IYSPushReceiver interface {
	// Name is the adapter's name: the {provider} of the webhook path.
	Name() string
	// Receive authenticates and stores one delivery and returns how many
	// items were new: iys.ErrUnauthenticated (nothing stored),
	// iys.ErrRejected (not the integrator's format) or a transient error.
	Receive(ctx context.Context, d iys.Delivery) (int, error)
}

// NoopIYS is the client while no İYS adapter is configured
// (CONSENT_IYS_PROVIDER=none): commercial grants are refused
// (ErrIYSUnavailable) and the submissions of withdrawals end as "skipped".
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

// IYSAdapterClient delivers consent changes through an İYS adapter: the
// IYSClient of a configured adapter.
type IYSAdapterClient struct{ a IYSSubmitter }

var _ IYSClient = (*IYSAdapterClient)(nil)

// NewIYSClient wraps an İYS adapter's Submit in consent's delivery port.
func NewIYSClient(a IYSSubmitter) *IYSAdapterClient { return &IYSAdapterClient{a: a} }

// Name implements IYSClient.
func (c *IYSAdapterClient) Name() string { return c.a.Name() }

// Real implements IYSClient: every adapter reaches İYS.
func (*IYSAdapterClient) Real() bool { return true }

// Submit implements IYSClient. Mapping: action granted → ONAY, withdrawn →
// RET; channel email → EPOSTA, sms → MESAJ, call → ARAMA; consents.channel
// → source (iysSource); consentDate is occurred_at.
func (c *IYSAdapterClient) Submit(ctx context.Context, r IYSRecord) (IYSReceipt, error) {
	typ, ok := iysTypes[r.Channel]
	if !ok {
		return IYSReceipt{}, fmt.Errorf("%w: unknown İYS channel %q", ErrIYSPermanent, r.Channel)
	}
	status, ok := map[Action]iys.Status{Granted: iys.Approved, Withdrawn: iys.Refused}[r.Action]
	if !ok {
		return IYSReceipt{}, fmt.Errorf("%w: unknown action %q", ErrIYSPermanent, r.Action)
	}
	rc, err := c.a.Submit(ctx, iys.Consent{Ref: r.SubmissionID.String(), UserID: r.UserID, Type: typ, Status: status,
		Source: iysSource(r.Source), ConsentDate: r.OccurredAt})
	if err != nil {
		return IYSReceipt{}, err
	}
	return IYSReceipt{TransactionID: rc.TransactionID}, nil
}

var (
	iysTypes    = map[IYSChannel]iys.Type{IYSEmail: iys.Email, IYSSMS: iys.Message, IYSCall: iys.Call}
	iysChannels = map[iys.Type]IYSChannel{iys.Email: IYSEmail, iys.Message: IYSSMS, iys.Call: IYSCall}
)

// iysSource maps consents.channel to the İYS source (T-184: admin and
// import to be confirmed with İYS / counsel).
func iysSource(ch Channel) string {
	switch ch {
	case ChannelMobile:
		return "HS_MOBIL"
	case ChannelEmailLink:
		return "HS_EPOSTA"
	default: // web, admin (erasure on the user's request), import
		return "HS_WEB"
	}
}

// FakeIYS is the recording İYS client of tests (05 §13.2): consent's
// delivery and both readers over the in-memory İYS (iysfake) with the
// fake's own synthetic addresses and no time rules. It counts as a real
// client; Fail makes the next submissions fail. It also stands for İYS's
// consent state: an accepted submission becomes the recipient's state (a
// record dated before the held one is refused, as İYS does), SetStatus
// records a state given elsewhere (with an İYS source, "IYS_*", it also
// enters the change feed: a refusal in İYS itself) and FailStatuses makes
// the reads fail.
type FakeIYS struct {
	once   sync.Once
	f      *iysfake.Fake
	client *IYSAdapterClient

	mu   sync.Mutex
	sent []IYSRecord
	fail error
}

var (
	_ IYSClient      = (*FakeIYS)(nil)
	_ IYSFeedReader  = (*FakeIYS)(nil)
	_ IYSStateReader = (*FakeIYS)(nil)
)

func (f *FakeIYS) init() {
	f.once.Do(func() {
		f.f = iysfake.New(iysfake.Options{})
		f.client = NewIYSClient(f.f)
	})
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
func (f *FakeIYS) Submit(ctx context.Context, r IYSRecord) (IYSReceipt, error) {
	f.init()
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		return IYSReceipt{}, fail
	}
	if _, err := f.client.Submit(ctx, r); err != nil {
		return IYSReceipt{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, r)
	return IYSReceipt{TransactionID: "fake-" + r.ConsentID.String()}, nil
}

// SetStatus makes st İYS's state of userID on ch, whatever was held.
func (f *FakeIYS) SetStatus(userID uuid.UUID, ch IYSChannel, st IYSState) {
	f.init()
	f.f.Set(iysfake.AddressOf(userID), iysTypes[ch], st)
}

// FailStatuses makes every following read (Changes, Statuses) return err
// (nil: answer again).
func (f *FakeIYS) FailStatuses(err error) {
	f.init()
	f.f.FailReads(err)
}

// StatusCalls counts the Statuses calls.
func (f *FakeIYS) StatusCalls() int {
	f.init()
	return f.f.StatusCalls()
}

// Feed implements IYSFeedReader.
func (f *FakeIYS) Feed() iys.Feed {
	f.init()
	return f.f.Feed()
}

// Changes implements IYSFeedReader.
func (f *FakeIYS) Changes(ctx context.Context, cursor string) (iys.ChangePage, error) {
	f.init()
	return f.f.Changes(ctx, cursor)
}

// Statuses implements IYSStateReader.
func (f *FakeIYS) Statuses(ctx context.Context, t iys.Type, userIDs []uuid.UUID) (map[uuid.UUID]iys.State, error) {
	f.init()
	return f.f.Statuses(ctx, t, userIDs)
}

// Sent returns the accepted submissions in order.
func (f *FakeIYS) Sent() []IYSRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}
