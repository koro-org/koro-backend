package students_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/students"
)

// clubAdmin returns a verified adult student who is the club_admin of club.
func (e *env) clubAdmin(t *testing.T, club uuid.UUID) authz.Actor {
	t.Helper()
	a := e.student(t, e.uniA, false)
	a.Memberships = append(a.Memberships, authz.Membership{OrgID: club, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin})
	return a
}

func staff(role authz.PlatformRole) authz.Actor {
	return authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), PlatformRoles: []authz.PlatformRole{role}, MFAVerified: true}
}

// TestGetMemberCard_NonMember_404 (03 k10, §7.2): the card exists only for
// an active member of the club_admin's own club. A pending or ended
// membership, another club's membership ID, an unknown ID and a club the
// caller does not belong to are 404; a plain member of the club is 403;
// platform staff 403 (404 in a sensitive club, Y2). Only a club_admin of
// the club reaches orgs.
func TestGetMemberCard_NonMember_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := e.clubAdmin(t, e.clubPlain)
	member := e.student(t, e.uniA, false)
	active := e.members.add(e.clubPlain, member.UserID, authz.MemberMember, orgs.MemberActive)
	pending := e.members.add(e.clubPlain, e.student(t, e.uniA, false).UserID, authz.MemberMember, orgs.MemberPending)
	ended := e.members.add(e.clubPlain, e.student(t, e.uniA, false).UserID, authz.MemberMember, orgs.MemberEnded)
	elsewhere := e.members.add(e.clubSens, member.UserID, authz.MemberMember, orgs.MemberActive)
	if _, err := e.svc.GetMemberCard(ctx, admin, e.clubPlain, active); err != nil {
		t.Fatalf("own club's active member: %v", err)
	}
	for name, id := range map[string]uuid.UUID{"pending": pending, "ended": ended, "another club's": elsewhere, "unknown": uuid.NewV7()} {
		if _, err := e.svc.GetMemberCard(ctx, admin, e.clubPlain, id); !errors.Is(err, students.ErrNotFound) {
			t.Errorf("%s membership: %v, want 404", name, err)
		}
	}
	otherAdmin := e.clubAdmin(t, uuid.NewV7())
	plain := e.student(t, e.uniA, false)
	plain.Memberships = []authz.Membership{{OrgID: e.clubPlain, OrgKind: authz.OrgClub, Role: authz.MemberMember}}
	minorAdmin := e.clubAdmin(t, e.clubPlain)
	minorAdmin.IsMinor = true
	for _, c := range []struct {
		name string
		a    authz.Actor
		club uuid.UUID
		want error
	}{
		{"another club's admin", otherAdmin, e.clubPlain, authz.ErrNotFound},
		{"a student outside the club", e.student(t, e.uniA, false), e.clubPlain, authz.ErrNotFound},
		{"a plain member", plain, e.clubPlain, authz.ErrForbidden},
		{"a minor club_admin (Y4)", minorAdmin, e.clubPlain, authz.ErrForbidden},
		{"support", staff(authz.PlatformSupport), e.clubPlain, authz.ErrForbidden},
		{"platform_admin, sensitive club", staff(authz.PlatformAdmin), e.clubSens, authz.ErrNotFound},
		{"an org that is no known club", admin, uuid.NewV7(), authz.ErrNotFound},
	} {
		if _, err := e.svc.GetMemberCard(ctx, c.a, c.club, active); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	for _, a := range e.members.calls {
		if !a.HasOrgRole(e.clubPlain, authz.MemberClubAdmin) || a.IsMinor {
			t.Fatalf("orgs was read for %v", a)
		}
	}
}

