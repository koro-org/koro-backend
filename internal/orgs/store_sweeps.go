package orgs

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
)

// sweepStore is the batch work of the orgs jobs and event handlers
// (lifecycle.go): the purge of a club that became sensitive, the admin
// sweep's walk over the club_admin roles and the expiry of invitations.
type sweepStore interface {
	// DeleteEndedMembers hard-deletes up to limit ended memberships of
	// orgID (a club that became sensitive, 02 §7.3) and reports how many.
	DeleteEndedMembers(ctx context.Context, orgID uuid.UUID, limit int) (int, error)
	// ActiveByRole returns up to limit active memberships holding role in
	// any org, by ID ascending after the ID after (uuid.Nil(): from the
	// start): the admin sweep walks them in batches.
	ActiveByRole(ctx context.Context, role authz.MemberRole, after uuid.UUID, limit int) ([]Membership, error)
	// CloseExpiredInvitations closes up to limit open invitations whose
	// expires_at ≤ now (revoked_at = expires_at: the schema has no separate
	// expired column, T-206) and returns how many it closed.
	CloseExpiredInvitations(ctx context.Context, now time.Time, limit int) (int, error)
}
