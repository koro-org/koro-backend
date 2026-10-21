//go:build integration

package talent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/db"
	"koro/internal/students"
	"koro/internal/talent"
)

// pgAuditor is the talent_auditor persona as a real account (the FK target
// of created_by, updated_by and lifted_by).
func (p pgEnv) pgAuditor(t *testing.T) authz.Actor {
	t.Helper()
	a := persona(t, "talent_auditor")
	a.UserID = p.user(t)
	return a
}

// pgStudentAt inserts a user and registers it as an adult student of uni
// in class year.
func (p pgEnv) pgStudentAt(t *testing.T, uni uuid.UUID, year int16) uuid.UUID {
	t.Helper()
	id := p.user(t)
	p.students.mu.Lock()
	defer p.students.mu.Unlock()
	p.students.cohorts[id] = students.Cohort{UniversityID: uni, ClassYear: &year}
	return id
}

// pgRow appends a ledger row through the store (as app_talent).
func (p pgEnv) pgRow(t *testing.T, user uuid.UUID, signal string, points int, at time.Time) talent.PointEvent {
	t.Helper()
	row := talent.PointEvent{ID: uuid.NewV7(), UserID: user, Signal: signal, SourceType: talent.SourceCheckin, SourceID: uuid.NewV7(),
		OccurredAt: at, RawPointsCP: points, PointsCP: points, ReasonCode: signal, ConfigVersionID: p.cfg.ID, SourceEventID: uuid.NewV7()}
	ce := contractEnv{store: talent.NewGormStore(p.d.ORM), tx: p.d.Tx}
	ce.must(t, func(ctx context.Context) error {
		_, _, err := ce.store.InsertPoint(ctx, row)
		return err
	})
	return row
}

// pgVersion inserts a config version of params effective from at.
func (p pgEnv) pgVersion(t *testing.T, at time.Time, params talent.ScoreParams) talent.ConfigVersion {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var v talent.ConfigVersion
	ce := contractEnv{store: talent.NewGormStore(p.d.ORM), tx: p.d.Tx}
	creator := p.user(t)
	ce.must(t, func(ctx context.Context) error {
		v, err = ce.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: at, Params: raw, CreatedBy: creator})
		return err
	})
	return v
}

// TestRecompute_GormPipeline (01 §7, 02 §15.5–15.7): the nightly run over
// PostgreSQL, every statement as app_talent: the scores (numeric pct_final
// to four places, band, cohort key, top contributors), the cohort rows, a
// neutralised user at band 0, the stale score of a non-student removed and
// a score_runs row per run; a second run writes the same scores.
func TestRecompute_GormPipeline(t *testing.T) {
	t.Parallel()
	p := newPGEnvWith(t, envOptions{collection: true})
	params := bigCaps(testParams())
	params.ShrinkK = 0
	v := p.pgVersion(t, t0.Add(-time.Hour), params)
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
	stale := p.user(t) // no student profile, an old score
	if _, err := p.ownr.Exec(t.Context(), `INSERT INTO talent.scores (user_id, raw_cp, pct_final, band, n_signals, cohort_key,
		config_version_id, run_id, calculated_at) VALUES ($1, 900, 0.9500, 3, 4, 'global', $2, gen_random_uuid(), now())`, stale, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores`); n != 11 {
		t.Fatalf("scores = %d, want 11 (10 ranked + 1 neutralised)", n)
	}
	key := "uni:" + uni.String() + ":year:2"
	if n := p.owner(t, `SELECT count(*) FROM talent.scores WHERE user_id = $1 AND pct_final = 1.0000 AND band = 3 AND raw_cp = 1000
		AND cohort_key = $2 AND config_version_id = $3 AND jsonb_array_length(top_contributors) = 1`, users[9], key, v.ID); n != 1 {
		t.Fatal("the top ranked score is not stored as computed")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores WHERE user_id = $1 AND pct_final = 0.4444 AND band = 0`, users[4]); n != 1 {
		t.Fatal("the fifth score is not 0.4444 / band 0")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores WHERE user_id = $1 AND neutralized AND band = 0 AND raw_cp = 0`, users[10]); n != 1 {
		t.Fatal("the neutralised user is not band 0")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.cohorts WHERE cohort_key = $1`, key); n != 11 {
		t.Fatalf("cohort rows = %d", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores WHERE user_id = $1`, stale); n != 0 {
		t.Fatal("the stale score of a non-student survived")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.score_runs WHERE status = 'succeeded' AND users_processed = 11
		AND config_version_id = $1 AND finished_at IS NOT NULL AND error IS NULL`, v.ID); n != 1 {
		t.Fatal("no succeeded run recorded")
	}
	bands, err := p.mod.Ranking.Bands(t.Context(), users)
	if err != nil || bands[users[9]] != talent.Band(3) || len(bands) != 5 {
		t.Fatalf("bands = %d lifted, %v", len(bands), err)
	}
	if err := p.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.score_runs WHERE status = 'succeeded'`); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.scores WHERE user_id = $1 AND pct_final = 1.0000 AND band = 3`, users[9]); n != 1 {
		t.Fatal("the second run changed the top score")
	}
}

