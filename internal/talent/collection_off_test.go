package talent_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/platform/obs"
	"koro/internal/platform/privacy"
	"koro/internal/talent"
)

// TestRecompute_CollectionOff_DeletesScoresAndCohorts (00 T-324; BE-087,
// user decision Q-02): a score computed while collection was on is
// deleted by the first nightly run after the flag went off: every score
// and cohort row (the neutralised user's included) goes in one batch,
// recorded in destruction_logs under talent_collection_off on the session
// role (the env's probe), and no score run is recorded. The ledger and the
// neutralisations stay. A second night deletes and records nothing. With
// collection on the rows stay and nothing is recorded; back on, the first
// run scores everyone from the ledger again, as before.
func TestRecompute_CollectionOff_DeletesScoresAndCohorts(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, bigCaps(testParams()))
	uni, y1 := uuid.NewV7(), int16(1)
	var users []uuid.UUID
	for i := range 12 {
		u := e.studentAt(uni, &y1)
		e.row(t, u, talent.SignalEventCheckin, 100*(i+1), day(float64(i)))
		users = append(users, u)
	}
	if err := e.mod.Auditor.Neutralize(t.Context(), users[11], talent.NeutralizeAdmin, nil); err != nil {
		t.Fatal(err)
	}
	e.recompute(t)
	e.recompute(t) // collection on: the rows stay, nothing is destroyed
	first := e.store.AllScores()
	if len(first) != 12 || len(e.store.Cohorts()) != 12 {
		t.Fatalf("collection on: %d scores, %d cohorts, want 12", len(first), len(e.store.Cohorts()))
	}
	if got := e.destr.all(); len(got) != 0 {
		t.Fatalf("collection on recorded destructions: %+v", got)
	}
	runs, ledger, neutral := len(e.store.Runs()), len(e.store.AllPoints()), len(e.store.AllNeutralizations())

	off := e.module(t, envOptions{})
	if err := off.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, c := len(e.store.AllScores()), len(e.store.Cohorts()); n != 0 || c != 0 {
		t.Fatalf("collection off left %d scores, %d cohorts", n, c)
	}
	got := e.destr.all()
	if len(got) != 1 {
		t.Fatalf("destruction records = %+v, want one batch", got)
	}
	d := got[0]
	if d.PolicyCode != "talent_collection_off" || d.Module != "talent" ||
		d.Method != privacy.MethodHardDelete || d.Count != 24 || d.RunID == uuid.Nil() || d.Target != "" ||
		!d.StartedAt.Equal(t0) || !d.PeriodCutoff.Equal(t0) || d.FinishedAt.IsZero() {
		t.Fatalf("destruction record = %+v", d)
	}
	if d.Details["job"] != talent.RecomputeJobName || d.Details["reason"] != "collection_off" || d.Details["scores"] != 12 ||
		d.Details["cohorts"] != 12 || len(d.Details) != 4 {
		t.Fatalf("details = %v (counts only, no personal data)", d.Details)
	}
	if len(e.store.Runs()) != runs || len(e.store.AllPoints()) != ledger || len(e.store.AllNeutralizations()) != neutral {
		t.Fatal("collection off touched the runs, the ledger or the neutralisations")
	}
	if b, err := off.Ranking.Bands(t.Context(), users); err != nil || len(b) != 0 {
		t.Fatalf("bands with collection off = %v, %v", b, err)
	}

	// A second night: nothing left, nothing recorded.
	if err := off.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.destr.all()); n != 1 {
		t.Fatalf("the second night recorded %d rows, want the first night's 1", n)
	}

	// Back on: the first run scores everyone from the ledger again.
	on := e.module(t, envOptions{collection: true})
	if err := on.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	again := e.store.AllScores()
	if len(again) != 12 || len(e.store.Cohorts()) != 12 {
		t.Fatalf("back on: %d scores, %d cohorts, want 12", len(again), len(e.store.Cohorts()))
	}
	for _, u := range users {
		a, b := first[u], again[u]
		a.RunID, b.RunID = uuid.Nil(), uuid.Nil()
		if !sameScore(a, b) {
			t.Fatalf("back on, %s scores %+v, before %+v", u, b, a)
		}
	}
	if n := len(e.destr.all()); n != 1 {
		t.Fatalf("collection on recorded %d rows, want 1", n)
	}
}

// TestRecompute_CollectionOff_BatchesOf500Users (01 §7: ≤ 1000 rows per
// transaction): 1230 users with a score and a cohort row go in batches of
// 500 users, one talent transaction and one destruction record each, all
// of one run: 1000 + 1000 + 460 rows; the short third batch ends the run
// without an empty fourth transaction.
func TestRecompute_CollectionOff_BatchesOf500Users(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	tx := &countTx{}
	mod, err := talent.New(talent.Deps{Store: e.store, Tx: tx, Audit: e.audit, Clock: e.clk, Log: obs.Discard(),
		Students: e.students, Directory: e.dir, Accounts: e.accounts, Events: e.events, Memberships: e.members, Likes: e.likes,
		Destructions: e.destr})
	if err != nil {
		t.Fatal(err)
	}
	for range 1230 {
		e.store.PutScore(uuid.NewV7(), talent.BandNeutral, false)
	}
	if err := mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(e.store.AllScores()) != 0 || len(e.store.Cohorts()) != 0 {
		t.Fatal("rows left after the purge")
	}
	got := e.destr.all()
	if len(got) != 3 || tx.n != 3 {
		t.Fatalf("%d records in %d transactions, want 3 and 3", len(got), tx.n)
	}
	for i, want := range []int{1000, 1000, 460} {
		if got[i].Count != want || got[i].RunID != got[0].RunID || got[i].Details["scores"] != want/2 {
			t.Fatalf("batch %d = %+v, want %d rows of run %s", i, got[i], want, got[0].RunID)
		}
	}
}

// TestRecompute_CollectionOff_RecordFailureFailsTheJob: a destruction
// that cannot be recorded fails the run with a retried error (PostgreSQL
// rolls the batch back with it: TestRecompute_CollectionOff_Gorm).
func TestRecompute_CollectionOff_RecordFailureFailsTheJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t, false)
	e.store.PutScore(uuid.NewV7(), talent.BandNeutral, false)
	e.destr.err = errors.New("kvkk down")
	err := e.mod.Recompute(t.Context())
	if err == nil || !errors.Is(err, e.destr.err) || isPermanent(err) {
		t.Fatalf("failed record: %v, want a retried error", err)
	}
	if got := e.destr.all(); len(got) != 0 {
		t.Fatalf("records = %+v", got)
	}
}
