package stats_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/directory"
	"koro/internal/entitlements"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/identity"
	"koro/internal/messaging"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/clock"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/stats"
	"koro/internal/students"
)

// now is 2026-10-31 12:00 Istanbul.
var now = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

// fake answers every port from its fields and records what it was asked.
type fake struct {
	orgs       map[uuid.UUID]orgs.OrgSummary
	clubs      map[uuid.UUID]directory.Club
	users      identity.UserStats
	orgStats   orgs.OrgStats
	members    orgs.MemberStats
	content    feed.ContentStats
	posts      feed.OrgPostStats
	events     events.EventStats
	figures    []events.EventFigures
	totals     map[uuid.UUID]events.EventTotals
	sales      orders.SalesStats
	reach      outreach.OutreachStats
	contacts   messaging.ContactRequestStats
	usage      []entitlements.FeatureUsage
	students   students.UniversityStudents
	uniClubs   directory.UniversityClubs
	uniErr     error
	claims     int
	offerings  int
	totalsAsk  []uuid.UUID // OrgEventTotals' orgIDs
	offerAsk   []uuid.UUID // PendingUniversityDecisions' orgIDs
	studentAsk uuid.UUID
	from, to   time.Time // the last range asked
}

func (f *fake) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	o, ok := f.orgs[id]
	if !ok {
		return orgs.OrgSummary{}, orgs.ErrNotFound
	}
	return o, nil
}

