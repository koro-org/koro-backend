package consent_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// latestOf returns the newest unscoped row of (user, purpose).
func (e *env) latestOf(t *testing.T, user uuid.UUID, p consent.Purpose) consent.Consent {
	t.Helper()
	c, ok, err := e.store.Latest(t.Context(), user, p, nil)
	if err != nil || !ok {
		t.Fatalf("Latest(%s) = %v, %v", p, ok, err)
	}
	return c
}

// syncNow runs consent.iys_status_sync once.
func (e *env) syncNow(t *testing.T) error {
	t.Helper()
	return e.job(t, consent.StatusSyncJobName).Handle(t.Context())
}

// recordedEvents counts the consent.recorded events written so far.
func (e *env) recordedEvents() int {
	n := 0
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicConsentRecorded {
			n++
		}
	}
	return n
}

// TestIYSStatusSync_RefusalInIYSWithdrawsConsent (T-312, T-354): a
// recipient whose approval Koro delivered refuses in İYS itself; the hourly
// reconciliation reads it from the change feed and appends a withdrawal
// through channel iys, audited as the system job with İYS's date and
// source, without sending the refusal back to İYS. FilterActive stops
// reaching the user. A second run reads nothing new and changes nothing;
// the channel's state records each run.
func TestIYSStatusSync_RefusalInIYSWithdrawsConsent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	refuser, keeper := actor(t, "student").UserID, actor(t, "student_b").UserID
	grantID := e.record(t, grant(refuser, text, consent.ChannelWeb))
	e.record(t, grant(keeper, text, consent.ChannelMobile))
	for _, err := range e.deliverAll(t) { // İYS now holds both approvals
		if err != nil {
			t.Fatal(err)
		}
	}
	job := e.job(t, consent.StatusSyncJobName)
	if job.Cron != "20 * * * *" || job.Queue != queue.Default || job.Unique <= 0 || job.Unique >= time.Hour || job.Timeout <= 0 {
		t.Fatalf("job = %+v", job)
	}

	e.clk.Advance(2 * time.Hour)
	refusedAt := e.clk.Now().Add(-30 * time.Minute).Truncate(time.Second)
	e.iys.SetStatus(refuser, consent.IYSEmail, consent.IYSState{Status: consent.IYSRefused, At: refusedAt, Source: "IYS_WEB"})
	submissions, events := len(e.iys.Sent()), e.recordedEvents()
	started := e.clk.Now().UTC()
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}

	c := e.latestOf(t, refuser, consent.PurposeCommercialEmail)
	if c.Action != consent.Withdrawn || c.Channel != consent.ChannelIYS || c.NoticeVersionID != nil || c.IP != "" {
		t.Fatalf("ledger after the İYS refusal = %+v", c)
	}
	if k := e.latestOf(t, keeper, consent.PurposeCommercialEmail); k.Action != consent.Granted {
		t.Fatalf("an approval in İYS was touched: %+v", k)
	}
	if got, _ := e.svc.FilterActive(ctx, []uuid.UUID{refuser, keeper}, consent.PurposeCommercialEmail); !slices.Equal(got, []uuid.UUID{keeper}) {
		t.Fatalf("FilterActive = %v, want only the keeper", got)
	}
	if _, err := e.store.SubmissionOf(ctx, c.ID); !errors.Is(err, consent.ErrNotFound) {
		t.Fatalf("İYS submission of an İYS withdrawal: %v, want none", err)
	}
	if len(e.iys.Sent()) != submissions || e.recordedEvents() != events {
		t.Fatal("the İYS refusal was sent back to İYS")
	}
	var meta map[string]any
	for _, a := range e.audit.of(string(authz.ConsentRecordCreate)) {
		if a.ResourceID != nil && *a.ResourceID == c.ID {
			if a.SystemJob != consent.StatusSyncJobName || a.ActorUserID != nil {
				t.Fatalf("audit actor = %v/%q, want the system job", a.ActorUserID, a.SystemJob)
			}
			meta = a.Metadata
		}
	}
	switch {
	case meta == nil:
		t.Fatal("İYS withdrawal not audited")
	case meta["channel"] != "iys" || meta["consent_action"] != "withdrawn" || meta["iys_reason"] != "refused" ||
		meta["iys_source"] != "IYS_WEB" || meta["iys_status_at"] != refusedAt.UTC().Format(time.RFC3339) ||
		meta["grant_consent_id"] != grantID.String() || meta["subject_user_id"] != refuser.String():
		t.Fatalf("audit metadata = %v", meta)
	}
	st, ok, err := e.store.IYSSync(ctx, consent.IYSEmail)
	if err != nil || !ok || !st.SyncedAt.Equal(started) || st.Checked != 2 || st.Withdrawn != 1 {
		t.Fatalf("iys_status_syncs(email) = %+v, %v, %v", st, ok, err)
	}

	// Idempotent: the chain is no longer a grant.
	rows, _ := e.store.AllOf(ctx, refuser)
	e.clk.Advance(time.Hour)
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.store.AllOf(ctx, refuser); len(again) != len(rows) {
		t.Fatalf("second run wrote %d more rows", len(again)-len(rows))
	}
	if st, _, _ := e.store.IYSSync(ctx, consent.IYSEmail); st.Checked != 0 || st.Withdrawn != 0 || !st.SyncedAt.Equal(e.clk.Now().UTC()) {
		t.Fatalf("second run state = %+v (the feed had nothing new)", st)
	}

	// The user consents again in Koro: the newer approval reaches İYS and is kept.
	e.record(t, grant(refuser, text, consent.ChannelWeb))
	for _, err := range e.deliverAll(t) {
		if err != nil {
			t.Fatal(err)
		}
	}
	e.clk.Advance(time.Hour)
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	if c := e.latestOf(t, refuser, consent.PurposeCommercialEmail); c.Action != consent.Granted {
		t.Fatalf("a newer Koro grant was withdrawn: %+v", c)
	}
}

