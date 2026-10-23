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
// the BE-025 and BE-046 actions are never granted on the own-profile
// resource (another resource type, or no company in OrgID).
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
		authz.StudentsAccessGrantRevoke, authz.StudentsAccessGrantResume, authz.StudentsAccessGrantListCompany, authz.StudentsProfileReadCompany,
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

// ownGrants is the resource of the caller's level 3 grants (03 §3.5
// access_grant; the /me path has no OrgID): id is one grant or uuid.Nil().
func ownGrants(owner, id uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeAccessGrant, ID: id, OwnerUserID: &owner}
}

// companyStudent is a company view of a student (BE-046): typ is
// student_profile, student_cv or access_grant; OrgID is the company of the
// path, OwnerUserID the student; access is the Attrs access_grant.
func companyStudent(typ, company string, student uuid.UUID, access string) authz.Resource {
	org := authztest.ID(company)
	r := authz.Resource{Type: typ, ID: student, OrgID: &org, OwnerUserID: &student}
	switch typ {
	case authz.TypeStudentProfile:
		r.Attrs = map[string]string{authz.AttrAccessGrant: access, "discoverable": "", "minor": "false"}
	case authz.TypeStudentCV:
		r.Attrs = map[string]string{authz.AttrAccessGrant: access}
	}
	return r
}

// TestStudentsPolicy_AccessGrants pins 03 §5.4 for the BE-046 actions and
// the 18 personas: list_own and the /me revoke are the O cell of every
// account, verified or not (BE-077: usr O; companies and staff take the usr
// column; another student's grant 404 for everyone; anonymous 403);
// list_company and the company revoke are cmm S / cma S on
// company_a's management surface (other_company_member 404, platform roles
// 403, the rest 404); read_company and cv.read_company add G: without an
// active grant company roles get 404 (Y5), staff 403 either way.
func TestStudentsPolicy_AccessGrants(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	granted := authztest.ID("student_granted")
	staff := []string{"moderator", "support", "dpo", "platform_admin", "finance_admin", "talent_auditor"}
	companyA := []string{"company_member", "company_admin"}
	for _, p := range authztest.Personas() {
		own := error(nil)
		if !p.Actor.IsUser() {
			own = authz.ErrForbidden
		}
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsAccessGrantListOwn, ownGrants(p.Actor.UserID, uuid.Nil())), own, false)
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsAccessGrantRevoke, ownGrants(p.Actor.UserID, uuid.NewV7())), own, false)
		if p.Actor.IsUser() {
			other := ownGrants(authztest.ID("student_b"), uuid.NewV7())
			wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsAccessGrantRevoke, other), authz.ErrNotFound, false)
			wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsAccessGrantListOwn, other), authz.ErrNotFound, false)
		}

		surface, withGrant, without := error(authz.ErrNotFound), error(authz.ErrNotFound), error(authz.ErrNotFound)
		switch {
		case !p.Actor.IsUser():
			surface, withGrant, without = authz.ErrForbidden, authz.ErrForbidden, authz.ErrForbidden
		case slices.Contains(companyA, p.Name):
			surface, withGrant = nil, nil
		case slices.Contains(staff, p.Name):
			surface, withGrant, without = authz.ErrForbidden, authz.ErrForbidden, authz.ErrForbidden
		}
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsAccessGrantListCompany,
			authz.Resource{Type: authz.TypeAccessGrant, OrgID: ptrID(authztest.ID("company_a"))}), surface, false)
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsAccessGrantRevoke,
			companyStudent(authz.TypeAccessGrant, "company_a", granted, "")), surface, false)
		for _, c := range []struct {
			act authz.Action
			typ string
		}{{authz.StudentsProfileReadCompany, authz.TypeStudentProfile}, {authz.StudentsCvReadCompany, authz.TypeStudentCV}} {
			wantErr(t, authz.Can(ctx, p.Actor, c.act, companyStudent(c.typ, "company_a", granted, "active")), withGrant, false)
			wantErr(t, authz.Can(ctx, p.Actor, c.act, companyStudent(c.typ, "company_a", granted, "none")), without, false)
		}
	}
}

func ptrID(id uuid.UUID) *uuid.UUID { return &id }

