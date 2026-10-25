// Package iysfake is an in-memory İYS with every adapter capability
// (Submit, the change feed, single states): the adapter of tests and of
// consent.FakeIYS. It keeps İYS's rules that the callers depend on
// (iyscontract pins them for every adapter):
//
//   - İYS keeps the latest record of an address and type; a record dated
//     before the held one is refused (H178); a refusal İYS already holds,
//     or of an address it has no record of, changes nothing (H174, H175:
//     Receipt.AlreadyHeld).
//   - The change feed lists only what recipients give İYS itself (an İYS
//     source, "IYS_*"), never the brand's own submissions.
//   - With a Clock: the feed hides its last Lag (one hour) and keeps Window
//     (seven days), and a consent older than three business days is
//     refused (H467, iys.ErrTooLate). Without one these time rules are off.
package iysfake

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/iys"
)

// Defaults of the time rules (with a Clock).
const (
	DefaultLag      = time.Hour
	DefaultWindow   = 7 * 24 * time.Hour
	DefaultPageSize = 1000
)

// Recipients resolves users and addresses in both directions (identity's
// role; iys.ErrRecipientGone when there is none).
type Recipients interface {
	Recipient(ctx context.Context, user uuid.UUID, t iys.Type) (string, error)
	User(ctx context.Context, t iys.Type, address string) (uuid.UUID, error)
}

// Options configures a Fake.
type Options struct {
	// Clock turns on İYS's time rules (feed lag and window, the
	// three-business-day rule). nil: no time rules.
	Clock clock.Clock
	// Recipients resolves users and addresses; nil → Addresses.
	Recipients Recipients
	// Lag and Window of the change feed (defaults with a Clock: one hour,
	// seven days; without one: none).
	Lag, Window time.Duration
	// PageSize of the change feed (default 1000).
	PageSize int
}

// Fake is the in-memory İYS.
type Fake struct {
	o Options

	mu          sync.Mutex
	held        map[key]iys.State
	feed        []entry
	submitted   []iys.Consent
	tx          int
	submitFail  error
	readFail    error
	statusCalls int
	changeCalls int
}

type key struct {
	address string
	typ     iys.Type
}

type entry struct {
	seq        int
	address    string
	typ        iys.Type
	st         iys.State
	recordedAt time.Time
}

// New returns an empty İYS.
func New(o Options) *Fake {
	if o.Recipients == nil {
		o.Recipients = Addresses{}
	}
	if o.Clock != nil {
		if o.Lag == 0 {
			o.Lag = DefaultLag
		}
		if o.Window == 0 {
			o.Window = DefaultWindow
		}
	}
	if o.PageSize <= 0 {
		o.PageSize = DefaultPageSize
	}
	return &Fake{o: o, held: map[key]iys.State{}}
}

// Name names the adapter in logs.
func (*Fake) Name() string { return "fake" }

// Feed describes the change feed.
func (f *Fake) Feed() iys.Feed { return iys.Feed{Name: "fake", Window: f.o.Window} }

// Submit registers one consent change.
func (f *Fake) Submit(ctx context.Context, c iys.Consent) (iys.Receipt, error) {
	if !iys.ValidType(c.Type) || (c.Status != iys.Approved && c.Status != iys.Refused) || iys.PlainSource(c.Source) == "" ||
		!strings.HasPrefix(c.Source, "HS_") || c.ConsentDate.IsZero() {
		return iys.Receipt{}, fmt.Errorf("%w: fake İYS: invalid consent record", iys.ErrRejected)
	}
	f.mu.Lock()
	fail := f.submitFail
	f.mu.Unlock()
	if fail != nil {
		return iys.Receipt{}, fail
	}
	address, err := f.o.Recipients.Recipient(ctx, c.UserID, c.Type)
	switch {
	case errors.Is(err, iys.ErrRecipientGone):
		return iys.Receipt{}, fmt.Errorf("%w: %w", iys.ErrRejected, iys.ErrRecipientGone)
	case err != nil:
		return iys.Receipt{}, fmt.Errorf("fake İYS: recipient: %w", err)
	}
	at := c.ConsentDate.UTC().Truncate(time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.o.Clock != nil && !f.o.Clock.Now().Before(iys.RegistrationDeadline(at)) {
		return iys.Receipt{}, fmt.Errorf("%w: %w (fake İYS)", iys.ErrRejected, iys.ErrTooLate)
	}
	k := key{address: address, typ: c.Type}
	cur, found := f.held[k]
	switch {
	case c.Status == iys.Refused && (!found || cur.Status == iys.Refused):
		f.submitted = append(f.submitted, c)
		return iys.Receipt{AlreadyHeld: true}, nil
	case found && at.Before(cur.At):
		return iys.Receipt{}, fmt.Errorf("%w: fake İYS: a newer record is held", iys.ErrRejected)
	}
	f.held[k] = iys.State{Status: c.Status, At: at, Source: c.Source}
	f.submitted = append(f.submitted, c)
	f.tx++
	return iys.Receipt{TransactionID: "fake-tx-" + strconv.Itoa(f.tx)}, nil
}

// Changes returns the next page of the change feed after cursor (the
// sequence number of the last listed change).
func (f *Fake) Changes(ctx context.Context, cursor string) (iys.ChangePage, error) {
	after := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return iys.ChangePage{}, fmt.Errorf("%w: fake İYS: malformed cursor", iys.ErrRejected)
		}
		after = n
	}
	f.mu.Lock()
	f.changeCalls++
	if err := f.readFail; err != nil {
		f.mu.Unlock()
		return iys.ChangePage{}, err
	}
	var now time.Time
	if f.o.Clock != nil {
		now = f.o.Clock.Now()
	}
	var visible []entry
	for _, e := range f.feed {
		switch {
		case e.seq <= after:
		case !now.IsZero() && e.recordedAt.After(now.Add(-f.o.Lag)):
		case !now.IsZero() && f.o.Window > 0 && e.recordedAt.Before(now.Add(-f.o.Window)):
		default:
			visible = append(visible, e)
		}
	}
	f.mu.Unlock()
	page := iys.ChangePage{Cursor: cursor}
	if len(visible) > f.o.PageSize {
		visible, page.More = visible[:f.o.PageSize], true
	}
	for _, e := range visible {
		page.Cursor = strconv.Itoa(e.seq)
		user, err := f.o.Recipients.User(ctx, e.typ, e.address)
		switch {
		case errors.Is(err, iys.ErrRecipientGone):
			page.Skipped++
			continue
		case err != nil:
			return iys.ChangePage{}, fmt.Errorf("fake İYS: recipient: %w", err)
		}
		page.Changes = append(page.Changes, iys.Change{UserID: user, Type: e.typ, State: e.st, RecordedAt: e.recordedAt})
	}
	switch {
	case page.More:
		page.Through = visible[len(visible)-1].recordedAt
	case !now.IsZero():
		page.Through = now.Add(-f.o.Lag)
	}
	return page, nil
}

