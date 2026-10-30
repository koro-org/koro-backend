package orgs

import (
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
//
// Store is split by aggregate into embedded interfaces (05 §2.2, BE-147);
// a service file reaches the store only through the narrow view of the
// aggregate it works on (the accessors below), the retention sweeper
// through the purge it holds (retention.go), the KVKK provider
// (privacy.go), which covers every table of a data subject, through the
// whole Store.
type Store interface {
	orgStore        // organizations, the org root (store_org.go)
	companyStore    // the company profile (store_company.go)
	memberStore     // org_members, the membership lifecycle (store_members.go)
	directoryStore  // the membership reads of other modules (store_directory.go)
	sweepStore      // the batch work of the jobs and event handlers (store_sweeps.go)
	invitationStore // org_invitations (store_invitations.go)
	retentionStore  // the purge of the retention sweeper (store_retention.go)
	subjectStore    // every row of a data subject, KVKK (store_subject.go)
}

// orgs is the organizations view of the store.
func (s *service) orgs() orgStore { return s.d.Store }

// companies is the company profile view of the store.
func (s *service) companies() companyStore { return s.d.Store }

// members is the membership lifecycle view of the store.
func (s *service) members() memberStore { return s.d.Store }

// directory is the view of the membership reads other modules make.
func (s *service) directory() directoryStore { return s.d.Store }

// sweeps is the batch work view of the jobs and event handlers.
func (s *service) sweeps() sweepStore { return s.d.Store }

// invitations is the org_invitations view of the store.
func (s *service) invitations() invitationStore { return s.d.Store }

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
