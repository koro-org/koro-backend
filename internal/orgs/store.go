package orgs

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of orgs (05 §2.2) over organizations,
// companies and org_members (02 §7.1–7.3). The GORM store (store_gorm.go)
// and the in-memory store (store_mem.go) pass the same contract test
// (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any; the
// service draws the boundaries (OrgAdmin.Create joins the caller's).
//
// Org scope: organizations is the org root, addressed by its own id.
// companies and org_members are scoped by org_id, and their scope columns
// come from the parent organizations row, never from the caller (06 H-38):
// InsertCompany and InsertMember copy org_kind from the org they belong to.
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
	// logo, website, city, verified_*, closed_at) and returns the stored row:
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
