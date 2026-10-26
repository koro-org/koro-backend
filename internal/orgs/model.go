package orgs

import (
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
)

// Kind is organizations.kind (02 §7.1). ngo is phase 2: the v1 API never
// creates one.
type Kind = authz.OrgKind

// Org kinds.
const (
	KindPlatform   = authz.OrgPlatform
	KindUniversity = authz.OrgUniversity
	KindClub       = authz.OrgClub
	KindCompany    = authz.OrgCompany
	KindNGO        = authz.OrgNGO
)

// Status is organizations.status (02 §7.1).
type Status string

// Org statuses.
const (
	StatusPendingReview Status = "pending_review" // a new company until a platform admin verifies it
	StatusActive        Status = "active"
	StatusSuspended     Status = "suspended" // writes on behalf of the org are 403 orgs.org_suspended (03 §2.3)
	StatusClosed        Status = "closed"    // soft delete: never removed (02 §7.1)
)

// Valid reports whether s is a status of the CHECK constraint.
func (s Status) Valid() bool {
	switch s {
	case StatusPendingReview, StatusActive, StatusSuspended, StatusClosed:
		return true
	}
	return false
}

// validKind reports whether k is a kind of the CHECK constraint.
func validKind(k Kind) bool {
	switch k {
	case KindPlatform, KindUniversity, KindClub, KindCompany, KindNGO:
		return true
	}
	return false
}

// Org is one organizations row (02 §7.1). The generated search_vector
// column is not mapped. The org holds no personal data (02 §7.1);
// CreatedBy and VerifiedBy are references to accounts.
type Org struct {
	ID          uuid.UUID  `gorm:"type:uuid;column:id;primaryKey" json:"id"`
	Kind        Kind       `gorm:"column:kind" json:"kind"`
	Name        string     `gorm:"column:name" json:"name"`
	Slug        string     `gorm:"column:slug" json:"slug"`
	Status      Status     `gorm:"column:status" json:"status"`
	Description *string    `gorm:"column:description" json:"description,omitempty"`
	LogoFileID  *uuid.UUID `gorm:"type:uuid;column:logo_file_id" json:"logo_file_id,omitempty"`
	WebsiteURL  *string    `gorm:"column:website_url" json:"website_url,omitempty"`
	City        *string    `gorm:"column:city" json:"city,omitempty"`
	VerifiedAt  *time.Time `gorm:"column:verified_at" json:"verified_at,omitempty"`
	VerifiedBy  *uuid.UUID `gorm:"type:uuid;column:verified_by" json:"verified_by,omitempty"`
	ClosedAt    *time.Time `gorm:"column:closed_at" json:"closed_at,omitempty"`
	CreatedBy   *uuid.UUID `gorm:"type:uuid;column:created_by" json:"created_by,omitempty"`
	CreatedAt   time.Time  `gorm:"column:created_at;->" json:"created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;->" json:"updated_at"`
}

// TableName implements gorm's Tabler.
func (Org) TableName() string { return "organizations" }

// Verified reports whether a platform admin has verified the org.
func (o Org) Verified() bool { return o.VerifiedAt != nil }

// Summary returns the cross-module view of the org.
func (o Org) Summary() OrgSummary {
	return OrgSummary{ID: o.ID, Kind: o.Kind, Name: o.Name, Status: o.Status, Slug: o.Slug,
		LogoFileID: o.LogoFileID, Verified: o.Verified()}
}

// Company is one companies row (02 §7.2): the company profile of an org of
// kind company. Invoice title, tax number (VKN) and MERSİS number are not
// here but in parties.legal_parties (02 §7.2); none of these columns is
// personal data.
type Company struct {
	OrgID            uuid.UUID  `gorm:"type:uuid;column:org_id;primaryKey" json:"org_id"`
	OrgKind          Kind       `gorm:"column:org_kind" json:"org_kind"`
	SectorCode       *string    `gorm:"column:sector_code" json:"sector_code,omitempty"`
	SizeBand         *string    `gorm:"column:size_band" json:"size_band,omitempty"`
	EmailDomain      *string    `gorm:"column:email_domain" json:"email_domain,omitempty"`
	DomainVerifiedAt *time.Time `gorm:"column:domain_verified_at" json:"domain_verified_at,omitempty"`
	HQCity           *string    `gorm:"column:hq_city" json:"hq_city,omitempty"`
	CreatedAt        time.Time  `gorm:"column:created_at;->" json:"created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at;->" json:"updated_at"`
}

// TableName implements gorm's Tabler.
func (Company) TableName() string { return "companies" }

// Size bands of companies.size_band.
var sizeBands = []string{"1-10", "11-50", "51-200", "201-1000", "1000+"}

