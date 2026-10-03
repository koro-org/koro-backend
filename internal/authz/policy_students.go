package authz

// studentsPolicy is the policy of the students module (03 §5.4). Only tickets of
// the students module edit this file (BE-001 R5). Written by BE-023.
//
//	students.profile.read_own        O for verified students (stu, min, clm, cla, stf);
//	students.profile.update_own      usr k-uv: 403 (the service answers
//	students.profile_section.manage  students.verification_required); every other
//	students.privacy.read_own        role ✗ (403). k9 (a minor turning level 2 or
//	students.privacy.update_own      contact sharing on) is a service rule: 422.
//
// The O cell is the student path of the actor's own profile: the grants are
// student and student_minor (an account without an active verification holds
// neither, whatever else it is), and the resource must be the actor's own
// (visibility). No MFA: student roles do not require it, so a company_admin
// who is also a student edits their profile without MFA (03 §1.3).
//
// Resources (03 §3.5; 06 H-27): student_profile with OwnerUserID = the
// profile's user (always the actor on /me) and Attrs access_grant ("none":
// no company is involved), discoverable and minor.
//
// The CV, access grant, company, member card and admin actions stay denied
// until BE-025 and BE-046 write them.
func studentsPolicy() Policy {
	own := map[Action]bool{
		StudentsProfileReadOwn: true, StudentsProfileUpdateOwn: true, StudentsProfileSectionManage: true,
		StudentsPrivacyReadOwn: true, StudentsPrivacyUpdateOwn: true,
	}
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			return own[act] && r.Type == TypeStudentProfile && a.Owns(r)
		},
		Grants: func(_ Actor, act Action, _ Resource) []Grant {
			if own[act] {
				return []Grant{{Role: RoleStudent}, {Role: RoleStudentMinor}}
			}
			return nil
		},
	}
}
