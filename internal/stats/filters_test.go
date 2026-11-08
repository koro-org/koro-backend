package stats_test

import (
	"net/http"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/outreach"
	"koro/internal/stats"
	"koro/internal/students"
)

// TestClub_EventFilter (BE-190): event_id narrows the events to that one
// event of the club, read in its own start (whatever the range); an event
// of another org, an unknown or a malformed ID is the same 422; members
// and sponsorship keep the range.
func TestClub_EventFilter(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	club, other := authztest.ID("club_a"), authztest.ID("club_b")
	start := time.Date(2024, 3, 5, 15, 0, 0, 0, time.UTC) // outside the default range
	mine, neighbour, theirs := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	f.summaries[mine] = events.EventSummary{ID: mine, OrgID: club, StartsAt: start, Status: events.StatusCompleted}
	f.summaries[theirs] = events.EventSummary{ID: theirs, OrgID: other, StartsAt: start, Status: events.StatusCompleted}
	f.figures = []events.EventFigures{
		{EventID: neighbour, OrgID: club, Title: "Aynı saniye", Status: events.StatusCompleted, StartsAt: start, Registered: 99},
		{EventID: mine, OrgID: club, Title: "Seçilen", Status: events.StatusCompleted, StartsAt: start, Registered: 20, CheckedIn: 15},
	}
	ctx, admin := t.Context(), persona(t, "club_admin")
	st, err := svc.Club(ctx, admin, club, stats.RangeQuery{EventID: mine.String()})
	if err != nil {
		t.Fatal(err)
	}
	if f.figuresAsk != [2]time.Time{start, start.Add(time.Second)} {
		t.Fatalf("figures asked for %v", f.figuresAsk)
	}
	e := st.Events
	if len(e.Items) != 1 || e.Items[0].EventID != mine.String() || e.Count != 1 || e.Registered != 20 || *e.AttendanceRate != 0.75 {
		t.Fatalf("events = %+v", e)
	}
	if st.Range.From != "2025-11-01" || len(st.Members.Monthly) != 12 {
		t.Fatalf("the members lost the range: %+v", st.Range)
	}
	for _, id := range []string{theirs.String(), uuid.NewV7().String(), "nope"} {
		_, err := svc.Club(ctx, admin, club, stats.RangeQuery{EventID: id})
		wantStatus(t, err, http.StatusUnprocessableEntity)
	}
	// The authorisation comes first.
	_, err = svc.Club(ctx, persona(t, "other_club_admin"), club, stats.RangeQuery{EventID: "nope"})
	wantStatus(t, err, http.StatusNotFound)
}

// TestCompany_AudienceFilters (BE-190): the company's university, department
// and class year filters reach outreach as key filters (a university with
// its departments); the estimates come back as outreach publishes them; a
// malformed value is 422 after the authorisation; no filter is no filter.
func TestCompany_AudienceFilters(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	ctx, company := t.Context(), authztest.ID("company_a")
	uni := authztest.UniversityID("uni_a")
	d1, d2, dept := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	f.structure[uni] = directory.UniversityStructure{Departments: []directory.DepartmentBrief{{ID: d1}, {ID: d2}}}
	f.reach = outreach.OutreachStats{ActiveSegments: 1, Segments: []outreach.SegmentEstimate{
		{ID: uuid.NewV7(), Name: "Üçüncü sınıf", Estimate: outreach.PublicEstimate(64)}}, Eligible: outreach.PublicEstimate(7)}
	st, err := svc.Company(ctx, persona(t, "company_admin"), company,
		stats.RangeQuery{UniversityID: uni.String(), DepartmentID: dept.String(), ClassYear: "3"})
	if err != nil {
		t.Fatal(err)
	}
	a := f.audienceAsk
	if a == nil || a.Uni == nil || *a.Uni != uni || !slices.Equal(a.UniDepts, []uuid.UUID{d1, d2}) || *a.Dept != dept || *a.Year != 3 {
		t.Fatalf("filter asked = %+v", a)
	}
	if st.Audience.ActiveSegments != 1 || *st.Audience.Segments[0].Estimate.Count != 60 || !st.Dispatches.Eligible.BelowThreshold {
		t.Fatalf("audience = %+v, dispatches = %+v", st.Audience, st.Dispatches)
	}
	if _, err := svc.Company(ctx, persona(t, "company_admin"), company, stats.RangeQuery{}); err != nil ||
		f.audienceAsk.Uni != nil || f.audienceAsk.Dept != nil || f.audienceAsk.Year != nil {
		t.Fatalf("no filter = %+v, %v", f.audienceAsk, err)
	}
	for _, q := range []stats.RangeQuery{{UniversityID: "x"}, {DepartmentID: "x"}, {ClassYear: "0"}, {ClassYear: "9"}, {ClassYear: "üç"}} {
		_, err := svc.Company(ctx, persona(t, "company_admin"), company, q)
		wantStatus(t, err, http.StatusUnprocessableEntity)
	}
	_, err = svc.Company(ctx, persona(t, "other_company_member"), company, stats.RangeQuery{ClassYear: "9"})
	wantStatus(t, err, http.StatusNotFound)
}