// TestGetMemberCard_NoEmailField (04 §V.4, T-69): the card's field set is
// exactly the allow-list: no e-mail, CV, skills, experience, other clubs,
// privacy settings, birth date or talent value, in the type and in a real
// answer.
func TestGetMemberCard_NoEmailField(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := e.clubAdmin(t, e.clubPlain)
	member := e.student(t, e.uniA, false)
	e.basics(t, member, e.deptA, 3, t0.AddDate(1, 0, 0))
	if _, err := e.svc.CreateMyCV(ctx, member, e.files.upload(member.UserID, files.PurposeCV), true); err != nil {
		t.Fatal(err)
	}
	id := e.members.add(e.clubPlain, member.UserID, authz.MemberMember, orgs.MemberActive)
	e.regs = &regsFake{count: map[uuid.UUID]int{member.UserID: 4}}

	want := []string{"user_id", "first_name", "last_name", "avatar_url", "university", "university.name", "department",
		"department.name", "class_year", "membership", "membership.membership_id", "membership.role", "membership.status",
		"membership.joined_at", "membership.source", "club_event_registrations_count"}
	wantFields(t, "MemberCardDTO type", jsonFields(reflect.TypeFor[students.MemberCardDTO]()), want)

	app := newHTTP(t, e)
	rec := e.callAs(t, app, admin, http.MethodGet, "/api/v1/orgs/"+e.clubPlain.String()+"/members/"+id.String()+"/profile")
	if rec.Code != http.StatusOK {
		t.Fatalf("getMemberCard = %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	wantFields(t, "MemberCardDTO answer", presentFields(t, body), want)
	if body["club_event_registrations_count"] != float64(4) || body["class_year"] != float64(3) {
		t.Fatalf("card = %s", rec.Body)
	}
	for _, f := range presentFields(t, body) {
		if forbiddenRe.MatchString(f) || f == "email" || f == "cv" || f == "skills" {
			t.Fatalf("member card member %q", f)
		}
	}
	// Without the events reader the count is null, never a made-up 0.
	e.regs = nil
	c, err := e.svc.GetMemberCard(ctx, admin, e.clubPlain, id)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(c); c.RegistrationsCount != nil || len(b) == 0 {
		t.Fatalf("count without events = %v", c.RegistrationsCount)
	}
}

// TestGetMemberCard_SensitiveClubAudited (03 k10, §6.5): every card of a
// sensitive club is audited (with its subject); a plain club's card is not.
func TestGetMemberCard_SensitiveClubAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	member := e.student(t, e.uniA, false)
	plainID := e.members.add(e.clubPlain, member.UserID, authz.MemberMember, orgs.MemberActive)
	sensID := e.members.add(e.clubSens, member.UserID, authz.MemberMember, orgs.MemberActive)
	if _, err := e.svc.GetMemberCard(ctx, e.clubAdmin(t, e.clubPlain), e.clubPlain, plainID); err != nil {
		t.Fatal(err)
	}
	if got := e.audit.Of(authz.StudentsProfileReadMemberCard); len(got) != 0 {
		t.Fatalf("plain club card audited: %+v", got)
	}
	admin := e.clubAdmin(t, e.clubSens)
	for range 2 {
		if _, err := e.svc.GetMemberCard(ctx, admin, e.clubSens, sensID); err != nil {
			t.Fatal(err)
		}
	}
	got := e.audit.Of(authz.StudentsProfileReadMemberCard)
	if len(got) != 2 || got[0].Metadata[audit.MetaSensitive] != true || got[0].Metadata[audit.MetaSubjectUserID] != member.UserID.String() ||
		*got[0].OrgID != e.clubSens || *got[0].ActorUserID != admin.UserID {
		t.Fatalf("sensitive card audit = %+v", got)
	}
}

// TestAdminGetStudentProfile_Audited (03 §6.3, §6.5, 04 §V.5, 06 H-43):
// support and platform_admin read by break-glass only (no reason: 403
// authz.break_glass_reason_required); every read is audited with the
// reason, break_glass and the subject (the student is told from it);
// other staff and students are 403; a student without a profile is 404.
func TestAdminGetStudentProfile_Audited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	st := e.student(t, e.uniA, false)
	e.basics(t, st, e.deptA, 2, t0.AddDate(2, 0, 0))
	reason := "Destek talebi 4711: profil görünmüyor"
	sup := staff(authz.PlatformSupport)
	_, err := e.svc.AdminGetStudentProfile(ctx, sup, st.UserID, students.AdminAccess{})
	wantCode(t, err, "authz.break_glass_reason_required")
	for _, a := range []authz.Actor{sup, staff(authz.PlatformAdmin)} {
		v, err := e.svc.AdminGetStudentProfile(ctx, a, st.UserID, students.AdminAccess{Reason: reason})
		if err != nil || v.View.Profile.UserID != st.UserID || v.ShowBirthDate {
			t.Fatalf("break-glass read = %+v, %v", v.ShowBirthDate, err)
		}
	}
	got := e.audit.Of(authz.StudentsProfileReadAdmin)
	if len(got) != 2 {
		t.Fatalf("admin reads audited = %d, want 2", len(got))
	}
	for _, ev := range got {
		if ev.Reason != reason || ev.Metadata[audit.MetaBreakGlass] != true || ev.Metadata[audit.MetaSubjectUserID] != st.UserID.String() ||
			*ev.ResourceID != st.UserID || ev.Outcome != audit.Success {
			t.Fatalf("audit = %+v", ev)
		}
	}
	for _, a := range []authz.Actor{staff(authz.PlatformModerator), staff(authz.PlatformFinanceAdmin), staff(authz.PlatformTalentAuditor), st} {
		if _, err := e.svc.AdminGetStudentProfile(ctx, a, st.UserID, students.AdminAccess{Reason: reason}); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("%v: %v, want 403", a, err)
		}
	}
	noProfile := e.student(t, e.uniA, false)
	if _, err := e.svc.AdminGetStudentProfile(ctx, sup, noProfile.UserID, students.AdminAccess{Reason: reason}); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("no profile: %v, want 404", err)
	}
	if len(e.audit.Of(authz.StudentsProfileReadAdmin)) != 2 {
		t.Fatal("refused reads were audited")
	}
}

