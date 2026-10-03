package authz

import "uuid"

// Role is a role path of the matrix (03 §1.1 columns, plus the sub-roles of
// k-cs and k-own and the P/O cells). A Grant names the path that allows an
// action; the core derives MFA and the absolute bans from it.
type Role string

// Role paths.
const (
	RolePublic         Role = "public" // P cell: everyone, anonymous included
	RoleSelf           Role = "self"   // O cell: any user on their own resource
	RoleUserUnverified Role = "user_unverified"
	RoleStudent        Role = "student"
	RoleStudentMinor   Role = "student_minor"
	RoleClubMember     Role = "club_member"
	RoleClubAdmin      Role = "club_admin"
	RoleEventStaff     Role = "event_staff"
	RoleUniversityRep  Role = "university_rep"
	RoleCareerStaff    Role = "career_staff" // university_rep minus k-cs; no MFA
	RoleCompanyMember  Role = "company_member"
	RoleCompanyAdmin   Role = "company_admin"
	RoleCompanyOwner   Role = "company_owner" // company_admin plus k-own
	RoleModerator      Role = "moderator"
	RoleSupport        Role = "support"
	RoleDPO            Role = "dpo"
	RolePlatformAdmin  Role = "platform_admin"
	RoleFinanceAdmin   Role = "finance_admin"
	RoleTalentAuditor  Role = "talent_auditor"
)

// mfaRoles require an MFA-verified session for every grant they give
// (k-mfa, 03 §5.0, §6.4); career_staff and company_member do not.
var mfaRoles = map[Role]bool{
	RoleUniversityRep: true, RoleCompanyAdmin: true, RoleCompanyOwner: true,
	RoleModerator: true, RoleSupport: true, RoleDPO: true,
	RolePlatformAdmin: true, RoleFinanceAdmin: true, RoleTalentAuditor: true,
}

// knownRoles lists every valid Grant.Role.
var knownRoles = map[Role]bool{
	RolePublic: true, RoleSelf: true, RoleUserUnverified: true, RoleStudent: true, RoleStudentMinor: true,
	RoleClubMember: true, RoleClubAdmin: true, RoleEventStaff: true, RoleUniversityRep: true,
	RoleCareerStaff: true, RoleCompanyMember: true, RoleCompanyAdmin: true, RoleCompanyOwner: true,
	RoleModerator: true, RoleSupport: true, RoleDPO: true, RolePlatformAdmin: true,
	RoleFinanceAdmin: true, RoleTalentAuditor: true,
}

// Grant is one role path that allows an action (03 §3.2-4). The conditions
// are checked by the core in the order MFA → four eyes → break-glass.
type Grant struct {
	Role Role
	// MFA marks an action-level M cell (the role-level k-mfa is implied by Role).
	MFA bool
	// FourEyes lists Attrs keys (user IDs of earlier steps) that must differ
	// from the actor, in addition to the core table of 03 §6.1 (absolute.go).
	// A listed key must be present; an empty value means "no earlier step".
	FourEyes []string
	// BreakGlass marks a B cell: valid only for support and platform_admin;
	// requires Attrs[access_reason] of 10–500 characters (03 §6.3) and never
	// reveals sensitive club data (03 Y2).
	BreakGlass bool
}

// Policy is the rule set of one module, written in policy_<module>.go by the
// module's tickets (BE-001 R5). It is pure: no I/O, no module imports.
//
// The zero Policy (Grants == nil) is the "deny everything" stub: every action
// of the module is ErrForbidden until the module ticket writes its policy.
type Policy struct {
	// Types declares module-specific resource types beyond 03 §3.5 with
	// their mandatory Attrs keys. A type may be declared only once.
	Types map[string][]string
	// Visible reports whether a may know that r exists (03 §3.6); false is
	// ErrNotFound. nil means nothing is visible.
	Visible func(a Actor, act Action, r Resource) bool
	// Grants returns every role path that allows act on r (03 §1.3: union).
	// A public action returns Grant{Role: RolePublic} for every actor.
	Grants func(a Actor, act Action, r Resource) []Grant
}

// OrgSurfaceVisible is the visibility of an org management surface
// (/orgs/{org_id}/..., 03 §3.6, §7.6): any active member of the org and
// every platform role; everyone else gets 404.
func OrgSurfaceVisible(a Actor, orgID *uuid.UUID) bool {
	if a.HasAnyPlatformRole() {
		return true
	}
	return orgID != nil && a.MemberOf(*orgID)
}

// AdminSurfaceVisible is the visibility of /admin/** (03 §3.2, §7.2-3): the
// surface is no secret, so every user sees it and an actor without a
// platform role gets 403 before the service reads anything.
func AdminSurfaceVisible(a Actor) bool { return a.IsUser() }
