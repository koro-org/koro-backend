package feed

import (
	"context"
	"uuid"
)

// logoStore is org_dropped_logos (BE-180): the logos an org replaced while a
// report on it was open or in review, kept for the moderators (report_org.go)
// until their retention (retention.go).
type logoStore interface {
	// DropLogo records a kept logo. A file is kept once
	// (org_dropped_logos_pkey, problem.ErrConflict).
	DropLogo(ctx context.Context, l DroppedLogo) error
	// DroppedLogosOf returns the kept logos of the org, oldest first.
	DroppedLogosOf(ctx context.Context, orgID uuid.UUID) ([]DroppedLogo, error)
	// ForgetDroppedLogos deletes the rows of the files (past their retention)
	// and returns how many went; a file without one is no error.
	ForgetDroppedLogos(ctx context.Context, fileIDs []uuid.UUID) (int, error)
}
