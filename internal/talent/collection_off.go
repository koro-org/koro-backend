package talent

import (
	"context"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Collection off (00 T-324; BE-087, user decision of 2026-10-23): the
// scores and cohorts are the profiling output of the nightly run. While
// talent.default_enabled is false they serve no purpose (every band reads
// 0), so the nightly run deletes them instead of computing them: a profile
// computed while the flag was on does not outlive the flag (KVKK md. 4/2-ç,
// d). The ledger, the counters, the score runs and the neutralisations
// keep their own retention (BE-079): the ledger is what a run scores from
// once the flag is back on, a neutralisation must hold the moment it is.
//
// Every batch deletes the rows of at most purgeBatch users (≤ 1000 rows,
// 01 §7) in a talent transaction of its own and records them in
// destruction_logs in the same transaction, on the session role
// (Store.AsApp): the destruction commits with its record or not at all.
// The rows of one run share its run ID. A batch that deletes nothing
// records nothing, so a night with nothing left (every night after the
// first) leaves no row: the purge is idempotent.
//
// PolicyCollectionOff is a job policy (retention_policies.txt
// job:talent.recompute): the flag decides, not an age or the legal
// approval of a period, so the policy's period, approval and enabled
// columns are not read; the policy is the record's category.

// PolicyCollectionOff is the retention policy code of the destruction
// record (000134).
const PolicyCollectionOff = "talent_collection_off"

// purgeBatch is the users per purge transaction: a user has at most one
// score and one cohort row, so a batch deletes at most RetentionBatch rows.
const purgeBatch = RetentionBatch / 2

// purgeScores deletes every score and cohort row (collection off) and
// returns how many of each went.
func (s *service) purgeScores(ctx context.Context) (scores, cohorts int, err error) {
	runID := uuid.NewV7()
	// Fewer rows than users asked for: fewer users were left.
	_, err = db.Batches(ctx, db.TxFunc(s.inTx), purgeBatch, func(ctx context.Context) (int, error) {
		started := s.now()
		ns, nc, err := s.d.Store.PurgeScores(ctx, purgeBatch)
		if err != nil || ns+nc == 0 {
			return 0, err
		}
		if err := s.d.Store.AsApp(ctx, func(ctx context.Context) error {
			return s.d.Destructions.Record(ctx, privacy.Destruction{PolicyCode: PolicyCollectionOff, RunID: runID,
				Module: providerName, Method: privacy.MethodHardDelete, Count: ns + nc, PeriodCutoff: started,
				StartedAt: started, FinishedAt: s.now(),
				Details: map[string]any{"job": RecomputeJobName, "reason": "collection_off", "scores": ns, "cohorts": nc}})
		}); err != nil {
			return 0, err
		}
		// Counted on commit: an error returns the committed batches only.
		db.AfterCommit(ctx, func(context.Context) { scores, cohorts = scores+ns, cohorts+nc })
		return ns + nc, nil
	})
	if err != nil {
		return scores, cohorts, fmt.Errorf("%s: purge scores (collection off): %w", RecomputeJobName, err)
	}
	if scores+cohorts > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "talent scores deleted: collection is off (talent.default_enabled, T-324)",
			slog.String("run_id", runID.String()), slog.Int("scores", scores), slog.Int("cohorts", cohorts))
	}
	return scores, cohorts, nil
}
