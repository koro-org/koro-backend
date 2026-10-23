package students_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// company names a company org and returns its recruiter (cmm, no MFA) and
// its company_admin (cma, MFA-verified session).
func (e *env) company(name string) (org uuid.UUID, recruiter, admin authz.Actor) {
	org = uuid.NewV7()
	e.orgNames.name(org, name)
	member := func(role authz.MemberRole, mfa bool) authz.Actor {
		return authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentNone, MFAVerified: mfa,
			Memberships: []authz.Membership{{OrgID: org, OrgKind: authz.OrgCompany, Role: role}}}
	}
	return org, member(authz.MemberRecruiter, false), member(authz.MemberCompanyAdmin, true)
}

// profiled is an adult verified student with a stored profile.
func (e *env) profiled(t *testing.T) authz.Actor {
	t.Helper()
	a := e.student(t, e.uniA, false)
	if _, err := e.svc.UpdateMyProfile(t.Context(), a, students.ProfilePatch{Department: &e.deptA, ClassYear: ptr[int16](3),
		Headline: ptr("Yazılım öğrencisi")}); err != nil {
		t.Fatal(err)
	}
	return a
}

// granted is a profiled student who accepted a contact request of org; it
// returns the student and the request ID.
func (e *env) granted(t *testing.T, org uuid.UUID) (authz.Actor, uuid.UUID) {
	t.Helper()
	a := e.profiled(t)
	req := uuid.NewV7()
	if err := e.svc.Grant(t.Context(), a.UserID, org, students.SourceContactRequest, req); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	return a, req
}

// withCV gives a its current CV and returns the CV's file.
func (e *env) withCV(t *testing.T, a authz.Actor) uuid.UUID {
	t.Helper()
	f := e.files.upload(a.UserID, files.PurposeCV)
	if _, err := e.svc.CreateMyCV(t.Context(), a, f, true); err != nil {
		t.Fatal(err)
	}
	return f
}

func staffActor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("no persona %s", name)
	}
	return p.Actor
}

// TestCompanyProfile_NoGrant_404 (03 Y5, G; 04 §2.4 "İzin yok → 404"): a
// company without an active grant of its own gets 404 for the profile and
// the CV, exactly as for an unknown student, malformed or nil IDs; another
// company's grant does not count; a minor, an account that is no longer
// active or a student without a profile is not found even with a grant;
// platform staff see the company's surface and get 403 whatever the grant
// (no existence leak); no refused read is audited.
func TestCompanyProfile_NoGrant_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, admA := e.company("Acme")
	orgB, recB, _ := e.company("Beta")
	s := e.profiled(t)
	e.withCV(t, s)

	notFound := func(name string, a authz.Actor, org, user uuid.UUID) {
		t.Helper()
		if _, err := e.svc.GetCompanyStudentProfile(ctx, a, org, user); !errors.Is(err, problem.ErrNotFound) {
			t.Errorf("%s: profile %v, want 404", name, err)
		}
		if _, err := e.svc.DownloadCompanyStudentCV(ctx, a, org, user); !errors.Is(err, problem.ErrNotFound) {
			t.Errorf("%s: cv %v, want 404", name, err)
		}
	}
	notFound("recruiter without a grant", recA, orgA, s.UserID)
	notFound("admin without a grant", admA, orgA, s.UserID)
	notFound("unknown student", recA, orgA, uuid.NewV7())
	notFound("nil student", recA, orgA, uuid.Nil())

	// Company B's grant gives company A nothing (and B only its own path).
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	notFound("A with B's grant", recA, orgA, s.UserID)
	notFound("B on A's path", recB, orgA, s.UserID)
	if _, err := e.svc.GetCompanyStudentProfile(ctx, recB, orgB, s.UserID); err != nil {
		t.Fatalf("B's own grant: %v", err)
	}

	// Staff: the surface is visible, the action is not theirs (403), with or
	// without a grant.
	for _, name := range []string{"platform_admin", "support", "dpo"} {
		for _, org := range []uuid.UUID{orgA, orgB} {
			if _, err := e.svc.GetCompanyStudentProfile(ctx, staffActor(t, name), org, s.UserID); !errors.Is(err, authz.ErrForbidden) {
				t.Errorf("%s on %v: %v, want 403", name, org, err)
			}
		}
	}

	// With a grant: a minor (birth date corrected), an inactive account and a
	// missing profile are not found.
	m, _ := e.granted(t, orgA)
	e.users.setMinor(m.UserID, true)
	notFound("minor with a grant", recA, orgA, m.UserID)
	susp, _ := e.granted(t, orgA)
	e.users.mu.Lock()
	sum := e.users.sum[susp.UserID]
	sum.Status = identity.StatusSuspended
	e.users.sum[susp.UserID] = sum
	e.users.mu.Unlock()
	notFound("suspended account with a grant", recA, orgA, susp.UserID)
	bare := e.student(t, e.uniA, false)
	if err := e.svc.Grant(ctx, bare.UserID, orgA, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	notFound("no profile with a grant", recA, orgA, bare.UserID)

	if n := len(e.audit.of(string(authz.StudentsProfileReadCompany))); n != 1 { // B's single successful read
		t.Fatalf("profile read audit rows = %d, want 1", n)
	}
	if n := len(e.audit.of(string(authz.StudentsCvReadCompany))); n != 0 {
		t.Fatalf("refused CV downloads were audited: %d", n)
	}
}

