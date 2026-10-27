package talent

import (
	"context"
	"time"
	"uuid"
)

// neutralizationStore is talent.neutralizations (02 §15): the opt-outs,
// objections and manual neutralisations of a user (neutralize.go,
// admin.go, ingest.go, report.go).
type neutralizationStore interface {
	// Neutralizations returns the user's neutralisations, oldest first.
	Neutralizations(ctx context.Context, userID uuid.UUID) ([]Neutralization, error)
	// GetNeutralization returns one neutralisation or ErrNotFound.
	GetNeutralization(ctx context.Context, id uuid.UUID) (Neutralization, error)
	// InsertNeutralization adds n unless the user has an active one of the
	// same reason (neutralizations_active_uq): inserted false then, and the
	// active row is returned.
	InsertNeutralization(ctx context.Context, n Neutralization) (row Neutralization, inserted bool, err error)
	// LiftNeutralization ends an active neutralisation; lifted is false when
	// it was not active.
	LiftNeutralization(ctx context.Context, id uuid.UUID, by *uuid.UUID, at time.Time) (row Neutralization, lifted bool, err error)
}
