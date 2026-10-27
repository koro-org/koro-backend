package talent

import (
	"context"
	"uuid"

	"koro/internal/platform/civil"
)

// counterStore is talent.daily_counters (02 §15): the points applied per
// user, day and signal behind the daily and weekly caps (ingest.go).
type counterStore interface {
	// LockCounter returns the points applied so far to (user, day, signal),
	// creating the counter row and locking it until the transaction ends
	// (concurrent consumers of one user and signal queue behind it).
	LockCounter(ctx context.Context, userID uuid.UUID, day civil.Date, signal string) (int, error)
	// AppliedBetween sums the applied points of (user, signal) from..to
	// (both included): the weekly cap.
	AppliedBetween(ctx context.Context, userID uuid.UUID, signal string, from, to civil.Date) (int, error)
	// AddCounter adds delta to a counter LockCounter created.
	AddCounter(ctx context.Context, userID uuid.UUID, day civil.Date, signal string, delta int) error
}
