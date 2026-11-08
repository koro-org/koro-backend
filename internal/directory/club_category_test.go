package directory_test

import (
	"maps"
	"slices"
	"testing"
	"uuid"

	"koro/internal/directory"
)

// TestCategory_Targetable (BE-207, Q-58): the categories an audience may
// name are the club categories without the sensitive ones and other, in
// 02 order; an unknown value is not targetable.
func TestCategory_Targetable(t *testing.T) {
	t.Parallel()
	want := []directory.Category{
		directory.CategoryAcademic, directory.CategoryTechnology, directory.CategoryCareer, directory.CategoryEntrepreneurship,
		directory.CategoryArts, directory.CategoryCulture, directory.CategorySports, directory.CategorySocialResponsibility,
		directory.CategoryHobby,
	}
	if got := directory.TargetableCategories(); !slices.Equal(got, want) {
		t.Fatalf("TargetableCategories = %v, want %v", got, want)
	}
	for _, c := range []directory.Category{directory.CategoryReligious, directory.CategoryPolitical, directory.CategoryIdentity,
		directory.CategoryOther, "", "astrology", "Technology"} {
		if c.Targetable() {
			t.Errorf("%q is targetable", c)
		}
	}
}

// TestTargetableClubCategories_NeverSensitive (BE-207, 03 Y2): the category
// of each known club an audience may reach through it; a sensitive club
// (whatever its category), a club of category other or of a sensitive
// category and an unknown ID have no key.
func TestTargetableClubCategories_NeverSensitive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-K", "Kırklareli Üniversitesi", "Kırklareli")
	robot := e.club(t, uni, "Robotik Kulübü", directory.CategoryTechnology)
	run := e.club(t, uni, "Koşu Kulübü", directory.CategorySports)
	misc := e.club(t, uni, "Çeşitli Kulübü", directory.CategoryOther)
	faith := e.club(t, uni, "İnanç Topluluğu", directory.CategoryReligious)
	health, err := e.svc.AdminCreateClub(ctx, pad(t), directory.ClubInput{UniversityID: uni.ID, Name: "Destek Grubu",
		Category: directory.CategorySports, JoinPolicy: directory.JoinApproval, IsSensitive: true, SensitiveReason: "health"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.TargetableClubCategories(ctx, []uuid.UUID{robot.Club.OrgID, run.Club.OrgID, misc.Club.OrgID,
		faith.Club.OrgID, health.Club.OrgID, uuid.NewV7()})
	want := map[uuid.UUID]directory.Category{robot.Club.OrgID: directory.CategoryTechnology, run.Club.OrgID: directory.CategorySports}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("TargetableClubCategories = %v, %v; want %v", got, err, want)
	}
	if got, err := e.svc.TargetableClubCategories(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("no clubs = %v, %v", got, err)
	}
}
