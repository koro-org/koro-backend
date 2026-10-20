package talent

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// Store is the persistence of the talent schema (02 §15). Every method
// runs inside a talent transaction: the service opens it with its
// TxManager and calls Enter first, which switches the transaction to the
// app_talent role (SET LOCAL ROLE app_talent; 01 §4.11, 02 §0.6). app_rw
// has no privilege on the schema, so a method called without Enter fails
// (errNoTalentTx in both stores; 42501 in PostgreSQL if it slipped
// through). Inside AsApp the transaction is back on the session role for
// public writes (the audit row) and the talent tables are unreachable.
//
// Talent tables have no org scope (02 §15: "Org kapsamı: yok"); every
// statement is keyed by the subject's user ID.
type Store interface {
	// Enter switches the transaction carried by ctx to app_talent and
	// returns the context the other methods require.
	Enter(ctx context.Context) (context.Context, error)
	// AsApp runs fn on the session role (app_rw) inside the same
	// transaction, then switches back to app_talent. fn writes public rows
	// only (audit); the talent tables are unreachable inside it.
	AsApp(ctx context.Context, fn func(context.Context) error) error

	// SignalDef returns one signal definition or ErrNotFound.
	SignalDef(ctx context.Context, code string) (SignalDef, error)
	// SignalDefs returns every definition (report categories).
	SignalDefs(ctx context.Context) ([]SignalDef, error)
	// CurrentConfig returns the config version in force at at (the latest
	// effective_from ≤ at) or ErrNotFound.
	CurrentConfig(ctx context.Context, at time.Time) (ConfigVersion, error)
	// InsertConfigVersion appends a config version (BE-052 surface; tests).
	InsertConfigVersion(ctx context.Context, v ConfigVersion) (ConfigVersion, error)

	// LockCounter returns the points applied so far to (user, day, signal),
	// creating the counter row and locking it until the transaction ends
	// (concurrent consumers of one user and signal queue behind it).
	LockCounter(ctx context.Context, userID uuid.UUID, day civil.Date, signal string) (int, error)
	// AppliedBetween sums the applied points of (user, signal) from..to
	// (both included): the weekly cap.
	AppliedBetween(ctx context.Context, userID uuid.UUID, signal string, from, to civil.Date) (int, error)
	// AddCounter adds delta to a counter LockCounter created.
	AddCounter(ctx context.Context, userID uuid.UUID, day civil.Date, signal string, delta int) error

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

	// Bands returns the stored bands > 0 of the users among userIDs whose
	// score is not neutralised and who have no active neutralisation.
	Bands(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Band, error)
	// StoredBand returns the user's stored band and whether a score row
	// exists (the report shows it as is).
	StoredBand(ctx context.Context, userID uuid.UUID) (band Band, found bool, err error)
	// HasData reports whether any talent row of the user exists.
	HasData(ctx context.Context, userID uuid.UUID) (bool, error)
	// EraseUser deletes every talent row of the user (KVKK erasure): the
	// counters, cohort, score and neutralisations directly, the
	// append-only ledger through talent.point_events_erase_user (000121),
	// which requires a completed erasure request of the user.
	EraseUser(ctx context.Context, userID uuid.UUID) (int, error)
}