// TestIYSStatusSync_InFlightGrantThenConflict: İYS still holds an older
// refusal while Koro's newer approval is on its way (submission failing);
// within IYSInFlightGrace the grant stays. If İYS still holds the older
// refusal after it, İYS did not take the approval and its state governs
// sending: the unconfirmed-grant check withdraws it as a conflict (an
// alarm, counted). İYS approvals and recipients İYS does not know never
// change the ledger.
func TestIYSStatusSync_InFlightGrantThenConflict(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	late, unknown, approved := actor(t, "student").UserID, actor(t, "student_b").UserID, actor(t, "club_member").UserID
	e.iys.SetStatus(late, consent.IYSEmail, consent.IYSState{Status: consent.IYSRefused, At: e.clk.Now().Add(-48 * time.Hour), Source: "HS_WEB"})
	e.iys.SetStatus(approved, consent.IYSEmail, consent.IYSState{Status: consent.IYSApproved, At: e.clk.Now().Add(-time.Hour), Source: "HS_WEB"})
	e.iys.Fail(errors.New("iys: 503")) // Koro's approvals do not get through
	e.record(t, grant(late, text, consent.ChannelWeb))
	e.record(t, grant(unknown, text, consent.ChannelWeb))
	e.record(t, grant(approved, text, consent.ChannelWeb))
	_ = e.deliverAll(t)

	e.clk.Advance(consent.IYSInFlightGrace - time.Hour)
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	for _, u := range []uuid.UUID{late, unknown, approved} {
		if c := e.latestOf(t, u, consent.PurposeCommercialEmail); c.Action != consent.Granted {
			t.Fatalf("within the grace period %s: %+v", u, c)
		}
	}

	e.clk.Advance(2 * time.Hour)
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	c := e.latestOf(t, late, consent.PurposeCommercialEmail)
	if c.Action != consent.Withdrawn || c.Channel != consent.ChannelIYS {
		t.Fatalf("after the grace period: %+v, want an İYS withdrawal", c)
	}
	var reason any
	for _, a := range e.audit.of(string(authz.ConsentRecordCreate)) {
		if a.ResourceID != nil && *a.ResourceID == c.ID {
			reason = a.Metadata["iys_reason"]
		}
	}
	if reason != "conflict" {
		t.Fatalf("iys_reason = %v, want conflict", reason)
	}
	if got := alarmCount(e, consent.IYSAlarmConflict); got != 1 {
		t.Fatalf("conflict alarms = %d, want 1", got)
	}
	for _, u := range []uuid.UUID{unknown, approved} {
		if c := e.latestOf(t, u, consent.PurposeCommercialEmail); c.Action != consent.Granted {
			t.Fatalf("%s changed: %+v", u, c)
		}
	}
}

