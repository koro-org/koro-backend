package consent_test

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/iys"
	"koro/internal/platform/iys/iysfake"
)

// alarmCount is the module's İYS alarm counter of reason.
func alarmCount(e *env, reason string) int64 {
	for _, a := range e.mod.IYSAlarms() {
		if a.Reason == reason {
			return a.Count
		}
	}
	return -1
}

// reads selects the readers an adapter offers.
type reads struct{ feed, states bool }

var bothReads = reads{feed: true, states: true}

// newFeedEnv is consent over its port and the in-memory İYS with İYS's
// time rules (the change list hides its last hour and keeps seven days;
// consents older than three business days are refused), wired as
// cmd/koro wires an adapter: its Submit behind NewIYSClient and the
// readers it offers.
func newFeedEnv(t *testing.T, r reads, opts ...func(*consent.Deps)) (*env, *iysfake.Fake) {
	t.Helper()
	var f *iysfake.Fake
	e := newEnv(t, append([]func(*consent.Deps){func(d *consent.Deps) {
		f = iysfake.New(iysfake.Options{Clock: d.Clock})
		d.IYS, d.IYSFeed, d.IYSStates = consent.NewIYSClient(f), nil, nil
		if r.feed {
			d.IYSFeed = f
		}
		if r.states {
			d.IYSStates = f
		}
	}}, opts...)...)
	return e, f
}

// deliverLast hands only the newest consent.recorded event to the
// subscription.
func (e *env) deliverLast(t *testing.T) error {
	t.Helper()
	evs := e.out.Events()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Topic == domainevent.TopicConsentRecorded {
			return e.subscription(t).Handle(t.Context(), evs[i])
		}
	}
	t.Fatal("no consent.recorded event")
	return nil
}

func refuseInIYS(f *iysfake.Fake, user uuid.UUID, source string) {
	f.Set(iysfake.AddressOf(user), iys.Email, iys.State{Status: iys.Refused, Source: source})
}

