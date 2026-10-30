package orgs

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
)

// memberStore is the org_members rows of the membership lifecycle (02
// §7.3): joining, approval, role changes, ending, the member lists and the
// caller's own memberships (members.go, manage.go, invitations.go,
// lifecycle.go, the review workers).
type memberStore interface {
	// InsertMember writes a membership of m.OrgID and returns it as stored;
	// org_kind is the parent org's kind (06 H-38). ErrNotFound when the org
	// does not exist; 409 when the user already has an open membership of
	// the org or the company already has an active owner.
	InsertMember(ctx context.Context, m Membership) (Membership, error)
	// LockActiveMember returns the active membership of userID in orgID,
	// locked, or ErrNotFound.
	LockActiveMember(ctx context.Context, orgID, userID uuid.UUID) (Membership, error)
	// LockOwner returns the active company_owner membership of orgID,
	// locked, or ErrNotFound.
	LockOwner(ctx context.Context, orgID uuid.UUID) (Membership, error)
	// EndMember ends one open membership of orgID with reason and returns
	// it; ErrNotFound when it is not open (or not orgID's).
	EndMember(ctx context.Context, orgID, id uuid.UUID, now time.Time, reason EndReason) (Membership, error)
	// EndOpenMembers ends every pending or active membership of orgID with
	// reason and returns them as they were before (their status tells
	// which were active).
	EndOpenMembers(ctx context.Context, orgID uuid.UUID, now time.Time, reason EndReason) ([]Membership, error)
	// GetMember returns membership id of orgID (any status) or ErrNotFound.
	GetMember(ctx context.Context, orgID, id uuid.UUID) (Membership, error)
	// LockMember is GetMember with the row locked for the transaction.
	LockMember(ctx context.Context, orgID, id uuid.UUID) (Membership, error)
	// LockMemberOfUser returns membership id of userID (any org), locked, or
	// ErrNotFound: /me/memberships/{id} addresses the caller's own row.
	LockMemberOfUser(ctx context.Context, userID, id uuid.UUID) (Membership, error)
	// LockOpenMember returns the pending or active membership of userID in
	// orgID, locked, or ErrNotFound.
	LockOpenMember(ctx context.Context, orgID, userID uuid.UUID) (Membership, error)
	// LockActiveByRoles returns the active memberships of orgID holding one
	// of roles, locked (the last-admin guard serialises on them).
	LockActiveByRoles(ctx context.Context, orgID uuid.UUID, roles []authz.MemberRole) ([]Membership, error)
	// LockUserActiveByRoles returns the active memberships of userID (any
	// org) holding one of roles, locked, oldest first.
	LockUserActiveByRoles(ctx context.Context, userID uuid.UUID, roles []authz.MemberRole) ([]Membership, error)
	// ActivateMember turns a pending membership of orgID active (joined_at,
	// approved_by) and returns it; ErrNotFound when it is not pending.
	ActivateMember(ctx context.Context, orgID, id uuid.UUID, now time.Time, approvedBy uuid.UUID) (Membership, error)
	// DeleteMember hard-deletes one membership of orgID (a sensitive club's
	// membership is deleted when it ends, 02 §7.3). Deleting a missing row
	// is not an error (idempotent).
	DeleteMember(ctx context.Context, orgID, id uuid.UUID) error
	// ListMembers returns up to limit memberships of orgID matching f after
	// the position after (nil: first page), in f.Sort order.
	ListMembers(ctx context.Context, orgID uuid.UUID, f MemberFilter, after *MemberAfter, limit int) ([]Membership, error)
	// ListUserMemberships returns up to limit memberships of a user with one
	// of statuses (empty: pending and active), newest first, after the
	// membership ID after (nil: first page).
	ListUserMemberships(ctx context.Context, userID uuid.UUID, statuses []MemberStatus, after *uuid.UUID, limit int) ([]Membership, error)
}