// TestIYSStatusSync_BatchesAndFailures: the first run reconciles every
// grant (the feed has no history yet) in batches of at most IYSStatusBatch
// users (the fake refuses more), every one is checked; a failed İYS read
// ends the run with an error, changes nothing and leaves the channel's
// state as it was (no refusal is lost: the next run repeats the reads).
func TestIYSStatusSync_BatchesAndFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	users := make([]uuid.UUID, 2*consent.IYSStatusBatch+5)
	for i := range users {
		users[i] = uuid.NewV7()
		if _, err := e.svc.Record(ctx, grant(users[i], text, consent.ChannelWeb)); err != nil {
			t.Fatal(err)
		}
	}
	e.clk.Advance(time.Minute)
	refused := users[consent.IYSStatusBatch+3]
	e.iys.SetStatus(refused, consent.IYSEmail, consent.IYSState{Status: consent.IYSRefused, At: e.clk.Now(), Source: "IYS_MOBIL"})

	e.iys.FailStatuses(errors.New("iys: 503 for ada@example.com"))
	err := e.syncNow(t)
	if err == nil || strings.Contains(err.Error(), "ada@example.com") {
		t.Fatalf("run with İYS down = %v, want a scrubbed error", err)
	}
	if _, ok, _ := e.store.IYSSync(ctx, consent.IYSEmail); ok {
		t.Fatal("an incomplete run wrote the channel state")
	}
	if c := e.latestOf(t, refused, consent.PurposeCommercialEmail); c.Action != consent.Granted {
		t.Fatalf("a failed run changed the ledger: %+v", c)
	}

	e.iys.FailStatuses(nil)
	calls := e.iys.StatusCalls()
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	if got := e.iys.StatusCalls() - calls; got != 3 { // the refusal came through the feed: 204 grants left
		t.Fatalf("%d status reads for %d grants, want 3", got, len(users))
	}
	st, ok, _ := e.store.IYSSync(ctx, consent.IYSEmail)
	if !ok || st.Checked != len(users) || st.Withdrawn != 1 {
		t.Fatalf("state = %+v", st)
	}
	if c := e.latestOf(t, refused, consent.PurposeCommercialEmail); c.Action != consent.Withdrawn || c.Channel != consent.ChannelIYS {
		t.Fatalf("refusal in the second batch: %+v", c)
	}
}

// TestFilterActive_HoldsWhileIYSSyncStale (CONSENT_IYS_SYNC_MAX_AGE): with
// the hold on, an İYS purpose's FilterActive fails with ErrIYSSyncStale
// until the channel was reconciled, and again once the last reconciliation
// is older than the limit. Purposes without İYS (push, discovery) are never
// held; with the hold off nothing is.
func TestFilterActive_HoldsWhileIYSSyncStale(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *consent.Deps) { d.IYSSyncMaxAge = 6 * time.Hour })
	ctx := t.Context()
	user := actor(t, "student").UserID
	email := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	push := e.consentText(t, consent.PurposeCommercialPush, "2026.10.01")
	e.record(t, grant(user, email, consent.ChannelWeb))
	e.record(t, grant(user, push, consent.ChannelWeb))
	ids := []uuid.UUID{user}

	if _, err := e.svc.FilterActive(ctx, ids, consent.PurposeCommercialEmail); !errors.Is(err, consent.ErrIYSSyncStale) {
		t.Fatalf("never reconciled: %v, want ErrIYSSyncStale", err)
	}
	if got, err := e.svc.FilterActive(ctx, ids, consent.PurposeCommercialPush); err != nil || len(got) != 1 {
		t.Fatalf("push is held: %v, %v", got, err)
	}
	if got, err := e.svc.FilterActive(ctx, nil, consent.PurposeCommercialEmail); err != nil || len(got) != 0 {
		t.Fatalf("empty input: %v, %v", got, err)
	}
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.FilterActive(ctx, ids, consent.PurposeCommercialEmail); err != nil || !slices.Equal(got, ids) {
		t.Fatalf("after the reconciliation: %v, %v", got, err)
	}
	e.clk.Advance(6*time.Hour + time.Minute)
	_, err := e.svc.FilterActive(ctx, ids, consent.PurposeCommercialEmail)
	if !errors.Is(err, consent.ErrIYSSyncStale) || !strings.Contains(err.Error(), "email") {
		t.Fatalf("late reconciliation: %v, want ErrIYSSyncStale", err)
	}
	if err := e.syncNow(t); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.FilterActive(ctx, ids, consent.PurposeCommercialEmail); err != nil {
		t.Fatalf("after the next run: %v", err)
	}

	off := newEnv(t)
	off.record(t, grant(user, off.consentText(t, consent.PurposeCommercialEmail, "2026.10.01"), consent.ChannelWeb))
	if got, err := off.svc.FilterActive(ctx, ids, consent.PurposeCommercialEmail); err != nil || len(got) != 1 {
		t.Fatalf("hold off: %v, %v", got, err)
	}
}

