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
