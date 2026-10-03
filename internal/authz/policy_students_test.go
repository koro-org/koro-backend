package authz_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// studentProfile is the resource the students service builds for /me
// (06 H-27: OwnerUserID always set; 03 §3.5 Attrs).
func studentProfile(owner uuid.UUID, minor bool) authz.Resource {
	m := "false"
	if minor {
		m = "true"
	}
	return authz.Resource{Type: authz.TypeStudentProfile, ID: owner, OwnerUserID: &owner, Attrs: map[string]string{
		authz.AttrAccessGrant: "none", "discoverable": "", "minor": m,
	}}
}

// studentsOwnActions are the five actions of BE-023 (03 §5.4).
var studentsOwnActions = []authz.Action{
	authz.StudentsProfileReadOwn, authz.StudentsProfileUpdateOwn, authz.StudentsProfileSectionManage,
	authz.StudentsPrivacyReadOwn, authz.StudentsPrivacyUpdateOwn,
}

// verifiedStudents hold the O cell (stu, min, clm, cla, stf; 03 §5.0: clm,
// cla and stf personas are verified students).
var verifiedStudents = []string{"student", "student_minor", "club_member", "club_admin", "other_club_admin", "event_staff"}

// TestStudentsPolicy_Matrix pins 03 §5.4 for the 18 personas on their own
// profile: verified students O; anonymous and every account without an
// active student verification (usr k-uv, uni, companies, platform staff) 403.
func TestStudentsPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, act := range studentsOwnActions {
		t.Run(string(act), func(t *testing.T) {
			t.Parallel()
			for _, p := range authztest.Personas() {
				want := error(authz.ErrForbidden)
				if slices.Contains(verifiedStudents, p.Name) {
					want = nil
				}
				res := studentProfile(p.Actor.UserID, p.Actor.IsMinor)
				wantErr(t, authz.Can(ctx, p.Actor, act, res), want, false)
			}
		})
	}
}

// TestStudentsPolicy_OtherProfileIsHidden: the O cell is the actor's own
// profile only; another student's profile is 404 for everyone (staff
// included), never 403 (03 §7.2 IDOR rule).
func TestStudentsPolicy_OtherProfileIsHidden(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	other := studentProfile(authztest.ID("student_b"), false)
	for _, act := range studentsOwnActions {
		for _, name := range []string{"student", "student_minor", "club_admin", "company_admin", "other_company_member",
			"support", "dpo", "platform_admin"} {
			wantErr(t, authz.Can(ctx, persona(t, name), act, other), authz.ErrNotFound, false)
		}
	}
	// A profile without an owner is nobody's.
	noOwner := authz.Resource{Type: authz.TypeStudentProfile, Attrs: map[string]string{
		authz.AttrAccessGrant: "none", "discoverable": "", "minor": "false",
	}}
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.StudentsProfileReadOwn, noOwner), authz.ErrNotFound, false)
}

// TestStudentsPolicy_EdgeCases: no MFA on the student path (a company
// admin who is also a student, without an MFA session); a lapsed student
// is the usr column (403); missing Attrs keys deny as a programmer error;
// the actions of BE-025/BE-046 stay denied.
func TestStudentsPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	uni := authztest.UniversityID("uni_a")
	companyStudent := authz.Actor{
		Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentVerified, UniversityID: &uni,
		Memberships: []authz.Membership{{OrgID: authztest.ID("company_a"), OrgKind: authz.OrgCompany, Role: authz.MemberCompanyAdmin}},
	}
	wantErr(t, authz.Can(ctx, companyStudent, authz.StudentsPrivacyUpdateOwn, studentProfile(companyStudent.UserID, false)), nil, false)

	lapsed := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentLapsed}
	wantErr(t, authz.Can(ctx, lapsed, authz.StudentsProfileReadOwn, studentProfile(lapsed.UserID, false)), authz.ErrForbidden, false)

	st := persona(t, "student")
	missing := authz.Resource{Type: authz.TypeStudentProfile, OwnerUserID: &st.UserID, Attrs: map[string]string{"minor": "false"}}
	wantErr(t, authz.Can(ctx, st, authz.StudentsProfileReadOwn, missing), authz.ErrForbidden, true)

	for _, act := range []authz.Action{authz.StudentsCvManage, authz.StudentsCvReadOwn, authz.StudentsAccessGrantListOwn,
		authz.StudentsProfileReadCompany, authz.StudentsProfileReadMemberCard, authz.StudentsProfileReadAdmin} {
		if err := authz.Can(ctx, st, act, studentProfile(st.UserID, false)); err == nil {
			t.Fatalf("%s is granted before its ticket", act)
		}
	}
}
