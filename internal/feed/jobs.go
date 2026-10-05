package feed

import (
	"context"
	"log/slog"
	"time"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Task and job names of feed (01 §6.2, §7).
const (
	// NotifyFollowersTaskName consumes post.published (01 §6.2): followers
	// with notify=true get a push digest of a non-commercial post.
	NotifyFollowersTaskName = "feed.notify_followers"
	// AutoFollowTaskName consumes membership.started (01 §6.2): the member
	// follows the org (auto_membership), except in a sensitive club.
	AutoFollowTaskName = "feed.auto_follow"
	// RemoveAutoFollowTaskName consumes membership.ended: the automatic
	// follow goes (a manual one stays).
	RemoveAutoFollowTaskName = "feed.remove_auto_follow"
	// DropSensitiveAutoFollowsTaskName consumes club.sensitivity_changed:
	// a club that became sensitive loses its automatic follows, which
	// would otherwise reveal its memberships (02 §12.7).
	DropSensitiveAutoFollowsTaskName = "feed.drop_sensitive_auto_follows"
	// ReconcileCountersJobName is feed.reconcile_counters (01 §7: hourly at
	// :35, low).
	ReconcileCountersJobName = "feed.reconcile_counters"
)

// Subscribe registers the outbox subscriptions of feed (01 §6.2):
// post.published → feed.notify_followers; membership.started/ended → the
// automatic follow; club.sensitivity_changed → the automatic follows of a
// club that became sensitive; post.submitted_for_review,
// content_report.created and post.moderated → the moderation notices
// (BE-032, notices.go). post.liked/unliked and post.saved are talent's
// (P3).
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicPostPublished, NotifyFollowersTaskName, m.svc.handlePostPublished,
		outbox.Queue(queue.Low))
	q := outbox.Queue(queue.Default)
	r.Subscribe(domainevent.TopicPostSubmittedForReview, NotifyReviewQueueTaskName, m.svc.handleSubmittedForReview, q)
	r.Subscribe(domainevent.TopicContentReportCreated, NotifyReportCreatedTaskName, m.svc.handleReportCreated, q)
	r.Subscribe(domainevent.TopicPostModerated, NotifyPostModeratedTaskName, m.svc.handlePostModerated, q)
	r.Subscribe(domainevent.TopicMembershipStarted, AutoFollowTaskName, m.svc.handleMembershipStarted, q)
	r.Subscribe(domainevent.TopicMembershipEnded, RemoveAutoFollowTaskName, m.svc.handleMembershipEnded, q)
	r.Subscribe(domainevent.TopicClubSensitivityChanged, DropSensitiveAutoFollowsTaskName,
		m.svc.handleSensitivityChanged, q)
}

// Jobs returns the scheduled jobs of feed (01 §7).
func (m Module) Jobs() []queue.Job {
	return []queue.Job{
		{Name: ReconcileCountersJobName, Cron: "35 * * * *", Queue: queue.Low, Unique: 50 * time.Minute,
			Handle: m.svc.reconcileCounters},
	}
}

// handlePostPublished is the follower notification hook. In P1 it is a
// no-op (T-84: the push adapter goes live with BE-055 and the fan-out of a
// push digest to followers with notify=true is not in scope): the payload
// is checked and the event acknowledged, so no task is retried for
// nothing. A commercial post never notifies followers (01 §6.2).
func (s *service) handlePostPublished(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.PostPublished](e)
	if err != nil {
		return queue.Permanent(err)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelDebug, "post.published: follower push is a no-op in P1 (T-84)",
		slog.String("post_id", p.PostID.String()), slog.Bool("commercial", p.Commercial))
	return nil
}

// handleMembershipStarted is feed.auto_follow (membership.started).
// Idempotent: the follow is recomputed from the active memberships.
func (s *service) handleMembershipStarted(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.MembershipStarted](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.syncAutoFollow(ctx, p.UserID, p.OrgID, authz.OrgKind(p.OrgKind))
}

// handleMembershipEnded is feed.remove_auto_follow (membership.ended). A
// role change (the old membership ends, a new one starts) keeps the
// follow: another active membership of the org remains.
func (s *service) handleMembershipEnded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.MembershipEnded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.syncAutoFollow(ctx, p.UserID, p.OrgID, authz.OrgKind(p.OrgKind))
}

// handleSensitivityChanged is feed.drop_sensitive_auto_follows
// (club.sensitivity_changed): a club that became sensitive loses every
// automatic follow (a club that stopped being sensitive gets them back with
// the next membership event; no backfill). The flag is never logged.
func (s *service) handleSensitivityChanged(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ClubSensitivityChanged](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if !p.Sensitive {
		return nil
	}
	_, err = s.d.Store.RemoveAutoFollowsOf(ctx, p.ClubOrgID)
	return err
}

// reconcileCounters is feed.reconcile_counters: like_count and save_count
// back to the number of their rows (a drift is a bug or a crash between
// statements; it is logged with the count only).
func (s *service) reconcileCounters(ctx context.Context) error {
	n, err := s.d.Store.ReconcileCounters(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "feed counters corrected", slog.Int("posts", n))
	}
	return nil
}
