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
// the actions of BE-046 stay denied, and the BE-025 actions are never
// granted on another resource type than their own.
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
		authz.StudentsAccessGrantRevoke, authz.StudentsAccessGrantListCompany, authz.StudentsProfileReadCompany,
		authz.StudentsCvReadCompany, authz.StudentsProfileReadMemberCard, authz.StudentsProfileReadAdmin} {
		if err := authz.Can(ctx, st, act, studentProfile(st.UserID, false)); err == nil {
			t.Fatalf("%s is granted on the student's own profile resource", act)
		}
	}
	// The own-profile actions are not granted on a CV resource either.
	if err := authz.Can(ctx, st, authz.StudentsProfileReadOwn, ownCV(st.UserID)); err == nil {
		t.Fatal("students.profile.read_own granted on a student_cv")
	}
}

// ownCV is the resource of the caller's CVs (03 §3.5 student_cv).
func ownCV(owner uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeStudentCV, OwnerUserID: &owner, Attrs: map[string]string{authz.AttrAccessGrant: "none"}}
}

// TestStudentsPolicy_CVs pins 03 §5.4 students.cv.manage / read_own: the O
// cell of verified students on their own CVs (403 for every account
// without a verification, 404 for another student's CVs, staff included).
func TestStudentsPolicy_CVs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, act := range []authz.Action{authz.StudentsCvManage, authz.StudentsCvReadOwn} {
		for _, p := range authztest.Personas() {
			want := error(authz.ErrForbidden)
			if slices.Contains(verifiedStudents, p.Name) {
				want = nil
			}
			wantErr(t, authz.Can(ctx, p.Actor, act, ownCV(p.Actor.UserID)), want, false)
		}
		for _, name := range []string{"student", "club_admin", "support", "dpo", "platform_admin", "company_admin"} {
			wantErr(t, authz.Can(ctx, persona(t, name), act, ownCV(authztest.ID("student_b"))), authz.ErrNotFound, false)
		}
	}
}

// memberCard is the member card resource of a club (03 k10).
func memberCard(club string, sensitive bool) authz.Resource {
	org := authztest.ID(club)
	s := "false"
	if sensitive {
		s = "true"
	}
	return authz.Resource{Type: authz.TypeStudentProfile, ID: authztest.ID("m_card"), OrgID: &org, Attrs: map[string]string{
		authz.AttrAccessGrant: "none", "discoverable": "", "minor": "", authz.AttrSensitive: s,
	}}
}

// TestStudentsPolicy_MemberCard pins 03 §5.4 students.profile.read_member_card
// (cla S·k10) for the 18 personas on club_a, and on club_sensitive: the club's
// club_admin only (helper club_admin_sensitive); a member of the club 403, a
// non-member 404, platform staff 403 (404 in a sensitive club: Y2, k4); a
// minor club_admin never (Y4).
func TestStudentsPolicy_MemberCard(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	staffRoles := []string{"moderator", "support", "dpo", "platform_admin", "finance_admin", "talent_auditor"}
	for _, p := range authztest.Personas() {
		if !p.Actor.IsUser() {
			continue
		}
		want := error(authz.ErrNotFound)
		switch {
		case p.Name == "club_admin":
			want = nil
		case p.Name == "club_member", slices.Contains(staffRoles, p.Name):
			want = authz.ErrForbidden
		}
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsProfileReadMemberCard, memberCard("club_a", false)), want, false)

		want = authz.ErrNotFound
		if p.Name == "club_member" { // a plain member of club_sensitive
			want = authz.ErrForbidden
		}
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsProfileReadMemberCard, memberCard("club_sensitive", true)), want, false)
	}
	wantErr(t, authz.Can(ctx, persona(t, "club_admin_sensitive"), authz.StudentsProfileReadMemberCard,
		memberCard("club_sensitive", true)), nil, false)
	minor := persona(t, "club_admin")
	minor.IsMinor = true
	wantErr(t, authz.Can(ctx, minor, authz.StudentsProfileReadMemberCard, memberCard("club_a", false)), authz.ErrForbidden, false)
	// No org: nobody's card.
	noOrg := memberCard("club_a", false)
	noOrg.OrgID = nil
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.StudentsProfileReadMemberCard, noOrg), authz.ErrNotFound, false)
}

// adminView is the admin student view resource (03 §3.5; k11 dsr_id).
func adminView(reason, dsr string) authz.Resource {
	u := authztest.ID("student")
	return authz.Resource{Type: authz.TypeStudentProfile, ID: u, OwnerUserID: &u, Attrs: map[string]string{
		authz.AttrAccessGrant: "none", "discoverable": "", "minor": "", authz.AttrAccessReason: reason, authz.AttrDSRID: dsr,
	}}
}

// TestStudentsPolicy_AdminView pins 03 §5.4 students.profile.read_admin
// (sup B, dpo ✓·k11, pad B): every non-staff user 403 (admin surface);
// support and platform_admin need the reason (break_glass_reason_required);
// the dpo needs the verified request (dsr_id), with or without a reason;
// MFA for every staff path.
func TestStudentsPolicy_AdminView(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	reason := "Destek talebi 4711: profil incelemesi"
	dsr := authztest.ID("dsr_student_open").String()
	for _, p := range authztest.Personas() {
		if !p.Actor.IsUser() {
			continue
		}
		var bare, withReason, withDSR error = authz.ErrForbidden, authz.ErrForbidden, authz.ErrForbidden
		switch p.Name {
		case "support", "platform_admin":
			bare, withReason, withDSR = authz.ErrBreakGlassReason, nil, authz.ErrBreakGlassReason
		case "dpo":
			withDSR = nil
		}
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsProfileReadAdmin, adminView("", "")), bare, false)
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsProfileReadAdmin, adminView(reason, "")), withReason, false)
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsProfileReadAdmin, adminView("", dsr)), withDSR, false)
	}
	for _, name := range []string{"support", "platform_admin", "dpo"} {
		a := persona(t, name)
		a.MFAVerified = false
		wantErr(t, authz.Can(ctx, a, authz.StudentsProfileReadAdmin, adminView(reason, dsr)), authz.ErrMFARequired, false)
	}
	// The resource must name its target user.
	noOwner := adminView(reason, "")
	noOwner.OwnerUserID = nil
	wantErr(t, authz.Can(ctx, persona(t, "support"), authz.StudentsProfileReadAdmin, noOwner), authz.ErrNotFound, false)
}
