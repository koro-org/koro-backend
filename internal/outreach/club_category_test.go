package outreach_test

import (
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/outreach"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

func categoryKey(c directory.Category) string { return "club_category:" + string(c) }

// TestDefinition_ClubCategoryDimension (BE-207, Q-58): the club categories
// are one dimension (club_category:<category>), sorted and de-duplicated
// like the others; a category an audience may not name — religious,
// political, identity, other or an unknown value — is 422 at its place in
// the request and nothing is counted.
func TestDefinition_ClubCategoryDimension(t *testing.T) {
	t.Parallel()
	d := outreach.Definition{ClubCategory: []directory.Category{directory.CategoryTechnology, directory.CategoryArts}, Year: []int16{3}}
	want := sortedKeys(categoryKey(directory.CategoryTechnology), categoryKey(directory.CategoryArts), yearKey(3))
	if got := d.Keys(); !slices.Equal(got, want) || d.DimCount() != 2 {
		t.Fatalf("Keys = %v (dims %d), want %v", got, d.DimCount(), want)
	}

	e := newEnv(t)
	e.audience.add(12, true, uniKey(uniA), categoryKey(directory.CategoryTechnology))
	cma := actor(t, "company_admin")
	got, err := e.svc.CountAudience(t.Context(), cma, companyA, outreach.Definition{ClubCategory: []directory.Category{
		directory.CategoryTechnology, directory.CategoryTechnology}})
	if err != nil || got.Count == nil || *got.Count != 10 || got.Dimensions != 1 {
		t.Fatalf("a repeated category = %+v, %v; want 10 in one dimension", got, err)
	}
	before := e.audience.callCount()
	for _, tc := range []struct {
		cats []directory.Category
		loc  string
	}{
		{[]directory.Category{directory.CategoryReligious}, "body.definition.club_category[0]"},
		{[]directory.Category{directory.CategoryTechnology, directory.CategoryPolitical}, "body.definition.club_category[1]"},
		{[]directory.Category{directory.CategoryIdentity}, "body.definition.club_category[0]"},
		{[]directory.Category{directory.CategoryOther}, "body.definition.club_category[0]"},
		{[]directory.Category{"astrology"}, "body.definition.club_category[0]"},
	} {
		_, err := e.svc.CountAudience(t.Context(), cma, companyA, outreach.Definition{ClubCategory: tc.cats})
		pe, ok := errors.AsType[*problem.Error](err)
		if !ok || pe.Status() != 422 || len(pe.Fields()) != 1 || pe.Fields()[0].Location != tc.loc {
			t.Errorf("%v: %v, want 422 at %s", tc.cats, err, tc.loc)
		}
		_, err = e.svc.CreateSegment(t.Context(), cma, companyA, outreach.SegmentInput{Name: "Kategori",
			Definition: outreach.Definition{ClubCategory: tc.cats}})
		problemtest.WantErr(t, err, problem.ErrValidation)
	}
	if n := e.audience.callCount(); n != before {
		t.Fatalf("a refused category was counted %d times", n-before)
	}
}

// TestCountAudience_ClubCategory_CompanyOnly (BE-207, 03 k16): a company
// and the platform count the students of the club categories (with the
// other dimensions); a club and a university never name a category (422
// outreach.target_out_of_scope, nothing counted). A saved segment keeps the
// categories, compiles to their keys and is audited by its shape only.
func TestCountAudience_ClubCategory_CompanyOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	tech, sports := categoryKey(directory.CategoryTechnology), categoryKey(directory.CategorySports)
	e.audience.add(24, true, uniKey(uniA), yearKey(3), tech)
	e.audience.add(13, true, uniKey(uniB), yearKey(3), tech, sports)
	e.audience.add(30, false, uniKey(uniA), tech) // not eligible: never counted
	cats := []directory.Category{directory.CategorySports, directory.CategoryTechnology}

	for _, tc := range []struct {
		name  string
		actor authz.Actor
		org   uuid.UUID
		def   outreach.Definition
		want  int
	}{
		{"categories", actor(t, "company_admin"), companyA, outreach.Definition{ClubCategory: cats}, 30},
		{"with a university", actor(t, "company_member"), companyA,
			outreach.Definition{Uni: []uuid.UUID{uniA}, ClubCategory: []directory.Category{directory.CategoryTechnology}}, 20},
		{"platform", actor(t, "platform_admin"), platformOrg, outreach.Definition{Year: []int16{3}, ClubCategory: cats}, 30},
	} {
		got, err := e.svc.CountAudience(ctx, tc.actor, tc.org, tc.def)
		if err != nil || got.Count == nil || *got.Count != tc.want {
			t.Fatalf("%s: %+v, %v; want %d", tc.name, got, err, tc.want)
		}
	}

	before := e.audience.callCount()
	for _, tc := range []struct {
		name  string
		actor authz.Actor
		org   uuid.UUID
		def   outreach.Definition
	}{
		{"club_admin", actor(t, "club_admin"), clubA, outreach.Definition{Club: []uuid.UUID{clubA}, ClubCategory: cats}},
		{"university_rep", actor(t, "university_rep"), uniOrgA, outreach.Definition{Uni: []uuid.UUID{uniA}, ClubCategory: cats}},
	} {
		_, err := e.svc.CountAudience(ctx, tc.actor, tc.org, tc.def)
		problemtest.WantErr(t, err, outreach.ErrTargetOutOfScope)
		_, err = e.svc.CreateSegment(ctx, tc.actor, tc.org, outreach.SegmentInput{Name: "Kategori", Definition: tc.def})
		problemtest.WantErr(t, err, outreach.ErrTargetOutOfScope)
	}
	if n := e.audience.callCount(); n != before {
		t.Fatalf("an out-of-scope category was counted %d times", n-before)
	}

	cma := actor(t, "company_admin")
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Teknoloji ve spor",
		Definition: outreach.Definition{ClubCategory: []directory.Category{directory.CategoryTechnology, directory.CategorySports,
			directory.CategoryTechnology}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seg.Definition.ClubCategory, []directory.Category{directory.CategorySports, directory.CategoryTechnology}) ||
		seg.Estimate.Count == nil || *seg.Estimate.Count != 30 {
		t.Fatalf("segment = %+v", seg)
	}
	ev := e.audit.Last()
	if ev.Metadata["club_category"] != 2 || ev.Metadata["dimensions"] != 1 {
		t.Fatalf("audit shape = %v", ev.Metadata)
	}
	c, err := e.svc.Compile(ctx, cma, outreach.SegmentSpec{OrgID: companyA, SegmentID: &seg.ID})
	if err != nil || !slices.Equal(c.Keys, sortedKeys(sports, tech)) || c.DimCount != 1 || c.Size != 30 {
		t.Fatalf("Compile = %+v, %v", c, err)
	}
}

