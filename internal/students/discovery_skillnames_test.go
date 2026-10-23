package students_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/students"
)

// TestSkillNames_OneCallPerLevelTwoPage (BE-084): a level 2 page — a
// Search page and an AnonProfiles batch — names the skills of all its
// profiles with one SkillNamer call (no N+1); every profile carries the
// names, and a skill the namer does not know keeps an empty name. A level
// 3 profile makes one call too.
func TestSkillNames_OneCallPerLevelTwoPage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	idx := e.index(t)
	var as []authz.Actor
	for range 12 {
		a := e.discoverable(t, e.uniA, e.deptA)
		if _, err := e.svc.PutMySkills(ctx, a, []students.SkillInput{{SkillID: e.skill1}, {SkillID: e.skill2}}); err != nil {
			t.Fatal(err)
		}
		as = append(as, a)
	}
	// A skill the catalog no longer names (the fake knows only skill1/2):
	// stored directly, as PutMySkills would refuse it.
	gone := uuid.NewV7()
	if _, err := e.store.ReplaceSkills(ctx, as[0].UserID, []students.Skill{{UserID: as[0].UserID, SkillID: e.skill1},
		{UserID: as[0].UserID, SkillID: gone}}); err != nil {
		t.Fatal(err)
	}
	check := func(name string, d students.AnonProfileDTO, user uuid.UUID) {
		t.Helper()
		if len(d.Skills) == 0 {
			t.Fatalf("%s: %v has no skills", name, user)
		}
		for _, k := range d.Skills {
			want := map[string]string{e.skill1.String(): "Go", e.skill2.String(): "SQL", gone.String(): ""}[k.SkillID]
			if k.Name != want {
				t.Fatalf("%s: skill %s named %q, want %q", name, k.SkillID, k.Name, want)
			}
		}
	}

	e.skillN.Store(0)
	page, err := e.svc.Search(ctx, students.DiscoveryQuery{Limit: 10})
	if err != nil || len(page.Items) != 10 || page.BelowThreshold {
		t.Fatalf("search = %d items, below=%v, %v", len(page.Items), page.BelowThreshold, err)
	}
	if n := e.skillN.Load(); n != 1 {
		t.Fatalf("SkillNames calls for a page of 10 = %d, want 1", n)
	}
	for _, it := range page.Items {
		d, err := it.WithAnonID("x")
		if err != nil {
			t.Fatal(err)
		}
		check("search", d, it.UserID)
	}

	e.skillN.Store(0)
	ids := make([]uuid.UUID, len(as))
	for i, a := range as {
		ids[i] = a.UserID
	}
	got, err := idx.AnonProfiles(ctx, ids)
	if err != nil || len(got) != len(as) {
		t.Fatalf("AnonProfiles = %d, %v", len(got), err)
	}
	if n := e.skillN.Load(); n != 1 {
		t.Fatalf("SkillNames calls for a batch of %d = %d, want 1", len(as), n)
	}
	for id, p := range got {
		d, err := p.WithAnonID("x")
		if err != nil {
			t.Fatal(err)
		}
		check("anon profiles", d, id)
	}

	org, rec, _ := e.company("Ekip A.Ş.")
	if err := e.svc.Grant(ctx, as[0].UserID, org, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	e.skillN.Store(0)
	l3, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, as[0].UserID)
	if err != nil {
		t.Fatal(err)
	}
	if n := e.skillN.Load(); n != 1 {
		t.Fatalf("SkillNames calls for a level 3 profile = %d, want 1", n)
	}
	names := map[string]string{}
	for _, k := range l3.Skills {
		names[k.SkillID] = k.Name
	}
	if len(names) != 2 || names[e.skill1.String()] != "Go" || names[gone.String()] != "" {
		t.Fatalf("level 3 skills = %+v", l3.Skills)
	}
}
