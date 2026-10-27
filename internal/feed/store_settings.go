package feed

import (
	"context"
	"uuid"
)

// settingsStore is feed_org_settings (02 §12.1): an org's trust level and
// approved post count, read when a post is written or reported and moved
// by moderation (BE-032).
type settingsStore interface {
	// OrgSettings returns the feed settings of orgID; ok=false without a
	// row (the org is new, 02 §12.1).
	OrgSettings(ctx context.Context, orgID uuid.UUID) (OrgSettings, bool, error)

	// AddApprovedPost counts one more approved post of orgID (a new
	// settings row when there was none) and returns the row.
	AddApprovedPost(ctx context.Context, orgID uuid.UUID) (OrgSettings, error)
	// PromoteTrusted makes orgID trusted when it is new and has at least
	// threshold approved posts (feed.org_settings.auto_trust) and reports
	// whether it changed.
	PromoteTrusted(ctx context.Context, orgID uuid.UUID, threshold int) (bool, error)
	// SetTrust writes the trust level, restricted_until, reason and
	// updated_by of s (a new row when there was none; the approved post
	// count stays) and returns the row.
	SetTrust(ctx context.Context, s OrgSettings) (OrgSettings, error)
}