// TestIYSStatusSync_FeedLagHorizonAndGap (BE-105): the first run has no
// feed history and reconciles every grant once; afterwards only the change
// feed is read (no single queries). A refusal in İYS is listed an hour
// later and then withdraws the grant; the channel's synced_at is the
// feed's horizon (an hour behind), which the hold accepts. Eight days
// without a run lose changes (İYS keeps seven): the hold stops commercial
// e-mail, and the next run reconciles every grant again and finds the
// refusal the feed no longer lists.
func TestIYSStatusSync_FeedLagHorizonAndGap(t *testing.T) {
	t.Parallel()
	e, f := newFeedEnv(t, bothReads, func(d *consent.Deps) { d.IYSSyncMaxAge = 3 * time.Hour })
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	ada, bea := actor(t, "student").UserID, actor(t, "student_b").UserID
	e.record(t, grant(ada, text, consent.ChannelWeb))
	e.record(t, grant(bea, text, consent.ChannelMobile))
	for _, err := range e.deliverAll(t) {
		if err != nil {
			t.Fatal(err)
		}
	}
	calls := f.StatusCalls()
	started := e.clk.Now().UTC()
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	if got := f.StatusCalls() - calls; got != 1 {
		t.Fatalf("first run: %d status reads, want 1 (every grant once)", got)
	}
	st, ok, _ := e.store.IYSSync(ctx, consent.IYSEmail)
	if !ok || !st.SyncedAt.Equal(started.Add(-iysfake.DefaultLag)) || st.Checked != 2 || st.Withdrawn != 0 {
		t.Fatalf("first run state = %+v, want synced an hour behind (İYS's lag)", st)
	}
	feed, ok, _ := e.store.IYSFeed(ctx, "fake")
	if !ok || feed.Horizon == nil || !feed.Horizon.Equal(started.Add(-iysfake.DefaultLag)) || feed.SweepDueAt != nil {
		t.Fatalf("feed after the first run = %+v", feed)
	}
	ids := []uuid.UUID{ada, bea}
	if got, err := e.svc.FilterActive(ctx, ids, consent.PurposeCommercialEmail); err != nil || len(got) != 2 {
		t.Fatalf("FilterActive after the first run = %v, %v", got, err)
	}

	e.clk.Advance(10 * time.Minute)
	refusedAt := e.clk.Now().UTC().Truncate(time.Second)
	refuseInIYS(f, ada, "IYS_MOBIL")
	calls = f.StatusCalls()
	e.clk.Advance(30 * time.Minute)
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	if c := e.latestOf(t, ada, consent.PurposeCommercialEmail); c.Action != consent.Granted {
		t.Fatalf("withdrawn before İYS lists the refusal: %+v", c)
	}
	e.clk.Advance(31 * time.Minute)
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	c := e.latestOf(t, ada, consent.PurposeCommercialEmail)
	if c.Action != consent.Withdrawn || c.Channel != consent.ChannelIYS {
		t.Fatalf("after İYS listed the refusal: %+v", c)
	}
	if f.StatusCalls() != calls {
		t.Fatal("a run with feed history read single states")
	}
	var meta map[string]any
	for _, a := range e.audit.of(string(authz.ConsentRecordCreate)) {
		if a.ResourceID != nil && *a.ResourceID == c.ID {
			meta = a.Metadata
		}
	}
	if meta["iys_reason"] != "refused" || meta["iys_source"] != "IYS_MOBIL" || meta["iys_status_at"] != refusedAt.Format(time.RFC3339) {
		t.Fatalf("audit metadata = %v", meta)
	}
	if st, _, _ := e.store.IYSSync(ctx, consent.IYSEmail); !st.SyncedAt.Equal(e.clk.Now().UTC().Add(-iysfake.DefaultLag)) ||
		st.Checked != 1 || st.Withdrawn != 1 {
		t.Fatalf("state = %+v", st)
	}

	// Eight days without a run; bea refused in İYS half a day after the
	// last one, which İYS's change list no longer shows.
	e.clk.Advance(12 * time.Hour)
	refuseInIYS(f, bea, "IYS_WEB")
	e.clk.Advance(7*24*time.Hour + 12*time.Hour)
	if _, err := e.svc.FilterActive(ctx, ids, consent.PurposeCommercialEmail); !errors.Is(err, consent.ErrIYSSyncStale) {
		t.Fatalf("FilterActive after eight days = %v, want held", err)
	}
	calls = f.StatusCalls()
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	if c := e.latestOf(t, bea, consent.PurposeCommercialEmail); c.Action != consent.Withdrawn || c.Channel != consent.ChannelIYS {
		t.Fatalf("a refusal older than İYS's change list was not reconciled: %+v", c)
	}
	if f.StatusCalls() == calls {
		t.Fatal("the gap did not reconcile the grants")
	}
	if feed, _, _ := e.store.IYSFeed(ctx, "fake"); feed.SweepDueAt != nil {
		t.Fatalf("reconciliation still due: %+v", feed)
	}
	if got, err := e.svc.FilterActive(ctx, ids, consent.PurposeCommercialEmail); err != nil || len(got) != 0 {
		t.Fatalf("FilterActive after the reconciliation = %v, %v", got, err)
	}
}