// TestCountAudience_ClubCategory_PrivacyRules (BE-207, BE-203): the club
// category is under the same rules as every dimension — several
// categories each reach 10 students with the other dimensions or the
// count is refused at the small one (no number), a narrow intersection is
// below the threshold with no number and Compile refuses it, the nightly
// recount agrees.
func TestCountAudience_ClubCategory_PrivacyRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withDailyLimit(50))
	ctx, cma := t.Context(), actor(t, "company_admin")
	tech, arts := categoryKey(directory.CategoryTechnology), categoryKey(directory.CategoryArts)
	e.audience.add(15, true, uniKey(uniA), yearKey(4), tech)
	e.audience.add(3, true, uniKey(uniA), yearKey(4), arts)
	e.audience.add(20, true, uniKey(uniA), yearKey(3), arts)

	def := outreach.Definition{Year: []int16{4}, ClubCategory: []directory.Category{directory.CategoryTechnology, directory.CategoryArts}}
	_, err := e.svc.CountAudience(ctx, cma, companyA, def)
	wantValueRefused(t, err, "body.definition.club_category[1]")
	_, err = e.svc.Compile(ctx, cma, specFor(companyA, def))
	wantValueRefused(t, err, "body.audience.definition.club_category[1]")
	if a := alarmOf(e, outreach.AlarmValueFloor); a != 2 {
		t.Fatalf("value_floor alarms = %d, want 2", a)
	}

	narrow := outreach.Definition{Year: []int16{4}, ClubCategory: []directory.Category{directory.CategoryArts}}
	got, err := e.svc.CountAudience(ctx, cma, companyA, narrow)
	if err != nil || got.Count != nil || !got.BelowThreshold {
		t.Fatalf("3 students = %+v, %v; want below the threshold", got, err)
	}
	_, err = e.svc.Compile(ctx, cma, specFor(companyA, narrow))
	problemtest.WantErr(t, err, outreach.ErrAudienceTooSmall)

	wide := outreach.Definition{Year: []int16{3, 4}, ClubCategory: []directory.Category{directory.CategoryTechnology, directory.CategoryArts}}
	seg, err := e.svc.CreateSegment(ctx, cma, companyA, outreach.SegmentInput{Name: "Geniş", Definition: wide})
	if err != nil || seg.Estimate.Count == nil || *seg.Estimate.Count != 30 {
		t.Fatalf("every value large enough = %+v, %v; want 30", seg, err)
	}
	e.audience.reset()
	e.audience.add(15, true, yearKey(4), tech)
	e.audience.add(3, true, yearKey(3), arts)
	e.recount(t)
	if v, err := e.svc.GetSegment(ctx, cma, companyA, seg.ID); err != nil || v.Estimate.Count != nil || !v.Estimate.BelowThreshold {
		t.Fatalf("after the recount = %+v, %v; want a NULL estimate", v, err)
	}
}

