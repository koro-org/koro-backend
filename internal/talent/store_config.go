package talent

import (
	"context"
	"time"
	"uuid"
)

// configStore is talent.signal_defs and talent.config_versions (02 §15):
// the signal definitions and the config versions every ledger row and
// score refers to (ingest.go, recompute.go, multipliers.go, admin.go).
type configStore interface {
	// SignalDef returns one signal definition or ErrNotFound.
	SignalDef(ctx context.Context, code string) (SignalDef, error)
	// SignalDefs returns every definition (report categories).
	SignalDefs(ctx context.Context) ([]SignalDef, error)
	// CurrentConfig returns the config version in force at at (the latest
	// effective_from ≤ at) or ErrNotFound.
	CurrentConfig(ctx context.Context, at time.Time) (ConfigVersion, error)
	// InsertConfigVersion appends a config version (createTalentConfigVersion;
	// effective_from is unique: 409 on a clash).
	InsertConfigVersion(ctx context.Context, v ConfigVersion) (ConfigVersion, error)
	// UpdateSignalDef replaces the tunable fields of d's definition
	// (base points, caps, half-life, enabled) and records by as updated_by;
	// ErrNotFound for an unknown code. updated_at is the database's.
	UpdateSignalDef(ctx context.Context, d SignalDef, by uuid.UUID) (SignalDef, error)
}
