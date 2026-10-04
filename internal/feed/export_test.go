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