// TestAdminGetStudentProfile_DPORequiresOpenRequest (03 k11): the dpo
// reads only the subject of an open data subject request whose ID is the
// reason (birth date shown, audited with the request); another student's
// request, a closed request or free text is 403.
func TestAdminGetStudentProfile_DPORequiresOpenRequest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	st, other := e.student(t, e.uniA, false), e.student(t, e.uniA, false)
	e.basics(t, st, e.deptA, 2, t0.AddDate(2, 0, 0))
	e.basics(t, other, e.deptA, 2, t0.AddDate(2, 0, 0))
	dsr, closed := uuid.NewV7(), uuid.NewV7()
	e.dsrs.open[dsr] = st.UserID
	dpo := staff(authz.PlatformDPO)
	for name, reason := range map[string]string{
		"free text":                 "KVKK başvurusu inceleniyor",
		"closed request":            closed.String(),
		"another student's request": dsr.String(),
	} {
		target := st.UserID
		if name == "another student's request" {
			target = other.UserID
		}
		if _, err := e.svc.AdminGetStudentProfile(ctx, dpo, target, students.AdminAccess{Reason: reason}); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("%s: %v, want 403", name, err)
		}
	}
	v, err := e.svc.AdminGetStudentProfile(ctx, dpo, st.UserID, students.AdminAccess{Reason: " " + dsr.String() + " "})
	if err != nil || !v.ShowBirthDate {
		t.Fatalf("dpo with the open request = %+v, %v", v.ShowBirthDate, err)
	}
	got := e.audit.Of(authz.StudentsProfileReadAdmin)
	if len(got) != 1 || got[0].Metadata["dsr_id"] != dsr.String() || got[0].Metadata[audit.MetaBreakGlass] != nil {
		t.Fatalf("dpo audit = %+v", got)
	}
	// A non-dpo never reads a request: support with the same UUID as reason
	// is break-glass and sees no birth date.
	sv, err := e.svc.AdminGetStudentProfile(ctx, staff(authz.PlatformSupport), st.UserID, students.AdminAccess{Reason: dsr.String()})
	if err != nil || sv.ShowBirthDate {
		t.Fatalf("support with a request ID = %+v, %v", sv.ShowBirthDate, err)
	}
}

// TestAdminDTO_Fields (04 §V.5): no CV, no sensitive club membership, no
// talent value, no password hash; the birth date only on the dpo path;
// a plain club membership is listed, a sensitive one never.
func TestAdminDTO_Fields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	st := e.student(t, e.uniA, false)
	e.basics(t, st, e.deptA, 2, t0.AddDate(2, 0, 0))
	if _, err := e.svc.CreateMyCV(ctx, st, e.files.upload(st.UserID, files.PurposeCV), true); err != nil {
		t.Fatal(err)
	}
	e.orgs.join(st.UserID, e.clubPlain, authz.MemberMember)
	e.orgs.join(st.UserID, e.clubSens, authz.MemberMember)
	unknownClub := uuid.NewV7()
	e.orgs.join(st.UserID, unknownClub, authz.MemberMember)
	at := t0.AddDate(0, -1, 0)
	e.regs = &regsFake{list: map[uuid.UUID][]students.RegistrationSummary{st.UserID: {{EventID: uuid.NewV7(), EventTitle: "Kariyer Günü",
		Status: "confirmed", RegisteredAt: at}}}}
	dsr := uuid.NewV7()
	e.dsrs.open[dsr] = st.UserID

	app := newHTTP(t, e)
	path := "/api/v1/admin/users/" + st.UserID.String() + "/student-profile"
	rec := e.callAs(t, app, staff(authz.PlatformSupport), http.MethodGet, path, "X-Koro-Access-Reason", "Destek talebi 4711: kayıt sorunu")
	if rec.Code != http.StatusOK {
		t.Fatalf("support = %d %s", rec.Code, rec.Body)
	}
	var d students.AdminStudentViewDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.BirthDate != "" || d.Email == "" || d.Profile.Department == nil || len(d.Registrations) != 1 || d.Student.Status != "verified" ||
		d.Student.University == nil || d.Student.University.Name != "Boğaziçi Üniversitesi" {
		t.Fatalf("support view = %+v", d)
	}
	if len(d.Memberships) != 1 || d.Memberships[0].OrgID != e.clubPlain.String() {
		t.Fatalf("memberships = %+v, want only the plain club", d.Memberships)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	for _, f := range presentFields(t, body) {
		if f == "cvs" || f == "password_hash" || f == "audience_keys" || slices.Contains([]string{"talent", "band", "score"}, f) ||
			(forbiddenRe.MatchString(f) && f != "birth_date" && f != "privacy.activity_ranking_choice") {
			t.Fatalf("admin view member %q", f)
		}
	}
	rec = e.callAs(t, app, staff(authz.PlatformDPO), http.MethodGet, path, "X-Koro-Access-Reason", dsr.String())
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &d) != nil || d.BirthDate != "2003-04-01" {
		t.Fatalf("dpo = %d %s", rec.Code, rec.Body)
	}
	for _, f := range jsonFields(reflect.TypeFor[students.AdminStudentViewDTO]()) {
		if f == "birth_date" || f == "privacy.activity_ranking_choice" { // the dpo's column; the choice, not a talent value
			continue
		}
		if forbiddenRe.MatchString(f) || f == "cvs" || f == "email_normalized" || f == "password_hash" {
			t.Fatalf("AdminStudentViewDTO member %q", f)
		}
	}
}