// TestCompanyProfile_L3DTOFieldsOnly (04 §V.3, §V.0-2): with every sharing
// toggle on the company receives exactly the level 3 allow-list — the
// non-sensitive club only, the club_role entry, the e-mail — and never the
// birth date, minor flag, edu e-mail, privacy settings, audience keys or a
// sensitive club; the e-mail needs the contact_info_share consent in force
// at read time (T-237); toggles off drop clubs, club_role entries and the
// contact.
func TestCompanyProfile_L3DTOFieldsOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, rec, _ := e.company("Acme")
	s, req := e.granted(t, org)
	e.withCV(t, s)
	if _, err := e.svc.PutMySkills(ctx, s, []students.SkillInput{{SkillID: e.skill1, Level: ptr[int16](4)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMyLanguages(ctx, s, []students.LanguageInput{{LanguageCode: "en", Level: "C1"}}); err != nil {
		t.Fatal(err)
	}
	for _, in := range []students.ExperienceInput{
		{Kind: students.KindInternship, Title: "Stajyer", OrganizationName: "Acme Ltd", StartMonth: ym(2025, 7), Description: "Go servisleri"},
		{Kind: students.KindClubRole, Title: "Başkan", OrganizationName: "İnanç Topluluğu", StartMonth: ym(2024, 1)},
	} {
		if _, err := e.svc.CreateMyExperience(ctx, s, in); err != nil {
			t.Fatal(err)
		}
	}
	e.orgs.join(s.UserID, e.clubPlain, authz.MemberMember)
	e.orgs.join(s.UserID, e.clubSens, authz.MemberClubAdmin)
	if _, err := e.svc.UpdateMyPrivacy(ctx, s, students.PrivacyPatch{ShowClubsOnGrant: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	// The e-mail is shared with this company at the acceptance (BE-089).
	if err := e.svc.ShareContactWithCompany(ctx, s, org, *proof(), consent.ChannelWeb, req); err != nil {
		t.Fatal(err)
	}

	d, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID)
	if err != nil {
		t.Fatal(err)
	}
	all := []string{
		"user_id", "first_name", "last_name", "university", "university.university_id", "university.name",
		"department", "department.department_id", "department.name", "degree_level", "class_year", "headline",
		"skills", "skills[].skill_id", "skills[].name", "skills[].level", "languages", "languages[].language_code",
		"languages[].level", "experiences", "experiences[].kind", "experiences[].title", "experiences[].organization_name",
		"experiences[].start_month", "experiences[].description", "clubs", "clubs[].org_id", "clubs[].name", "clubs[].role",
		"cv", "cv.available", "cv.uploaded_at", "contact", "contact.email", "grant", "grant.granted_at", "grant.source",
		"grant.contact_request_id",
	}
	wantFields(t, "level 3 answer", presentFields(t, d), all)
	for _, f := range presentFields(t, d) {
		if !slices.Contains(jsonFields(reflect.TypeFor[students.StudentProfileL3DTO]()), f) {
			t.Errorf("member %q outside the StudentProfileL3DTO allow-list", f)
		}
	}
	if len(d.Clubs) != 1 || d.Clubs[0].Name != "Satranç Kulübü" || d.Clubs[0].Role != "member" || len(d.Experiences) != 2 ||
		d.Contact == nil || !strings.HasSuffix(d.Contact.Email, "@example.test") || !d.CV.Available ||
		d.Grant.ContactRequestID != req.String() || d.Skills[0].Name != "Go" || d.University.Name != "Boğaziçi Üniversitesi" {
		t.Fatalf("level 3 = %+v", d)
	}
	b, _ := json.Marshal(d)
	for _, leak := range []string{e.clubSens.String(), "club_admin", "birth", "is_minor", "edu_email", "privacy",
		"discoverable", "audience", "talent", "show_"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("level 3 leaks %q: %s", leak, b)
		}
	}

	// The contact consent withdrawn through the consent module (the setting
	// still on): no e-mail at the next read.
	e.consent.mu.Lock()
	e.consent.removed[s.UserID] = true
	e.consent.mu.Unlock()
	if d, err = e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); err != nil || d.Contact != nil {
		t.Fatalf("contact without the consent in force: %+v, %v", d.Contact, err)
	}
	// Toggles off: no clubs, no club_role entry, no contact.
	if _, err := e.svc.UpdateMyPrivacy(ctx, s, students.PrivacyPatch{ShowClubsOnGrant: ptr(false), ShowContactOnGrant: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if d, err = e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); err != nil {
		t.Fatal(err)
	}
	if d.Clubs != nil || d.Contact != nil || len(d.Experiences) != 1 || d.Experiences[0].Kind != "internship" {
		t.Fatalf("toggles off = %+v", d)
	}
}

// TestCompanyCv_EveryReadAudited (03 §6.5, 02 §9.6): every CV download of a
// company writes students.cv.read_company with the company, the student
// and the CV (no URL without its record: a failing audit fails the call);
// the URL lives 1 minute; every profile read is audited too; a student
// without a current CV is 404 and nothing is audited.
func TestCompanyCv_EveryReadAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, rec, adm := e.company("Acme")
	s, _ := e.granted(t, org)

	if _, err := e.svc.DownloadCompanyStudentCV(ctx, rec, org, s.UserID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("no current CV: %v, want 404", err)
	}
	file := e.withCV(t, s)
	for _, a := range []authz.Actor{rec, adm, rec} {
		dl, err := e.svc.DownloadCompanyStudentCV(ctx, a, org, s.UserID)
		if err != nil || !strings.Contains(dl.URL, file.String()) {
			t.Fatalf("download = %+v, %v", dl, err)
		}
	}
	e.files.mu.Lock()
	ttls := slices.Clone(e.files.ttls)
	e.files.mu.Unlock()
	if ttls[len(ttls)-1] != time.Minute {
		t.Fatalf("company CV URL lifetime = %v, want 1m", ttls[len(ttls)-1])
	}
	rows := e.audit.of(string(authz.StudentsCvReadCompany))
	if len(rows) != 3 {
		t.Fatalf("CV read audit rows = %d, want 3", len(rows))
	}
	for i, ev := range rows {
		actor := []authz.Actor{rec, adm, rec}[i]
		if ev.OrgID == nil || *ev.OrgID != org || ev.ActorOrgID == nil || *ev.ActorOrgID != org ||
			ev.ActorUserID == nil || *ev.ActorUserID != actor.UserID || ev.ResourceType != authz.TypeStudentCV ||
			ev.ResourceID == nil || *ev.ResourceID != s.UserID || ev.Metadata[audit.MetaSubjectUserID] != s.UserID.String() ||
			ev.Metadata["file_id"] != file.String() || ev.Metadata["cv_id"] == nil || ev.Metadata["grant_id"] == nil {
			t.Fatalf("audit row %d = %+v", i, ev)
		}
	}
	if _, err := e.svc.GetCompanyStudentProfile(ctx, adm, org, s.UserID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.audit.of(string(authz.StudentsProfileReadCompany))); n != 1 {
		t.Fatalf("profile read audit rows = %d", n)
	}
	// No record, no URL.
	e.audit.setFail(errors.New("audit down"))
	if dl, err := e.svc.DownloadCompanyStudentCV(ctx, rec, org, s.UserID); err == nil || dl.URL != "" {
		t.Fatalf("download without its audit record = %+v, %v", dl, err)
	}
	if d, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); err == nil || d.UserID != "" {
		t.Fatalf("profile without its audit record = %+v, %v", d, err)
	}
}