// TestCompany_ClubCategoryFilter (BE-207, Q-58): club_category reaches
// outreach as a key filter next to the others; only a category an audience
// may name is accepted: a sensitive category, other or an unknown value is
// 422 after the authorisation and outreach is not asked.
func TestCompany_ClubCategoryFilter(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	ctx, company, cma := t.Context(), authztest.ID("company_a"), persona(t, "company_admin")
	if _, err := svc.Company(ctx, cma, company, stats.RangeQuery{ClubCategory: "technology", ClassYear: "3"}); err != nil {
		t.Fatal(err)
	}
	if a := f.audienceAsk; a == nil || a.ClubCategory == nil || *a.ClubCategory != directory.CategoryTechnology || *a.Year != 3 {
		t.Fatalf("filter asked = %+v", a)
	}
	for _, c := range directory.TargetableCategories() {
		if _, err := svc.Company(ctx, cma, company, stats.RangeQuery{ClubCategory: string(c)}); err != nil ||
			*f.audienceAsk.ClubCategory != c {
			t.Fatalf("%s: asked %+v, %v", c, f.audienceAsk, err)
		}
	}
	for _, bad := range []string{"religious", "political", "identity", "other", "astrology", "Technology"} {
		f.audienceAsk = nil
		_, err := svc.Company(ctx, cma, company, stats.RangeQuery{ClubCategory: bad})
		wantStatus(t, err, http.StatusUnprocessableEntity)
		if f.audienceAsk != nil {
			t.Fatalf("%s: outreach asked %+v", bad, f.audienceAsk)
		}
	}
	_, err := svc.Company(ctx, persona(t, "other_company_member"), company, stats.RangeQuery{ClubCategory: "religious"})
	wantStatus(t, err, http.StatusNotFound)
}