// ownerRole is the single company owner (org_members_one_owner_uq).
const ownerRole = authz.MemberCompanyOwner

// MemberStatus is org_members.status (02 §7.3).
type MemberStatus string

// Membership statuses.
const (
	MemberPending MemberStatus = "pending"
	MemberActive  MemberStatus = "active"
	MemberEnded   MemberStatus = "ended"
)

// Source is org_members.source.
type Source string

// Membership sources. A company founder and an ownership transfer use
// SourceAdmin: the row is written by the platform's own flow, not by a join
// request, an invitation, a claim or an import.
const (
	SourceJoinRequest Source = "join_request"
	SourceInvitation  Source = "invitation"
	SourceClaim       Source = "claim"
	SourceAdmin       Source = "admin"
	SourceImport      Source = "import"
)

// EndReason is org_members.end_reason.
type EndReason string

// End reasons (org_members_end_reason_chk).
const (
	EndLeft               EndReason = "left"                // the member left (or withdrew a join request)
	EndRemoved            EndReason = "removed"             // an admin removed the member
	EndRejected           EndReason = "rejected"            // a join request was rejected
	EndGraduated          EndReason = "graduated"           // student.graduated: club_admin ends (01 §6.2)
	EndVerificationLapsed EndReason = "verification_lapsed" // the student verification lapsed: club_admin ends
	EndClaimRevoked       EndReason = "claim_revoked"       // directory revoked a club claim (BE-022)
	EndAccountErased      EndReason = "account_erased"      // KVKK erasure (privacy provider)
	EndOrgClosed          EndReason = "org_closed"
	EndRoleChanged        EndReason = "role_changed"
)

// Valid reports whether r is a reason of the CHECK constraint.
func (r EndReason) Valid() bool {
	switch r {
	case EndLeft, EndRemoved, EndRejected, EndGraduated, EndVerificationLapsed, EndClaimRevoked,
		EndAccountErased, EndOrgClosed, EndRoleChanged:
		return true
	}
	return false
}