// TestRevokeGrant_ByStudent_CompanyLosesAccess (04 §2.4
// revokeMyAccessGrant): the student lists the company, revokes the grant
// (reason optional, stored, never shown) and the company's very next
// profile read, CV download and list miss the student; Has turns false;
// the revocation is audited; revoking again changes nothing; another
// student's grant is 404, for an unverified account too (BE-077: the /me
// path needs no verification). The old request never grants again; a new
// accepted request does.
func TestRevokeGrant_ByStudent_CompanyLosesAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, rec, _ := e.company("Acme")
	s, req := e.granted(t, org)
	e.withCV(t, s)
	if _, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); err != nil {
		t.Fatal(err)
	}
	mine, err := e.svc.ListMyAccessGrants(ctx, s, nil, 0)
	if err != nil || len(mine.Items) != 1 || mine.Items[0].CompanyName != "Acme" || mine.Items[0].Grant.CompanyOrgID != org {
		t.Fatalf("my grants = %+v, %v", mine, err)
	}
	grant := mine.Items[0].Grant.ID

	other := e.profiled(t)
	if err := e.svc.RevokeMyAccessGrant(ctx, other, grant, ""); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("another student's grant: %v, want 404", err)
	}
	if err := e.svc.RevokeMyAccessGrant(ctx, e.unverified(), grant, ""); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("unverified, another student's grant: %v, want 404", err)
	}
	if err := e.svc.RevokeMyAccessGrant(ctx, s, grant, strings.Repeat("x", 301)); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("a 301-character reason: %v, want 422", err)
	}

	if err := e.svc.RevokeMyAccessGrant(ctx, s, grant, "Artık ilgilenmiyorum"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("profile after the revocation: %v, want 404", err)
	}
	if _, err := e.svc.DownloadCompanyStudentCV(ctx, rec, org, s.UserID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("CV after the revocation: %v, want 404", err)
	}
	if p, err := e.svc.ListCompanyStudents(ctx, rec, org, students.CompanyStudentQuery{}); err != nil || len(p.Items) != 0 {
		t.Fatalf("company list after the revocation = %+v, %v", p, err)
	}
	if ok, err := e.svc.Has(ctx, s.UserID, org); err != nil || ok {
		t.Fatalf("Has after the revocation = %v, %v", ok, err)
	}
	if mine, _ = e.svc.ListMyAccessGrants(ctx, s, nil, 0); len(mine.Items) != 0 {
		t.Fatalf("my grants after the revocation = %+v", mine.Items)
	}
	g, err := e.store.GetGrant(ctx, s.UserID, grant)
	if err != nil || g.Active() || *g.RevokedByKind != students.RevokedByStudent || *g.RevokeReason != "Artık ilgilenmiyorum" {
		t.Fatalf("stored grant = %+v, %v", g, err)
	}
	rows := e.audit.of(string(authz.StudentsAccessGrantRevoke))
	if len(rows) != 1 || *rows[0].ActorUserID != s.UserID || rows[0].Metadata["revoked_by"] != "student" ||
		*rows[0].ResourceID != grant || *rows[0].OrgID != org || rows[0].Reason != "" {
		t.Fatalf("revocation audit = %+v", rows)
	}
	for _, ev := range rows {
		if b, _ := json.Marshal(ev.Metadata); strings.Contains(string(b), "ilgilenmiyorum") {
			t.Fatal("the reason reached the audit metadata")
		}
	}
	if err := e.svc.RevokeMyAccessGrant(ctx, s, grant, ""); err != nil {
		t.Fatalf("revoking again: %v", err)
	}
	if n := len(e.audit.of(string(authz.StudentsAccessGrantRevoke))); n != 1 {
		t.Fatalf("a second revocation was audited: %d", n)
	}

	// The accepted request does not grant again; a new one does.
	if err := e.svc.Grant(ctx, s.UserID, org, students.SourceContactRequest, req); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.Has(ctx, s.UserID, org); ok {
		t.Fatal("the revoked request granted again")
	}
	if err := e.svc.Grant(ctx, s.UserID, org, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); err != nil {
		t.Fatalf("after a new acceptance: %v", err)
	}
}