// TestAdmin_GormPipeline (04 §T over PostgreSQL): a config version by the
// auditor (signal catalogue updated with updated_by, the snapshot in the
// params, the record with the reason and the proof's kind; a refused proof
// is one denied record and nothing else); a neutralisation of an unknown
// user is 404; a reversal appends and the original cannot be updated even
// by the owner (koro_forbid_mutation, 23001); the report and the
// distribution are audited.
func TestAdmin_GormPipeline(t *testing.T) {
	t.Parallel()
	p := newPGEnvWith(t, envOptions{collection: true, auditorAPI: true})
	auditor := p.pgAuditor(t)
	ten, on := 10, true
	in := talent.ConfigInput{Reason: accessReason, Params: testParams(), Notes: "pilot",
		Signals: []talent.SignalUpdate{{Code: talent.SignalFeedLike, BasePointsCP: &ten, Enabled: &on}}}
	if _, err := p.mod.Admin.CreateConfigVersion(t.Context(), auditor, in, identity.ReauthProof{Password: "wrong"}); !errors.Is(err, identity.ErrReauthFailed) {
		t.Fatalf("wrong proof = %v", err)
	}
	if n := p.owner(t, `SELECT count(*) FROM audit_events WHERE action = 'talent.config.manage' AND outcome = 'denied'
		AND reason = 'reauth_failed' AND actor_user_id = $1`, auditor.UserID); n != 1 {
		t.Fatalf("denied records = %d, want 1", n)
	}
	cv, err := p.mod.Admin.CreateConfigVersion(t.Context(), auditor, in, goodProof)
	if err != nil {
		t.Fatal(err)
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.config_versions WHERE id = $1 AND created_by = $2 AND effective_from = $3
		AND jsonb_array_length(params->'signal_defs') = 10 AND (params->>'cohort_min')::int = 10`, cv.Active.ID, auditor.UserID, t0); n != 1 {
		t.Fatal("the version is not stored with its auditor and snapshot")
	}
	if n := p.owner(t, `SELECT count(*) FROM talent.signal_defs WHERE code = 'feed_like' AND base_points_cp = 10 AND daily_cap_cp IS NULL
		AND updated_by = $1`, auditor.UserID); n != 1 {
		t.Fatal("feed_like was not replaced by the auditor")
	}
	if n := p.owner(t, `SELECT count(*) FROM audit_events WHERE action = 'talent.config.manage' AND outcome = 'success'
		AND resource_id = $1 AND reason = $2 AND metadata->>'reauth' = 'password'`, cv.Active.ID, accessReason); n != 1 {
		t.Fatal("the version is not audited")
	}

	if _, err := p.mod.Admin.CreateNeutralization(t.Context(), auditor, uuid.NewV7(), talent.NeutralizationInput{Reason: accessReason,
		Note: "yok"}, goodProof); !errors.Is(err, talent.ErrNotFound) {
		t.Fatalf("neutralisation of an unknown user = %v, want 404", err)
	}
	subject := p.pgStudent(t)
	n, err := p.mod.Admin.CreateNeutralization(t.Context(), auditor, subject, talent.NeutralizationInput{Reason: accessReason, Note: "şüpheli"}, goodProof)
	if err != nil || n.CreatedBy == nil || *n.CreatedBy != auditor.UserID {
		t.Fatalf("neutralisation = %+v, %v", n, err)
	}
	if _, err := p.mod.Admin.LiftNeutralization(t.Context(), auditor, n.ID, talent.NeutralizationInput{Reason: accessReason}, goodProof); err != nil {
		t.Fatal(err)
	}
	if c := p.owner(t, `SELECT count(*) FROM talent.neutralizations WHERE id = $1 AND lifted_by = $2 AND lifted_at IS NOT NULL`, n.ID, auditor.UserID); c != 1 {
		t.Fatal("the lift is not stored")
	}

	orig := p.pgRow(t, subject, talent.SignalEventCheckin, 500, t0.Add(-time.Hour))
	rev, err := p.mod.Admin.ReversePoint(t.Context(), auditor, orig.ID, talent.ReversalInput{Reason: accessReason, Why: "hatalı okutma"}, goodProof)
	if err != nil {
		t.Fatal(err)
	}
	if c := p.owner(t, `SELECT count(*) FROM talent.point_events WHERE id = $1 AND reversal_of = $2 AND points_cp = -500
		AND source_type = 'reversal' AND reason_code = 'auditor_reversal' AND source_event_id = id`, rev.ID, orig.ID); c != 1 {
		t.Fatal("the reversal row is not stored as appended")
	}
	if _, err := p.mod.Admin.ReversePoint(t.Context(), auditor, orig.ID, talent.ReversalInput{Reason: accessReason, Why: "x"},
		goodProof); !errors.Is(err, talent.ErrAlreadyReversed) {
		t.Fatalf("second reversal = %v", err)
	}
	if _, err := p.ownr.Exec(t.Context(), `UPDATE talent.point_events SET points_cp = 0 WHERE id = $1`, orig.ID); db.SQLState(err) != db.CodeRestrictViolation {
		t.Fatalf("UPDATE of a ledger row = %v, want 23001 (append-only)", err)
	}

	if _, err := p.mod.Admin.Report(t.Context(), auditor, subject, accessReason); err != nil {
		t.Fatal(err)
	}
	if _, err := p.mod.Admin.Distribution(t.Context(), auditor, talent.DistributionQuery{Reason: accessReason}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.mod.Admin.Config(t.Context(), auditor, accessReason); err != nil {
		t.Fatal(err)
	}
	for _, act := range []string{"talent.report.read", "talent.distribution.read", "talent.config.read", "talent.reversal.create"} {
		if c := p.owner(t, `SELECT count(*) FROM audit_events WHERE action = $1 AND outcome = 'success' AND reason = $2
			AND actor_user_id = $3`, act, accessReason, auditor.UserID); c != 1 {
			t.Errorf("%s records = %d, want 1", act, c)
		}
	}
}
