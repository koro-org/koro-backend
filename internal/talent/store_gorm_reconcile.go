package talent

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// StampReversalDropped implements Store: the first drop creates the row
// (the migrations write none), every later one moves the counter (the row
// lock orders concurrent drops) and the instant only forwards.
//
// cross-org: the one-row reconciliation state, no user data.
func (s gormStore) StampReversalDropped(ctx context.Context, at time.Time) error {
	c, err := s.conn(ctx)
	if err != nil {
		return err
	}
	if err := c.Exec(`INSERT INTO talent.collection_state AS cs (id, reversals_dropped, last_dropped_at)
		VALUES (true, 1, CAST( ? AS timestamptz))
		ON CONFLICT (id) DO UPDATE SET reversals_dropped = cs.reversals_dropped + 1,
		  last_dropped_at = GREATEST(cs.last_dropped_at, EXCLUDED.last_dropped_at)`, at).Error; err != nil {
		return wrap("stamp reversal dropped", err)
	}
	return nil
}

type stateRow struct {
	ReversalsDropped int64      `gorm:"column:reversals_dropped"`
	LastDroppedAt    *time.Time `gorm:"column:last_dropped_at"`
	ReconciledDrops  int64      `gorm:"column:reconciled_drops"`
	ReconciledAt     *time.Time `gorm:"column:reconciled_at"`
}

// CollectionState implements Store; without a row nothing was dropped.
//
// cross-org: the one-row reconciliation state, no user data.
func (s gormStore) CollectionState(ctx context.Context) (CollectionState, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return CollectionState{}, err
	}
	var rows []stateRow
	if err := c.Raw(`SELECT reversals_dropped, last_dropped_at, reconciled_drops, reconciled_at
		FROM talent.collection_state WHERE id`).Scan(&rows).Error; err != nil {
		return CollectionState{}, wrap("collection state", err)
	}
	if len(rows) == 0 {
		return CollectionState{}, nil
	}
	return CollectionState(rows[0]), nil
}

// FinishReconcile implements Store. The row exists once a drop was
// counted; without it only a reconciliation of nothing (through 0) is
// valid, and it records nothing.
//
// cross-org: the one-row reconciliation state, no user data.
func (s gormStore) FinishReconcile(ctx context.Context, through int64, at time.Time) error {
	c, err := s.conn(ctx)
	if err != nil {
		return err
	}
	res := c.Exec(`UPDATE talent.collection_state SET reconciled_drops = ?, reconciled_at = CAST( ? AS timestamptz)
		WHERE id AND reconciled_drops <= ?`, through, at, through)
	if res.Error != nil {
		return wrap("finish reconcile", res.Error)
	}
	if res.RowsAffected == 0 && through > 0 {
		var n int64
		if err := c.Raw(`SELECT count(*) FROM talent.collection_state WHERE id`).Scan(&n).Error; err != nil {
			return wrap("finish reconcile", err)
		}
		if n == 0 {
			return fmt.Errorf("talent: finish reconcile: %d drops reconciled, none counted", through)
		}
	}
	return nil
}

// Unreversed implements Store: the originals of the signals by primary key
// (keyset), the reversal looked up through point_events_reversal_uq.
//
// cross-org: the reconciliation's ledger walk (every talent subject).
func (s gormStore) Unreversed(ctx context.Context, signals []string, after uuid.UUID, limit int) ([]PointEvent, error) {
	c, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > RetentionBatch {
		return nil, fmt.Errorf("talent: unreversed: batch size %d outside 1..%d", limit, RetentionBatch)
	}
	var rows []pointRow
	err = c.Raw(`SELECT `+pointCols+` FROM talent.point_events p
		WHERE p.signal_code = ANY( ?) AND p.reversal_of IS NULL AND p.id > ?
		  AND NOT EXISTS (SELECT 1 FROM talent.point_events r WHERE r.reversal_of = p.id)
		ORDER BY p.id LIMIT ?`, db.TextArray(signals), after, limit).Scan(&rows).Error
	if err != nil {
		return nil, wrap("unreversed", err)
	}
	out := make([]PointEvent, len(rows))
	for i, r := range rows {
		out[i] = r.point()
	}
	return out, nil
}
