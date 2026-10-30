package orgs

import (
	"context"
	"time"
)

// retentionStore is the purge of the retention sweeper (retention.go;
// 02 §24.2, BE-057).
type retentionStore interface {
	// PurgeExpired deletes up to limit rows of the retention policy whose
	// anchor is before cutoff, oldest first, and returns how many it
	// deleted; with dryRun it counts every such row and deletes nothing
	// (BE-057). PolicyMembershipEnded: ended memberships by ended_at;
	// PolicyOrgInvitation: invitations accepted, revoked or expired, by
	// COALESCE(accepted_at, revoked_at, expires_at). Unknown policy: error.
	PurgeExpired(ctx context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error)
}
