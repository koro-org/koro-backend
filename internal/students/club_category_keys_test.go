package students_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/platform/domainevent"
	"koro/internal/students"
)

// categoryKeysIn returns the club category keys among keys.
func categoryKeysIn(keys []string) []string {
	var out []string
	for _, k := range keys {
		if strings.HasPrefix(k, "club_category:") {
			out = append(out, k)
		}
	}
	return out
}

// TestClubCategoryKeys_NonSensitiveClubsOnly (BE-207, Q-58, 03 Y2): the
// stored audience holds one club_category key per targetable category of
// the student's non-sensitive clubs; a sensitive club gives none whatever
// its category, nor does a club of category other. The membership event,
// the nightly refresh and the viewer keys agree; a student whose only club
// is sensitive holds no category key.
func TestClubCategoryKeys_NonSensitiveClubsOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	robot, misc := uuid.NewV7(), uuid.NewV7()
	e.catalog.setCategory(e.clubPlain, directory.CategoryTechnology)
	e.catalog.setCategory(robot, directory.CategoryTechnology)
	e.catalog.setCategory(e.clubSens, directory.CategorySports) // sensitive (e.g. health) with a plain category
	e.catalog.setCategory(misc, directory.CategoryOther)
	a := e.student(t, e.uniA, false)
	for _, c := range []uuid.UUID{e.clubPlain, robot, e.clubSens, misc} {
		e.orgs.join(a.UserID, c, authz.MemberMember)
	}
	want := []string{students.ClubCategoryKey(directory.CategoryTechnology)}
	p := e.basics(t, a, e.deptA, 2, t0.AddDate(2, 0, 0))
	if got := categoryKeysIn(p.AudienceKeys); !slices.Equal(got, want) {
		t.Fatalf("category keys = %v, want %v (all keys %v)", got, want, p.AudienceKeys)
	}
	e.deliver(t, students.AddClubKeyTaskName, domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: e.clubSens,
		OrgKind: "club", UserID: a.UserID, Role: "member"}, e.clubSens)
	e.job(t, students.AudienceRefreshJobName)
	if got := categoryKeysIn(e.profile(t, a.UserID).AudienceKeys); !slices.Equal(got, want) {
		t.Fatalf("category keys after the event and the refresh = %v, want %v", got, want)
	}
	vk, err := e.svc.ViewerKeys(t.Context(), a.UserID)
	if err != nil || !slices.Equal(categoryKeysIn(vk.Keys), want) {
		t.Fatalf("viewer category keys = %v, %v; want %v", vk.Keys, err, want)
	}

	b := e.student(t, e.uniA, false)
	e.orgs.join(b.UserID, e.clubSens, authz.MemberMember)
	if p := e.basics(t, b, e.deptA, 2, t0.AddDate(2, 0, 0)); len(categoryKeysIn(p.AudienceKeys)) != 0 {
		t.Fatalf("a sensitive club's member holds %v", p.AudienceKeys)
	}
	if vk, err := e.svc.ViewerKeys(t.Context(), b.UserID); err != nil || len(categoryKeysIn(vk.Keys)) != 0 {
		t.Fatalf("a sensitive club's member's viewer keys = %v, %v", vk.Keys, err)
	}
}

