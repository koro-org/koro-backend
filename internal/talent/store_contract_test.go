package talent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/talent"
)

// contractEnv is a store with its transaction manager and a user factory
// (FK targets in PostgreSQL).
type contractEnv struct {
	store talent.Store
	tx    db.TxManager
	user  func(t *testing.T) uuid.UUID
}

// in runs fn in a talent transaction of ce.
func (ce contractEnv) in(t *testing.T, fn func(ctx context.Context) error) error {
	t.Helper()
	return ce.tx.WithTx(t.Context(), func(ctx context.Context) error {
		ctx, err := ce.store.Enter(ctx)
		if err != nil {
			return err
		}
		return fn(ctx)
	})
}

func (ce contractEnv) must(t *testing.T, fn func(ctx context.Context) error) {
	t.Helper()
	if err := ce.in(t, fn); err != nil {
		t.Fatal(err)
	}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) contractEnv {
		return contractEnv{store: talent.NewMemStore(clock.NewFake(t0).Now), tx: db.NopTx{},
			user: func(*testing.T) uuid.UUID { return uuid.NewV7() }}
	})
}

// storeContract pins the Store behaviour both stores share.
func storeContract(t *testing.T, newEnv func(t *testing.T) contractEnv) {
	t.Run("signals_and_config", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		creator := ce.user(t)
		ce.must(t, func(ctx context.Context) error {
			d, err := ce.store.SignalDef(ctx, talent.SignalEventNoShow)
			if err != nil {
				return err
			}
			if d.BasePointsCP != -200 || d.WeeklyCapCP == nil || *d.WeeklyCapCP != 400 || d.DailyCapCP != nil || !d.Enabled {
				t.Errorf("event_no_show = %+v", d)
			}
			if _, err := ce.store.SignalDef(ctx, "no_such_signal"); !errors.Is(err, talent.ErrNotFound) {
				t.Errorf("unknown signal: %v", err)
			}
			defs, err := ce.store.SignalDefs(ctx)
			if err != nil || len(defs) != 10 {
				t.Errorf("signal defs = %d, %v", len(defs), err)
			}
			if _, err := ce.store.CurrentConfig(ctx, t0); !errors.Is(err, talent.ErrNotFound) {
				t.Errorf("config without versions: %v", err)
			}
			for _, at := range []time.Time{t0.Add(-48 * time.Hour), t0.Add(-time.Hour), t0.Add(time.Hour)} {
				if _, err := ce.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: at,
					Params: json.RawMessage(`{"cohort_min":30}`), Notes: "v", CreatedBy: creator}); err != nil {
					return err
				}
			}
			v, err := ce.store.CurrentConfig(ctx, t0)
			if err != nil {
				return err
			}
			if !v.EffectiveFrom.Equal(t0.Add(-time.Hour)) || v.CreatedBy != creator || v.CreatedAt.IsZero() || len(v.Params) == 0 {
				t.Errorf("current config = %+v", v)
			}
			return nil
		})
	})

	t.Run("ledger_dedupe_and_reversal", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		user, creator := ce.user(t), ce.user(t)
		var cfg talent.ConfigVersion
		ce.must(t, func(ctx context.Context) error {
			var err error
			cfg, err = ce.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: t0.Add(-time.Hour), CreatedBy: creator})
			return err
		})
		src := uuid.NewV7()
		orig := talent.PointEvent{ID: uuid.NewV7(), UserID: user, Signal: talent.SignalFeedLike, SourceType: talent.SourcePost, SourceID: src,
			OccurredAt: t0, RawPointsCP: 10, PointsCP: 10, ReasonCode: talent.SignalFeedLike, ConfigVersionID: cfg.ID, SourceEventID: uuid.NewV7()}
		ce.must(t, func(ctx context.Context) error {
			row, ok, err := ce.store.InsertPoint(ctx, orig)
			if err != nil || !ok || row.ID != orig.ID || row.CreatedAt.IsZero() || !row.OccurredAt.Equal(t0) {
				t.Fatalf("insert = %+v %v %v", row, ok, err)
			}
			dup := orig
			dup.ID = uuid.NewV7()
			if _, ok, err := ce.store.InsertPoint(ctx, dup); err != nil || ok {
				t.Fatalf("duplicate inserted = %v, %v", ok, err)
			}
			got, err := ce.store.FindPoint(ctx, orig.Key())
			if err != nil || got.ID != orig.ID {
				t.Fatalf("find = %+v %v", got, err)
			}
			if n, err := ce.store.CountCredited(ctx, user, talent.SignalFeedLike); err != nil || n != 1 {
				t.Fatalf("credited = %d %v", n, err)
			}
			id := orig.ID
			rev := talent.PointEvent{ID: uuid.NewV7(), UserID: user, Signal: talent.SignalFeedLike, SourceType: talent.SourceReversal,
				SourceID: orig.ID, OccurredAt: t0, RawPointsCP: -10, PointsCP: -10, ReasonCode: "post_unliked",
				ConfigVersionID: cfg.ID, ReversalOf: &id, SourceEventID: uuid.NewV7()}
			if _, ok, err := ce.store.InsertPoint(ctx, rev); err != nil || !ok {
				t.Fatalf("reversal = %v %v", ok, err)
			}
			rev2 := rev
			rev2.ID, rev2.SourceID = uuid.NewV7(), uuid.NewV7() // another key, same original
			if _, ok, err := ce.store.InsertPoint(ctx, rev2); err != nil || ok {
				t.Fatalf("second reversal of one row inserted = %v %v", ok, err)
			}
			if n, err := ce.store.CountCredited(ctx, user, talent.SignalFeedLike); err != nil || n != 0 {
				t.Fatalf("credited after reversal = %d %v", n, err)
			}
			pts, err := ce.store.Points(ctx, user)
			if err != nil || len(pts) != 2 {
				t.Fatalf("points = %d %v", len(pts), err)
			}
			if has, err := ce.store.HasData(ctx, user); err != nil || !has {
				t.Fatalf("has data = %v %v", has, err)
			}
			return nil
		})
		// A reversal without reversal_of violates the CHECK.
		bad := orig
		bad.ID, bad.SourceType, bad.SourceID = uuid.NewV7(), talent.SourceReversal, uuid.NewV7()
		if err := ce.in(t, func(ctx context.Context) error { _, _, err := ce.store.InsertPoint(ctx, bad); return err }); err == nil {
			t.Fatal("a reversal without its original was accepted")
		}
	})

	t.Run("leave", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		ce.must(t, func(ctx context.Context) error {
			lctx, err := ce.store.Leave(ctx)
			if err != nil {
				return err
			}
			if _, err := ce.store.SignalDefs(lctx); err == nil {
				t.Error("store read after Leave")
			}
			if _, err := ce.store.Leave(lctx); err == nil {
				t.Error("Leave outside the talent role succeeded")
			}
			return nil
		})
		if _, err := ce.store.Leave(t.Context()); err == nil {
			t.Error("Leave outside a transaction succeeded")
		}
	})

	t.Run("counters", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		user := ce.user(t)
		day := civil.Today(t0)
		ce.must(t, func(ctx context.Context) error {
			for i, d := range []civil.Date{day.AddDays(-7), day.AddDays(-6), day} {
				if v, err := ce.store.LockCounter(ctx, user, d, talent.SignalActiveDay); err != nil || v != 0 {
					t.Fatalf("lock %s = %d %v", d, v, err)
				}
				if err := ce.store.AddCounter(ctx, user, d, talent.SignalActiveDay, 20*(i+1)); err != nil {
					return err
				}
			}
			if v, err := ce.store.LockCounter(ctx, user, day, talent.SignalActiveDay); err != nil || v != 60 {
				t.Fatalf("relock = %d %v", v, err)
			}
			if n, err := ce.store.AppliedBetween(ctx, user, talent.SignalActiveDay, day.AddDays(-6), day); err != nil || n != 100 {
				t.Fatalf("week = %d %v, want 40 + 60", n, err)
			}
			return nil
		})
	})

	t.Run("neutralizations", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		user, auditor := ce.user(t), ce.user(t)
		dsr := uuid.NewV7()
		ce.must(t, func(ctx context.Context) error {
			n, ok, err := ce.store.InsertNeutralization(ctx, talent.Neutralization{ID: uuid.NewV7(), UserID: user, Reason: talent.NeutralizeObjection, DSRID: &dsr})
			if err != nil || !ok || n.CreatedAt.IsZero() || !n.Active() {
				t.Fatalf("insert = %+v %v %v", n, ok, err)
			}
			again, ok, err := ce.store.InsertNeutralization(ctx, talent.Neutralization{ID: uuid.NewV7(), UserID: user, Reason: talent.NeutralizeObjection, DSRID: &dsr})
			if err != nil || ok || again.ID != n.ID {
				t.Fatalf("second active objection = %+v %v %v", again, ok, err)
			}
			adm, ok, err := ce.store.InsertNeutralization(ctx, talent.Neutralization{ID: uuid.NewV7(), UserID: user, Reason: talent.NeutralizeAdmin, Note: "n", CreatedBy: &auditor})
			if err != nil || !ok {
				t.Fatalf("admin = %v %v", ok, err)
			}
			at := t0.Add(time.Hour)
			lifted, ok, err := ce.store.LiftNeutralization(ctx, adm.ID, &auditor, at)
			if err != nil || !ok || lifted.LiftedAt == nil || !lifted.LiftedAt.Equal(at) || *lifted.LiftedBy != auditor {
				t.Fatalf("lift = %+v %v %v", lifted, ok, err)
			}
			if _, ok, err := ce.store.LiftNeutralization(ctx, adm.ID, &auditor, at); err != nil || ok {
				t.Fatalf("second lift = %v %v", ok, err)
			}
			if _, _, err := ce.store.LiftNeutralization(ctx, uuid.NewV7(), nil, at); !errors.Is(err, talent.ErrNotFound) {
				t.Fatalf("lift unknown = %v", err)
			}
			all, err := ce.store.Neutralizations(ctx, user)
			if err != nil || len(all) != 2 {
				t.Fatalf("all = %d %v", len(all), err)
			}
			got, err := ce.store.GetNeutralization(ctx, adm.ID)
			if err != nil || got.Note != "n" || got.Active() {
				t.Fatalf("get = %+v %v", got, err)
			}
			return nil
		})
		// An objection without its request violates the CHECK.
		if err := ce.in(t, func(ctx context.Context) error {
			_, _, err := ce.store.InsertNeutralization(ctx, talent.Neutralization{ID: uuid.NewV7(), UserID: ce.user(t), Reason: talent.NeutralizeObjection})
			return err
		}); err == nil {
			t.Fatal("an objection without its request was accepted")
		}
	})

	t.Run("signal_update_and_point_lookup", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		user, auditor := ce.user(t), ce.user(t)
		ce.must(t, func(ctx context.Context) error {
			five, week := 5, 70
			d, err := ce.store.UpdateSignalDef(ctx, talent.SignalDef{Code: talent.SignalFeedLike, BasePointsCP: 5, DailyCapCP: &five,
				WeeklyCapCP: &week, HalfLifeDays: nil, Enabled: false}, auditor)
			if err != nil || d.BasePointsCP != 5 || *d.DailyCapCP != 5 || *d.WeeklyCapCP != 70 || d.HalfLifeDays != nil || d.Enabled ||
				d.Category != talent.CategoryEngagement || !d.ExcludedWhenSensitive {
				t.Fatalf("update = %+v %v", d, err)
			}
			if got, err := ce.store.SignalDef(ctx, talent.SignalFeedLike); err != nil || got.BasePointsCP != 5 || got.Enabled {
				t.Fatalf("after update = %+v %v", got, err)
			}
			if _, err := ce.store.UpdateSignalDef(ctx, talent.SignalDef{Code: "no_such_signal"}, auditor); !errors.Is(err, talent.ErrNotFound) {
				t.Fatalf("unknown signal = %v", err)
			}
			cfg, err := ce.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: t0.Add(-time.Hour), CreatedBy: auditor})
			if err != nil {
				return err
			}
			p := talent.PointEvent{ID: uuid.NewV7(), UserID: user, Signal: talent.SignalEventCheckin, SourceType: talent.SourceCheckin,
				SourceID: uuid.NewV7(), OccurredAt: t0.Add(-48 * time.Hour), RawPointsCP: 500, PointsCP: 500, ReasonCode: "event_checkin",
				ConfigVersionID: cfg.ID, SourceEventID: uuid.NewV7()}
			if _, _, err := ce.store.InsertPoint(ctx, p); err != nil {
				return err
			}
			got, err := ce.store.GetPoint(ctx, p.ID)
			if err != nil || got.ID != p.ID || got.PointsCP != 500 || got.ReversalOf != nil {
				t.Fatalf("get point = %+v %v", got, err)
			}
			if _, err := ce.store.GetPoint(ctx, uuid.NewV7()); !errors.Is(err, talent.ErrNotFound) {
				t.Fatalf("unknown point = %v", err)
			}
			since, err := ce.store.PointsSince(ctx, []uuid.UUID{user}, t0.Add(-72*time.Hour))
			if err != nil || len(since) != 1 {
				t.Fatalf("points since = %d %v", len(since), err)
			}
			if since, err := ce.store.PointsSince(ctx, []uuid.UUID{user}, t0.Add(-24*time.Hour)); err != nil || len(since) != 0 {
				t.Fatalf("points since after the row = %d %v", len(since), err)
			}
			users, err := ce.store.ScoringUsers(ctx, t0.Add(-72*time.Hour))
			if err != nil || len(users) != 1 || users[0] != user {
				t.Fatalf("scoring users = %v %v", users, err)
			}
			return nil
		})
	})

	t.Run("scores_cohorts_and_runs", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		a, b, c, creator := ce.user(t), ce.user(t), ce.user(t), ce.user(t)
		uni := uuid.NewV7()
		var cfg talent.ConfigVersion
		var run talent.ScoreRun
		ce.must(t, func(ctx context.Context) error {
			var err error
			cfg, err = ce.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: t0.Add(-time.Hour), CreatedBy: creator})
			if err != nil {
				return err
			}
			run = talent.ScoreRun{ID: uuid.NewV7(), ConfigVersionID: cfg.ID, StartedAt: t0}
			if err := ce.store.InsertRun(ctx, run); err != nil {
				return err
			}
			at := t0.Add(time.Minute)
			scores := []talent.Score{
				{UserID: a, RawCP: 1200, PctBP: 6667, Band: 1, NSignals: 4, CohortKey: "uni:" + uni.String() + ":year:3",
					ConfigVersionID: cfg.ID, RunID: run.ID, TopContributors: []talent.Contribution{{Signal: talent.SignalEventCheckin, ValueCP: 900}},
					CalculatedAt: t0},
				{UserID: b, RawCP: 0, PctBP: 5000, Band: 0, NSignals: 0, CohortKey: "uni:" + uni.String(), ConfigVersionID: cfg.ID,
					RunID: run.ID, Neutralized: true, CalculatedAt: t0},
				{UserID: c, RawCP: 300, PctBP: 9000, Band: 3, NSignals: 1, CohortKey: "global", ConfigVersionID: cfg.ID, RunID: run.ID,
					CalculatedAt: t0},
			}
			if err := ce.store.PutScores(ctx, scores); err != nil {
				return err
			}
			got, ok, err := ce.store.Score(ctx, a)
			if err != nil || !ok || got.RawCP != 1200 || got.PctBP != 6667 || got.Band != 1 || got.CohortKey != scores[0].CohortKey ||
				len(got.TopContributors) != 1 || got.TopContributors[0].ValueCP != 900 || !got.CalculatedAt.Equal(t0) || got.RunID != run.ID {
				t.Fatalf("score = %+v %v %v", got, ok, err)
			}
			if _, ok, err := ce.store.Score(ctx, creator); err != nil || ok {
				t.Fatalf("score of an unscored user = %v %v", ok, err)
			}
			// Replace: the next run overwrites the row.
			scores[0].RawCP, scores[0].PctBP, scores[0].Band = 1300, 7500, 2
			if err := ce.store.PutScores(ctx, scores[:1]); err != nil {
				return err
			}
			if got, _, _ := ce.store.Score(ctx, a); got.RawCP != 1300 || got.Band != 2 {
				t.Fatalf("replaced score = %+v", got)
			}
			counts, err := ce.store.BandCounts(ctx, "")
			if err != nil || counts[2] != 1 || counts[3] != 1 || counts[0] != 0 { // b is neutralised
				t.Fatalf("band counts = %v %v", counts, err)
			}
			counts, err = ce.store.BandCounts(ctx, "uni:"+uni.String())
			if err != nil || counts[2] != 1 || counts[3] != 0 {
				t.Fatalf("university band counts = %v %v", counts, err)
			}
			if _, _, err := ce.store.InsertNeutralization(ctx, talent.Neutralization{ID: uuid.NewV7(), UserID: c, Reason: talent.NeutralizeAdmin}); err != nil {
				return err
			}
			if counts, _ := ce.store.BandCounts(ctx, ""); counts[3] != 0 {
				t.Fatalf("an actively neutralised user is counted: %v", counts)
			}
			neutral, err := ce.store.ActiveNeutralized(ctx, []uuid.UUID{a, b, c})
			if err != nil || !neutral[c] || neutral[a] || len(neutral) != 1 {
				t.Fatalf("active neutralized = %v %v", neutral, err)
			}
			users, err := ce.store.ScoringUsers(ctx, t0)
			if err != nil || len(users) != 3 {
				t.Fatalf("scoring users = %v %v", users, err)
			}
			if n, err := ce.store.DeleteScores(ctx, []uuid.UUID{b, c}); err != nil || n != 4 {
				t.Fatalf("delete = %d %v, want 2 scores + 2 cohorts", n, err)
			}
			if users, _ := ce.store.ScoringUsers(ctx, t0); len(users) != 1 || users[0] != a {
				t.Fatalf("after delete = %v", users)
			}
			run.Status, run.UsersProcessed, run.FinishedAt = talent.RunSucceeded, 3, &at
			return ce.store.FinishRun(ctx, run)
		})
		// A neutralised score must be band 0 (scores_neutralized_chk).
		if err := ce.in(t, func(ctx context.Context) error {
			return ce.store.PutScores(ctx, []talent.Score{{UserID: a, Band: 2, Neutralized: true, CohortKey: "global",
				ConfigVersionID: cfg.ID, RunID: run.ID, CalculatedAt: t0}})
		}); err == nil {
			t.Fatal("a neutralised score with a band was accepted")
		}
	})
	// BE-087 (T-324): collection off deletes the score and cohort rows of
	// at most limit users per call, lowest user IDs first, both rows of a
	// user together; 1..1000 only.
	t.Run("purge_scores", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		creator := ce.user(t)
		users := make([]uuid.UUID, 5)
		for i := range users {
			users[i] = ce.user(t)
		}
		slices.SortFunc(users, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		ce.must(t, func(ctx context.Context) error {
			cfg, err := ce.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: t0.Add(-time.Hour), CreatedBy: creator})
			if err != nil {
				return err
			}
			var scores []talent.Score
			for _, u := range users {
				scores = append(scores, talent.Score{UserID: u, PctBP: 5000, CohortKey: "global", ConfigVersionID: cfg.ID,
					RunID: uuid.NewV7(), CalculatedAt: t0})
			}
			return ce.store.PutScores(ctx, scores)
		})
		ce.must(t, func(ctx context.Context) error {
			for _, limit := range []int{0, 1001} {
				if _, _, err := ce.store.PurgeScores(ctx, limit); err == nil {
					t.Errorf("purge with batch size %d accepted", limit)
				}
			}
			return nil
		})
		for i, want := range []int{2, 2, 1, 0} {
			ce.must(t, func(ctx context.Context) error {
				s, c, err := ce.store.PurgeScores(ctx, 2)
				if err != nil || s != want || c != want {
					t.Fatalf("purge %d = %d scores, %d cohorts, %v; want %d each", i, s, c, err, want)
				}
				left, err := ce.store.ScoringUsers(ctx, t0)
				if err != nil || !slices.Equal(left, users[min(2*(i+1), len(users)):]) {
					t.Fatalf("after purge %d: %v %v, want the highest IDs %v", i, left, err, users[min(2*(i+1), len(users)):])
				}
				return nil
			})
		}
	})
}
