package talent

import (
	"context"
	"time"

	"koro/internal/platform/civil"
)

// retentionStore is the purges of the retention sweeper (retention.go).
//
// Retention (02 §24.2; BE-079). Every purge takes a batch size limit
// (1..1000) and a dry-run flag: dry counts what a purge would delete
// now and deletes nothing; otherwise one batch of at most limit rows
// goes, oldest first, and the count is returned.
type retentionStore interface {
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
}