// TestRevokeGrant_ByCompany: the company gives up its access (recruiter or
// admin); the student no longer lists it; without an active grant 404;
// another company 404 and staff 403 before anything is read; the record
// carries the company.
func TestRevokeGrant_ByCompany(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, _, admB := e.company("Beta")
	s, _ := e.granted(t, orgA)
	if err := e.svc.RevokeCompanyAccessGrant(ctx, admB, orgA, s.UserID, ""); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("another company: %v", err)
	}
	if err := e.svc.RevokeCompanyAccessGrant(ctx, admB, orgB, s.UserID, ""); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("no grant of its own: %v", err)
	}
	if err := e.svc.RevokeCompanyAccessGrant(ctx, staffActor(t, "platform_admin"), orgA, s.UserID, ""); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("staff: %v", err)
	}
	if err := e.svc.RevokeCompanyAccessGrant(ctx, recA, orgA, s.UserID, "Pozisyon kapandı"); err != nil {
		t.Fatal(err)
	}
	if mine, _ := e.svc.ListMyAccessGrants(ctx, s, nil, 0); len(mine.Items) != 0 {
		t.Fatalf("my grants = %+v", mine.Items)
	}
	if err := e.svc.RevokeCompanyAccessGrant(ctx, recA, orgA, s.UserID, ""); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("revoking a revoked grant: %v, want 404", err)
	}
	rows := e.audit.of(string(authz.StudentsAccessGrantRevoke))
	if len(rows) != 1 || rows[0].Metadata["revoked_by"] != "company" || *rows[0].ActorOrgID != orgA || *rows[0].ActorUserID != recA.UserID {
		t.Fatalf("revocation audit = %+v", rows)
	}
	all, _ := e.store.AllGrants(ctx, s.UserID)
	if len(all) != 1 || *all[0].RevokedByKind != students.RevokedByCompany {
		t.Fatalf("stored = %+v", all)
	}
}

