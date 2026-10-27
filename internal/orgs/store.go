package orgs

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
)

// Store is the persistence boundary of orgs (05 §2.2) over organizations,
// companies, org_members and org_invitations (02 §7.1–7.4). The GORM store (store_gorm.go)
// and the in-memory store (store_mem.go) pass the same contract test
// (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any; the
// service draws the boundaries (OrgAdmin.Create joins the caller's).
//
// Org scope: organizations is the org root, addressed by its own id.
// companies and org_members are scoped by org_id, and their scope columns
// come from the parent organizations row, never from the caller (06 H-38):
// InsertCompany, InsertMember and InsertInvitation copy org_kind from the
// org they belong to.
type Store interface {
	// InsertOrg writes a new org and returns it as stored (created_at and
	// updated_at from the database). It reports false and writes nothing
	// when the slug is taken (ON CONFLICT DO NOTHING: the transaction stays
	// usable, so the caller can try another slug).
	InsertOrg(ctx context.Context, o Org) (Org, bool, error)
	// GetOrg returns the org or ErrNotFound.
	GetOrg(ctx context.Context, id uuid.UUID) (Org, error)
	// GetOrgBySlug returns the org of a slug or ErrNotFound.
	GetOrgBySlug(ctx context.Context, slug string) (Org, error)
	// GetOrgs returns the orgs of ids that exist (any order).
	GetOrgs(ctx context.Context, ids []uuid.UUID) ([]Org, error)
	// LockOrg returns the org locked for the rest of the transaction
	// (SELECT … FOR UPDATE) or ErrNotFound.
	LockOrg(ctx context.Context, id uuid.UUID) (Org, error)
	// UpdateOrg writes the mutable columns of o (name, status, description,
	// logo, website, city, verified_*, closed_at, rejection_reason) and
	// returns the stored row:
	// updated_at changes on every call, so the ETag does too (04 §1.13).
	// ErrNotFound when the org is gone.
	UpdateOrg(ctx context.Context, o Org) (Org, error)
	// ListOrgs returns up to limit orgs matching f after the position
	// after (nil: the first page), in f.Sort order.
	ListOrgs(ctx context.Context, f ListFilter, after *ListAfter, limit int) ([]Org, error)

	// InsertCompany writes the company profile of an org; org_kind is the
	// parent org's kind (a non-company org fails the CHECK: 422).
	InsertCompany(ctx context.Context, c Company) error
	// GetCompany returns the company profile of orgID or ErrNotFound.
	GetCompany(ctx context.Context, orgID uuid.UUID) (Company, error)
	// UpdateCompany writes the profile columns (sector, size band, e-mail
	// domain, HQ city) of orgID's company and returns the stored row.
	UpdateCompany(ctx context.Context, c Company) (Company, error)

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
	// MembershipsOfUser returns every membership row of a user, oldest first
	// (KVKK export).
	MembershipsOfUser(ctx context.Context, userID uuid.UUID) ([]Membership, error)

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
	// DeleteEndedMembers hard-deletes up to limit ended memberships of
	// orgID (a club that became sensitive, 02 §7.3) and reports how many.
	DeleteEndedMembers(ctx context.Context, orgID uuid.UUID, limit int) (int, error)
	// DeleteMember hard-deletes one membership of orgID (a sensitive club's
	// membership is deleted when it ends, 02 §7.3). Deleting a missing row
	// is not an error (idempotent).
	DeleteMember(ctx context.Context, orgID, id uuid.UUID) error
	// ListMembers returns up to limit memberships of orgID matching f after
	// the position after (nil: first page), in f.Sort order.
	ListMembers(ctx context.Context, orgID uuid.UUID, f MemberFilter, after *MemberAfter, limit int) ([]Membership, error)
	// ActiveMembershipsOfUser returns the active memberships of a user in
	// every org, oldest first (Actor resolution, 03 §2.1).
	ActiveMembershipsOfUser(ctx context.Context, userID uuid.UUID) ([]Membership, error)
	// EndedMemberships returns the IDs among ids of the memberships that
	// ended (talent's reconciliation, BE-090).
	EndedMemberships(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error)
	// ListUserMemberships returns up to limit memberships of a user with one
	// of statuses (empty: pending and active), newest first, after the
	// membership ID after (nil: first page).
	ListUserMemberships(ctx context.Context, userID uuid.UUID, statuses []MemberStatus, after *uuid.UUID, limit int) ([]Membership, error)
	// ActiveMemberIDs returns the user IDs of the active members of orgID.
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
	// ActiveByRole returns up to limit active memberships holding role in
	// any org, by ID ascending after the ID after (uuid.Nil(): from the
	// start): the admin sweep walks them in batches.
	ActiveByRole(ctx context.Context, role authz.MemberRole, after uuid.UUID, limit int) ([]Membership, error)

	// InsertInvitation writes an invitation of inv.OrgID and returns it as
	// stored; org_kind is the parent org's kind (06 H-38). ErrNotFound when
	// the org does not exist; ErrInvitationExists when an open invitation to
	// the same address exists (org_invitations_open_uq).
	InsertInvitation(ctx context.Context, inv Invitation) (Invitation, error)
	// GetInvitation returns invitation id of orgID or ErrNotFound.
	GetInvitation(ctx context.Context, orgID, id uuid.UUID) (Invitation, error)
	// LockInvitationByTokenHash returns the invitation of a token hash,
	// locked, or ErrNotFound (the token is the credential: any org).
	LockInvitationByTokenHash(ctx context.Context, hash []byte) (Invitation, error)
	// LockOpenInvitationTo returns the open (neither accepted nor revoked)
	// invitation of orgID to emailNormalized, locked, or ErrNotFound.
	LockOpenInvitationTo(ctx context.Context, orgID uuid.UUID, emailNormalized string) (Invitation, error)
	// CloseInvitation sets revoked_at = at on an open invitation of orgID and
	// returns it; ErrNotFound when it is not open.
	CloseInvitation(ctx context.Context, orgID, id uuid.UUID, at time.Time) (Invitation, error)
	// AcceptInvitation marks an open invitation of orgID accepted by userID
	// and returns it; ErrNotFound when it is not open.
	AcceptInvitation(ctx context.Context, orgID, id, userID uuid.UUID, now time.Time) (Invitation, error)
	// ListOpenInvitations returns up to limit open, unexpired invitations of
	// orgID, newest first, after the invitation ID after (nil: first page).
	ListOpenInvitations(ctx context.Context, orgID uuid.UUID, now time.Time, after *uuid.UUID, limit int) ([]Invitation, error)
	// CloseExpiredInvitations closes up to limit open invitations whose
	// expires_at ≤ now (revoked_at = expires_at: the schema has no separate
	// expired column, T-206) and returns how many it closed.
	CloseExpiredInvitations(ctx context.Context, now time.Time, limit int) (int, error)
	// PurgeExpired deletes up to limit rows of the retention policy whose
	// anchor is before cutoff, oldest first, and returns how many it
	// deleted; with dryRun it counts every such row and deletes nothing
	// (BE-057). PolicyMembershipEnded: ended memberships by ended_at;
	// PolicyOrgInvitation: invitations accepted, revoked or expired, by
	// COALESCE(accepted_at, revoked_at, expires_at). Unknown policy: error.
	PurgeExpired(ctx context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error)
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

// Sort orders of ListOrgs.
const (
	SortNewest = "id_desc"  // newest first (UUIDv7)
	SortName   = "name_asc" // by name (Turkish collation), then id
)

// ListFilter narrows ListOrgs. Zero fields do not filter.
type ListFilter struct {
	Kinds    []Kind
	Statuses []Status
	City     string
	// Q is free text (2–100 characters): full-text match on name and
	// description, or a substring of the normalised name (koro_norm,
	// trigram index).
	Q string
	// OrgIDs restricts the result to these orgs when non-nil (an empty,
	// non-nil slice matches nothing).
	OrgIDs []uuid.UUID
	Sort   string // SortNewest (default) or SortName
}

// ListAfter is the keyset position of a page: the last row's sort values.
type ListAfter struct {
	Name string // SortName only
	ID   uuid.UUID
}

// Member list orders.
const (
	SortMemberNewest = "id_desc"   // newest membership first (UUIDv7)
	SortMemberJoined = "since_asc" // by joined_at (requested_at for pending), then id
)

// MemberFilter narrows ListMembers. Zero fields do not filter, except
// Statuses: empty means pending and active (ended rows are not listed).
type MemberFilter struct {
	Statuses []MemberStatus
	Role     authz.MemberRole
	Sort     string // SortMemberNewest (default) or SortMemberJoined
}

// MemberAfter is the keyset position of a member page.
type MemberAfter struct {
	Since time.Time // SortMemberJoined only
	ID    uuid.UUID
}

// since is the SortMemberJoined key of m: joined_at, else requested_at,
// else created_at (the same COALESCE as the SQL store).
func (m Membership) since() time.Time {
	switch {
	case m.JoinedAt != nil:
		return *m.JoinedAt
	case m.RequestedAt != nil:
		return *m.RequestedAt
	}
	return m.CreatedAt
}

// Since returns the sort key of SortMemberJoined (cursor encoding).
func (m Membership) Since() time.Time { return m.since() }
