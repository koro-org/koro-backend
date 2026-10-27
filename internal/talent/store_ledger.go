package talent

import (
	"context"
	"uuid"
)

// ledgerStore is talent.point_events (02 §15), the append-only ledger:
// the credits and reversals of the consumers (ingest.go), the manual
// reversal (admin.go) and the reconciliation (reconcile.go), the report
// (report.go).
type ledgerStore interface {
	// InsertPoint appends p; inserted is false when its dedupe key (or, for
	// a reversal, its original) already has a row: the consumer saw the
	// event before. created_at comes from the database.
	InsertPoint(ctx context.Context, p PointEvent) (row PointEvent, inserted bool, err error)
	// FindPoint returns the row of a dedupe key or ErrNotFound.
	FindPoint(ctx context.Context, k SourceKey) (PointEvent, error)
	// CountCredited counts the user's rows of signal with positive points
	// that no reversal cancelled (the "at most 2 clubs" rule).
	CountCredited(ctx context.Context, userID uuid.UUID, signal string) (int, error)
	// Points returns the user's ledger, oldest first.
	Points(ctx context.Context, userID uuid.UUID) ([]PointEvent, error)
	// GetPoint returns one ledger row or ErrNotFound.
	GetPoint(ctx context.Context, id uuid.UUID) (PointEvent, error)
}
