package authz

import (
	"context"
	"errors"
	"slices"
	"uuid"
)

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

// orgParty reports whether a has a stake in the org r names: a current
// member of it, any role (BE-183, BE-194, BE-208). Staff never decide on an
// org they belong to, nor on a report naming it; the rule binds platform
// admins too. Memberships ended before are not known (the actor carries the
// active ones).
func orgParty(a Actor, r Resource) bool { return r.OrgID != nil && a.MemberOf(*r.OrgID) }

// staffPaths are the role paths of the platform roles (03 §1.1 rows 11–16).
var staffPaths = map[Role]bool{
	RoleModerator: true, RoleSupport: true, RoleDPO: true, RolePlatformAdmin: true, RoleFinanceAdmin: true,
	RoleTalentAuditor: true,
}

// ownOrg reports whether orgID is an org of a's own for the party rule of
// an org's surface (BE-214, Q-69): a current member of it, any role, other
// than the platform org. The platform org is no party's: its members are
// the platform staff themselves, their staff membership grants nothing
// (03 §1.2), their authority there is the platform role (k-po), and its
// surface is the platform's own management, so the rule would lock the
// platform out of its own org.
func ownOrg(a Actor, orgID *uuid.UUID) bool {
	return orgID != nil && a.MemberOf(*orgID) && !platformOrg(a, orgID)
}

// asMember is the party rule of an org's own surface (BE-214, Q-69; T-438):
// a platform staff member who belongs to the org r names (ownOrg) acts
// there with the membership only, so the platform staff paths (pad, fin,
// dpo, sup, mod, tal; break-glass included) are dropped from gs and the
// membership's grants stay. Every policy wraps the grants of its org
// surface (/orgs/{org_id}/..., 03 §3.6, §7.6) with it, and those of the
// resource-surface operations that run one org's own content (a club's
// profile, an event and its registrations and exports, an offering);
// TestOrgSurfaceParty_StaffActsAsMember checks every one of them against
// the OpenAPI spec.
// Staff decisions on an org (the admin surface) follow their own party
// rule (orgParty).
func asMember(a Actor, r Resource, gs []Grant) []Grant {
	if !ownOrg(a, r.OrgID) {
		return gs
	}
	return slices.DeleteFunc(slices.Clone(gs), func(g Grant) bool { return staffPaths[g.Role] })
}

// ActsAsStaff reports whether a acts as platform staff on orgID: it holds
// one of roles (any platform role when roles is empty) and orgID is no org
// of its own (BE-214, Q-69: a member acts on its org with the membership
// only; the platform org is no party's). The services ask it wherever a
// platform role lifts an org's rule: the suspended org's write ban (03
// §2.3), the tax identifier review, unmasked identifiers, a submission as
// the first eye. uuid.Nil (no org: an admin-surface act) asks for the
// roles alone.
func ActsAsStaff(a Actor, orgID uuid.UUID, roles ...PlatformRole) bool {
	if ownOrg(a, &orgID) {
		return false
	}
	if len(roles) == 0 {
		return a.HasAnyPlatformRole()
	}
	return slices.ContainsFunc(roles, a.HasPlatformRole)
}

// PartyRefused reports whether err, the refusal of act on r to a, is a
// party rule's (BE-208, BE-212, BE-214): a belongs to r's org and the same
// person without that membership would be allowed, so a platform role it
// holds is what the rule set aside. The services record such a refusal
// (denied audit, reason org_member); any other refusal reports false.
func PartyRefused(ctx context.Context, a Actor, act Action, r Resource, err error) bool {
	if !errors.Is(err, ErrForbidden) || r.OrgID == nil || !a.MemberOf(*r.OrgID) || !a.HasAnyPlatformRole() {
		return false
	}
	org := *r.OrgID
	outside := a
	outside.Memberships = slices.DeleteFunc(slices.Clone(a.Memberships), func(m Membership) bool { return m.OrgID == org })
	return Can(ctx, outside, act, r) == nil
}

// AdminSurfaceVisible is the visibility of /admin/** (03 §3.2, §7.2-3): the
// surface is no secret, so every user sees it and an actor without a
// platform role gets 403 before the service reads anything.
func AdminSurfaceVisible(a Actor) bool { return a.IsUser() }
