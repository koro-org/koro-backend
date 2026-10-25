// Package iyscontract is the behaviour every İYS adapter shows (BE-105):
// Run drives an adapter and the İYS (or integrator) double behind it
// through the same cases, so that consent works with any of them unchanged.
// The fake (iysfake) and the official adapter (over iystest) pass it; an
// integrator's adapter (Netgsm, BE-110) plugs into the same Run with its
// own double. The cases of a capability run when the adapter has it
// (ChangeFeed, StateReader). Never import it outside tests.
package iyscontract

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/iys"
)

// Start is the suite's clock: a Wednesday (business days around it).
var Start = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// Adapter registers consent changes: what every adapter offers.
type Adapter interface {
	Name() string
	Submit(ctx context.Context, c iys.Consent) (iys.Receipt, error)
}

// ChangeFeed is the change feed capability.
type ChangeFeed interface {
	Feed() iys.Feed
	Changes(ctx context.Context, cursor string) (iys.ChangePage, error)
}

// StateReader is the single-state capability.
type StateReader interface {
	Statuses(ctx context.Context, t iys.Type, userIDs []uuid.UUID) (map[uuid.UUID]iys.State, error)
}

// Env is what the suite hands the adapter under test.
type Env struct {
	// Clock is shared by the adapter and its double.
	Clock *clock.Fake
	// Recipients is the user ↔ address directory the adapter resolves
	// through (e-mail only, like identity).
	Recipients *Directory
}

// Harness is the adapter under test and the İYS behind it.
type Harness struct {
	Adapter Adapter
	// Lag is how long after an act in İYS the change can be listed (the
	// official change list: one hour; a push feed: 0).
	Lag time.Duration
	// Act makes the recipient of address give İYS status itself (İYS web,
	// e-Devlet, 3338) now, with an İYS source; for a push adapter it also
	// delivers the integrator's notification.
	Act func(t *testing.T, address string, typ iys.Type, status iys.Status)
	// Held returns İYS's record of address.
	Held func(t *testing.T, address string, typ iys.Type) (iys.State, bool)
	// SetDown makes İYS (or the integrator) unreachable.
	SetDown func(down bool)
}

