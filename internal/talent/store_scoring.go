package talent

import (
	"context"
	"time"
	"uuid"
)

// scoringStore is the nightly run, talent.recompute (01 §7): the users
// and ledger rows it scores, talent.score_runs, and the writes and
// deletions of talent.scores and talent.cohorts (recompute.go,
// collection_off.go, score_dropped.go).
type scoringStore interface {
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
}
