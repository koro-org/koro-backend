package talent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// Dropped scores (00 T-342; BE-101): with collection on, the nightly run
// deletes the score and cohort rows of a user it no longer scores: no
// ledger row left in lookback_days (every row dropped by a gaming
// threshold included), no student profile or a minor (T-23). The rows are
// the profiling output of an earlier run, so their deletion is a
// destruction of personal data and is recorded like the collection-off
// purge (BE-087, collection_off.go): every batch deletes the rows of at
// most purgeBatch users (≤ 1000 rows, 01 §7) in a talent transaction of
// its own and records them in destruction_logs in the same transaction,
// on the session role (Store.AsApp), under PolicyScoreDropped; the rows
// of one night share the score run's ID. A batch that deletes nothing
// (the user had no row yet, or the previous night already deleted it)
// records nothing.
//
// The record holds counts only: which of the three reasons applied stays
// out of it, since in a small batch the count of minors would point at a
// person (02 §16.3: no personal data in the record).
//
// PolicyScoreDropped is a job policy (retention_policies.txt
// job:talent.recompute): the run decides, not an age or the legal
// approval of a period, so the policy's period, approval and enabled
// columns are not read; the policy is the record's category.

// PolicyScoreDropped is the retention policy code of the destruction
// record (000142).
const PolicyScoreDropped = "talent_score_dropped"

// dropScores deletes the score and cohort rows of users (the run's
// unscored users, sorted) and records each batch that deleted a row;
// cutoff is the run's instant.
func (s *service) dropScores(ctx context.Context, runID uuid.UUID, cutoff time.Time, users []uuid.UUID) error {
	scores, cohorts := 0, 0
	for batch := range slices.Chunk(users, purgeBatch) {
		started := s.now()
		var ns, nc int
		err := s.inTx(ctx, func(ctx context.Context) error {
			var err error
			if ns, nc, err = s.d.Store.DeleteScores(ctx, batch); err != nil || ns+nc == 0 {
				return err
			}
			return s.d.Store.AsApp(ctx, func(ctx context.Context) error {
				return s.d.Destructions.Record(ctx, privacy.Destruction{PolicyCode: PolicyScoreDropped, RunID: runID,
					Module: providerName, Method: privacy.MethodHardDelete, Count: ns + nc, PeriodCutoff: cutoff,
					StartedAt: started, FinishedAt: s.now(),
					Details: map[string]any{"job": RecomputeJobName, "reason": "score_dropped", "scores": ns, "cohorts": nc}})
			})
		})
		if err != nil {
			return fmt.Errorf("%s: drop scores: %w", RecomputeJobName, err)
		}
		scores, cohorts = scores+ns, cohorts+nc
	}
	if scores+cohorts > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "talent scores deleted: users no longer scored (T-342)",
			slog.String("run_id", runID.String()), slog.Int("scores", scores), slog.Int("cohorts", cohorts))
	}
	return nil
}