// Run runs the contract against the adapters newHarness builds (one per
// case, cases in parallel).
func Run(t *testing.T, newHarness func(t *testing.T, env Env) Harness) {
	t.Helper()
	setup := func(t *testing.T) (Env, Harness) {
		t.Helper()
		env := Env{Clock: clock.NewFake(Start), Recipients: NewDirectory()}
		h := newHarness(t, env)
		if h.Adapter == nil || h.Act == nil || h.Held == nil || h.SetDown == nil {
			t.Fatal("iyscontract: incomplete harness")
		}
		return env, h
	}

	t.Run("names", func(t *testing.T) {
		t.Parallel()
		_, h := setup(t)
		if h.Adapter.Name() == "" {
			t.Fatal("no name")
		}
		if f, ok := h.Adapter.(ChangeFeed); ok {
			if feed := f.Feed(); !iys.ValidFeed(feed.Name) || feed.Window < 0 || feed != f.Feed() {
				t.Fatalf("change feed %+v", feed)
			}
		}
	})

	t.Run("submit registers approvals and refusals idempotently", func(t *testing.T) {
		t.Parallel()
		env, h := setup(t)
		ctx := t.Context()
		ada, bea := env.Recipients.Add("ada.yilmaz@ogrenci.example.test"), env.Recipients.Add("bea.kaya@ogrenci.example.test")
		granted := env.Clock.Now().Add(-90 * time.Minute)
		r, err := h.Adapter.Submit(ctx, consent(ada, iys.Approved, "HS_WEB", granted))
		if err != nil || r.AlreadyHeld || r.TransactionID == "" {
			t.Fatalf("approval = %+v, %v", r, err)
		}
		st, ok := h.Held(t, env.Recipients.Address(ada), iys.Email)
		if !ok || st.Status != iys.Approved || !st.At.Equal(granted.Truncate(time.Second)) || st.Source != "HS_WEB" {
			t.Fatalf("İYS holds %+v (%v) after the approval", st, ok)
		}
		withdrawn := env.Clock.Now().Add(-time.Minute)
		if r, err := h.Adapter.Submit(ctx, consent(ada, iys.Refused, "HS_EPOSTA", withdrawn)); err != nil || r.AlreadyHeld {
			t.Fatalf("refusal = %+v, %v", r, err)
		}
		if st, _ := h.Held(t, env.Recipients.Address(ada), iys.Email); st.Status != iys.Refused || !st.At.Equal(withdrawn.Truncate(time.Second)) {
			t.Fatalf("İYS holds %+v after the refusal", st)
		}
		// A repeated refusal (İYS H174) and a refusal of a recipient İYS
		// has no record of (H175) change nothing and succeed.
		if r, err := h.Adapter.Submit(ctx, consent(ada, iys.Refused, "HS_WEB", env.Clock.Now())); err != nil || !r.AlreadyHeld {
			t.Fatalf("repeated refusal = %+v, %v, want AlreadyHeld", r, err)
		}
		if r, err := h.Adapter.Submit(ctx, consent(bea, iys.Refused, "HS_WEB", env.Clock.Now())); err != nil || !r.AlreadyHeld {
			t.Fatalf("refusal of an unknown recipient = %+v, %v, want AlreadyHeld", r, err)
		}
		if _, ok := h.Held(t, env.Recipients.Address(bea), iys.Email); ok {
			t.Fatal("a refusal created a record")
		}
	})

	t.Run("submit failures are classified and never repeat the address", func(t *testing.T) {
		t.Parallel()
		env, h := setup(t)
		ctx := t.Context()
		ada := env.Recipients.Add("ada.yilmaz@ogrenci.example.test")
		gone := uuid.NewV7() // no address (an erased account)
		var errs []error
		_, err := h.Adapter.Submit(ctx, consent(gone, iys.Approved, "HS_WEB", env.Clock.Now()))
		if !errors.Is(err, iys.ErrRejected) || !errors.Is(err, iys.ErrRecipientGone) {
			t.Fatalf("recipient gone = %v, want ErrRejected + ErrRecipientGone", err)
		}
		errs = append(errs, err)
		// Taken a week ago: more than three business days (İYS H467).
		_, err = h.Adapter.Submit(ctx, consent(ada, iys.Approved, "HS_WEB", env.Clock.Now().Add(-7*24*time.Hour)))
		if !errors.Is(err, iys.ErrTooLate) || !errors.Is(err, iys.ErrRejected) {
			t.Fatalf("a week-old consent = %v, want ErrTooLate + ErrRejected", err)
		}
		errs = append(errs, err)
		h.SetDown(true)
		_, err = h.Adapter.Submit(ctx, consent(ada, iys.Approved, "HS_WEB", env.Clock.Now()))
		if err == nil || errors.Is(err, iys.ErrRejected) {
			t.Fatalf("outage = %v, want a transient error", err)
		}
		errs = append(errs, err)
		h.SetDown(false)
		if _, err := h.Adapter.Submit(ctx, consent(ada, iys.Approved, "HS_WEB", env.Clock.Now())); err != nil {
			t.Fatalf("after the outage: %v", err)
		}
		noAddress(t, errs, env)
	})

	t.Run("changes list what recipients give İYS itself, once, in pages", func(t *testing.T) {
		t.Parallel()
		env, h := setup(t)
		feed, ok := h.Adapter.(ChangeFeed)
		if !ok {
			t.Skip("no change feed")
		}
		ctx := t.Context()
		ada, bea := env.Recipients.Add("ada.yilmaz@ogrenci.example.test"), env.Recipients.Add("bea.kaya@ogrenci.example.test")
		if _, err := h.Adapter.Submit(ctx, consent(ada, iys.Approved, "HS_WEB", env.Clock.Now())); err != nil {
			t.Fatal(err)
		}
		start := readAll(t, feed, "")
		env.Clock.Advance(time.Minute)
		refusedAt := env.Clock.Now().Truncate(time.Second)
		h.Act(t, env.Recipients.Address(ada), iys.Email, iys.Refused)
		h.Act(t, "stranger@elsewhere.example.test", iys.Email, iys.Refused) // no Koro user
		h.Act(t, env.Recipients.Address(bea), iys.Email, iys.Approved)
		if h.Lag > 0 {
			if got := readAll(t, feed, start.cursor); len(got.changes) != 0 {
				t.Fatalf("changes listed before İYS's lag: %+v", got.changes)
			}
		}
		env.Clock.Advance(h.Lag + time.Minute)
		got := readAll(t, feed, start.cursor)
		if len(got.changes) != 2 || got.skipped < 1 {
			t.Fatalf("changes = %+v (skipped %d), want ada's refusal and bea's approval", got.changes, got.skipped)
		}
		c := got.changes[0]
		if c.UserID != ada || c.Type != iys.Email || c.State.Status != iys.Refused || !c.State.At.Equal(refusedAt) ||
			!strings.HasPrefix(c.State.Source, "IYS_") || c.RecordedAt.Before(refusedAt) {
			t.Fatalf("ada's change = %+v", c)
		}
		if b := got.changes[1]; b.UserID != bea || b.State.Status != iys.Approved {
			t.Fatalf("bea's change = %+v", b)
		}
		if got.through.After(env.Clock.Now()) {
			t.Fatalf("through = %v, after now", got.through)
		}
		if again := readAll(t, feed, got.cursor); len(again.changes) != 0 || again.cursor != got.cursor {
			t.Fatalf("re-read from the cursor = %+v", again)
		}
		// Koro's own submissions are never in the feed.
		if _, err := h.Adapter.Submit(ctx, consent(bea, iys.Refused, "HS_WEB", env.Clock.Now())); err != nil {
			t.Fatal(err)
		}
		env.Clock.Advance(h.Lag + time.Minute)
		if own := readAll(t, feed, got.cursor); len(own.changes) != 0 {
			t.Fatalf("Koro's own refusal listed: %+v", own.changes)
		}
	})

	t.Run("changes page through more than one page", func(t *testing.T) {
		t.Parallel()
		env, h := setup(t)
		feed, ok := h.Adapter.(ChangeFeed)
		if !ok {
			t.Skip("no change feed")
		}
		const n = 1205
		want := map[uuid.UUID]bool{}
		for i := range n {
			u := env.Recipients.Add(fmt.Sprintf("ogrenci%04d@ogrenci.example.test", i))
			want[u] = true
			h.Act(t, env.Recipients.Address(u), iys.Email, iys.Refused)
		}
		env.Clock.Advance(h.Lag + time.Minute)
		got := readAll(t, feed, "")
		if got.pages < 2 {
			t.Fatalf("%d changes in %d page(s), want more than one", n, got.pages)
		}
		seen := map[uuid.UUID]bool{}
		for _, c := range got.changes {
			if seen[c.UserID] || !want[c.UserID] {
				t.Fatalf("change of %s listed twice or unknown", c.UserID)
			}
			seen[c.UserID] = true
		}
		if len(seen) != n {
			t.Fatalf("%d of %d changes listed", len(seen), n)
		}
	})

	t.Run("statuses read the current state of single recipients", func(t *testing.T) {
		t.Parallel()
		env, h := setup(t)
		states, ok := h.Adapter.(StateReader)
		if !ok {
			t.Skip("no single states")
		}
		ctx := t.Context()
		ada, bea := env.Recipients.Add("ada.yilmaz@ogrenci.example.test"), env.Recipients.Add("Bea.Kaya@Ogrenci.example.test")
		cem, gone := env.Recipients.Add("cem.demir@ogrenci.example.test"), uuid.NewV7()
		if _, err := h.Adapter.Submit(ctx, consent(ada, iys.Approved, "HS_WEB", env.Clock.Now())); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Adapter.Submit(ctx, consent(bea, iys.Approved, "HS_MOBIL", env.Clock.Now())); err != nil {
			t.Fatal(err)
		}
		env.Clock.Advance(time.Hour)
		refusedAt := env.Clock.Now().Truncate(time.Second)
		h.Act(t, env.Recipients.Address(bea), iys.Email, iys.Refused)
		got, err := states.Statuses(ctx, iys.Email, []uuid.UUID{ada, bea, cem, gone})
		if err != nil {
			t.Fatal(err)
		}
		if st, ok := got[ada]; !ok || st.Status != iys.Approved {
			t.Fatalf("ada = %+v, %v; want ONAY", st, ok)
		}
		if st, ok := got[bea]; !ok || st.Status != iys.Refused || !st.At.Equal(refusedAt) || !strings.HasPrefix(st.Source, "IYS_") {
			t.Fatalf("bea = %+v, %v; want İYS's refusal with its date and source", st, ok)
		}
		if _, ok := got[cem]; ok {
			t.Fatal("a recipient İYS has no record of is present")
		}
		if _, ok := got[gone]; ok {
			t.Fatal("a user without an address is present")
		}
		if got, err := states.Statuses(ctx, iys.Email, nil); err != nil || len(got) != 0 {
			t.Fatalf("no users = %v, %v", got, err)
		}
	})

	t.Run("reads fail transiently while İYS is down", func(t *testing.T) {
		t.Parallel()
		env, h := setup(t)
		ada := env.Recipients.Add("ada.yilmaz@ogrenci.example.test")
		h.SetDown(true)
		var errs []error
		if feed, ok := h.Adapter.(ChangeFeed); ok {
			_, err := feed.Changes(t.Context(), "")
			if err == nil || errors.Is(err, iys.ErrRejected) {
				t.Fatalf("Changes while down = %v", err)
			}
			errs = append(errs, err)
		}
		if states, ok := h.Adapter.(StateReader); ok {
			_, err := states.Statuses(t.Context(), iys.Email, []uuid.UUID{ada})
			if err == nil || errors.Is(err, iys.ErrRejected) {
				t.Fatalf("Statuses while down = %v", err)
			}
			errs = append(errs, err)
		}
		noAddress(t, errs, env)
	})
}

