package talent_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/talent"
)

func TestStore_ReconcileContract_Mem(t *testing.T) {
	t.Parallel()
	reconcileContract(t, func(t *testing.T) contractEnv {
		return contractEnv{store: talent.NewMemStore(clock.NewFake(t0).Now), tx: db.NopTx{},
			user: func(*testing.T) uuid.UUID { return uuid.NewV7() }}
	})
}

// reconcileContract pins the reconciliation state and the ledger walk
// (000138; 00 T-321, BE-090) in both stores.
func reconcileContract(t *testing.T, newEnv func(t *testing.T) contractEnv) {
	t.Run("state: drops counted, reconciliation records the count it started from", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		state := func() talent.CollectionState {
			t.Helper()
			var st talent.CollectionState
			ce.must(t, func(ctx context.Context) error {
				var err error
				st, err = ce.store.CollectionState(ctx)
				return err
			})
			return st
		}
		if st := state(); st.Pending() || st.ReversalsDropped != 0 || st.LastDroppedAt != nil || st.ReconciledAt != nil {
			t.Fatalf("initial state = %+v", st)
		}
		ce.must(t, func(ctx context.Context) error {
			if err := ce.store.StampReversalDropped(ctx, t0.Add(time.Hour)); err != nil {
				return err
			}
			// An earlier instant still counts; the last drop stays the latest.
			return ce.store.StampReversalDropped(ctx, t0)
		})
		st := state()
		if !st.Pending() || st.ReversalsDropped != 2 || st.LastDroppedAt == nil || !st.LastDroppedAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("state after two drops = %+v", st)
		}
		at := t0.Add(2 * time.Hour)
		ce.must(t, func(ctx context.Context) error { return ce.store.FinishReconcile(ctx, 1, at) })
		if st := state(); !st.Pending() || st.ReconciledDrops != 1 {
			t.Fatalf("a reconciliation of the first drop left %+v", st)
		}
		ce.must(t, func(ctx context.Context) error { return ce.store.FinishReconcile(ctx, 2, at) })
		if st := state(); st.Pending() || st.ReconciledDrops != 2 || st.ReconciledAt == nil || !st.ReconciledAt.Equal(at) {
			t.Fatalf("state after the reconciliation = %+v", st)
		}
		// An older run finishing late never moves the record back.
		ce.must(t, func(ctx context.Context) error { return ce.store.FinishReconcile(ctx, 1, at.Add(time.Hour)) })
		if st := state(); st.ReconciledDrops != 2 || !st.ReconciledAt.Equal(at) {
			t.Fatalf("an older reconciliation moved the record: %+v", st)
		}
		// More than was dropped: the CHECK refuses.
		if err := ce.in(t, func(ctx context.Context) error { return ce.store.FinishReconcile(ctx, 3, at) }); err == nil {
			t.Fatal("a reconciliation beyond the drops was recorded")
		}
		if err := ce.tx.WithTx(t.Context(), func(ctx context.Context) error { return ce.store.StampReversalDropped(ctx, t0) }); err == nil {
			t.Fatal("a drop was counted outside a talent transaction")
		}
	})

	t.Run("unreversed: originals of the signals without a reversal, keyset by ID", func(t *testing.T) {
		t.Parallel()
		ce := newEnv(t)
		user, creator := ce.user(t), ce.user(t)
		var rows []talent.PointEvent
		ce.must(t, func(ctx context.Context) error {
			cfg, err := ce.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: t0.Add(-time.Hour), CreatedBy: creator})
			if err != nil {
				return err
			}
			add := func(signal, src string) (talent.PointEvent, error) {
				p, _, err := ce.store.InsertPoint(ctx, talent.PointEvent{ID: uuid.NewV7(), UserID: user, Signal: signal, SourceType: src,
					SourceID: uuid.NewV7(), OccurredAt: t0, RawPointsCP: 10, PointsCP: 10, ReasonCode: signal,
					ConfigVersionID: cfg.ID, SourceEventID: uuid.NewV7()})
				return p, err
			}
			for _, x := range [][2]string{{talent.SignalFeedLike, talent.SourcePost}, {talent.SignalFeedLike, talent.SourcePost},
				{talent.SignalEventCheckin, talent.SourceCheckin}, {talent.SignalFeedLike, talent.SourcePost},
				{talent.SignalActiveDay, talent.SourceDay}} {
				p, err := add(x[0], x[1])
				if err != nil {
					return err
				}
				rows = append(rows, p)
			}
			// rows[1] is reversed: neither it nor its reversal is listed.
			id := rows[1].ID
			_, _, err = ce.store.InsertPoint(ctx, talent.PointEvent{ID: uuid.NewV7(), UserID: user, Signal: talent.SignalFeedLike,
				SourceType: talent.SourceReversal, SourceID: id, OccurredAt: t0, RawPointsCP: -10, PointsCP: -10,
				ReasonCode: "post_unliked", ConfigVersionID: cfg.ID, ReversalOf: &id, SourceEventID: uuid.NewV7()})
			return err
		})
		signals := []string{talent.SignalEventCheckin, talent.SignalFeedLike}
		ids := func(ps []talent.PointEvent) []uuid.UUID {
			out := []uuid.UUID{}
			for _, p := range ps {
				out = append(out, p.ID)
			}
			return out
		}
		var first, rest []talent.PointEvent
		ce.must(t, func(ctx context.Context) error {
			var err error
			if first, err = ce.store.Unreversed(ctx, signals, uuid.Nil(), 2); err != nil {
				return err
			}
			rest, err = ce.store.Unreversed(ctx, signals, first[len(first)-1].ID, 1000)
			return err
		})
		got := append(ids(first), ids(rest)...)
		want := []uuid.UUID{rows[0].ID, rows[2].ID, rows[3].ID}
		if len(first) != 2 || len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("Unreversed = %v then %v, want %v", ids(first), ids(rest), want)
		}
		if first[0].UserID != user || first[0].Signal != talent.SignalFeedLike || first[0].PointsCP != 10 {
			t.Fatalf("row = %+v", first[0])
		}
		for _, limit := range []int{0, 1001} {
			if err := ce.in(t, func(ctx context.Context) error {
				_, err := ce.store.Unreversed(ctx, signals, uuid.Nil(), limit)
				return err
			}); err == nil {
				t.Fatalf("Unreversed with limit %d succeeded", limit)
			}
		}
	})
}
