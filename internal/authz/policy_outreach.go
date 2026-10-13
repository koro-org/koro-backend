package authz

import "uuid"

// outreachPolicy is the policy of the outreach module (03 §5.8). Only
// tickets of the outreach module edit this file (BE-001 R5). Written by
// BE-030 for the segment and audience count actions:
//
//	outreach.segment.list     S: club_admin, university_rep, career_staff, company_member (recruiter),
//	outreach.segment.read       company_admin/owner of the org; platform_admin k-po
//	outreach.segment.manage   the same (k16, the targeting scope, is the service's: 422
//	                            outreach.target_out_of_scope / outreach.sensitive_target)
//	outreach.audience.count   the same S roles (k16 for club_admin, U for university_rep: the
//	                            service's scope check); platform_admin for every org (✓)
//
// and by BE-035 for the dispatch actions:
//
//	outreach.dispatch.create  S·k18: the same S roles of the post's org; platform_admin k-po (the
//	                            platform org's posts). k18 (a company's targeted_dispatch unit and
//	                            the commercial consent filter) and k16 (the scope) are the service's
//	outreach.dispatch.list    the same S roles; platform_admin for every org (✓)
//	outreach.dispatch.read
//	outreach.dispatch.cancel
//
// Resources (03 §3.5): segment and dispatch with OrgID = the org (of the
// path; of the post row for createDispatch, 06 H-38); every action
// addresses the org management surface (/orgs/{org_id}/segments,
// /orgs/{org_id}/audience-counts, /orgs/{org_id}/dispatches): the org's
// active members and every platform role see it (403 without the grant),
// everyone else gets 404 (03 §3.6). A segment of another org is the service's 404 (it reads the
// segment within the path's org). MFA follows from the role paths (k-mfa:
// university_rep, company_admin, platform_admin; not career_staff or
// company_member). career_staff takes the university_rep cells (03 §1.1
// row 8: k-cs excludes only member/invitation management and offering
// approval). k-po (H-39, T-190): platform_admin manages the segments of
// the platform org only (platformOrg of policy_files.go); counting is ✓.
func outreachPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			return (r.Type == TypeSegment || r.Type == TypeDispatch) && OrgSurfaceVisible(a, r.OrgID)
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			switch act {
			case OutreachSegmentList, OutreachSegmentRead, OutreachSegmentManage, OutreachDispatchCreate:
				gs := targetingGrants(a, r.OrgID)
				if platformOrg(a, r.OrgID) { // k-po
					gs = append(gs, Grant{Role: RolePlatformAdmin})
				}
				return gs
			case OutreachAudienceCount, OutreachDispatchList, OutreachDispatchRead, OutreachDispatchCancel:
				return append(targetingGrants(a, r.OrgID), Grant{Role: RolePlatformAdmin})
			}
			return nil
		},
	}
}

// targetingRoles are the org roles that target an org's audience (cell S
// of cla, uni, cmm and cma in 03 §5.8).
var targetingRoles = []struct {
	member MemberRole
	role   Role
}{
	{MemberClubAdmin, RoleClubAdmin},
	{MemberUniversityRep, RoleUniversityRep},
	{MemberCareerStaff, RoleCareerStaff},
	{MemberRecruiter, RoleCompanyMember},
	{MemberCompanyAdmin, RoleCompanyAdmin},
	{MemberCompanyOwner, RoleCompanyAdmin},
}

// targetingGrants returns the S grants the actor holds in orgID.
func targetingGrants(a Actor, orgID *uuid.UUID) []Grant {
	if orgID == nil {
		return nil
	}
	var gs []Grant
	for _, m := range targetingRoles {
		if a.HasOrgRole(*orgID, m.member) {
			gs = append(gs, Grant{Role: m.role})
		}
	}
	return gs
}