func consent(user uuid.UUID, st iys.Status, source string, at time.Time) iys.Consent {
	return iys.Consent{Ref: uuid.NewV7().String(), UserID: user, Type: iys.Email, Status: st, Source: source, ConsentDate: at}
}

type feedRead struct {
	changes []iys.Change
	cursor  string
	through time.Time
	skipped int
	pages   int
}

// readAll reads the feed from cursor until it has no more.
func readAll(t *testing.T, p ChangeFeed, cursor string) feedRead {
	t.Helper()
	out := feedRead{cursor: cursor}
	for range 100 {
		page, err := p.Changes(t.Context(), out.cursor)
		if err != nil {
			t.Fatalf("Changes(%q): %v", out.cursor, err)
		}
		if !iys.ValidCursor(page.Cursor) || (page.More && page.Through.IsZero()) {
			t.Fatalf("page cursor %q, more %v, through %v", page.Cursor, page.More, page.Through)
		}
		out.pages++
		out.changes = append(out.changes, page.Changes...)
		out.cursor, out.through, out.skipped = page.Cursor, page.Through, out.skipped+page.Skipped
		if !page.More {
			return out
		}
	}
	t.Fatal("the feed never ends")
	return out
}

func noAddress(t *testing.T, errs []error, env Env) {
	t.Helper()
	for _, err := range errs {
		for _, a := range env.Recipients.all() {
			if err != nil && strings.Contains(strings.ToLower(err.Error()), strings.ToLower(a)) {
				t.Fatalf("error repeats an address: %v", err)
			}
		}
	}
}

