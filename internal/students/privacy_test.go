package students_test

import (
	"errors"
	"slices"
	"testing"
	"uuid"

	"koro/internal/files"
	"koro/internal/students"
)

// TestPrivacyProvider_ExportAndErase (01 §3.3, 02 §9: hard delete with the
// account): Export returns the profile, settings and every section of the
// subject only; Erase deletes them and the avatar file in one transaction
// and is idempotent.
func TestPrivacyProvider_ExportAndErase(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a, other := e.student(t, e.uniA, false), e.student(t, e.uniA, false)
	avatar := e.files.upload(a.UserID, files.PurposeAvatar)
	if _, err := e.svc.UpdateMyProfile(ctx, a, students.ProfilePatch{Department: &e.deptA, Headline: ptr("Başlık"), Avatar: &avatar}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMySkills(ctx, a, []students.SkillInput{{SkillID: e.skill1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PutMyLanguages(ctx, a, []students.LanguageInput{{LanguageCode: "de", Level: "A2"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateMyExperience(ctx, a, students.ExperienceInput{Kind: students.KindVolunteer, Title: "Gönüllü",
		Description: "Açıklama", StartMonth: ym(2024, 3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{ShowClubsOnGrant: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateMyProfile(ctx, other, students.ProfilePatch{Headline: ptr("Başkası")}); err != nil {
		t.Fatal(err)
	}

	sec, err := e.mod.Privacy.Export(ctx, a.UserID)
	if err != nil || sec.Module != "students" || e.mod.Privacy.Name() != "students" {
		t.Fatalf("Export = %+v, %v", sec, err)
	}
	var records []string
	for _, it := range sec.Items {
		records = append(records, it["record"].(string))
		if it["headline"] == "Başkası" {
			t.Fatal("another user's data in the export")
		}
	}
	slices.Sort(records)
	if want := []string{"experience", "language", "privacy_settings", "skill", "student_profile"}; !slices.Equal(records, want) {
		t.Fatalf("export records = %v, want %v", records, want)
	}

	res, err := e.mod.Privacy.Erase(ctx, a.UserID)
	if err != nil || res.Deleted != 6 { // skill, language, experience, settings, profile, avatar
		t.Fatalf("Erase = %+v, %v", res, err)
	}
	if _, err := e.store.GetProfile(ctx, a.UserID); !errors.Is(err, students.ErrNotFound) {
		t.Fatal("profile survived the erasure")
	}
	if !slices.Equal(e.files.deleted, []uuid.UUID{avatar}) || e.files.deleteTx[0] == 0 {
		t.Fatalf("avatar deletion = %v (tx %v)", e.files.deleted, e.files.deleteTx)
	}
	if res, err := e.mod.Privacy.Erase(ctx, a.UserID); err != nil || res.Deleted != 0 {
		t.Fatalf("second Erase = %+v, %v", res, err)
	}
	if sec, err := e.mod.Privacy.Export(ctx, a.UserID); err != nil || len(sec.Items) != 0 {
		t.Fatalf("export after erasure = %+v, %v", sec, err)
	}
	if _, err := e.store.GetProfile(ctx, other.UserID); err != nil {
		t.Fatal("another user's profile was erased")
	}
}

// TestPrivacyProvider_AccessGrants (01 §3.3, 02 §9.7; BE-046): the export
// lists the student's level 3 grants, revoked ones included, with the
// revocation reason of whoever wrote it (the company's too since BE-088,
// Q-08); erasure deletes every grant (no company keeps access) and another
// student's stay.
func TestPrivacyProvider_AccessGrants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, recB, _ := e.company("Beta")
	s, req := e.granted(t, orgA)
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	other, _ := e.granted(t, orgA)
	mine, _ := e.svc.ListMyAccessGrants(ctx, s, nil, 0)
	for _, g := range mine.Items {
		if g.Grant.CompanyOrgID == orgA {
			if err := e.svc.RevokeMyAccessGrant(ctx, s, g.Grant.ID, "Kendi notum"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.svc.RevokeCompanyAccessGrant(ctx, recB, orgB, s.UserID, "Şirketin iç notu"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}

	sec, err := e.mod.Privacy.Export(ctx, s.UserID)
	if err != nil {
		t.Fatal(err)
	}
	var grants []map[string]any
	for _, it := range sec.Items {
		if it["record"] == "access_grant" {
			grants = append(grants, it)
		}
	}
	if len(grants) != 3 {
		t.Fatalf("exported grants = %v", grants)
	}
	first := grants[0]
	if first["company_org_id"] != orgA.String() || first["source_id"] != req.String() || first["revoked_by"] != "student" ||
		first["revoke_reason"] != "Kendi notum" || first["revoked_at"] == nil {
		t.Fatalf("the student's revoked grant = %v", first)
	}
	if grants[1]["revoked_by"] != "company" || grants[1]["revoke_reason"] != "Şirketin iç notu" {
		t.Fatalf("the company's reason is not in the export: %v", grants[1])
	}
	if grants[2]["revoked_at"] != nil {
		t.Fatalf("the active grant = %v", grants[2])
	}

	if _, err := e.mod.Privacy.Erase(ctx, s.UserID); err != nil {
		t.Fatal(err)
	}
	if all, _ := e.store.AllGrants(ctx, s.UserID); len(all) != 0 {
		t.Fatalf("grants survived the erasure: %+v", all)
	}
	if ok, _ := e.svc.Has(ctx, s.UserID, orgB); ok {
		t.Fatal("company B kept access after the erasure")
	}
	if ok, _ := e.svc.Has(ctx, other.UserID, orgA); !ok {
		t.Fatal("another student's grant was erased")
	}
	if _, err := e.svc.GetCompanyStudentProfile(ctx, recA, orgA, other.UserID); err != nil {
		t.Fatalf("another student's level 3 view: %v", err)
	}
}
