package talent

import (
	"context"
	"uuid"
)

// subjectStore is every talent row of one data subject (KVKK; privacy.go,
// which holds the whole Store).
type subjectStore interface {
	// HasData reports whether any talent row of the user exists.
	HasData(ctx context.Context, userID uuid.UUID) (bool, error)
	// EraseUser deletes every talent row of the user (KVKK erasure): the
	// counters, cohort, score and neutralisations directly, the
	// append-only ledger through talent.point_events_erase_user (000121),
	// which requires a completed erasure request of the user.
	EraseUser(ctx context.Context, userID uuid.UUID) (int, error)
}