// Directory is the suite's identity: e-mail addresses of users.
type Directory struct {
	mu     sync.Mutex
	byUser map[uuid.UUID]string
}

// NewDirectory returns an empty directory.
func NewDirectory() *Directory { return &Directory{byUser: map[uuid.UUID]string{}} }

// Add creates a user with address and returns the user.
func (d *Directory) Add(address string) uuid.UUID {
	id := uuid.NewV7()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.byUser[id] = address
	return id
}

// Address is the address of user ("" when none).
func (d *Directory) Address(user uuid.UUID) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.byUser[user]
}

func (d *Directory) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.byUser))
	for _, a := range d.byUser {
		out = append(out, a)
	}
	return out
}

// Recipient returns the address of user (e-mail only), or
// iys.ErrRecipientGone.
func (d *Directory) Recipient(_ context.Context, user uuid.UUID, t iys.Type) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a := d.byUser[user]; t == iys.Email && a != "" {
		return a, nil
	}
	return "", iys.ErrRecipientGone
}

// User returns the user of address (case-insensitive), or
// iys.ErrRecipientGone.
func (d *Directory) User(_ context.Context, t iys.Type, address string) (uuid.UUID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t == iys.Email {
		for id, a := range d.byUser {
			if strings.EqualFold(a, address) {
				return id, nil
			}
		}
	}
	return uuid.Nil(), iys.ErrRecipientGone
}