// TestStudentsPolicy_AccessGrantEdges: the company owner is cma; a company
// admin without an MFA session gets authz.mfa_required (only once the
// student is visible: without a grant the answer stays 404); the recruiter
// needs no MFA; another company's grant is not this company's (Attrs come
// from the path's company: company_b sees 404 on company_a's path and its
// own path decides on its own grant); a company view without an org or a
// student is nobody's; the /me revoke never reaches a company resource and
// the company revoke never a /me one.
func TestStudentsPolicy_AccessGrantEdges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	granted := authztest.ID("student_granted")
	owner := persona(t, "company_owner_a")
	wantErr(t, authz.Can(ctx, owner, authz.StudentsProfileReadCompany,
		companyStudent(authz.TypeStudentProfile, "company_a", granted, "active")), nil, false)
	wantErr(t, authz.Can(ctx, owner, authz.StudentsAccessGrantRevoke,
		companyStudent(authz.TypeAccessGrant, "company_a", granted, "")), nil, false)

	admin := persona(t, "company_admin")
	admin.MFAVerified = false
	wantErr(t, authz.Can(ctx, admin, authz.StudentsCvReadCompany,
		companyStudent(authz.TypeStudentCV, "company_a", granted, "active")), authz.ErrMFARequired, false)
	wantErr(t, authz.Can(ctx, admin, authz.StudentsCvReadCompany,
		companyStudent(authz.TypeStudentCV, "company_a", granted, "none")), authz.ErrNotFound, false)
	recruiter := persona(t, "company_member")
	if recruiter.MFAVerified {
		t.Fatal("the recruiter persona must have no MFA session")
	}
	wantErr(t, authz.Can(ctx, recruiter, authz.StudentsProfileReadCompany,
		companyStudent(authz.TypeStudentProfile, "company_a", granted, "active")), nil, false)

	b := persona(t, "other_company_member")
	wantErr(t, authz.Can(ctx, b, authz.StudentsProfileReadCompany,
		companyStudent(authz.TypeStudentProfile, "company_a", granted, "active")), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, b, authz.StudentsProfileReadCompany,
		companyStudent(authz.TypeStudentProfile, "company_b", granted, "none")), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, b, authz.StudentsProfileReadCompany,
		companyStudent(authz.TypeStudentProfile, "company_b", granted, "active")), nil, false)

	noOrg := companyStudent(authz.TypeStudentProfile, "company_a", granted, "active")
	noOrg.OrgID = nil
	wantErr(t, authz.Can(ctx, recruiter, authz.StudentsProfileReadCompany, noOrg), authz.ErrNotFound, false)
	noStudent := companyStudent(authz.TypeStudentCV, "company_a", granted, "active")
	noStudent.OwnerUserID = nil
	wantErr(t, authz.Can(ctx, recruiter, authz.StudentsCvReadCompany, noStudent), authz.ErrNotFound, false)
	noStudent = companyStudent(authz.TypeAccessGrant, "company_a", granted, "")
	noStudent.OwnerUserID = nil
	wantErr(t, authz.Can(ctx, recruiter, authz.StudentsAccessGrantRevoke, noStudent), authz.ErrNotFound, false)

	// The student's own grant through the company path: not visible to a
	// student who is no member of the company.
	st := persona(t, "student")
	wantErr(t, authz.Can(ctx, st, authz.StudentsAccessGrantRevoke,
		companyStudent(authz.TypeAccessGrant, "company_a", st.UserID, "")), authz.ErrNotFound, false)
	// A company account reaches its own grants through the usr column (BE-077;
	// it holds none: the service answers 404), never another user's.
	wantErr(t, authz.Can(ctx, recruiter, authz.StudentsAccessGrantRevoke, ownGrants(recruiter.UserID, uuid.NewV7())), nil, false)
	wantErr(t, authz.Can(ctx, recruiter, authz.StudentsAccessGrantRevoke, ownGrants(st.UserID, uuid.NewV7())),
		authz.ErrNotFound, false)
	// A student whose verification lapsed (usr) lists and revokes their own
	// grants; the other own-profile actions stay k-uv (403).
	lapsed := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentLapsed}
	wantErr(t, authz.Can(ctx, lapsed, authz.StudentsAccessGrantListOwn, ownGrants(lapsed.UserID, uuid.Nil())), nil, false)
	wantErr(t, authz.Can(ctx, lapsed, authz.StudentsAccessGrantRevoke, ownGrants(lapsed.UserID, uuid.NewV7())), nil, false)
	wantErr(t, authz.Can(ctx, lapsed, authz.StudentsCvReadOwn, ownCV(lapsed.UserID)), authz.ErrForbidden, false)
	// Wrong resource types are refused.
	wantErr(t, authz.Can(ctx, st, authz.StudentsAccessGrantListOwn, studentProfile(st.UserID, false)), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, recruiter, authz.StudentsProfileReadCompany,
		companyStudent(authz.TypeStudentCV, "company_a", granted, "active")), authz.ErrNotFound, false)
}

// TestStudentsPolicy_AccessGrantResume pins 03 §5.4 for BE-088 (T-332):
// students.access_grant.resume is the O cell of verified students only (stu,
// min, clm, cla, stf); an account without an active verification (usr: a
// lapsed student, the companies, staff) is refused (403, the service answers
// students.verification_required); anonymous 403; another user's grant 404
// for every account; a company path (OrgID) or another resource type is
// never this action's (404).
func TestStudentsPolicy_AccessGrantResume(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, p := range authztest.Personas() {
		want := error(nil)
		if !p.Actor.IsUser() || !p.Actor.IsVerifiedStudent() {
			want = authz.ErrForbidden
		}
		wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsAccessGrantResume, ownGrants(p.Actor.UserID, uuid.NewV7())), want, false)
		if p.Actor.IsUser() {
			other := ownGrants(authztest.ID("student_b"), uuid.NewV7())
			wantErr(t, authz.Can(ctx, p.Actor, authz.StudentsAccessGrantResume, other), authz.ErrNotFound, false)
		}
	}
	st := persona(t, "student")
	lapsed := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentLapsed}
	wantErr(t, authz.Can(ctx, lapsed, authz.StudentsAccessGrantResume, ownGrants(lapsed.UserID, uuid.NewV7())), authz.ErrForbidden, false)
	company := companyStudent(authz.TypeAccessGrant, "company_a", st.UserID, "")
	wantErr(t, authz.Can(ctx, st, authz.StudentsAccessGrantResume, company), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, st, authz.StudentsAccessGrantResume, studentProfile(st.UserID, false)), authz.ErrNotFound, false)
}
