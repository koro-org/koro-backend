package talent_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/platform/privacy"
	"koro/internal/talent"
)

// TestRecompute_DroppedScores_RecordedInDestructionLogs (00 T-342;
// BE-101): with collection on, the nightly run deletes the score and
// cohort rows of a user it no longer scores (no ledger row in the window,
// no student profile, a minor) and records them in one destruction_logs
// row of talent_score_dropped on the session role (the env's probe),
// under the score run's ID, counts only. A scored user keeps the rows; a
// dropped user without rows adds nothing to the count; a second night
// deletes and records nothing.
func TestRecompute_DroppedScores_RecordedInDestructionLogs(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, bigCaps(testParams()))
	uni, y1 := uuid.NewV7(), int16(1)
	kept := e.studentAt(uni, &y1)
	e.row(t, kept, talent.SignalEventCheckin, 100, day(1))
	noLedger := e.studentAt(uni, &y1)
	minor := e.minor()
	e.row(t, minor, talent.SignalEventCheckin, 100, day(1))
	stranger := uuid.NewV7() // no student profile
	e.row(t, stranger, talent.SignalEventCheckin, 100, day(1))
	for _, u := range []uuid.UUID{noLedger, minor, stranger} {
		e.store.PutScore(u, talent.BandNeutral, false) // computed by an earlier run
	}
	e.recompute(t)

	all := e.store.AllScores()
	if _, ok := all[kept]; !ok || len(all) != 1 || len(e.store.Cohorts()) != 1 {
		t.Fatalf("after the run: %d scores, %d cohorts, want only the scored user's", len(all), len(e.store.Cohorts()))
	}
	got := e.destr.all()
	if len(got) != 1 {
		t.Fatalf("destruction records = %+v, want one batch", got)
	}
	runs := e.store.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	d := got[0]
	if talent.PolicyScoreDropped != "talent_score_dropped" || d.PolicyCode != talent.PolicyScoreDropped || d.Module != "talent" ||
		d.Method != privacy.MethodHardDelete || d.Count != 6 || d.RunID != runs[0].ID || d.Target != "" ||
		!d.PeriodCutoff.Equal(t0) || !d.StartedAt.Equal(t0) || d.FinishedAt.IsZero() {
		t.Fatalf("destruction record = %+v", d)
	}
	if d.Details["job"] != talent.RecomputeJobName || d.Details["reason"] != "score_dropped" || d.Details["scores"] != 3 ||
		d.Details["cohorts"] != 3 || len(d.Details) != 4 {
		t.Fatalf("details = %v (counts only, no personal data)", d.Details)
	}

	// A second night: nothing left to delete, nothing recorded.
	e.recompute(t)
	if n := len(e.destr.all()); n != 1 {
		t.Fatalf("the second night recorded %d rows, want the first night's 1", n)
	}
}

// TestRecompute_DroppedScores_BatchesOf500Users (01 §7: ≤ 1000 rows per
// transaction): 1230 dropped users with a score and a cohort row go in
// batches of 500 users, one destruction record each, all of the run.
func TestRecompute_DroppedScores_BatchesOf500Users(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, bigCaps(testParams()))
	for range 1230 {
		e.store.PutScore(uuid.NewV7(), talent.BandNeutral, false)
	}
	e.recompute(t)
	if len(e.store.AllScores()) != 0 || len(e.store.Cohorts()) != 0 {
		t.Fatal("rows left after the run")
	}
	got, run := e.destr.all(), e.store.Runs()[0].ID
	if len(got) != 3 {
		t.Fatalf("%d records, want 3", len(got))
	}
	for i, want := range []int{1000, 1000, 460} {
		if got[i].Count != want || got[i].RunID != run || got[i].Details["scores"] != want/2 || got[i].PolicyCode != talent.PolicyScoreDropped {
			t.Fatalf("batch %d = %+v, want %d rows of run %s", i, got[i], want, run)
		}
	}
}

// TestRecompute_DroppedScores_RecordFailureFailsTheRun: a deletion that
// cannot be recorded fails the run (recorded as failed) with a retried
// error (PostgreSQL rolls the batch back with it:
// TestRecompute_DroppedScores_Gorm).
func TestRecompute_DroppedScores_RecordFailureFailsTheRun(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, bigCaps(testParams()))
	e.store.PutScore(uuid.NewV7(), talent.BandNeutral, false)
	e.destr.err = errors.New("kvkk down")
	err := e.mod.Recompute(t.Context())
	if err == nil || !errors.Is(err, e.destr.err) || isPermanent(err) {
		t.Fatalf("failed record: %v, want a retried error", err)
	}
	if runs := e.store.Runs(); len(runs) != 1 || runs[0].Status != talent.RunFailed {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
	if got := e.destr.all(); len(got) != 0 {
		t.Fatalf("records = %+v", got)
	}
}
