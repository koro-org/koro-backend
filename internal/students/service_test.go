package students_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// TestUpdateProfile_UniversityImmutable: the university comes from the
// student verification (02 §9.1, 04 §2.4). The patch type has no
// university member at all (the HTTP layer rejects one: handler test), the
// first write takes the actor's verified university, and no patch moves
// it; a profile whose student verified at another university follows the
// verification and loses the department of the old one.
func TestUpdateProfile_UniversityImmutable(t *testing.T) {
	t.Parallel()
	if _, ok := reflect.TypeFor[students.ProfilePatch]().FieldByNameFunc(func(n string) bool {
		return strings.Contains(strings.ToLower(n), "university")
	}); ok {
		t.Fatal("ProfilePatch must not carry a university")
	}
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	v, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Department: &e.deptA, Headline: ptr("Merhaba")})
	if err != nil {
		t.Fatal(err)
	}
	if v.Profile.UniversityID != e.uniA || v.UniversityName != "Boğaziçi Üniversitesi" {
		t.Fatalf("university = %v %q", v.Profile.UniversityID, v.UniversityName)
	}
	// A department of another university cannot smuggle the university in.
	_, err = e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Department: &e.deptB})
	wantCode(t, err, "students.unknown_department")
	if p := e.profile(t, a.UserID); p.UniversityID != e.uniA || *p.DepartmentID != e.deptA {
		t.Fatalf("stored = %v / %v", p.UniversityID, p.DepartmentID)
	}
	// The student re-verifies at uni B: the next write follows the
	// verification (not the request) and drops the old department.
	e.users.verify(a.UserID, e.uniB, t0)
	b := e.uniB
	a.UniversityID = &b
	v, err = e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{City: ptr("Ankara")})
	if err != nil {
		t.Fatal(err)
	}
	if v.Profile.UniversityID != e.uniB || v.Profile.DepartmentID != nil {
		t.Fatalf("after re-verification = %v / %v", v.Profile.UniversityID, v.Profile.DepartmentID)
	}
	if len(e.audit.Of(authz.StudentsProfileUpdateOwn)) < 2 {
		t.Fatal("profile writes are not audited")
	}
}

// TestUpdateProfile_UnknownDepartment_422: a department that does not
// exist, is inactive, or belongs to another university is 422
// students.unknown_department naming body.department_id; nothing is
// written.
func TestUpdateProfile_UnknownDepartment_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	for name, id := range map[string]uuid.UUID{"missing": uuid.NewV7(), "inactive": e.deptA2, "other university": e.deptB} {
		_, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Department: &id})
		wantCode(t, err, "students.unknown_department")
		var pe *problem.Error
		if !errors.As(err, &pe) || pe.Status() != 422 || len(pe.Fields()) != 1 || pe.Fields()[0].Location != "body.department_id" {
			t.Fatalf("%s: %v (%v)", name, err, pe.Fields())
		}
	}
	if p, err := e.store.GetProfile(ctx, a.UserID); err == nil && p.DepartmentID != nil {
		t.Fatalf("department stored: %v", p.DepartmentID)
	}
}

// TestProfile_VerificationRequired_403: an account without an active
// student verification (usr, k-uv) gets 403 students.verification_required
// on every own-profile operation; nothing is written.
func TestProfile_VerificationRequired_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.unverified()
	checks := map[string]error{}
	_, checks["get"] = e.svc.GetMyProfile(ctx, u)
	_, checks["update"] = e.svc.UpdateMyProfile(ctx, u, students.ProfilePatch{Headline: ptr("x")})
	_, checks["skills"] = e.svc.PutMySkills(ctx, u, nil)
	_, checks["languages"] = e.svc.PutMyLanguages(ctx, u, nil)
	_, checks["experience"] = e.svc.CreateMyExperience(ctx, u, students.ExperienceInput{Kind: students.KindJob, Title: "x", StartMonth: ym(2025, 1)})
	_, checks["privacy"] = e.svc.GetMyPrivacy(ctx, u)
	_, checks["privacy update"] = e.svc.UpdateMyPrivacy(ctx, u, students.PrivacyPatch{ShowClubsOnGrant: ptr(true)})
	checks["delete"] = e.svc.DeleteMyExperience(ctx, u, uuid.NewV7())
	for name, err := range checks {
		if !errors.Is(err, students.ErrVerificationRequired) {
			t.Errorf("%s: %v, want students.verification_required", name, err)
		}
	}
	if _, err := e.store.GetProfile(ctx, u.UserID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("a profile was written for an unverified account: %v", err)
	}
}

