package authz

import "uuid"

// AttrDSRID is the Attrs key of students.profile.read_admin: the open data
// subject request whose subject is the profile owner, as the students
// service verified it (03 k11; "" without one).
const AttrDSRID = "dsr_id"

// studentsPolicy is the policy of the students module (03 §5.4). Only tickets of
// the students module edit this file (BE-001 R5). Written by BE-023, extended by
// BE-025, BE-046, BE-077 and BE-088.
//
//	students.profile.read_own          O for verified students (stu, min, clm, cla, stf);
//	students.profile.update_own        usr k-uv: 403 (the service answers
//	students.profile_section.manage    students.verification_required); every other
//	students.privacy.read_own          role ✗ (403). k9 (a minor turning level 2 or
//	students.privacy.update_own        contact sharing on) is a service rule: 422.
//	students.cv.manage                 the same O cell on the student's own CVs
//	students.cv.read_own               (student_cv, access_grant "none").
//	students.profile.read_member_card  cla S·k10: the club_admin of the club (Y4: never a
//	                                   minor); the club's surface is visible to its active
//	                                   members (403 without the role) and to platform
//	                                   roles (403) except in a sensitive club (404, Y2/k4);
//	                                   everyone else 404. k10 "the owner is an active
//	                                   member" and the sensitive-club audit are service
//	                                   rules (404, audit).
//	students.profile.read_admin        sup B, pad B, dpo ✓·k11 (Attrs dsr_id: an open
//	                                   request of the owner, verified by the service); the
//	                                   admin surface: every other user 403.
//
// BE-046 (level 3 access grants, 03 §5.4, Y5):
//
//	students.access_grant.list_own      O for usr, stu and min (BE-077, KVKK md. 11: no student
//	                                    verification needed, so a student whose verification
//	                                    lapsed lists and revokes the grants they gave; every
//	                                    account without a verification takes the usr column) on
//	                                    access_grant with OwnerUserID = the actor, no OrgID.
//	students.access_grant.revoke        two paths, told apart by OrgID: /me (no OrgID) is the
//	                                    O cell above (another student's grant is the service's
//	                                    404 after the decision); /orgs/{org_id} (OrgID = the
//	                                    company of the path) is cmm S and cma S on the org
//	                                    management surface (active members 403 without the
//	                                    role, platform roles 403, everyone else 404).
//	students.access_grant.resume        O for stu and min only (BE-088, T-332: resuming a suspended
//	                                    grant is a new consent act and needs an active student
//	                                    verification; usr k-uv: 403, the service answers
//	                                    students.verification_required) on access_grant with
//	                                    OwnerUserID = the actor, no OrgID. The suspension window
//	                                    and the grant's state are service rules (409).
//	students.access_grant.list_company  cmm S, cma S on the org management surface.
//	students.profile.read_company       cmm S·G, cma S·G: the org management surface, and Y5
//	students.cv.read_company            (absolute.go) hides the student (404) from company
//	                                    roles unless Attrs access_grant = "active" (an active
//	                                    grant of that very company, read by the service
//	                                    before the decision). Platform roles see the surface
//	                                    and get 403 whatever the grant (no existence leak).
//
// cmm is the recruiter, cma the company_admin and company_owner (03 §1.2);
// k-mfa applies to cma through its role path.
//
// The O cell is the student path of the actor's own profile: the grants are
// student and student_minor (an account without an active verification holds
// neither, whatever else it is), and the resource must be the actor's own
// (visibility). No MFA: student roles do not require it, so a company_admin
// who is also a student edits their profile without MFA (03 §1.3). The staff
// grants carry k-mfa through their roles.
//
// Resources (03 §3.5; 06 H-27): student_profile with Attrs access_grant
// ("none": no company is involved), discoverable and minor; OwnerUserID = the
// profile's user on /me and on the admin view (with Attrs access_reason and
// dsr_id); the member card passes OrgID = the club and Attrs sensitive before
// any member is read. student_cv with OwnerUserID = the actor and access_grant.
// The company views (BE-046) pass OrgID = the company of the path and
// OwnerUserID = the student: student_profile (read_company), student_cv
// (cv.read_company) with access_grant "active" or "none", access_grant
// (list_company without an owner, the company revoke with the student).
func studentsPolicy() Policy {
	own := map[Action]bool{
		StudentsProfileReadOwn: true, StudentsProfileUpdateOwn: true, StudentsProfileSectionManage: true,
		StudentsPrivacyReadOwn: true, StudentsPrivacyUpdateOwn: true,
	}
	cv := map[Action]bool{StudentsCvManage: true, StudentsCvReadOwn: true}
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch {
			case own[act]:
				return r.Type == TypeStudentProfile && a.Owns(r)
			case cv[act]:
				return r.Type == TypeStudentCV && a.Owns(r)
			case act == StudentsAccessGrantListOwn, act == StudentsAccessGrantResume:
				return r.Type == TypeAccessGrant && r.OrgID == nil && a.Owns(r)
			case act == StudentsAccessGrantRevoke:
				if r.Type != TypeAccessGrant {
					return false
				}
				if r.OrgID == nil { // the student's path (/me)
					return a.Owns(r)
				}
				return r.OwnerUserID != nil && OrgSurfaceVisible(a, r.OrgID) // the company's path
			case act == StudentsAccessGrantListCompany:
				return r.Type == TypeAccessGrant && r.OrgID != nil && OrgSurfaceVisible(a, r.OrgID)
			case act == StudentsProfileReadCompany:
				return r.Type == TypeStudentProfile && r.OrgID != nil && r.OwnerUserID != nil && OrgSurfaceVisible(a, r.OrgID)
			case act == StudentsCvReadCompany:
				return r.Type == TypeStudentCV && r.OrgID != nil && r.OwnerUserID != nil && OrgSurfaceVisible(a, r.OrgID)
			case act == StudentsProfileReadMemberCard:
				if r.Type != TypeStudentProfile || r.OrgID == nil {
					return false
				}
				return a.MemberOf(*r.OrgID) || (a.HasAnyPlatformRole() && !r.Flag(AttrSensitive))
			case act == StudentsProfileReadAdmin:
				return r.Type == TypeStudentProfile && r.OwnerUserID != nil && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			switch {
			case own[act], cv[act], act == StudentsAccessGrantResume:
				return []Grant{{Role: RoleStudent}, {Role: RoleStudentMinor}}
			case act == StudentsAccessGrantListOwn:
				return ownGrantsGrants()
			case act == StudentsAccessGrantRevoke:
				if r.OrgID == nil {
					return ownGrantsGrants()
				}
				return companyGrants(a, r.OrgID)
			case act == StudentsAccessGrantListCompany, act == StudentsProfileReadCompany, act == StudentsCvReadCompany:
				return companyGrants(a, r.OrgID)
			case act == StudentsProfileReadMemberCard:
				if r.OrgID != nil && a.HasOrgRole(*r.OrgID, MemberClubAdmin) {
					return []Grant{{Role: RoleClubAdmin}}
				}
			case act == StudentsProfileReadAdmin:
				gs := []Grant{{Role: RoleSupport, BreakGlass: true}, {Role: RolePlatformAdmin, BreakGlass: true}}
				if r.Attr(AttrDSRID) != "" { // k11: only for the subject of an open request
					gs = append(gs, Grant{Role: RoleDPO})
				}
				return gs
			}
			return nil
		},
	}
}

// ownGrantsGrants is the O cell of the student's own level 3 grants: usr,
// stu and min (BE-077: no student verification needed; KVKK md. 11).
func ownGrantsGrants() []Grant {
	return []Grant{{Role: RoleUserUnverified}, {Role: RoleStudent}, {Role: RoleStudentMinor}}
}

// companyGrants returns the S grants of cmm (recruiter) and cma
// (company_admin, company_owner) the actor holds in orgID (03 §1.2, §5.4).
func companyGrants(a Actor, orgID *uuid.UUID) []Grant {
	if orgID == nil {
		return nil
	}
	var gs []Grant
	if a.HasOrgRole(*orgID, MemberRecruiter) {
		gs = append(gs, Grant{Role: RoleCompanyMember})
	}
	if a.HasOrgRole(*orgID, MemberCompanyAdmin, MemberCompanyOwner) {
		gs = append(gs, Grant{Role: RoleCompanyAdmin})
	}
	return gs
}