// TestClubCategoryKeys_FollowSensitivityAndCategoryChanges (BE-207): when a
// club becomes sensitive its members lose the category key it gave (a
// member also in another club of the category keeps it through that club)
// and get it back when the flag is cleared; club.updated with a category
// change moves the members' key to the new category; another profile
// change does not recompute anyone (the nightly refresh does).
func TestClubCategoryKeys_FollowSensitivityAndCategoryChanges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club, twin := e.clubPlain, uuid.NewV7()
	e.catalog.setCategory(club, directory.CategoryTechnology)
	e.catalog.setCategory(twin, directory.CategoryTechnology)
	a, b := e.student(t, e.uniA, false), e.student(t, e.uniA, false)
	e.orgs.join(a.UserID, club, authz.MemberMember)
	e.orgs.join(b.UserID, club, authz.MemberMember)
	e.orgs.join(b.UserID, twin, authz.MemberMember)
	e.basics(t, a, e.deptA, 2, t0.AddDate(2, 0, 0))
	e.basics(t, b, e.deptA, 2, t0.AddDate(2, 0, 0))
	tech, arts := students.ClubCategoryKey(directory.CategoryTechnology), students.ClubCategoryKey(directory.CategoryArts)
	check := func(step string, user uuid.UUID, want ...string) {
		t.Helper()
		if got := categoryKeysIn(e.profile(t, user).AudienceKeys); !slices.Equal(got, want) {
			t.Fatalf("%s: category keys = %v, want %v", step, got, want)
		}
	}
	check("setup", a.UserID, tech)
	check("setup", b.UserID, tech)

	e.catalog.setSensitive(club, true)
	for range 2 { // a redelivery changes nothing
		e.deliver(t, students.DropSensitiveClubKeyTaskName, domainevent.ClubSensitivityChanged{ClubOrgID: club, Sensitive: true}, club)
	}
	check("sensitive", a.UserID)
	check("sensitive, through the other club", b.UserID, tech)
	e.catalog.setSensitive(club, false)
	e.deliver(t, students.DropSensitiveClubKeyTaskName, domainevent.ClubSensitivityChanged{ClubOrgID: club, Sensitive: false}, club)
	check("cleared", a.UserID, tech)

	e.catalog.setCategory(club, directory.CategoryArts)
	e.deliver(t, students.RefreshClubCategoryTaskName, domainevent.ClubUpdated{ClubOrgID: club, Fields: []string{"category"}}, club)
	check("category changed", a.UserID, arts)
	check("category changed, the other club stays", b.UserID, arts, tech)

	e.catalog.setCategory(club, directory.CategoryHobby)
	e.deliver(t, students.RefreshClubCategoryTaskName, domainevent.ClubUpdated{ClubOrgID: club, Fields: []string{"join_policy"}}, club)
	check("another field", a.UserID, arts)
	e.job(t, students.AudienceRefreshJobName)
	check("nightly refresh", a.UserID, students.ClubCategoryKey(directory.CategoryHobby))
}

// TestAudienceCount_ClubCategoryDimension (BE-207): the club category is a
// dimension of its own (OR inside, AND with the others) that counts the
// members of the category's non-sensitive clubs only; k ≥ 10 applies as
// everywhere.
func TestAudienceCount_ClubCategoryDimension(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.catalog.setCategory(e.clubPlain, directory.CategoryTechnology)
	e.catalog.setCategory(e.clubSens, directory.CategoryTechnology) // sensitive: never counted through its category
	for _, a := range e.cohort(t, 12, e.uniA, 2, false) {
		e.orgs.join(a.UserID, e.clubPlain, authz.MemberMember)
	}
	for _, a := range e.cohort(t, 15, e.uniB, 2, false) {
		e.orgs.join(a.UserID, e.clubSens, authz.MemberMember)
	}
	e.job(t, students.AudienceRefreshJobName)
	tech, arts := students.ClubCategoryKey(directory.CategoryTechnology), students.ClubCategoryKey(directory.CategoryArts)
	for _, c := range []struct {
		keys []string
		want int // 0: too small
	}{
		{[]string{tech}, 12},
		{[]string{tech, arts}, 12},           // OR inside the dimension
		{[]string{tech, uniKey(e.uniA)}, 12}, // AND across
		{[]string{tech, uniKey(e.uniB)}, 0},  // the sensitive club's members: nobody
		{[]string{arts}, 0},
	} {
		n, err := e.svc.Count(ctx, students.AudienceSpec{Keys: c.keys})
		switch {
		case c.want == 0 && !errors.Is(err, students.ErrAudienceTooSmall):
			t.Errorf("Count(%v) = %d, %v; want too small", c.keys, n, err)
		case c.want > 0 && (err != nil || n != c.want):
			t.Errorf("Count(%v) = %d, %v; want %d", c.keys, n, err, c.want)
		}
	}
}

// TestAudienceSpec_ClubCategoryKeys (BE-207): a club category key must name
// a targetable category: a sensitive one, other, an unknown or a
// differently written value is 422 validation_failed, never a count.
func TestAudienceSpec_ClubCategoryKeys(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, keys := range [][]string{
		{"club_category:"}, {"club_category:religious"}, {"club_category:political"}, {"club_category:identity"},
		{"club_category:other"}, {"club_category:astrology"}, {"club_category:Technology"},
		{students.ClubCategoryKey(directory.CategoryTechnology), "club_category:identity"},
	} {
		if _, err := e.svc.Count(t.Context(), students.AudienceSpec{Keys: keys}); !errors.Is(err, students.ErrInvalidAudience) {
			t.Errorf("Count(%q) = %v, want ErrInvalidAudience", keys, err)
		}
	}
	for _, c := range directory.TargetableCategories() {
		keys := []string{students.ClubCategoryKey(c)}
		if _, err := e.svc.Count(t.Context(), students.AudienceSpec{Keys: keys}); !errors.Is(err, students.ErrAudienceTooSmall) {
			t.Errorf("Count(%q) = %v, want ErrAudienceTooSmall", keys, err)
		}
	}
}