// TestGrant_Rules (01 §4.9 AccessGrants.Grant, 04 §V.0-3): a replay of the
// same request and a second request of a company that already holds
// access change nothing; a request ID reused for another company is a
// conflict; a minor is 422 students.minor_restricted and an unknown
// account 404; the grant and its audit record are written in the caller's
// transaction, the student as the actor. Revoke (system) ends the grant of
// its source only; Has follows.
func TestGrant_Rules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, _, _ := e.company("Acme")
	other, _, _ := e.company("Beta")
	s, req := e.granted(t, org)
	for _, src := range []uuid.UUID{req, uuid.NewV7()} {
		if err := e.svc.Grant(ctx, s.UserID, org, students.SourceContactRequest, src); err != nil {
			t.Fatal(err)
		}
	}
	if all, _ := e.store.AllGrants(ctx, s.UserID); len(all) != 1 {
		t.Fatalf("grants = %d, want 1", len(all))
	}
	if err := e.svc.Grant(ctx, s.UserID, other, students.SourceContactRequest, req); !errors.Is(err, problem.ErrConflict) {
		t.Fatalf("a request reused for another company: %v, want 409", err)
	}
	m := e.student(t, e.uniA, true)
	if err := e.svc.Grant(ctx, m.UserID, org, students.SourceContactRequest, uuid.NewV7()); !errors.Is(err, students.ErrMinorRestricted) {
		t.Fatalf("minor: %v", err)
	}
	if err := e.svc.Grant(ctx, uuid.NewV7(), org, students.SourceContactRequest, uuid.NewV7()); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if err := e.svc.Grant(ctx, s.UserID, org, "advert", uuid.NewV7()); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("unknown source: %v", err)
	}
	rows := e.audit.of("students.access_grant.create")
	if len(rows) != 1 || rows[0].ActorUserID == nil || *rows[0].ActorUserID != s.UserID || *rows[0].OrgID != org ||
		rows[0].Metadata["source_id"] != req.String() {
		t.Fatalf("grant audit = %+v", rows)
	}

	// Revoke by the system: another source is no-op; the grant's own source
	// ends it.
	if err := e.svc.Revoke(ctx, s.UserID, org, students.SourceContactRequest, uuid.NewV7(), "x"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.Has(ctx, s.UserID, org); !ok {
		t.Fatal("an unrelated source revoked the grant")
	}
	if err := e.svc.Revoke(ctx, s.UserID, org, students.SourceContactRequest, req, "Talep geri çekildi"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.Has(ctx, s.UserID, org); ok {
		t.Fatal("the grant survived its source's revocation")
	}
	sys := e.audit.of(string(authz.StudentsAccessGrantRevoke))
	if len(sys) != 1 || sys[0].SystemJob != "students.access_grants" || sys[0].Metadata["revoked_by"] != "system" {
		t.Fatalf("system revocation audit = %+v", sys)
	}
}

// TestListGrants_PagesAndNames: both lists are newest first with keyset
// pages; the company's cards fold the name filter (Turkish letters) and
// carry level 3 card members only; staff 403, another company 404.
func TestListGrants_PagesAndNames(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, rec, _ := e.company("Acme")
	var ids []uuid.UUID
	for i := range 3 {
		s, _ := e.granted(t, org)
		e.users.mu.Lock()
		sum := e.users.sum[s.UserID]
		sum.FirstName = []string{"Çağla", "Işık", "Ömer"}[i]
		e.users.sum[s.UserID] = sum
		e.users.mu.Unlock()
		ids = append(ids, s.UserID)
		e.clk.Advance(time.Minute)
	}
	p, err := e.svc.ListCompanyStudents(ctx, rec, org, students.CompanyStudentQuery{Limit: 2})
	if err != nil || len(p.Items) != 2 || p.Next == nil || p.Items[0].UserID != ids[2].String() || p.Items[1].UserID != ids[1].String() {
		t.Fatalf("page 1 = %+v, %v", p, err)
	}
	p, err = e.svc.ListCompanyStudents(ctx, rec, org, students.CompanyStudentQuery{Limit: 2, After: p.Next})
	if err != nil || len(p.Items) != 1 || p.Next != nil || p.Items[0].UserID != ids[0].String() {
		t.Fatalf("page 2 = %+v, %v", p, err)
	}
	p, err = e.svc.ListCompanyStudents(ctx, rec, org, students.CompanyStudentQuery{Q: "IŞIK"})
	if err != nil || len(p.Items) != 1 || p.Items[0].FirstName != "Işık" || p.Items[0].University.Name != "Boğaziçi Üniversitesi" {
		t.Fatalf("name filter = %+v, %v", p, err)
	}
	if _, err := e.svc.ListCompanyStudents(ctx, staffActor(t, "support"), org, students.CompanyStudentQuery{}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("staff: %v", err)
	}
	_, _, admB := e.company("Beta")
	if _, err := e.svc.ListCompanyStudents(ctx, admB, org, students.CompanyStudentQuery{}); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("another company: %v", err)
	}

	// The student's list: three companies, newest first, two per page.
	s := e.profiled(t)
	for _, name := range []string{"A1", "A2", "A3"} {
		o, _, _ := e.company(name)
		if err := e.svc.Grant(ctx, s.UserID, o, students.SourceContactRequest, uuid.NewV7()); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Second)
	}
	mine, err := e.svc.ListMyAccessGrants(ctx, s, nil, 2)
	if err != nil || len(mine.Items) != 2 || mine.Next == nil || mine.Items[0].CompanyName != "A3" {
		t.Fatalf("my page 1 = %+v, %v", mine, err)
	}
	mine, err = e.svc.ListMyAccessGrants(ctx, s, mine.Next, 2)
	if err != nil || len(mine.Items) != 1 || mine.Next != nil || mine.Items[0].CompanyName != "A1" {
		t.Fatalf("my page 2 = %+v, %v", mine, err)
	}
	if p, err := e.svc.ListMyAccessGrants(ctx, rec, nil, 0); err != nil || len(p.Items) != 0 {
		t.Fatalf("a company account's own grants (BE-077: no verification needed) = %+v, %v", p, err)
	}
}

