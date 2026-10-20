package talent_test

import (
	"context"
	"encoding/json"
	"errors"
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
}
