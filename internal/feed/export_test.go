package feed

import (
	"context"

	"koro/internal/platform/domainevent"
)

// Test hooks of the feed package (feed_test).
var (
	SanitizeBody  = sanitizeBody
	AudienceMatch = audienceMatch
	ToPostDTO     = toPostDTO
	ToManageDTO   = toManageDTO
)

// HandlePostPublished runs the post.published hook of m.
func HandlePostPublished(m Module, ctx context.Context, e domainevent.Envelope) error {
	return m.svc.handlePostPublished(ctx, e)
}

// HandleMembershipStarted / HandleMembershipEnded / HandleSensitivityChanged
// run the membership and club consumers of m.
func HandleMembershipStarted(m Module, ctx context.Context, e domainevent.Envelope) error {
	return m.svc.handleMembershipStarted(ctx, e)
}

func HandleMembershipEnded(m Module, ctx context.Context, e domainevent.Envelope) error {
	return m.svc.handleMembershipEnded(ctx, e)
}

func HandleSensitivityChanged(m Module, ctx context.Context, e domainevent.Envelope) error {
	return m.svc.handleSensitivityChanged(ctx, e)
}

// ReconcileCounters runs the feed.reconcile_counters job of m.
func ReconcileCounters(m Module, ctx context.Context) error { return m.svc.reconcileCounters(ctx) }