// Membership is one org_members row (02 §7.3). It is personal data (in a
// sensitive club, special-category data): never logged, exported only to
// its user. OrgKind is always the kind of the parent org (06 H-38): the
// store takes it from organizations, never from the caller.
type Membership struct {
	ID          uuid.UUID        `gorm:"type:uuid;column:id;primaryKey"`
	OrgID       uuid.UUID        `gorm:"type:uuid;column:org_id"`
	OrgKind     Kind             `gorm:"column:org_kind"`
	UserID      uuid.UUID        `gorm:"type:uuid;column:user_id"`
	Role        authz.MemberRole `gorm:"column:role"`
	Status      MemberStatus     `gorm:"column:status"`
	Source      Source           `gorm:"column:source"`
	RequestedAt *time.Time       `gorm:"column:requested_at"`
	JoinedAt    *time.Time       `gorm:"column:joined_at"`
	EndedAt     *time.Time       `gorm:"column:ended_at"`
	EndReason   *EndReason       `gorm:"column:end_reason"`
	InvitedBy   *uuid.UUID       `gorm:"type:uuid;column:invited_by"`
	ApprovedBy  *uuid.UUID       `gorm:"type:uuid;column:approved_by"`
	CreatedAt   time.Time        `gorm:"column:created_at;->"`
	UpdatedAt   time.Time        `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Membership) TableName() string { return "org_members" }

// roleFitsKind is org_members_role_kind_chk.
func roleFitsKind(k Kind, r authz.MemberRole) bool {
	switch k {
	case KindClub:
		return r == authz.MemberMember || r == authz.MemberClubAdmin
	case KindUniversity:
		return r == authz.MemberUniversityRep || r == authz.MemberCareerStaff
	case KindCompany:
		return r == authz.MemberCompanyOwner || r == authz.MemberCompanyAdmin || r == authz.MemberRecruiter
	case KindPlatform:
		return r == authz.MemberStaff
	case KindNGO:
		return r == authz.MemberNGOAdmin || r == authz.MemberNGOMember
	}
	return false
}

// OrgSummary is an org as other modules see it (01 §4.7). LogoFileID and
// Verified come with the same row (GetMany stays one statement, T-251):
// the reader signs the logo itself (files.PresignGet, ≤ 5 min).
type OrgSummary struct {
	ID         uuid.UUID
	Kind       Kind
	Name       string
	Status     Status
	Slug       string
	LogoFileID *uuid.UUID // the org_logo upload, nil without a logo
	Verified   bool       // a platform admin verified the org (verified_at set)
}

// CreateOrg is the input of OrgAdmin.Create (directory: imported or
// claimed clubs and universities; 01 §4.7).
type CreateOrg struct {
	Kind        Kind   // club | university | company
	Name        string // 2–200 characters after normalisation
	Slug        string // optional: derived from Name, made unique with a suffix
	Status      Status // optional: active by default
	City        string // optional, ≤ 60 characters
	Description string // optional, ≤ 5000 characters
	WebsiteURL  string // optional, https:// only
	CreatedBy   *uuid.UUID
}

// OrgView is an org with its company profile (when it is a company) and
// its ETag (04 §1.13), as the HTTP operations return it.
type OrgView struct {
	Org     Org
	Company *Company
	ETag    string
	// LogoURL is a short-lived download URL of the logo (04 §1.14); empty
	// when there is none or the file is not served (scan not clean).
	LogoURL string
	// LogoPublicURL is the permanent public media URL of the logo (BE-120):
	// only for an active org that is not a sensitive club, once files has
	// made the public copy; empty otherwise.
	LogoPublicURL string
	// Manage is true when the reader may update the org (getOrg of a
	// company for its admins and platform admins, BE-127): the public
	// profile then carries the company's e-mail domain for the edit form.
	Manage bool
}

// reauthProof is the re-authentication proof of an R operation (03 §6.2).
type reauthProof = identity.ReauthProof

// invitableRole reports whether r may be the role of an invitation of an
// org of kind k (org_invitations_role_kind_chk: company_owner, the platform
// and the ngo roles are never invited).
func invitableRole(k Kind, r authz.MemberRole) bool {
	switch k {
	case KindClub:
		return r == authz.MemberMember || r == authz.MemberClubAdmin
	case KindUniversity:
		return r == authz.MemberUniversityRep || r == authz.MemberCareerStaff
	case KindCompany:
		return r == authz.MemberCompanyAdmin || r == authz.MemberRecruiter
	}
	return false
}

// mfaRole reports the member roles whose grant or loss closes the user's
// sessions (03 §2.2-6: company admin roles and university_rep are MFA roles,
// §6.4; the new session policy applies at once).
func mfaRole(r authz.MemberRole) bool {
	return r == authz.MemberCompanyOwner || r == authz.MemberCompanyAdmin || r == authz.MemberUniversityRep
}

// guardedRole reports the roles an org must keep at least one of
// (03 k6: the last club_admin and the company_owner cannot leave, be
// removed or be demoted through the HTTP operations).
func guardedRole(k Kind, r authz.MemberRole) bool {
	return k == KindClub && r == authz.MemberClubAdmin || k == KindCompany && r == authz.MemberCompanyOwner
}

// Join policies of clubs.join_policy (directory).
const (
	JoinOpen     = "open"     // a join request is active at once
	JoinApproval = "approval" // a club_admin approves it
)

// ClubFacts are the join-relevant facts of a club (directory's clubs row;
// hooks.clubInfo, T-77).
type ClubFacts struct {
	UniversityID uuid.UUID
	JoinPolicy   string // JoinOpen | JoinApproval
	Sensitive    bool   // clubs.is_sensitive: special-category data (02 §7.3)
}

// Invitation is one org_invitations row (02 §7.4). Email is personal data:
// never logged or audited. Only the SHA-256 of the derived token is stored
// (TokenHash); the token itself is re-derived from (KeyID, ID) by the worker
// that sends the e-mail and is never stored or put into an event.
type Invitation struct {
	ID              uuid.UUID        `gorm:"type:uuid;column:id;primaryKey"`
	OrgID           uuid.UUID        `gorm:"type:uuid;column:org_id"`
	OrgKind         Kind             `gorm:"column:org_kind"`
	Email           string           `gorm:"column:email"`
	EmailNormalized string           `gorm:"column:email_normalized"`
	Role            authz.MemberRole `gorm:"column:role"`
	KeyID           string           `gorm:"column:key_id"`
	TokenHash       []byte           `gorm:"column:token_hash"`
	InvitedBy       uuid.UUID        `gorm:"type:uuid;column:invited_by"`
	ExpiresAt       time.Time        `gorm:"column:expires_at"`
	AcceptedAt      *time.Time       `gorm:"column:accepted_at"`
	AcceptedUserID  *uuid.UUID       `gorm:"type:uuid;column:accepted_user_id"`
	RevokedAt       *time.Time       `gorm:"column:revoked_at"`
	CreatedAt       time.Time        `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Invitation) TableName() string { return "org_invitations" }

// Open reports whether the invitation was neither accepted nor revoked (an
// expired one is still open until orgs.invitations_expire closes it).
func (i Invitation) Open() bool { return i.AcceptedAt == nil && i.RevokedAt == nil }

// Expired reports whether the invitation can no longer be accepted at now.
func (i Invitation) Expired(now time.Time) bool { return !now.Before(i.ExpiresAt) }
