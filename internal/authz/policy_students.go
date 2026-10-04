package authz

// AttrDSRID is the Attrs key of students.profile.read_admin: the open data
// subject request whose subject is the profile owner, as the students
// service verified it (03 k11; "" without one).
const AttrDSRID = "dsr_id"

// studentsPolicy is the policy of the students module (03 §5.4). Only tickets of
// the students module edit this file (BE-001 R5). Written by BE-023, extended by
// BE-025.
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
//
// The access grant and company actions stay denied until BE-046 writes them.
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
			case own[act], cv[act]:
				return []Grant{{Role: RoleStudent}, {Role: RoleStudentMinor}}
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
