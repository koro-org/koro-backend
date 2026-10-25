package netgsm

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// PurgeJobName is the daily deletion of the webhook items older than the
// retention (policy RetentionPolicyCode; job:consent.iys_netgsm_purge in
// cmd/koro/testdata/kvkk/retention_policies.txt). It runs whichever İYS
// adapter is configured: items of an earlier Netgsm period go too.
const PurgeJobName = "consent.iys_netgsm_purge"

const (
	purgeCron    = "40 4 * * *"
	purgeUnique  = 23 * time.Hour
	purgeTimeout = 10 * time.Minute
)

// PurgeDeps configures PurgeJob.
type PurgeDeps struct {
	Store Store
	Clock clock.Clock
	// Period returns the period of the retention policy (kvkk's
	// retention_policies through cmd/koro); nil → Retention. A period below
	// Retention is raised to it: the feed's window needs the items.
	Period func(ctx context.Context) (time.Duration, error)
	Log    *slog.Logger // nil discards
}

// PurgeJob is the daily retention of iys_netgsm_events: operational data,
// deleted on its period without waiting for the legal approval (like
// outbox_published), in batches.
func PurgeJob(d PurgeDeps) queue.Job {
	if d.Log == nil {
		d.Log = obs.Discard()
	}
	return queue.Job{
		Name: PurgeJobName, Cron: purgeCron, Queue: queue.Low, Unique: purgeUnique, Timeout: purgeTimeout,
		Handle: func(ctx context.Context) error {
			if d.Store == nil || d.Clock == nil {
				return errors.New("iys netgsm: the purge needs Store and Clock")
			}
			period := Retention
			if d.Period != nil {
				p, err := d.Period(ctx)
				switch {
				case err != nil:
					return err
				case p < Retention:
					d.Log.LogAttrs(ctx, slog.LevelWarn, "iys netgsm retention policy below the feed window; the window applies",
						slog.String("policy", RetentionPolicyCode), slog.Duration("period", p), slog.Duration("window", Retention))
				default:
					period = p
				}
			}
			cutoff, total := d.Clock.Now().Add(-period), 0
			for {
				n, err := d.Store.Purge(ctx, cutoff, purgeBatch)
				if err != nil {
					return err
				}
				total += n
				if n < purgeBatch {
					break
				}
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			d.Log.LogAttrs(ctx, slog.LevelInfo, "iys netgsm webhook items purged", slog.Int("deleted", total),
				slog.Duration("period", period))
			return nil
		},
	}
}