// TestGetProfile_FreshAndFull: a verified student who never wrote the
// profile reads its starting point (nothing is written by the read); the
// full profile carries the sections and the private-by-default settings.
func TestGetProfile_FreshAndFull(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	v, err := e.svc.GetMyProfile(ctx, a)
	if err != nil || v.Stored || v.Profile.UniversityID != e.uniA || v.Privacy.Discoverable || v.Privacy.ShowContactOnGrant ||
		v.Privacy.ShowClubsOnGrant || v.Privacy.ActivityRankingChoice != students.RankingDefault {
		t.Fatalf("fresh view = %+v, %v", v, err)
	}
	if _, err := e.store.GetProfile(ctx, a.UserID); !errors.Is(err, students.ErrNotFound) {
		t.Fatal("a read wrote the profile")
	}
	if _, err := e.svc.PutMySkills(ctx, a, []students.SkillInput{{SkillID: e.skill1, Level: ptr[int16](4)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMyLanguages(ctx, a, []students.LanguageInput{{LanguageCode: "en", Level: "C1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateMyExperience(ctx, a, students.ExperienceInput{Kind: students.KindInternship, Title: "Stajyer",
		OrganizationName: "Acme", StartMonth: ym(2025, 7), EndMonth: ptr(ym(2025, 9))}); err != nil {
		t.Fatal(err)
	}
	v, err = e.svc.GetMyProfile(ctx, a)
	if err != nil || !v.Stored || len(v.Skills) != 1 || len(v.Languages) != 1 || len(v.Experiences) != 1 {
		t.Fatalf("full view = %+v, %v", v, err)
	}
	if got := []string(v.Profile.CompletedSections); !slices.Equal(got, []string{"skills", "experience", "languages"}) {
		t.Fatalf("completed sections = %v", got)
	}
}

// TestSections_CompletedEventsAndAudit: a section that becomes complete
// emits student.profile_section_completed (A) once; basics needs the
// department, the class year and the expected graduation.
func TestSections_CompletedEventsAndAudit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Department: &e.deptA, ClassYear: ptr[int16](3)}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.topics(), domainevent.TopicStudentProfileSectionCompleted) {
		t.Fatal("basics completed without the expected graduation")
	}
	g := ym(2027, 6)
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{ExpectedGraduation: &g}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMySkills(ctx, a, []students.SkillInput{{SkillID: e.skill1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMySkills(ctx, a, []students.SkillInput{{SkillID: e.skill1}, {SkillID: e.skill2}}); err != nil {
		t.Fatal(err)
	}
	var sections []string
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicStudentProfileSectionCompleted {
			p, err := domainevent.Decode[domainevent.StudentProfileSectionCompleted](ev)
			if err != nil || p.UserID != a.UserID {
				t.Fatalf("event = %+v, %v", p, err)
			}
			sections = append(sections, p.Section)
		}
	}
	if !slices.Equal(sections, []string{"basics", "skills"}) {
		t.Fatalf("completed events = %v", sections)
	}
	// Emptying the list un-completes the section.
	if _, err := e.svc.PutMySkills(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	if e.profile(t, a.UserID).Completed(students.SectionSkills) {
		t.Fatal("skills still complete with no skill")
	}
	if n := len(e.audit.Of(authz.StudentsProfileSectionManage)); n != 3 {
		t.Fatalf("section audit rows = %d", n)
	}
}

// TestSections_Validation: skills from the catalog only (≤ 30, no
// duplicates, level 1–5); languages ISO 639 + CEFR (no duplicates);
// experiences: kind, title, months in order.
func TestSections_Validation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	_, err := e.svc.PutMySkills(ctx, a, []students.SkillInput{{SkillID: uuid.NewV7()}})
	wantCode(t, err, "directory.unknown_skill")
	_, err = e.svc.PutMySkills(ctx, a, []students.SkillInput{{SkillID: e.skill1}, {SkillID: e.skill1}})
	wantCode(t, err, "validation_failed")
	_, err = e.svc.PutMySkills(ctx, a, []students.SkillInput{{SkillID: e.skill1, Level: ptr[int16](6)}})
	wantCode(t, err, "validation_failed")
	many := make([]students.SkillInput, 31)
	for i := range many {
		many[i] = students.SkillInput{SkillID: uuid.NewV7()}
	}
	_, err = e.svc.PutMySkills(ctx, a, many)
	wantCode(t, err, "validation_failed")
	for _, in := range [][]students.LanguageInput{
		{{LanguageCode: "EN", Level: "B2"}}, {{LanguageCode: "en", Level: "B9"}},
		{{LanguageCode: "en", Level: "B2"}, {LanguageCode: "en", Level: "C1"}},
	} {
		_, err := e.svc.PutMyLanguages(ctx, a, in)
		wantCode(t, err, "validation_failed")
	}
	for _, in := range []students.ExperienceInput{
		{Kind: "hobby", Title: "x", StartMonth: ym(2025, 1)},
		{Kind: students.KindJob, Title: "  ", StartMonth: ym(2025, 1)},
		{Kind: students.KindJob, Title: "x", StartMonth: ym(2025, 5), EndMonth: ptr(ym(2025, 4))},
		{Kind: students.KindJob, Title: "x\u202e", StartMonth: ym(2025, 1)},
	} {
		_, err := e.svc.CreateMyExperience(ctx, a, in)
		wantCode(t, err, "validation_failed")
	}
	_, err = e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{})
	wantCode(t, err, "students.empty_patch")
	_, err = e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{ClassYear: ptr[int16](9)})
	wantCode(t, err, "validation_failed")
	_, err = e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Headline: ptr(strings.Repeat("a", 161))})
	wantCode(t, err, "validation_failed")
}