// TestUniversity_FacultyDepartmentAndClubFilters (BE-190): the faculty map
// reaches students for the breakdown; a faculty filter asks its
// departments, a department filter the one department; a faculty without
// departments counts nothing (students not asked); a club filter reads one
// club's activity, a sensitive club's stays empty; filters that are not the
// university's are 422.
func TestUniversity_FacultyDepartmentAndClubFilters(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	ctx, rep, uniOrg := t.Context(), persona(t, "university_rep"), authztest.ID("uni_a")
	uni := authztest.UniversityID("uni_a")
	a, s, u := authztest.ID("club_a"), authztest.ID("club_sensitive"), authztest.ID("club_unclaimed")
	f.uniClubs = directory.UniversityClubs{UniversityID: uni, Clubs: []directory.ClubBrief{
		{OrgID: a, Claimed: true}, {OrgID: s, Sensitive: true}, {OrgID: u}}}
	fac, empty, d1, d2, loose := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	f.structure[uni] = directory.UniversityStructure{Faculties: []uuid.UUID{fac, empty},
		Departments: []directory.DepartmentBrief{{ID: d1, FacultyID: &fac}, {ID: d2, FacultyID: &fac}, {ID: loose}}}
	f.students = students.UniversityStudents{Total: ptr(40), ByFaculty: []students.StudentGroup{{Key: fac.String(), Count: 34}}}
	f.totals = map[uuid.UUID]events.EventTotals{a: {Events: 3, Registered: 47}, u: {Events: 5, Registered: 6}, s: {Events: 9, Registered: 90}}

	st, err := svc.University(ctx, rep, uniOrg, stats.RangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	q := f.studentQ
	if q == nil || len(q.Departments) != 0 || len(q.FacultyOf) != 2 || q.FacultyOf[d1] != fac || q.FacultyOf[d2] != fac {
		t.Fatalf("query = %+v", q)
	}
	if !slices.Equal(st.Students.ByFaculty, []stats.KeyEstimate{{Key: fac.String(), Count: 30}}) {
		t.Fatalf("by faculty = %+v", st.Students.ByFaculty)
	}

	if _, err := svc.University(ctx, rep, uniOrg, stats.RangeQuery{FacultyID: fac.String()}); err != nil ||
		!slices.Equal(f.studentQ.Departments, sorted(d1, d2)) {
		t.Fatalf("faculty filter asked %+v, %v", f.studentQ, err)
	}
	if _, err := svc.University(ctx, rep, uniOrg, stats.RangeQuery{DepartmentID: loose.String()}); err != nil ||
		!slices.Equal(f.studentQ.Departments, []uuid.UUID{loose}) {
		t.Fatalf("department filter asked %+v, %v", f.studentQ, err)
	}
	f.studentQ = nil
	st, err = svc.University(ctx, rep, uniOrg, stats.RangeQuery{FacultyID: empty.String()})
	if err != nil || f.studentQ != nil || !st.Students.Total.BelowThreshold || st.Students.ByFaculty == nil {
		t.Fatalf("a faculty without departments: asked %+v, students %+v, %v", f.studentQ, st.Students, err)
	}

	st, err = svc.University(ctx, rep, uniOrg, stats.RangeQuery{ClubID: a.String()})
	if err != nil || !slices.Equal(f.totalsAsk, []uuid.UUID{a}) || len(st.Clubs.Items) != 1 || st.Clubs.EventsInRange != 3 ||
		st.Clubs.Total != 3 || st.Clubs.Claimed != 1 {
		t.Fatalf("club filter: asked %v, clubs %+v, %v", f.totalsAsk, st.Clubs, err)
	}
	st, err = svc.University(ctx, rep, uniOrg, stats.RangeQuery{ClubID: s.String()})
	if err != nil || len(f.totalsAsk) != 0 || len(st.Clubs.Items) != 0 || st.Clubs.EventsInRange != 0 || st.Clubs.Total != 3 {
		t.Fatalf("sensitive club filter: asked %v, clubs %+v, %v", f.totalsAsk, st.Clubs, err)
	}

	for _, bad := range []stats.RangeQuery{
		{FacultyID: uuid.NewV7().String()}, {FacultyID: "x"}, {DepartmentID: uuid.NewV7().String()},
		{FacultyID: fac.String(), DepartmentID: d1.String()}, {ClubID: authztest.ID("club_b").String()}, {ClubID: "x"},
	} {
		_, err := svc.University(ctx, rep, uniOrg, bad)
		wantStatus(t, err, http.StatusUnprocessableEntity)
	}
	_, err = svc.University(ctx, rep, authztest.ID("uni_b"), stats.RangeQuery{FacultyID: "x"})
	wantStatus(t, err, http.StatusNotFound)
}

func sorted(ids ...uuid.UUID) []uuid.UUID {
	slices.SortFunc(ids, uuid.UUID.Compare)
	return ids
}
