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
	// Leave switches the transaction back to the session role for good
	// (SET LOCAL ROLE NONE) and returns a context the talent methods
	// refuse. Only the retention dry run uses it: it runs inside the
	// transaction kvkk.retention_sweep opened (and always rolls back), and
	// hands that transaction back on the role it found.
	Leave(ctx context.Context) (context.Context, error)

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
	// GetPoint returns one ledger row or ErrNotFound.
	GetPoint(ctx context.Context, id uuid.UUID) (PointEvent, error)

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
	// Score returns the user's score row; found is false without one.
	Score(ctx context.Context, userID uuid.UUID) (s Score, found bool, err error)
	// BandCounts counts the scores by band (0..3) that are not neutralised
	// and whose user has no active neutralisation, of the cohorts whose key
	// is cohortPrefix or starts with cohortPrefix + ":" ("" = every cohort):
	// getTalentDistribution.
	BandCounts(ctx context.Context, cohortPrefix string) (map[int]int, error)

	// ScoringUsers returns, sorted, the users the nightly run looks at: a
	// ledger row that occurred after since, a score or a cohort row.
	ScoringUsers(ctx context.Context, since time.Time) ([]uuid.UUID, error)
	// PointsSince returns the ledger rows of userIDs that occurred after
	// since, ordered by user, occurred_at and ID.
	PointsSince(ctx context.Context, userIDs []uuid.UUID, since time.Time) ([]PointEvent, error)
	// ActiveNeutralized reports the users among userIDs with an active
	// neutralisation of any reason.
	ActiveNeutralized(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	// InsertRun records the start of a nightly run (status running).
	InsertRun(ctx context.Context, r ScoreRun) error
	// FinishRun records the end of r: status, finished_at,
	// users_processed and the error text of a failed run.
	FinishRun(ctx context.Context, r ScoreRun) error
	// PutScores writes the scores (insert or replace by user) and the
	// cohort row of each user (talent.cohorts).
	PutScores(ctx context.Context, scores []Score) error
	// DeleteScores deletes the score and cohort rows of userIDs and
	// returns how many rows of each table went.
	DeleteScores(ctx context.Context, userIDs []uuid.UUID) (scores, cohorts int, err error)
	// PurgeScores deletes the score and cohort rows of at most limit users
	// (1..1000; the lowest user IDs holding either row) and returns how
	// many rows of each table went: the nightly run with collection off
	// (T-324, BE-087). A user's two rows go in the same statement; nothing
	// left → 0, 0.
	PurgeScores(ctx context.Context, limit int) (scores, cohorts int, err error)
	// HasData reports whether any talent row of the user exists.
	HasData(ctx context.Context, userID uuid.UUID) (bool, error)
	// EraseUser deletes every talent row of the user (KVKK erasure): the
	// counters, cohort, score and neutralisations directly, the
	// append-only ledger through talent.point_events_erase_user (000121),
	// which requires a completed erasure request of the user.
	EraseUser(ctx context.Context, userID uuid.UUID) (int, error)

	// Retention (02 §24.2; BE-079). Every purge takes a batch size limit
	// (1..1000) and a dry-run flag: dry counts what a purge would delete
	// now and deletes nothing; otherwise one batch of at most limit rows
	// goes, oldest first, and the count is returned.

	// PurgeLedger is talent.point_events_purge (000121; talent_ledger):
	// rows created before cutoff. A reversal points at its original, so
	// a batch deletes old reversals first and, once none is left, old
	// originals no reversal points at; an original with a younger reversal
	// stays until the reversal is old enough. A short batch is therefore
	// not the last one: the caller repeats until a batch deletes nothing.
	// The dry run counts the old rows without a younger reversal.
	// PostgreSQL clamps the cut-off to its own now() - retention and
	// refuses a real purge while the policy is not enabled and legally
	// approved (55000).
	PurgeLedger(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error)
	// PurgeConfigVersions is talent.config_versions_purge (000121; follows
	// talent_ledger): a version a later one superseded before cutoff (and
	// not after now), once no ledger row, score or score run refers to it.
	PurgeConfigVersions(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error)
	// PurgeCounters deletes the daily counters of the days before before
	// (talent_counters; the counter's day is its creation, Europe/Istanbul).
	PurgeCounters(ctx context.Context, before civil.Date, limit int, dry bool) (int, error)
	// PurgeRuns deletes the score runs started before cutoff (talent_runs;
	// a run is created when it starts), whatever their status.
	PurgeRuns(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error)
	// PurgeNeutralizations deletes the neutralisations lifted before
	// cutoff (talent_neutralization: its end is lifted_at); an active one
	// (an objection is never lifted) stays.
	PurgeNeutralizations(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error)

	// Reconciliation of a closed period (00 T-321; BE-090): the one-row
	// talent.collection_state (000138).

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
