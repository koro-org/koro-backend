package orgs

import (
	"context"
	"uuid"
)

// directoryStore is the membership reads other modules make through
// OrgDirectory and MembershipHistory (lifecycle.go): the Actor's org roles,
// an org's active members and talent's ended memberships.
type directoryStore interface {
	// ActiveMembershipsOfUser returns the active memberships of a user in
	// every org, oldest first (Actor resolution, 03 §2.1).
	ActiveMembershipsOfUser(ctx context.Context, userID uuid.UUID) ([]Membership, error)
	// EndedMemberships returns the IDs among ids of the memberships that
	// ended (talent's reconciliation, BE-090).
	EndedMemberships(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error)
	// ActiveMemberIDs returns the user IDs of the active members of orgID.
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
}