// TestDTO_NoTalentFields (01 §0-7, 03 §0, 04 §V.0-2): no type a company or
// a student receives from the BE-046 operations carries a talent value (no
// talent, band, score, point or rank member) or another forbidden member,
// and the level 2 carrier AnonProfile never marshals (it holds the user
// ID).
func TestDTO_NoTalentFields(t *testing.T) {
	t.Parallel()
	talent := regexp.MustCompile(`(?i)talent|band|score|point|rank`)
	for _, typ := range []reflect.Type{
		reflect.TypeFor[students.StudentAccessGrantList](), reflect.TypeFor[students.StudentL3CardList](),
		reflect.TypeFor[students.StudentProfileL3DTO](), reflect.TypeFor[students.StudentL3CardDTO](),
		reflect.TypeFor[students.AnonProfileDTO](), reflect.TypeFor[students.StudentCVDownloadDTO](),
	} {
		for _, f := range jsonFields(typ) {
			if forbiddenRe.MatchString(f) || talent.MatchString(f) {
				t.Errorf("%s member %q is forbidden", typ.Name(), f)
			}
		}
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[students.AnonProfile](), reflect.TypeFor[students.DiscoveryPage](),
		reflect.TypeFor[students.DiscoveryQuery](), reflect.TypeFor[students.GrantView](), reflect.TypeFor[students.AccessGrant]()} {
		for i := range typ.NumField() {
			if talent.MatchString(typ.Field(i).Name) {
				t.Errorf("%s.%s names a talent value", typ.Name(), typ.Field(i).Name)
			}
		}
	}
	if _, err := json.Marshal(students.AnonProfile{UserID: uuid.NewV7()}); err == nil {
		t.Fatal("an AnonProfile marshalled")
	}
	if _, err := json.Marshal(students.DiscoveryPage{}); err == nil {
		t.Fatal("a DiscoveryPage marshalled")
	}
}