// TestHTTP_ClubCategoryRoundTrip (BE-207): the definition DTO carries
// club_category; a category outside the schema's list is 422
// validation_failed before the service, a club category of a university is
// out of scope.
func TestHTTP_ClubCategoryRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.audience.add(21, true, uniKey(uniA), categoryKey(directory.CategoryCareer))
	app := newHTTP(t, e)
	rec, out := call(t, app, "company_member", http.MethodPost, segmentsPath(companyA), map[string]any{"name": "Kariyer",
		"definition": map[string]any{"club_category": []string{"career", "career"}}}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("createSegment = %d %v", rec.Code, out)
	}
	d, _ := out["definition"].(map[string]any)
	cnt, _ := out["count"].(map[string]any)
	if cats, _ := d["club_category"].([]any); len(cats) != 1 || cats[0] != "career" || cnt["count"] != float64(20) || out["dimensions"] != float64(1) {
		t.Fatalf("created %v", out)
	}
	for _, bad := range []string{"religious", "political", "identity", "other", "astrology"} {
		rec, out := call(t, app, "company_member", http.MethodPost, countPath(companyA),
			map[string]any{"definition": map[string]any{"club_category": []string{bad}}}, nil)
		if rec.Code != http.StatusUnprocessableEntity || out["code"] != "validation_failed" {
			t.Fatalf("%s = %d %v", bad, rec.Code, out)
		}
	}
	rec, out = call(t, app, "university_rep", http.MethodPost, countPath(uniOrgA),
		map[string]any{"definition": map[string]any{"uni": []string{uniA.String()}, "club_category": []string{"career"}}}, nil)
	if rec.Code != http.StatusUnprocessableEntity || out["code"] != "outreach.target_out_of_scope" {
		t.Fatalf("university with a category = %d %v", rec.Code, out)
	}
}

// TestDTO_ClubCategoryEnumIsDirectorys (BE-207): the schema's list of club
// categories is directory's targetable list (one vocabulary).
func TestDTO_ClubCategoryEnumIsDirectorys(t *testing.T) {
	t.Parallel()
	f, ok := reflect.TypeFor[outreach.SegmentDefinitionDTO]().FieldByName("ClubCategory")
	if !ok {
		t.Fatal("no ClubCategory field")
	}
	var want []string
	for _, c := range directory.TargetableCategories() {
		want = append(want, string(c))
	}
	if got := strings.Split(f.Tag.Get("enum"), ","); !slices.Equal(got, want) {
		t.Fatalf("enum = %v, want %v", got, want)
	}
	if f.Tag.Get("maxItems") != "9" || len(want) != 9 {
		t.Fatalf("maxItems = %s for %d categories", f.Tag.Get("maxItems"), len(want))
	}
}