func (f *fake) GetMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error) {
	out := map[uuid.UUID]orgs.OrgSummary{}
	for _, id := range ids {
		if o, ok := f.orgs[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

func (f *fake) GetClub(_ context.Context, id uuid.UUID) (directory.Club, error) {
	c, ok := f.clubs[id]
	if !ok {
		return directory.Club{}, directory.ErrNotFound
	}
	return c, nil
}

func (f *fake) UserStats(_ context.Context, from, to time.Time) (identity.UserStats, error) {
	f.from, f.to = from, to
	return f.users, nil
}

func (f *fake) OrgStats(context.Context, time.Time, time.Time) (orgs.OrgStats, error) {
	return f.orgStats, nil
}

func (f *fake) MemberStats(_ context.Context, _ uuid.UUID, from, to time.Time) (orgs.MemberStats, error) {
	f.from, f.to = from, to
	return f.members, nil
}

func (f *fake) ContentStats(context.Context, time.Time, time.Time) (feed.ContentStats, error) {
	return f.content, nil
}

func (f *fake) OrgPostStats(context.Context, uuid.UUID, time.Time, time.Time) (feed.OrgPostStats, error) {
	return f.posts, nil
}

func (f *fake) EventStats(context.Context, time.Time, time.Time) (events.EventStats, error) {
	return f.events, nil
}

func (f *fake) OrgEventFigures(context.Context, []uuid.UUID, time.Time, time.Time) ([]events.EventFigures, error) {
	return f.figures, nil
}

func (f *fake) OrgEventTotals(_ context.Context, ids []uuid.UUID, _, _ time.Time) (map[uuid.UUID]events.EventTotals, error) {
	f.totalsAsk = ids
	out := map[uuid.UUID]events.EventTotals{}
	for _, id := range ids {
		if t, ok := f.totals[id]; ok {
			out[id] = t
		}
	}
	return out, nil
}

func (f *fake) SalesStats(context.Context, uuid.UUID, time.Time, time.Time) (orders.SalesStats, error) {
	return f.sales, nil
}

func (f *fake) OrgOutreachStats(context.Context, uuid.UUID, time.Time, time.Time) (outreach.OutreachStats, error) {
	return f.reach, nil
}

func (f *fake) ContactRequestStats(context.Context, uuid.UUID, time.Time, time.Time) (messaging.ContactRequestStats, error) {
	return f.contacts, nil
}

func (f *fake) Usage(context.Context, uuid.UUID) ([]entitlements.FeatureUsage, error) {
	return f.usage, nil
}

func (f *fake) UniversityStudentStats(_ context.Context, id uuid.UUID, _, _ time.Time) (students.UniversityStudents, error) {
	f.studentAsk = id
	return f.students, nil
}

func (f *fake) UniversityClubStats(context.Context, uuid.UUID) (directory.UniversityClubs, error) {
	return f.uniClubs, f.uniErr
}

func (f *fake) PendingClaims(context.Context) (int, error) { return f.claims, nil }

func (f *fake) PendingUniversityDecisions(_ context.Context, ids []uuid.UUID) (int, error) {
	f.offerAsk = ids
	return f.offerings, nil
}

// world is the fake with the matrix orgs (authztest) and the service.
func world(t *testing.T) (*fake, stats.Service) {
	t.Helper()
	f := &fake{orgs: map[uuid.UUID]orgs.OrgSummary{}, clubs: map[uuid.UUID]directory.Club{}}
	for _, o := range authztest.Orgs() {
		id := authztest.ID(o.Name)
		f.orgs[id] = orgs.OrgSummary{ID: id, Kind: o.Kind, Name: o.Name, Status: orgs.StatusActive}
		if o.Kind == authz.OrgClub {
			f.clubs[id] = directory.Club{OrgID: id, IsSensitive: o.Sensitive}
		}
	}
	mod, err := stats.New(stats.Deps{Orgs: f, Clubs: f, Users: f, OrgCounts: f, Content: f, Events: f, Sales: f, Outreach: f,
		Contacts: f, Packages: f, Students: f, Universities: f, Offerings: f, Clock: clock.NewFake(now), Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	return f, mod.Service
}

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

// wantStatus fails unless err is a problem of status (problemtest).
func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	if got, _ := problemtest.StatusCode(err); got != status {
		t.Fatalf("error = %v, want %d", err, status)
	}
}

func ptr[T any](v T) *T { return &v }

func TestNew_RequiresEveryDependency(t *testing.T) {
	t.Parallel()
	if _, err := stats.New(stats.Deps{}); err == nil {
		t.Fatal("New without dependencies succeeded")
	}
}

// TestPlatform_SumsTheModulesFigures: the platform panel maps each module's
// counts, groups the orgs by kind, derives the queues and the attendance
// rate, and asks the default range (the last 30 Istanbul days).
func TestPlatform_SumsTheModulesFigures(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	f.users = identity.UserStats{ByStatus: map[identity.Status]int{identity.StatusActive: 40, identity.StatusErased: 2},
		ByPersona: map[identity.Persona]int{identity.PersonaStudent: 35, identity.PersonaCompany: 5}, EmailVerified: 38,
		NewInRange: 7, VerifiedStudents: 30}
	f.orgStats = orgs.OrgStats{Counts: []orgs.OrgCount{
		{Kind: orgs.KindClub, Status: orgs.StatusActive, Count: 12}, {Kind: orgs.KindCompany, Status: orgs.StatusActive, Count: 3},
		{Kind: orgs.KindCompany, Status: orgs.StatusPendingReview, Count: 2}}, NewInRange: map[orgs.Kind]int{orgs.KindCompany: 2}}
	f.content = feed.ContentStats{PostsByStatus: map[feed.Status]int{feed.StatusPublished: 9},
		ReportsByStatus: map[feed.ReportStatus]int{feed.ReportOpen: 2, feed.ReportInReview: 1, feed.ReportDismissed: 4}}
	f.events = events.EventStats{Upcoming: 3, Held: 4, Registered: 8, CheckedIn: 6}
	f.claims = 5
	st, err := svc.Platform(t.Context(), persona(t, "platform_admin"), stats.RangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Range.From != "2026-10-02" || st.Range.To != "2026-10-31" {
		t.Fatalf("range = %+v", st.Range)
	}
	if f.from != time.Date(2026, 10, 1, 21, 0, 0, 0, time.UTC) || f.to != time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC) {
		t.Fatalf("instants asked = %v – %v (Istanbul midnights)", f.from, f.to)
	}
	if st.Users.Total != 42 || st.Users.VerifiedStudents != 30 ||
		!slices.Equal(st.Users.ByPersona, []stats.KeyCount{{Key: "company", Count: 5}, {Key: "student", Count: 35}}) {
		t.Fatalf("users = %+v", st.Users)
	}
	if len(st.Orgs.ByKind) != 2 || st.Orgs.ByKind[1].Kind != "company" || st.Orgs.ByKind[1].Total != 5 ||
		st.Orgs.ByKind[1].NewInRange != 2 || st.Orgs.NewInRange != 2 || len(st.Orgs.ByKind[1].ByStatus) != 2 {
		t.Fatalf("orgs = %+v", st.Orgs)
	}
	if st.Queues != (stats.PlatformQueuesDTO{OrgReviews: 2, ClubClaims: 5, ContentReports: 3}) {
		t.Fatalf("queues = %+v", st.Queues)
	}
	if st.Events.AttendanceRate == nil || *st.Events.AttendanceRate != 0.75 {
		t.Fatalf("attendance rate = %v", st.Events.AttendanceRate)
	}
	for _, p := range []string{"moderator", "support", "company_admin", "club_admin"} {
		_, err := svc.Platform(t.Context(), persona(t, p), stats.RangeQuery{})
		wantStatus(t, err, http.StatusForbidden)
	}
}

// TestRange_ValidatedAfterTheAuthorisation: a malformed date, from after to
// and a range of more than 366 days are 422 naming the parameter; an
// outsider gets the 404/403 of the authorisation first.
func TestRange_ValidatedAfterTheAuthorisation(t *testing.T) {
	t.Parallel()
	_, svc := world(t)
	pad, ctx := persona(t, "platform_admin"), t.Context()
	for _, q := range []stats.RangeQuery{{From: "2026-13-01"}, {To: "31.10.2026"}, {From: "2026-10-05", To: "2026-10-04"},
		{From: "2025-10-30", To: "2026-10-31"}} {
		_, err := svc.Platform(ctx, pad, q)
		wantStatus(t, err, http.StatusUnprocessableEntity)
	}
	if _, err := svc.Platform(ctx, pad, stats.RangeQuery{From: "2025-10-31", To: "2026-10-31"}); err != nil {
		t.Fatalf("366 days: %v", err)
	}
	_, err := svc.Company(ctx, persona(t, "student"), authztest.ID("company_a"), stats.RangeQuery{From: "nope"})
	wantStatus(t, err, http.StatusNotFound)
	_, err = svc.Platform(ctx, persona(t, "student"), stats.RangeQuery{From: "nope"})
	wantStatus(t, err, http.StatusForbidden)
}

// TestClub_MembersEventsAndSponsorship: every month of the default range
// (12 calendar months) is in the series; the rates cover the events held by
// now; cancelled events are counted apart.
func TestClub_MembersEventsAndSponsorship(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	club := authztest.ID("club_a")
	f.members = orgs.MemberStats{Active: 30, Pending: 2, JoinedInRange: 12, LeftInRange: 2,
		Monthly: []orgs.MemberMonth{{Month: "2025-11", Joined: 2}, {Month: "2026-09", Joined: 10, Left: 2}}}
	past, future := now.Add(-48*time.Hour), now.Add(48*time.Hour)
	f.figures = []events.EventFigures{
		{EventID: uuid.NewV7(), Title: "Gelecek", Status: events.StatusPublished, StartsAt: future, Registered: 20},
		{EventID: uuid.NewV7(), Title: "İptal", Status: events.StatusCancelled, StartsAt: past, Registered: 0},
		{EventID: uuid.NewV7(), Title: "Geçmiş", Status: events.StatusCompleted, StartsAt: past, Registered: 10, CheckedIn: 8, NoShow: 2},
	}
	f.sales = orders.SalesStats{Orders: 3, ByStatus: map[orders.Status]int{orders.StatusPaid: 2, orders.StatusAwaitingPayment: 1}}
	st, err := svc.Club(t.Context(), persona(t, "club_admin"), club, stats.RangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Range.From != "2025-11-01" || len(st.Members.Monthly) != 12 || st.Members.Monthly[0] != (stats.MonthDTO{Month: "2025-11", Joined: 2}) ||
		st.Members.Monthly[10] != (stats.MonthDTO{Month: "2026-09", Joined: 10, Left: 2}) || st.Members.Monthly[11].Month != "2026-10" ||
		st.Members.NetGrowth != 10 {
		t.Fatalf("members = %+v (range %+v)", st.Members, st.Range)
	}
	e := st.Events
	if e.Count != 2 || e.Cancelled != 1 || e.Registered != 10 || e.CheckedIn != 8 || e.NoShow != 2 || *e.AttendanceRate != 0.8 ||
		len(e.Items) != 3 || e.Items[0].Held || e.Items[0].AttendanceRate != nil || !e.Items[2].Held || e.Partial {
		t.Fatalf("events = %+v", e)
	}
	if st.Sponsorship.Orders != 3 || len(st.Sponsorship.ByStatus) != 2 {
		t.Fatalf("sponsorship = %+v", st.Sponsorship)
	}
}

// TestClub_Authorisation: club_admin S, platform_admin; a plain member 403;
// a sensitive club's panel is hidden from platform staff (Y2: 404); another
// org kind is 404 once the caller may read panels there; an unknown org 404.
func TestClub_Authorisation(t *testing.T) {
	t.Parallel()
	_, svc := world(t)
	ctx := t.Context()
	for _, tc := range []struct {
		persona, org string
		status       int
	}{
		{"club_admin", "club_a", http.StatusOK},
		{"platform_admin", "club_a", http.StatusOK},
		{"club_member", "club_a", http.StatusForbidden},
		{"other_club_admin", "club_a", http.StatusNotFound},
		{"moderator", "club_a", http.StatusForbidden},
		{"platform_admin", "club_sensitive", http.StatusNotFound},
		{"support", "club_sensitive", http.StatusNotFound},
		{"club_member", "club_sensitive", http.StatusForbidden},
		{"club_admin_sensitive", "club_sensitive", http.StatusOK},
		{"platform_admin", "company_a", http.StatusNotFound}, // not a club
		{"company_admin", "company_a", http.StatusForbidden},
		{"platform_admin", "nope", http.StatusNotFound},
	} {
		_, err := svc.Club(ctx, persona(t, tc.persona), authztest.ID(tc.org), stats.RangeQuery{})
		if tc.status == http.StatusOK {
			if err != nil {
				t.Fatalf("%s on %s: %v", tc.persona, tc.org, err)
			}
			continue
		}
		wantStatus(t, err, tc.status)
	}
}

// TestClub_WithoutCatalogRowIsSensitive: a club org without its directory
// row fails closed: platform staff do not see its panel.
func TestClub_WithoutCatalogRowIsSensitive(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	delete(f.clubs, authztest.ID("club_a"))
	_, err := svc.Club(t.Context(), persona(t, "platform_admin"), authztest.ID("club_a"), stats.RangeQuery{})
	wantStatus(t, err, http.StatusNotFound)
	if _, err := svc.Club(t.Context(), persona(t, "club_admin"), authztest.ID("club_a"), stats.RangeQuery{}); err != nil {
		t.Fatalf("its club_admin: %v", err)
	}
}

// TestCompany_ReachContactsAndPackage: the estimates pass through as they
// are published, the acceptance rate counts the answered requests only, the
// package keeps the feature order.
func TestCompany_ReachContactsAndPackage(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	counted := now.Add(-time.Hour)
	f.reach = outreach.OutreachStats{ActiveSegments: 2, Dispatches: 3,
		Segments: []outreach.SegmentEstimate{{ID: uuid.NewV7(), Name: "Bilgisayar", Estimate: outreach.PublicEstimate(137), CountedAt: &counted},
			{ID: uuid.NewV7(), Name: "Küçük", Estimate: outreach.PublicEstimate(4)}},
		ByStatus: map[outreach.DispatchStatus]int{outreach.DispatchCompleted: 3}, Eligible: outreach.PublicEstimate(260),
		Sent: outreach.PublicEstimate(9)}
	f.posts = feed.OrgPostStats{Published: 4, Targeted: 2, Likes: 31, Saves: 5}
	f.contacts = messaging.ContactRequestStats{Sent: 10, ByStatus: map[messaging.Status]int{messaging.StatusAccepted: 3,
		messaging.StatusDeclined: 1, messaging.StatusExpired: 2, messaging.StatusPending: 3, messaging.StatusWithdrawn: 1}}
	f.usage = []entitlements.FeatureUsage{{Feature: entitlements.FeatureTargetedPost, Total: 10, Used: 4, Remaining: 6}}
	for _, p := range []string{"company_member", "company_admin", "platform_admin"} {
		st, err := svc.Company(t.Context(), persona(t, p), authztest.ID("company_a"), stats.RangeQuery{})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		seg := st.Audience.Segments
		if len(seg) != 2 || *seg[0].Estimate.Count != 100 || seg[1].Estimate.Count != nil || !seg[1].Estimate.BelowThreshold {
			t.Fatalf("segments = %+v", seg)
		}
		if *st.Dispatches.Eligible.Count != 250 || !st.Dispatches.Sent.BelowThreshold || st.Dispatches.Count != 3 {
			t.Fatalf("dispatches = %+v", st.Dispatches)
		}
		if *st.ContactRequests.AcceptanceRate != 0.5 || st.Posts.Likes != 31 || st.Package[0].Remaining != 6 {
			t.Fatalf("company = %+v", st)
		}
	}
	_, err := svc.Company(t.Context(), persona(t, "university_rep"), authztest.ID("uni_a"), stats.RangeQuery{})
	wantStatus(t, err, http.StatusForbidden)
	_, err = svc.Company(t.Context(), persona(t, "platform_admin"), authztest.ID("uni_a"), stats.RangeQuery{})
	wantStatus(t, err, http.StatusNotFound)
	_, err = svc.Company(t.Context(), persona(t, "other_company_member"), authztest.ID("company_a"), stats.RangeQuery{})
	wantStatus(t, err, http.StatusNotFound)
}

// TestUniversity_SmallGroupsAndSensitiveClubs: student counts arrive with
// the groups below 10 suppressed and leave rounded to their bucket; a
// sensitive club is not asked for its events (but counts as a club and in
// the offering queue); participants below 10 have no number; the most
// active clubs come first with their names.
func TestUniversity_SmallGroupsAndSensitiveClubs(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	uniID := authztest.UniversityID("uni_a")
	a, s, u := authztest.ID("club_a"), authztest.ID("club_sensitive"), authztest.ID("club_unclaimed")
	f.uniClubs = directory.UniversityClubs{UniversityID: uniID, PendingClaims: 2, Clubs: []directory.ClubBrief{
		{OrgID: a, Claimed: true}, {OrgID: s, Sensitive: true, Claimed: true}, {OrgID: u}}}
	f.students = students.UniversityStudents{Total: ptr(1234), NewInRange: nil,
		ByDegreeLevel: []students.StudentGroup{{Key: "bachelor", Count: 1180}, {Key: "master", Count: 54}},
		ByClassYear:   []students.StudentGroup{{Key: "3", Count: 17}}}
	f.totals = map[uuid.UUID]events.EventTotals{a: {Events: 3, Registered: 47}, u: {Events: 5, Registered: 6}, s: {Events: 9, Registered: 90}}
	f.offerings = 4
	st, err := svc.University(t.Context(), persona(t, "university_rep"), authztest.ID("uni_a"), stats.RangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if f.studentAsk != uniID || slices.Contains(f.totalsAsk, s) || len(f.totalsAsk) != 2 || len(f.offerAsk) != 3 {
		t.Fatalf("asked students %v, events %v, offerings %v", f.studentAsk, f.totalsAsk, f.offerAsk)
	}
	sd := st.Students
	if *sd.Total.Count != 1200 || !sd.NewInRange.BelowThreshold ||
		!slices.Equal(sd.ByDegreeLevel, []stats.KeyEstimate{{Key: "bachelor", Count: 1100}, {Key: "master", Count: 50}}) ||
		!slices.Equal(sd.ByClassYear, []stats.KeyEstimate{{Key: "3", Count: 10}}) || sd.ByDepartment == nil {
		t.Fatalf("students = %+v", sd)
	}
	c := st.Clubs
	if c.Total != 3 || c.Claimed != 2 || c.ActiveInRange != 2 || c.EventsInRange != 8 || *c.Participants.Count != 50 {
		t.Fatalf("clubs = %+v", c)
	}
	if len(c.Items) != 2 || c.Items[0].OrgID != u.String() || c.Items[0].Name != "club_unclaimed" ||
		!c.Items[0].Participants.BelowThreshold || *c.Items[1].Participants.Count != 40 {
		t.Fatalf("club items = %+v", c.Items)
	}
	if st.Pending != (stats.UniversityPendingDTO{ClubClaims: 2, OfferingApprovals: 4}) {
		t.Fatalf("pending = %+v", st.Pending)
	}
	_, err = svc.University(t.Context(), persona(t, "university_rep"), authztest.ID("uni_b"), stats.RangeQuery{})
	wantStatus(t, err, http.StatusNotFound)
	_, err = svc.University(t.Context(), persona(t, "platform_admin"), authztest.ID("club_a"), stats.RangeQuery{})
	wantStatus(t, err, http.StatusNotFound)
}

// TestUniversity_WithoutDirectoryRow: a university org the directory does
// not know answers empty figures instead of failing.
func TestUniversity_WithoutDirectoryRow(t *testing.T) {
	t.Parallel()
	f, svc := world(t)
	f.uniErr = directory.ErrNotFound
	st, err := svc.University(t.Context(), persona(t, "platform_admin"), authztest.ID("uni_a"), stats.RangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Students.Total.BelowThreshold || st.Clubs.Total != 0 || st.Clubs.Items == nil || f.studentAsk != uuid.Nil() {
		t.Fatalf("stats = %+v", st)
	}
}