// statusless is a real İYS client that reads nothing (an adapter that
// only registers consents).
type statusless struct{ consent.NoopIYS }

func (statusless) Name() string { return "iys" }
func (statusless) Real() bool   { return true }

// TestNew_IYSStatusReader: the readers come from the İYS client when it
// reads İYS (or are given); without one consent.iys_status_sync is not
// scheduled and the hold refuses to start; a negative hold is a wiring
// bug. iys is never a channel a grant can use.
func TestNew_IYSStatusReader(t *testing.T) {
	t.Parallel()
	jobs := func(e *env) []string {
		var out []string
		for _, j := range e.mod.Jobs() {
			out = append(out, j.Name)
		}
		return out
	}
	if got := jobs(newEnv(t)); !slices.Equal(got, []string{consent.ReconcileJobName, consent.StatusSyncJobName}) {
		t.Fatalf("jobs with FakeIYS = %v", got)
	}
	if got := jobs(newEnv(t, func(d *consent.Deps) { d.IYS = statusless{} })); !slices.Equal(got, []string{consent.ReconcileJobName}) {
		t.Fatalf("jobs with a client that cannot read states = %v", got)
	}
	reader := &consent.FakeIYS{}
	if got := jobs(newEnv(t, func(d *consent.Deps) { d.IYS, d.IYSFeed = statusless{}, reader })); len(got) != 2 {
		t.Fatalf("jobs with an explicit feed reader = %v", got)
	}
	if got := jobs(newEnv(t, func(d *consent.Deps) { d.IYS, d.IYSStates = statusless{}, reader })); len(got) != 2 {
		t.Fatalf("jobs with an explicit state reader = %v", got)
	}

	base := newEnvDeps(t)
	for name, mut := range map[string]func(*consent.Deps){
		"hold without a reader": func(d *consent.Deps) { d.IYS, d.IYSSyncMaxAge = statusless{}, 6*time.Hour },
		"hold with the no-op":   func(d *consent.Deps) { d.IYS, d.IYSSyncMaxAge = nil, 6*time.Hour },
		"negative hold":         func(d *consent.Deps) { d.IYSSyncMaxAge = -time.Hour },
	} {
		d := base
		mut(&d)
		if _, err := consent.New(d); err == nil {
			t.Errorf("%s: New accepted it", name)
		} else if !strings.Contains(err.Error(), "IYS_SYNC_MAX_AGE") && !strings.Contains(err.Error(), "IYSSyncMaxAge") {
			t.Errorf("%s: %v", name, err)
		}
	}

	e := newEnv(t)
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	user := actor(t, "student").UserID
	if _, err := e.svc.Record(t.Context(), grant(user, text, consent.ChannelIYS)); !errors.Is(err, consent.ErrChannelNotAllowed) {
		t.Fatalf("grant through iys = %v, want %v", err, consent.ErrChannelNotAllowed)
	}
}

// newEnvDeps returns the dependencies newEnv builds the module from.
func newEnvDeps(t *testing.T) consent.Deps {
	t.Helper()
	var out consent.Deps
	newEnv(t, func(d *consent.Deps) { out = *d })
	return out
}
