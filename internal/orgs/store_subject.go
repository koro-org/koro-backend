package orgs

import (
	"context"
	"uuid"
)

// subjectStore is what orgs holds about one data subject, every org
// (KVKK export and erasure; privacy.go).
type subjectStore interface {
	// MembershipsOfUser returns every membership row of a user, oldest first
	// (KVKK export).
	MembershipsOfUser(ctx context.Context, userID uuid.UUID) ([]Membership, error)
	// InvitationsOfUser returns the invitations accepted by userID or (when
	// emailNormalized is not empty) addressed to emailNormalized, oldest
	// first (KVKK).
	InvitationsOfUser(ctx context.Context, userID uuid.UUID, emailNormalized string) ([]Invitation, error)
	// DeleteInvitationsOfUser deletes the invitations InvitationsOfUser
	// returns and reports how many it deleted.
	DeleteInvitationsOfUser(ctx context.Context, userID uuid.UUID, emailNormalized string) (int, error)
	// OrgsCreatedBy returns the orgs a user registered, oldest first (KVKK
	// export).
	OrgsCreatedBy(ctx context.Context, userID uuid.UUID) ([]Org, error)
}
