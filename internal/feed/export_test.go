package feed

import (
	"context"
	"fmt"

	"koro/internal/platform/domainevent"
)

// Test hooks of the feed package (feed_test).
var (
	SanitizeBody  = sanitizeBody
	AudienceMatch = audienceMatch
	ToPostDTO     = toPostDTO
	ToManageDTO   = toManageDTO
	ToTargetedDTO = toTargetedDTO
	ToReportDTO   = toReportDTO
	// NoticeTemplates are the notify templates of the moderation notices.
	NoticeTemplates = []string{templateReviewSubmitted, templateReportCreated, templatePostApproved, templatePostActioned,
		templatePostRestored, templateOrgModerated, templateReportResolved}
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

// HandleEvent runs the BE-032 consumer of e's topic (the moderation
// notices).
func HandleEvent(m Module, ctx context.Context, e domainevent.Envelope) error {
	switch e.Topic {
	case domainevent.TopicPostSubmittedForReview:
		return m.svc.handleSubmittedForReview(ctx, e)
	case domainevent.TopicContentReportCreated:
		return m.svc.handleReportCreated(ctx, e)
	case domainevent.TopicPostModerated:
		return m.svc.handlePostModerated(ctx, e)
	}
	return fmt.Errorf("no feed consumer of %s", e.Topic)
}

// HandleTask runs the plain task typ of m.
func HandleTask(m Module, ctx context.Context, typ string, payload []byte) error {
	for _, t := range m.Tasks() {
		if t.Name == typ {
			return t.Handle(ctx, payload)
		}
	}
	return fmt.Errorf("no feed task %s", typ)
}
