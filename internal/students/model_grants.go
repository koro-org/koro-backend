package students

import (
	"time"
	"uuid"
)

// GrantSource is profile_access_grants.source (02 §9.7): what the student
// accepted to give a company level 3 access.
type GrantSource string

// Grant sources. application is phase 3 (no writer yet).
const (
	SourceContactRequest GrantSource = "contact_request"
	SourceApplication    GrantSource = "application"
)

// Valid reports whether s is a source of the CHECK.
func (s GrantSource) Valid() bool { return s == SourceContactRequest || s == SourceApplication }

// RevokedBy is profile_access_grants.revoked_by_kind.
type RevokedBy string

// Who ended a grant: the student (revokeMyAccessGrant), the company
// (revokeCompanyAccessGrant) or another module (AccessGrants.Revoke).
const (
	RevokedByStudent RevokedBy = "student"
	RevokedByCompany RevokedBy = "company"
	RevokedBySystem  RevokedBy = "system"
)

// AccessGrant is a profile_access_grants row (02 §9.7): a company's level 3
// access to one student. It is revoked, never edited away; a revoked grant
// is hard deleted 6 months after its revocation (access_grant_revoked).
// A grant is suspended when the student's verification lapses (BE-077):
// the company has no access, and it never becomes active again by itself:
// the student revokes it, resumes it once verified again within the
// suspension window (BE-088: an explicit consent act, ResumedAt) or accepts
// a new request of the company; after the window the system revokes it
// (ReasonSuspensionExpired).
// org_kind is the constant 'company' of the composite FK and is not mapped.
type AccessGrant struct {
	ID            uuid.UUID   `gorm:"type:uuid;column:id;primaryKey"`
	StudentUserID uuid.UUID   `gorm:"type:uuid;column:student_user_id"`
	CompanyOrgID  uuid.UUID   `gorm:"type:uuid;column:company_org_id"`
	Source        GrantSource `gorm:"column:source"`
	SourceID      uuid.UUID   `gorm:"type:uuid;column:source_id"`
	GrantedAt     time.Time   `gorm:"column:granted_at"`
	SuspendedAt   *time.Time  `gorm:"column:suspended_at"`
	ResumedAt     *time.Time  `gorm:"column:resumed_at"`
	RevokedAt     *time.Time  `gorm:"column:revoked_at"`
	RevokedByKind *RevokedBy  `gorm:"column:revoked_by_kind"`
	RevokeReason  *string     `gorm:"column:revoke_reason"`
	CreatedAt     time.Time   `gorm:"column:created_at;->"`
	UpdatedAt     time.Time   `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (AccessGrant) TableName() string { return "profile_access_grants" }

// GrantStatus is the state of a grant as its student sees it.
type GrantStatus string

// Grant states (BE-077): active gives the company level 3 access; a
// suspended grant gives nothing (the verification lapsed); a revoked one
// has ended.
const (
	GrantActive    GrantStatus = "active"
	GrantSuspended GrantStatus = "suspended"
	GrantRevoked   GrantStatus = "revoked"
)

// ReasonSuspensionExpired is the revoke_reason of a suspended grant the
// system revoked when its suspension window ran out (BE-088, T-331); with
// revoked_by_kind 'system' it marks the revocations the student is told of.
const ReasonSuspensionExpired = "suspension_expired"

// ExpiryKey is one expiry the student is told of: the grants of a student
// the system revoked at one moment (students.expire_suspended_grants), and
// the keyset position of their walk (student_user_id, revoked_at).
type ExpiryKey struct {
	StudentUserID uuid.UUID
	RevokedAt     time.Time
}

// Open reports whether the grant has not ended (active or suspended).
func (g AccessGrant) Open() bool { return g.RevokedAt == nil }

// Active reports whether the grant is in force: open and not suspended.
func (g AccessGrant) Active() bool { return g.Open() && g.SuspendedAt == nil }

// Status is the grant's state.
func (g AccessGrant) Status() GrantStatus {
	switch {
	case !g.Open():
		return GrantRevoked
	case g.SuspendedAt != nil:
		return GrantSuspended
	}
	return GrantActive
}

// view is the grant as the level 3 projections take it (dto.go Grant).
func (g AccessGrant) view() Grant {
	out := Grant{CompanyOrgID: g.CompanyOrgID, Active: g.Active(), GrantedAt: g.GrantedAt, Source: string(g.Source)}
	if g.Source == SourceContactRequest {
		id := g.SourceID
		out.ContactRequestID = &id
	}
	return out
}

// GrantKey is the keyset position of a grant list: granted_at DESC, id
// DESC (the newest grant first).
type GrantKey struct {
	GrantedAt time.Time
	ID        uuid.UUID
}

// before reports whether g sorts after the position k in a newest-first
// list (strictly older, or as old with a smaller ID).
func (k GrantKey) before(g AccessGrant) bool {
	if c := g.GrantedAt.Compare(k.GrantedAt); c != 0 {
		return c < 0
	}
	return compareUUID(g.ID, k.ID) < 0
}

// DiscoveryQuery is a level 2 search (01 §4.9 Discoverable.Search; 04 §2.10
// searchDiscovery): OR inside a filter, AND across filters (the targeting
// semantics of 02 §9.1). There is deliberately no club filter of any kind
// (03 §5.10: no sensitive club filter; a club filter would also narrow a
// result to a club's members). An empty filter matches everyone.
type DiscoveryQuery struct {
	UniversityIDs []uuid.UUID
	DepartmentIDs []uuid.UUID
	ClassYears    []int16
	DegreeLevel   DegreeLevel // "" = any
	SkillIDs      []uuid.UUID
	LanguageCodes []string
	// After is the keyset position: the page holds user IDs > After.
	After uuid.UUID
	// Limit is the page size (1–MaxDiscoveryLimit; default
	// DefaultDiscoveryLimit).
	Limit int
}
