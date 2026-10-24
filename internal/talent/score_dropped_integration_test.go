//go:build integration

package talent_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/talent"
)

// TestRecompute_DroppedScores_Gorm (00 T-342; BE-101) over PostgreSQL,
// every talent statement as app_talent: a run scores three students; once
// one is a minor and another has no student profile, the next run deletes
// their score and cohort rows and writes one destruction_logs row of
// talent_score_dropped (seeded by 000142; the target is the policy's) on
// the session role in the same transaction, under the score run's ID; a
// refused record rolls the deletion back and fails the run; the ledger
// stays; a third night records nothing.
func TestRecompute_DroppedScores_Gorm(t *testing.T) {
	t.Parallel()
	p := newPGEnvWith(t, envOptions{collection: true})
	params := bigCaps(testParams())
	params.ShrinkK = 0
	p.pgVersion(t, t0.Add(-time.Hour), params)
	uni := uuid.NewV7()
	var users []uuid.UUID
	for i := 1; i <= 3; i++ {
		u := p.pgStudentAt(t, uni, 2)
		p.pgRow(t, u, talent.SignalProfileSection, 100*i, t0)
		users = append(users, u)
	}
	if err := p.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	scores, cohorts := `SELECT count(*) FROM talent.scores`, `SELECT count(*) FROM talent.cohorts`
	if n, c := p.owner(t, scores), p.owner(t, cohorts); n != 3 || c != 3 {
		t.Fatalf("first run: %d scores, %d cohorts, want 3", n, c)
	}
	logs := `SELECT count(*) FROM destruction_logs WHERE policy_code = 'talent_score_dropped'`
	if n := p.owner(t, logs); n != 0 {
		t.Fatalf("a run without drops recorded %d destructions", n)
	}
	ledger := p.owner(t, `SELECT count(*) FROM talent.point_events`)

	p.students.mu.Lock()
	c := p.students.cohorts[users[0]]
	c.IsMinor = true
	p.students.cohorts[users[0]] = c
	delete(p.students.cohorts, users[1])
	p.students.mu.Unlock()

	// A refused record rolls the batch back: nothing is deleted.
	p.pgRec.setFail(true)
	if err := p.mod.Recompute(t.Context()); err == nil {
		t.Fatal("a refused destruction record did not fail the run")
	}
	p.pgRec.setFail(false)
	if n := p.owner(t, scores); n != 3 {
		t.Fatalf("a refused record left %d scores, want the 3 rolled back", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.score_runs WHERE status = 'failed'`); n != 1 {
		t.Fatalf("failed runs = %d, want 1", n)
	}

	p.clk.Advance(time.Minute)
	if err := p.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, c := p.owner(t, scores), p.owner(t, cohorts); n != 1 || c != 1 {
		t.Fatalf("after the drop: %d scores, %d cohorts, want 1", n, c)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores WHERE user_id = $1`, users[2]); n != 1 {
		t.Fatal("the scored student lost the score")
	}
	at := t0.Add(time.Minute)
	if n := p.owner(t, logs+` AND module = 'talent' AND target = 'talent.scores, talent.cohorts' AND method = 'hard_delete'
		AND record_count = 4 AND details->>'job' = 'talent.recompute' AND details->>'reason' = 'score_dropped'
		AND (details->>'scores')::int = 2 AND (details->>'cohorts')::int = 2 AND started_at = $1 AND period_cutoff = $1
		AND run_id = (SELECT id FROM talent.score_runs WHERE started_at = $1 AND status = 'succeeded')`, at); n != 1 {
		t.Fatalf("destruction_logs rows of the drop = %d, want 1", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.point_events`); n != ledger {
		t.Fatalf("the drop touched the ledger: %d rows, want %d", n, ledger)
	}

	// A third night: nothing left to delete, nothing recorded.
	p.clk.Advance(time.Minute)
	if err := p.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := p.owner(t, logs); n != 1 {
		t.Fatalf("destruction_logs rows after the third night = %d, want 1", n)
	}
}
