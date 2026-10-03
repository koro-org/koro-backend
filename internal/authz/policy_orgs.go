package authz

// TypeOrgCatalog is the public org directory (orgs.org.search): no org,
// no owner, no Attrs. Declared by the orgs policy (Policy.Types).
const TypeOrgCatalog = "org_catalog"

// orgsPolicy is the policy of the orgs module (03 §5.2). Only tickets of
// the orgs module edit this file (BE-001 R5). Written by BE-017 for its 9
// actions:
//
//	orgs.org.read            P; a pending_review or closed org only for its members and
//	                           platform_admin, support, moderator (03 §3.6), else 404
//	orgs.org.search          P (resource org_catalog)
//	orgs.company.create      usr, stu, clm, cla, stf, uni, cmm, cma; not min, not platform
//	                           staff accounts (k-cmp below)
//	orgs.org.update          S: club_admin, university_rep (not career_staff, k-cs),
//	                           company_admin/owner of the org; platform_admin
//	orgs.org.close           S·k-own: the org's company_owner; platform_admin (R in the service)
//	orgs.ownership.transfer  S·k-own: the org's company_owner; platform_admin (R in the service)
//	orgs.org.list_admin      moderator, support, platform_admin (admin surface)
//	orgs.org.verify          platform_admin (admin surface)
//	orgs.org.suspend         platform_admin (admin surface; R in the service)
//
// Resources (03 §3.5): org (OrgID = the org itself; Attrs org_kind,
// org_status), org_catalog (search), admin_surface (the /admin operations:
// 403 without a platform role, before anything is read; 03 §7.2-3).
// updateOrg, closeOrg and transferOrgOwnership are the org's management
// surface (03 §3.6, §7.6): visible to its active members (403 without the
// role) and to every platform role; anyone else gets 404.
//
// k-cmp (BE-017 reading of 03 §5.2 orgs.company.create): the usr column is
// an account with no student verification and no platform role (platform
// roles are staff accounts and their cells are ✗), and a minor never
// registers a company, whatever membership they hold (min ✗).
//
// MFA follows from the role paths (k-mfa: uni, cma, mod, sup, pad). The
// suspended-org rule (403 orgs.org_suspended, 03 §2.3) is the service's:
// its code is not an authz code.
//
// The remaining orgs actions (members, memberships, invitations) belong to
// BE-019: until then they are visible and granted to no role path (403).
func orgsPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeOrgCatalog: nil},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case OrgsOrgRead:
				return r.Type == TypeOrg && orgProfileVisible(a, r)
			case OrgsOrgSearch:
				return r.Type == TypeOrgCatalog
			case OrgsCompanyCreate:
				return r.Type == TypeOrg && r.Attr("org_kind") == string(OrgCompany)
			case OrgsOrgUpdate, OrgsOrgClose, OrgsOwnershipTransfer:
				return r.Type == TypeOrg && OrgSurfaceVisible(a, r.OrgID)
			case OrgsOrgListAdmin, OrgsOrgVerify, OrgsOrgSuspend:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			}
			return true // BE-019's actions: no grant below → 403
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			pad := Grant{Role: RolePlatformAdmin}
			switch act {
			case OrgsOrgRead, OrgsOrgSearch:
				return []Grant{{Role: RolePublic}}
			case OrgsCompanyCreate:
				return companyCreateGrants(a)
			case OrgsOrgUpdate:
				gs := []Grant{pad}
				if r.OrgID == nil {
					return gs
				}
				org := *r.OrgID
				if a.HasOrgRole(org, MemberClubAdmin) {
					gs = append(gs, Grant{Role: RoleClubAdmin})
				}
				if a.HasOrgRole(org, MemberUniversityRep) { // k-cs: not career_staff
					gs = append(gs, Grant{Role: RoleUniversityRep})
				}
				if a.HasOrgRole(org, MemberCompanyAdmin, MemberCompanyOwner) {
					gs = append(gs, Grant{Role: RoleCompanyAdmin})
				}
				return gs
			case OrgsOrgClose, OrgsOwnershipTransfer:
				gs := []Grant{pad}
				if r.OrgID != nil && a.HasOrgRole(*r.OrgID, MemberCompanyOwner) { // k-own
					gs = append(gs, Grant{Role: RoleCompanyOwner})
				}
				return gs
			case OrgsOrgListAdmin:
				return []Grant{{Role: RoleModerator}, {Role: RoleSupport}, pad}
			case OrgsOrgVerify, OrgsOrgSuspend:
				return []Grant{pad}
			}
			return nil
		},
	}
}

// orgProfileVisible is 03 §3.6 "Org (public profile)".
func orgProfileVisible(a Actor, r Resource) bool {
	switch r.Attr("org_status") {
	case "active", "suspended":
		return true
	case "pending_review", "closed":
		if r.OrgID != nil && a.MemberOf(*r.OrgID) {
			return true
		}
		return a.HasPlatformRole(PlatformAdmin) || a.HasPlatformRole(PlatformSupport) || a.HasPlatformRole(PlatformModerator)
	}
	return false
}

// companyCreateGrants is 03 §5.2 orgs.company.create with k-cmp (see the
// policy comment). The core's holds() keeps only the paths the actor has.
func companyCreateGrants(a Actor) []Grant {
	if !a.IsUser() || a.IsMinor || a.HasAnyPlatformRole() {
		return nil
	}
	return []Grant{
		{Role: RoleUserUnverified}, {Role: RoleStudent}, {Role: RoleClubMember}, {Role: RoleClubAdmin},
		{Role: RoleUniversityRep}, {Role: RoleCareerStaff}, {Role: RoleCompanyMember}, {Role: RoleCompanyAdmin},
	}
}
