//go:build integration

package talent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
	"koro/internal/talent"
)

// pgDestructions writes the destruction_logs row the way kvkk's recorder
// does, in the caller's transaction (db.Conn): the FK to the seeded policy
// (000134) and the grants of the role the statement runs on are real, so a
// record written on app_talent fails (42501). fail makes Record fail.
type pgDestructions struct {
	orm  *gorm.DB
	mu   sync.Mutex
	fail bool
}

func (r *pgDestructions) Record(ctx context.Context, d privacy.Destruction) error {
	r.mu.Lock()
	fail := r.fail
	r.mu.Unlock()
	if fail {
		return errors.New("destruction record refused (test)")
	}
	details, err := json.Marshal(d.Details)
	if err != nil {
		return err
	}
	return db.Conn(ctx, r.orm).Exec(`INSERT INTO destruction_logs (policy_code, run_id, module, target, method, record_count,
		period_cutoff, started_at, finished_at, details)
		SELECT code, ?, ?, coalesce(nullif(?, ''), target), ?, ?, ?, ?, ?, CAST( ? AS jsonb) FROM retention_policies WHERE code = ?`,
		d.RunID, d.Module, d.Target, d.Method, d.Count, d.PeriodCutoff, d.StartedAt, d.FinishedAt, string(details), d.PolicyCode).Error
}

func (r *pgDestructions) setFail(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = v
}

// TestRecompute_CollectionOff_Gorm (00 T-324; BE-087) over PostgreSQL,
// every talent statement as app_talent: scores computed with collection on
// survive another run with collection on; the first run with collection
// off deletes every score and cohort row and writes one destruction_logs
// row of talent_collection_off (seeded by 000134; the target is the
// policy's) on the session role in the same transaction; a refused record
// rolls the deletion back; the ledger, the neutralisation and the score
// runs stay; a second night changes nothing; back on, the first run scores
// everyone again.
func TestRecompute_CollectionOff_Gorm(t *testing.T) {
	t.Parallel()
	p := newPGEnvWith(t, envOptions{collection: true})
	params := bigCaps(testParams())
	params.ShrinkK = 0
	p.pgVersion(t, t0.Add(-time.Hour), params)
	uni := uuid.NewV7()
	var users []uuid.UUID
	for i := 1; i <= 11; i++ {
		u := p.pgStudentAt(t, uni, 2)
		p.pgRow(t, u, talent.SignalProfileSection, 100*i, t0)
		users = append(users, u)
	}
	if err := p.mod.Auditor.Neutralize(t.Context(), users[10], talent.NeutralizeAdmin, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.mod.Recompute(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	top := `SELECT count(*) FROM talent.scores WHERE user_id = $1 AND pct_final = 1.0000 AND band = 3 AND raw_cp = 1000`
	if n, c := p.owner(t, `SELECT count(*) FROM talent.scores`), p.owner(t, `SELECT count(*) FROM talent.cohorts`); n != 11 || c != 11 {
		t.Fatalf("collection on: %d scores, %d cohorts, want 11", n, c)
	}
	if n := p.owner(t, top, users[9]); n != 1 {
		t.Fatal("the top score is not stored")
	}
	logs := `SELECT count(*) FROM destruction_logs WHERE policy_code = 'talent_collection_off'`
	if n := p.owner(t, logs); n != 0 {
		t.Fatalf("collection on recorded %d destructions", n)
	}
	ledger := p.owner(t, `SELECT count(*) FROM talent.point_events`)
	runs := p.owner(t, `SELECT count(*) FROM talent.score_runs`)

	off := p.module(t, envOptions{})
	// A refused record rolls the batch back: nothing is deleted.
	p.pgRec.setFail(true)
	if err := off.Recompute(t.Context()); err == nil {
		t.Fatal("a refused destruction record did not fail the run")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores`); n != 11 {
		t.Fatalf("a refused record left %d scores, want the 11 rolled back", n)
	}
	p.pgRec.setFail(false)

	if err := off.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, c := p.owner(t, `SELECT count(*) FROM talent.scores`), p.owner(t, `SELECT count(*) FROM talent.cohorts`); n != 0 || c != 0 {
		t.Fatalf("collection off left %d scores, %d cohorts", n, c)
	}
	if n := p.owner(t, logs+` AND module = 'talent' AND target = 'talent.scores, talent.cohorts' AND method = 'hard_delete'
		AND record_count = 22 AND details->>'job' = 'talent.recompute' AND (details->>'scores')::int = 11
		AND (details->>'cohorts')::int = 11 AND started_at = $1 AND period_cutoff = $1`, t0); n != 1 {
		t.Fatalf("destruction_logs rows of the purge = %d, want 1", n)
	}
	if p.owner(t, `SELECT count(*) FROM talent.point_events`) != ledger || p.owner(t, `SELECT count(*) FROM talent.score_runs`) != runs ||
		p.owner(t, `SELECT count(*) FROM talent.neutralizations WHERE lifted_at IS NULL`) != 1 {
		t.Fatal("collection off touched the ledger, the score runs or the neutralisation")
	}

	// A second night: nothing left, nothing recorded.
	if err := off.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := p.owner(t, logs); n != 1 {
		t.Fatalf("destruction_logs rows after the second night = %d, want 1", n)
	}

	// Back on: the ledger scores everyone again.
	on := p.module(t, envOptions{collection: true})
	if err := on.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, c := p.owner(t, `SELECT count(*) FROM talent.scores`), p.owner(t, `SELECT count(*) FROM talent.cohorts`); n != 11 || c != 11 {
		t.Fatalf("back on: %d scores, %d cohorts, want 11", n, c)
	}
	if n := p.owner(t, top, users[9]); n != 1 {
		t.Fatal("back on, the top score is not the same")
	}
	if n := p.owner(t, logs); n != 1 {
		t.Fatalf("collection on recorded destructions: %d rows", n)
	}
}

// TestPurgeScores_OrphanRows_Gorm: a cohort row without its score (or a
// score without its cohort) is purged too: the users are the union of
// both tables.
func TestPurgeScores_OrphanRows_Gorm(t *testing.T) {
	t.Parallel()
	p := newPGEnvWith(t, envOptions{})
	cohortOnly, scoreOnly := p.user(t), p.user(t)
	if _, err := p.ownr.Exec(t.Context(), `INSERT INTO talent.cohorts (user_id, cohort_key) VALUES ($1, 'global')`, cohortOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ownr.Exec(t.Context(), `INSERT INTO talent.scores (user_id, raw_cp, pct_final, band, n_signals, cohort_key,
		config_version_id, run_id, calculated_at) VALUES ($1, 0, 0.5, 0, 0, 'global', $2, gen_random_uuid(), now())`, scoreOnly, p.cfg.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores`) + p.owner(t, `SELECT count(*) FROM talent.cohorts`); n != 0 {
		t.Fatalf("%d orphan rows left", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM destruction_logs WHERE policy_code = 'talent_collection_off' AND record_count = 2
		AND (details->>'scores')::int = 1 AND (details->>'cohorts')::int = 1`); n != 1 {
		t.Fatalf("destruction_logs rows = %d, want 1", n)
	}
}
