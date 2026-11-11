package ingest

import (
	"context"
	"log/slog"
	"time"

	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Job names (01 §7 doc patch).
const (
	// FetchDueJobName runs the due sources every minute (Europe/Istanbul;
	// each source keeps its own interval: lists 24h, events 6h).
	FetchDueJobName = "ingest.fetch_due"
	// PurgeExpiredJobName deletes the imported events past expires_at
	// (imported_event), the runs older than RunRetention and the one-off
	// sitemap pages fetched more than ChildRetention ago.
	PurgeExpiredJobName = "ingest.purge_expired"
)

// purgeBatch is the rows of one purge transaction (01 §7: ≤ 1000).
const purgeBatch = 1000

// Jobs returns the scheduled jobs of ingest.
func (m Module) Jobs() []queue.Job {
	return []queue.Job{
		{Name: FetchDueJobName, Cron: "@every 1m", Queue: queue.Low, Unique: 55 * time.Second,
			Timeout: 5 * time.Minute, MaxRetry: 1, Handle: m.svc.fetchDue},
		{Name: PurgeExpiredJobName, Cron: "40 3 * * *", Queue: queue.Low, Unique: 23 * time.Hour,
			Handle: m.svc.purgeExpired},
	}
}

// Subscribe registers ingest.pause_claimed_sources (club_claim.decided).
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicClubClaimDecided, PauseClaimedTaskName, m.svc.handleClaimDecided,
		outbox.Queue(queue.Default))
}

func (s *service) fetchDue(ctx context.Context) error {
	res, err := s.RunDue(ctx)
	if res.Runs > 0 || err != nil {
		attrs := []slog.Attr{slog.Int("runs", res.Runs), slog.Int("skipped", res.Skipped)}
		if err != nil {
			attrs = append(attrs, slog.String("err", err.Error()))
		}
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "ingest tick", attrs...)
	}
	return err
}

// purgeExpired is ingest.purge_expired: operational and data-minimising,
// never deferred to the legal approval of the policy (its period is read
// at run time, hooks.retention).
func (s *service) purgeExpired(ctx context.Context) error {
	now := s.now()
	events, err := db.Batches(ctx, s.d.Tx, purgeBatch, func(ctx context.Context) (int, error) {
		return s.d.Store.DeleteExpiredEvents(ctx, now, purgeBatch)
	})
	if err != nil {
		return err
	}
	runs, err := db.Batches(ctx, s.d.Tx, purgeBatch, func(ctx context.Context) (int, error) {
		return s.d.Store.DeleteRunsBefore(ctx, now.Add(-s.d.Config.RunRetention), purgeBatch)
	})
	if err != nil {
		return err
	}
	children, err := db.Batches(ctx, s.d.Tx, purgeBatch, func(ctx context.Context) (int, error) {
		return s.d.Store.DeleteFetchedChildren(ctx, now.Add(-s.d.Config.ChildRetention), purgeBatch)
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "ingest purge", slog.Int("imported_events", events), slog.Int("runs", runs),
		slog.Int("sitemap_pages", children))
	return nil
}
