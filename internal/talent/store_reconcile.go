package talent

import (
	"context"
	"time"
	"uuid"
)

// reconcileStore is the reconciliation of a closed period (reconcile.go;
// the drop stamp: ingest.go).
//
// Reconciliation of a closed period (00 T-321; BE-090): the one-row
// talent.collection_state (000138).
type reconcileStore interface {
	// StampReversalDropped counts one reversal event dropped with
	// collection off at at (reversals_dropped + 1, last_dropped_at).
	StampReversalDropped(ctx context.Context, at time.Time) error
	// CollectionState returns the reconciliation state.
	CollectionState(ctx context.Context) (CollectionState, error)
	// FinishReconcile records a reconciliation that covered the first
	// through drops (the ReversalsDropped it started from) and finished at
	// at; a through below the recorded one changes nothing.
	FinishReconcile(ctx context.Context, through int64, at time.Time) error
	// Unreversed returns up to limit (1..1000) ledger rows of signals that
	// are originals no reversal points at, with an ID after after
	// (uuid.Nil(): from the start), by ID.
	Unreversed(ctx context.Context, signals []string, after uuid.UUID, limit int) ([]PointEvent, error)
}
