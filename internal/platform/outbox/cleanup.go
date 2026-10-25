package outbox

import (
	"context"
	"log/slog"
	"time"

	"koro/internal/platform/db"
	"koro/internal/platform/queue"
)

// Cleanup job (01 §7: outbox.cleanup, daily 04:10, low).
const (
	CleanupJobName   = "outbox.cleanup"
	cleanupCron      = "10 4 * * *"
	cleanupUnique    = 23 * time.Hour
	cleanupBatch     = 1000 // rows per statement (01 §7: ≤ 1000 per transaction)
	DefaultRetention = 7 * 24 * time.Hour
)

// CleanupJob deletes rows published longer ago than retention(ctx)
// (02 §1.1: 7 days, retention_policies code outbox_published), in batches.
func (o *Outbox) CleanupJob(retention func(ctx context.Context) (time.Duration, error)) queue.Job {
	return queue.Job{
		Name: CleanupJobName, Cron: cleanupCron, Queue: queue.Low, Unique: cleanupUnique,
		Handle: func(ctx context.Context) error {
			keep, err := retention(ctx)
			if err != nil {
				return err
			}
			cutoff := o.d.Clock.Now().Add(-keep)
			total, err := db.Drain(ctx, cleanupBatch, func(ctx context.Context) (int, error) {
				return o.d.Store.DeletePublished(ctx, cutoff, cleanupBatch)
			})
			if err != nil {
				return err
			}
			o.d.Log.LogAttrs(ctx, slog.LevelInfo, "outbox cleanup done", slog.Int("deleted", total))
			return nil
		},
	}
}
