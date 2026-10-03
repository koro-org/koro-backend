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
// BE-019 adds the membership lifecycle (03 §5.2):
//
//	orgs.member.list         S·k4: club_admin; university_rep (k-cs); recruiter (company_member);
//	                           company_admin/owner of the org; B: support, platform_admin
//	orgs.member.read         same cells as the list (resource org_member)
//	orgs.member.join         stu, min (clm, cla, stf are verified students: the student paths)
//	orgs.member.decide       S: club_admin
//	orgs.member.remove       S: club_admin, university_rep (k-cs), company_admin/owner; platform_admin
//	orgs.member.change_role  S·k5: club_admin, company_admin/owner; platform_admin (k5 in the service)
//	orgs.membership.list_own O (resource user: the caller's own account)
//	orgs.membership.leave    O (resource org_member owned by the caller; k6 in the service)
//	orgs.invitation.create   S·k5: club_admin, company_admin/owner; platform_admin
//	orgs.invitation.list     S: club_admin, company_admin/owner; platform_admin
//	orgs.invitation.revoke   S: club_admin, company_admin/owner; platform_admin
//	orgs.invitation.accept   O: the service sets OwnerUserID to the caller once the invited
//	                           address matched the caller's account (otherwise 404 before authz)
//
// Visibility: the member list and the invitation collection are the org's
// management surface (org resource). A sensitive club's list (Attrs
// sensitive=true, k4/Y2) is visible to the club's active members only:
// platform staff get 404 (§7.2: Y2 is the exception to "staff see
// management surfaces"), a plain member 403. A single membership
// (org_member) is the management surface too; the core's Y2 hides a
// sensitive one from everyone but its member and the club's club_admin.
// /me/memberships/{id} and an invitation being accepted are the caller's
// own (O): anyone else's is 404. joinClub addresses a club (resource
// club): the catalog is public (§7.6), so an account without a student
// verification gets 403 (the service turns it into
// students.verification_required, k-uv). university_rep is invited and
// assigned by platform_admin only (k5): club and company admins never hold
// a role in a university org.
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
			case OrgsMemberList:
				if r.Type != TypeOrg {
					return false
				}
				if r.Flag(AttrSensitive) {
					return r.OrgID != nil && a.MemberOf(*r.OrgID) // Y2: no staff
				}
				return OrgSurfaceVisible(a, r.OrgID)
			case OrgsInvitationList:
				return r.Type == TypeOrg && OrgSurfaceVisible(a, r.OrgID)
			case OrgsMemberRead, OrgsMemberDecide, OrgsMemberRemove, OrgsMemberChangeRole:
				return r.Type == TypeOrgMember && OrgSurfaceVisible(a, r.OrgID)
			case OrgsInvitationCreate, OrgsInvitationRevoke:
				return r.Type == TypeOrgInvitation && OrgSurfaceVisible(a, r.OrgID)
			case OrgsMembershipListOwn:
				return r.Type == TypeUser && a.Owns(r)
			case OrgsMembershipLeave:
				return r.Type == TypeOrgMember && a.Owns(r)
			case OrgsInvitationAccept:
				return r.Type == TypeOrgInvitation && a.Owns(r)
			case OrgsMemberJoin:
				return r.Type == TypeClub
			}
			return false
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
			case OrgsMemberList, OrgsMemberRead:
				return orgsMemberReadGrants(a, r)
			case OrgsMemberDecide:
				return orgsRoleGrants(a, r, false, MemberClubAdmin)
			case OrgsMemberRemove:
				return orgsRoleGrants(a, r, true, MemberClubAdmin, MemberUniversityRep, MemberCompanyAdmin, MemberCompanyOwner)
			case OrgsMemberChangeRole, OrgsInvitationCreate, OrgsInvitationList, OrgsInvitationRevoke:
				return orgsRoleGrants(a, r, true, MemberClubAdmin, MemberCompanyAdmin, MemberCompanyOwner)
			case OrgsMembershipListOwn, OrgsMembershipLeave, OrgsInvitationAccept:
				return []Grant{{Role: RoleSelf}}
			case OrgsMemberJoin:
				return []Grant{{Role: RoleStudent}, {Role: RoleStudentMinor}}
			}
			return nil
		},
	}
}

// orgsMemberRole maps an org role to its matrix role path (03 §1.2).
var orgsMemberRole = map[MemberRole]Role{
	MemberMember: RoleClubMember, MemberClubAdmin: RoleClubAdmin, MemberUniversityRep: RoleUniversityRep,
	MemberCareerStaff: RoleCareerStaff, MemberRecruiter: RoleCompanyMember, MemberCompanyAdmin: RoleCompanyAdmin,
	MemberCompanyOwner: RoleCompanyAdmin,
}

// orgsRoleGrants returns the S grants of the actor's roles among roles in the
// resource's org (cell S: that org only), plus platform_admin when pad.
func orgsRoleGrants(a Actor, r Resource, pad bool, roles ...MemberRole) []Grant {
	var gs []Grant
	if pad {
		gs = append(gs, Grant{Role: RolePlatformAdmin})
	}
	if r.OrgID == nil {
		return gs
	}
	for _, m := range roles {
		if a.HasOrgRole(*r.OrgID, m) {
			gs = append(gs, Grant{Role: orgsMemberRole[m]})
		}
	}
	return gs
}

// orgsMemberReadGrants is orgs.member.list / orgs.member.read (03 §5.2 with k4,
// k-cs): the org's club_admin, university_rep, recruiter, company_admin and
// owner; support and platform_admin by break-glass, which the core voids in
// a sensitive club (Y2 → 404).
func orgsMemberReadGrants(a Actor, r Resource) []Grant {
	gs := orgsRoleGrants(a, r, false, MemberClubAdmin, MemberUniversityRep, MemberRecruiter, MemberCompanyAdmin, MemberCompanyOwner)
	return append(gs, Grant{Role: RoleSupport, BreakGlass: true}, Grant{Role: RolePlatformAdmin, BreakGlass: true})
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