// TestIYSStatusSync_AdapterCapabilities (BE-105): the job adapts to what the
// adapter reads. Single states only (no feed): every run reconciles every
// grant (BE-103) and synced_at is the run's start. Feed only: the first
// run completes while no grant is in force, refusals arrive through the
// feed; after a gap with grants in force the run cannot reconcile: it
// fails with the sweep_unavailable alarm and the channel stays stale.
func TestIYSStatusSync_AdapterCapabilities(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	statuses, f := newFeedEnv(t, reads{states: true})
	text := statuses.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	ada := actor(t, "student").UserID
	statuses.record(t, grant(ada, text, consent.ChannelWeb))
	_ = statuses.deliverAll(t)
	for range 2 {
		calls := f.StatusCalls()
		started := statuses.clk.Now().UTC()
		if err := statuses.syncNow(t); err != nil {
			t.Fatal(err)
		}
		st, _, _ := statuses.store.IYSSync(ctx, consent.IYSEmail)
		if f.StatusCalls()-calls != 1 || !st.SyncedAt.Equal(started) || st.Checked != 1 {
			t.Fatalf("statuses-only run: %d reads, state %+v", f.StatusCalls()-calls, st)
		}
		statuses.clk.Advance(time.Hour)
	}
	if _, found, _ := statuses.store.IYSFeed(ctx, "fake"); found {
		t.Fatal("a feed row without a feed")
	}

	feedOnly, g := newFeedEnv(t, reads{feed: true})
	text = feedOnly.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	if err := feedOnly.syncNow(t); err != nil {
		t.Fatalf("first run without grants: %v", err)
	}
	feedOnly.record(t, grant(ada, text, consent.ChannelWeb))
	_ = feedOnly.deliverAll(t)
	refuseInIYS(g, ada, "IYS_KISAMESAJ")
	feedOnly.clk.Advance(time.Hour + time.Minute)
	if err := feedOnly.syncNow(t); err != nil {
		t.Fatal(err)
	}
	if c := feedOnly.latestOf(t, ada, consent.PurposeCommercialEmail); c.Action != consent.Withdrawn {
		t.Fatalf("refusal through the feed: %+v", c)
	}
	feedOnly.record(t, grant(ada, text, consent.ChannelWeb))
	_ = feedOnly.deliverAll(t)
	before, _, _ := feedOnly.store.IYSSync(ctx, consent.IYSEmail)
	feedOnly.clk.Advance(8 * 24 * time.Hour)
	err := feedOnly.syncNow(t)
	if err == nil || !strings.Contains(err.Error(), "cannot read single") {
		t.Fatalf("gap without single reads = %v", err)
	}
	if st, _, _ := feedOnly.store.IYSSync(ctx, consent.IYSEmail); !st.SyncedAt.Equal(before.SyncedAt) {
		t.Fatalf("an incomplete run moved the state: %+v", st)
	}
	if got := alarmCount(feedOnly, consent.IYSAlarmSweepUnavailable); got != 1 {
		t.Fatalf("sweep_unavailable alarms = %d, want 1", got)
	}
}

// TestIYS_TooLateAndAlreadyHeld (BE-105): a consent İYS refuses as older
// than three business days fails permanently with the too_late alarm
// (logged and counted) whatever adapter said so; a refusal of a recipient
// İYS has no record of is delivered as a success.
func TestIYS_TooLateAndAlreadyHeld(t *testing.T) {
	t.Parallel()
	var logs logSink
	e, f := newFeedEnv(t, bothReads, func(d *consent.Deps) { d.Log = slog.New(slog.NewJSONHandler(&logs, nil)) })
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	ada, bea := actor(t, "student").UserID, actor(t, "student_b").UserID
	f.FailSubmits(errors.New("fake İYS: 503"))
	lateID := e.record(t, grant(ada, text, consent.ChannelWeb))
	_ = e.deliverAll(t)
	f.FailSubmits(nil)
	e.clk.Advance(6 * 24 * time.Hour) // Saturday 3 October + 6 days: more than three business days
	if err := e.job(t, consent.ReconcileJobName).Handle(ctx); err != nil {
		t.Fatal(err)
	}
	sub, _ := e.store.SubmissionOf(ctx, lateID)
	if sub.Status != consent.SubmissionFailed || sub.LastError == nil || !strings.Contains(*sub.LastError, "three business days") {
		t.Fatalf("late grant submission = %+v", sub)
	}
	if got := alarmCount(e, consent.IYSAlarmTooLate); got != 1 || !strings.Contains(logs.String(), `"iys_alarm":"too_late"`) {
		t.Fatalf("too_late alarms = %d; logs %s", got, logs.String())
	}

	// bea's grant never reached İYS; her withdrawal finds no record there.
	f.FailSubmits(errors.New("fake İYS: 503"))
	e.record(t, grant(bea, text, consent.ChannelWeb))
	if err := e.deliverLast(t); err == nil {
		t.Fatal("delivery while İYS is down succeeded")
	}
	f.FailSubmits(nil)
	withdrawID := e.record(t, withdraw(bea, consent.PurposeCommercialEmail))
	if err := e.deliverLast(t); err != nil {
		t.Fatalf("refusal of a recipient İYS does not know = %v", err)
	}
	if sub, _ := e.store.SubmissionOf(ctx, withdrawID); sub.Status != consent.SubmissionSent {
		t.Fatalf("withdrawal submission = %+v, want sent", sub)
	}
	if _, held := f.Held(iysfake.AddressOf(bea), iys.Email); held {
		t.Fatal("the refusal created an İYS record")
	}
	if !slices.ContainsFunc(f.Submitted(), func(c iys.Consent) bool { return c.UserID == bea && c.Status == iys.Refused }) {
		t.Fatal("the refusal never reached the adapter")
	}
}
