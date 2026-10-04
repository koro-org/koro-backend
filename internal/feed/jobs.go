package feed

import (
	"context"
	"log/slog"

	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// NotifyFollowersTaskName consumes post.published (01 §6.2): followers
// with notify=true get a push digest of a non-commercial post.
const NotifyFollowersTaskName = "feed.notify_followers"

// Subscribe registers the outbox subscriptions of feed (01 §6.2):
// post.published → feed.notify_followers. post.submitted_for_review is
// consumed by the review queue notice of BE-032 (moderators, in-app).
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicPostPublished, NotifyFollowersTaskName, m.svc.handlePostPublished,
		outbox.Queue(queue.Low))
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