// TestExperience_OtherUsersEntryIs404 (IDOR): one student's experience ID
// is not found for another student, for update and delete alike; the
// owner's entry is untouched.
func TestExperience_OtherUsersEntryIs404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	owner, other := e.student(t, e.uniA, false), e.student(t, e.uniA, false)
	x, err := e.svc.CreateMyExperience(ctx, owner, students.ExperienceInput{Kind: students.KindJob, Title: "Garson", StartMonth: ym(2024, 6)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateMyExperience(ctx, other, x.ID, students.ExperiencePatch{Title: ptr("Ele geçirildi")}); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("update of another user's entry: %v", err)
	}
	if err := e.svc.DeleteMyExperience(ctx, other, x.ID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("delete of another user's entry: %v", err)
	}
	got, err := e.svc.UpdateMyExperience(ctx, owner, x.ID, students.ExperiencePatch{Title: ptr("Kasiyer"), ClearEndMonth: true})
	if err != nil || got.Title != "Kasiyer" {
		t.Fatalf("owner update = %+v, %v", got, err)
	}
	if err := e.svc.DeleteMyExperience(ctx, owner, x.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteMyExperience(ctx, owner, x.ID); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if e.profile(t, owner.UserID).Completed(students.SectionExperience) {
		t.Fatal("experience section complete after the last entry went")
	}
}

// TestAvatar_OwnUploadOnlyAndReplacedFileDeleted: only an avatar upload of
// the caller attaches; a replaced avatar is deleted in the same
// transaction; the view signs a short-lived URL.
func TestAvatar_OwnUploadOnlyAndReplacedFileDeleted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a, other := e.student(t, e.uniA, false), e.student(t, e.uniA, false)
	foreign := e.files.upload(other.UserID, files.PurposeAvatar)
	cv := e.files.upload(a.UserID, files.PurposeCV)
	for _, id := range []uuid.UUID{foreign, cv, uuid.NewV7()} {
		_, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Avatar: &id})
		wantCode(t, err, "validation_failed")
	}
	first := e.files.upload(a.UserID, files.PurposeAvatar)
	v, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Avatar: &first})
	if err != nil || v.AvatarURL == "" || e.files.attached[first].Module != "students" {
		t.Fatalf("avatar = %q, %v", v.AvatarURL, err)
	}
	second := e.files.upload(a.UserID, files.PurposeAvatar)
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Avatar: &second}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.files.deleted, []uuid.UUID{first}) || e.files.deleteTx[0] == 0 {
		t.Fatalf("deleted %v in tx %v", e.files.deleted, e.files.deleteTx)
	}
	urls, err := e.svc.AvatarURLs(ctx, []uuid.UUID{a.UserID, other.UserID})
	if err != nil || len(urls) != 1 || urls[a.UserID] == "" {
		t.Fatalf("AvatarURLs = %v, %v", urls, err)
	}
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{ClearAvatar: true}); err != nil {
		t.Fatal(err)
	}
	if e.profile(t, a.UserID).AvatarFileID != nil || len(e.files.deleted) != 2 {
		t.Fatalf("clear avatar: deleted %v", e.files.deleted)
	}
}

// TestCreateExperience_IdempotencyKeyReplays: the same key and body
// returns the first result without a second row (06 H-5).
func TestCreateExperience_IdempotencyKeyReplays(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.student(t, e.uniA, false)
	in := students.ExperienceInput{Kind: students.KindProject, Title: "Robot", StartMonth: ym(2025, 2)}
	ctx := idemCtx(t, "exp-1")
	first, err := e.svc.CreateMyExperience(ctx, a, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CreateMyExperience(idemCtx(t, "exp-1"), a, in)
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay = %v, %v (first %v)", again.ID, err, first.ID)
	}
	rows, _ := e.store.Experiences(t.Context(), a.UserID)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
}

// idemCtx carries an Idempotency-Key as the middleware would.
func idemCtx(t *testing.T, key string) context.Context {
	t.Helper()
	return idempotency.WithKey(t.Context(), idempotency.Key{Scope: "test:POST /me/student-profile/experiences", Value: key, Status: 201})
}