// Statuses returns İYS's current state of the users' addresses for t.
func (f *Fake) Statuses(ctx context.Context, t iys.Type, userIDs []uuid.UUID) (map[uuid.UUID]iys.State, error) {
	f.mu.Lock()
	f.statusCalls++
	fail := f.readFail
	f.mu.Unlock()
	switch {
	case fail != nil:
		return nil, fail
	case len(userIDs) > iys.MaxStatusBatch:
		return nil, fmt.Errorf("fake İYS: %d users in one status read (at most %d)", len(userIDs), iys.MaxStatusBatch)
	case !iys.ValidType(t):
		return nil, fmt.Errorf("%w: fake İYS: unknown type", iys.ErrRejected)
	}
	out := make(map[uuid.UUID]iys.State, len(userIDs))
	for _, id := range userIDs {
		address, err := f.o.Recipients.Recipient(ctx, id, t)
		switch {
		case errors.Is(err, iys.ErrRecipientGone):
			continue
		case err != nil:
			return nil, fmt.Errorf("fake İYS: recipient: %w", err)
		}
		f.mu.Lock()
		st, ok := f.held[key{address: address, typ: t}]
		f.mu.Unlock()
		if ok {
			out[id] = st
		}
	}
	return out, nil
}

// Set makes st İYS's record of address for t, whatever was held. A record
// with an İYS source ("IYS_*": the recipient acted in İYS itself) also goes
// to the change feed, recorded at the clock's now (st.At without a clock;
// st.At defaults to now).
func (f *Fake) Set(address string, t iys.Type, st iys.State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	recordedAt := st.At
	if f.o.Clock != nil {
		recordedAt = f.o.Clock.Now()
		if st.At.IsZero() {
			st.At = recordedAt
		}
	}
	st.At = st.At.UTC().Truncate(time.Second)
	f.held[key{address: address, typ: t}] = st
	if strings.HasPrefix(st.Source, "IYS_") {
		f.feed = append(f.feed, entry{seq: len(f.feed) + 1, address: address, typ: t, st: st, recordedAt: recordedAt.UTC()})
	}
}

// Held returns İYS's record of address for t.
func (f *Fake) Held(address string, t iys.Type) (iys.State, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.held[key{address: address, typ: t}]
	return st, ok
}

// FailSubmits makes every following Submit return err (nil: succeed again).
func (f *Fake) FailSubmits(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitFail = err
}

// FailReads makes every following Changes and Statuses return err (nil:
// answer again).
func (f *Fake) FailReads(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readFail = err
}

// Submitted returns the accepted submissions in order (AlreadyHeld ones
// included).
func (f *Fake) Submitted() []iys.Consent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.submitted)
}

// StatusCalls counts the Statuses calls; ChangeCalls the Changes calls.
func (f *Fake) StatusCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusCalls
}

// ChangeCalls counts the Changes calls.
func (f *Fake) ChangeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changeCalls
}

// Addresses is the fake's own directory: every user has the synthetic
// address AddressOf(user) for every type, and nothing else is a user.
type Addresses struct{}

const addressSuffix = "@iys-fake.test"

// AddressOf is the synthetic address of a user (no personal data).
func AddressOf(user uuid.UUID) string { return "u" + hex.EncodeToString(user[:]) + addressSuffix }

// Recipient implements Recipients.
func (Addresses) Recipient(_ context.Context, user uuid.UUID, _ iys.Type) (string, error) {
	return AddressOf(user), nil
}

// User implements Recipients.
func (Addresses) User(_ context.Context, _ iys.Type, address string) (uuid.UUID, error) {
	raw, ok := strings.CutSuffix(strings.TrimPrefix(address, "u"), addressSuffix)
	b, err := hex.DecodeString(raw)
	if !ok || err != nil || len(b) != 16 || !strings.HasPrefix(address, "u") {
		return uuid.Nil(), iys.ErrRecipientGone
	}
	return uuid.UUID(b), nil
}
