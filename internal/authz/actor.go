package authz

import (
	"errors"
	"log/slog"
	"slices"
	"uuid"
)

// Kind tells who is acting (03 §2.1). The zero value is invalid and every
// decision for it is ErrForbidden.
type Kind uint8

// Actor kinds.
const (
	KindUser      Kind = iota + 1 // valid session
	KindAnonymous                 // public operation without a (valid) credential
	KindSystem                    // worker/scheduler job (SystemActor)
)

// String returns the kind name.
func (k Kind) String() string {
	switch k {
	case KindUser:
		return "user"
	case KindAnonymous:
		return "anonymous"
	case KindSystem:
		return "system"
	}
	return "invalid"
}

// PlatformRole is platform_role_assignments.role (03 §1.1, rows 11–16).
type PlatformRole string

// Platform roles.
const (
	PlatformAdmin         PlatformRole = "platform_admin"
	PlatformModerator     PlatformRole = "moderator"
	PlatformSupport       PlatformRole = "support"
	PlatformDPO           PlatformRole = "dpo"
	PlatformFinanceAdmin  PlatformRole = "finance_admin"
	PlatformTalentAuditor PlatformRole = "talent_auditor"
)

// OrgKind is organizations.kind.
type OrgKind string

// Organisation kinds.
const (
	OrgClub       OrgKind = "club"
	OrgUniversity OrgKind = "university"
	OrgCompany    OrgKind = "company"
	OrgPlatform   OrgKind = "platform"
	OrgNGO        OrgKind = "ngo" // phase 2: no action is granted (03 §1.2)
)

// MemberRole is org_members.role (03 §1.2).
type MemberRole string

// Organisation member roles.
const (
	MemberMember        MemberRole = "member"         // club → club_member
	MemberClubAdmin     MemberRole = "club_admin"     // club → club_admin
	MemberUniversityRep MemberRole = "university_rep" // university → university_rep
	MemberCareerStaff   MemberRole = "career_staff"   // university → university_rep minus k-cs
	MemberCompanyOwner  MemberRole = "company_owner"  // company → company_admin plus k-own
	MemberCompanyAdmin  MemberRole = "company_admin"  // company → company_admin
	MemberRecruiter     MemberRole = "recruiter"      // company → company_member
	MemberStaff         MemberRole = "staff"          // platform org: authorship only, no permission
	MemberNGOAdmin      MemberRole = "ngo_admin"      // phase 2
	MemberNGOMember     MemberRole = "ngo_member"     // phase 2
)

// Membership is one active org membership (orgs.OrgDirectory.MembershipsOf).
// Sensitive club memberships are included: they are needed for decisions
// and the Actor is never serialised.
type Membership struct {
	OrgID   uuid.UUID
	OrgKind OrgKind
	Role    MemberRole
}

// StudentStatus is the student verification state of the actor (03 §2.1).
type StudentStatus string

// Student statuses.
const (
	StudentNone     StudentStatus = "none"
	StudentVerified StudentStatus = "verified" // status='verified' AND expires_at > now()
	StudentLapsed   StudentStatus = "lapsed"   // verified once, now expired or revoked
)

// Actor is who performs a request or a job (01 §3.1, 03 §2.1). It is built
// per request by the ActorResolver (no cache) and is constant within it.
//
// An Actor is never written to a response or a log: LogValue and String
// expose only the kind and the user ID, and MarshalJSON fails.
type Actor struct {
	Kind          Kind
	UserID        uuid.UUID
	SessionID     uuid.UUID
	PlatformRoles []PlatformRole
	Memberships   []Membership
	StudentStatus StudentStatus
	UniversityID  *uuid.UUID // universities.id of the active verification
	IsMinor       bool       // computed per request from users.birth_date
	MFAVerified   bool       // sessions.mfa_verified_at IS NOT NULL
	SystemJob     string     // job name when Kind is KindSystem
}

// Anonymous returns the actor of a public operation without a credential.
func Anonymous() Actor { return Actor{Kind: KindAnonymous} }

// SystemActor returns the actor of a worker or scheduler job. It may perform
// only the 03 §3.7 actions and is never used on HTTP paths.
func SystemActor(job string) Actor { return Actor{Kind: KindSystem, SystemJob: job} }

// IsUser reports whether the actor is an authenticated user.
func (a Actor) IsUser() bool { return a.Kind == KindUser }

// HasPlatformRole reports whether the actor holds platform role r.
func (a Actor) HasPlatformRole(r PlatformRole) bool {
	return a.Kind == KindUser && slices.Contains(a.PlatformRoles, r)
}

// HasAnyPlatformRole reports whether the actor holds at least one platform
// role (staff see org management surfaces and drafts; 03 §3.6, §7.2).
func (a Actor) HasAnyPlatformRole() bool { return a.Kind == KindUser && len(a.PlatformRoles) > 0 }

// MemberOf reports whether the actor has any active membership in orgID.
func (a Actor) MemberOf(orgID uuid.UUID) bool {
	if a.Kind != KindUser || orgID == uuid.Nil() {
		return false
	}
	return slices.ContainsFunc(a.Memberships, func(m Membership) bool { return m.OrgID == orgID })
}

// HasOrgRole reports whether the actor holds one of roles in orgID.
func (a Actor) HasOrgRole(orgID uuid.UUID, roles ...MemberRole) bool {
	if a.Kind != KindUser || orgID == uuid.Nil() {
		return false
	}
	return slices.ContainsFunc(a.Memberships, func(m Membership) bool {
		return m.OrgID == orgID && slices.Contains(roles, m.Role)
	})
}

// IsVerifiedStudent reports whether the actor has an active student
// verification (03 §1.1 student, student_minor).
func (a Actor) IsVerifiedStudent() bool {
	return a.Kind == KindUser && a.StudentStatus == StudentVerified
}

// Owns reports whether r is the actor's own resource (cell O, 03 §5.0).
func (a Actor) Owns(r Resource) bool {
	return a.Kind == KindUser && a.UserID != uuid.Nil() && r.OwnerUserID != nil && *r.OwnerUserID == a.UserID
}

// String identifies the actor without its roles or memberships.
func (a Actor) String() string {
	switch a.Kind {
	case KindUser:
		return "user:" + a.UserID.String()
	case KindSystem:
		return "system:" + a.SystemJob
	}
	return a.Kind.String()
}

// GoString keeps %#v from printing memberships (sensitive clubs).
func (a Actor) GoString() string { return "authz.Actor(" + a.String() + ")" }

// LogValue logs only the kind and the user ID (03 §2.1: never serialised).
func (a Actor) LogValue() slog.Value {
	return slog.GroupValue(slog.String("kind", a.Kind.String()), slog.String("user_id", a.UserID.String()))
}

// errActorNotSerializable is returned by MarshalJSON.
var errActorNotSerializable = errors.New("authz: Actor must never be serialised (03 §2.1)")

// MarshalJSON always fails: an Actor never appears in a response.
func (a Actor) MarshalJSON() ([]byte, error) { return nil, errActorNotSerializable }
