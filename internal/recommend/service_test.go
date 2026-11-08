package recommend_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/obs"
	"koro/internal/recommend"
	"koro/internal/students"
)

var t0 = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// fakes is every port of recommend with recorded calls.
type fakes struct {
	mu        sync.Mutex
	calls     []string
	choice    students.RecommendationChoice
	popular   students.PopularClubs
	follows   []uuid.UUID
	sensitive map[uuid.UUID]bool
	sensErr   error
	uniOrgs   []uuid.UUID
	signals   events.RecommendationSignals
	rows      []events.Event
	carded    []uuid.UUID
	query     events.RecommendQuery
	cards     map[uuid.UUID]directory.ClubCard
	asked     []uuid.UUID
	calls2    []feed.OpportunityCandidate // the opportunity candidates (BE-191)
	callQuery feed.OpportunityQuery
	callErr   error
	callCards []uuid.UUID
	contacted []uuid.UUID // the calls of the student's contact requests (BE-198)
	contactQ  contactQuery
	contactE  error
}

// contactQuery is what ContactedOpportunityPostIDs was asked.
type contactQuery struct {
	user  uuid.UUID
	since time.Time
	limit int
}

func (f *fakes) call(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakes) RecommendationChoice(context.Context, uuid.UUID) (students.RecommendationChoice, error) {
	f.call("choice")
	return f.choice, nil
}

func (f *fakes) PopularClubs(context.Context, uuid.UUID, int) (students.PopularClubs, error) {
	f.call("popular")
	return f.popular, nil
}

func (f *fakes) FollowedOrgIDs(context.Context, uuid.UUID, int) ([]uuid.UUID, error) {
	f.call("follows")
	return f.follows, nil
}

func (f *fakes) SensitiveOrgIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	f.call("sensitive")
	if f.sensErr != nil {
		return nil, f.sensErr
	}
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if f.sensitive[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakes) UniversityOrgs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	f.call("university")
	return f.uniOrgs, nil
}

func (f *fakes) RecommendableClubs(_ context.Context, ids []uuid.UUID, limit int) ([]directory.ClubCard, error) {
	f.call("clubs")
	f.asked = ids
	out := []directory.ClubCard{}
	for _, id := range ids {
		if c, ok := f.cards[id]; ok && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakes) RecommendationSignals(context.Context, uuid.UUID) (events.RecommendationSignals, error) {
	f.call("signals")
	return f.signals, nil
}

func (f *fakes) RecommendableEvents(_ context.Context, _ authz.Actor, q events.RecommendQuery) ([]events.Event, error) {
	f.call("events")
	f.query = q
	return f.rows, nil
}

func (f *fakes) EventCards(_ context.Context, rows []events.Event) ([]events.EventView, error) {
	f.call("cards")
	out := make([]events.EventView, len(rows))
	f.carded = f.carded[:0]
	for i, e := range rows {
		f.carded = append(f.carded, e.ID)
		out[i] = events.EventView{Event: e, Org: orgs.OrgSummary{ID: e.OrgID, Kind: orgs.KindClub, Name: "Kulüp", Status: orgs.StatusActive}}
	}
	return out, nil
}

func (f *fakes) RecommendableOpportunities(_ context.Context, _ authz.Actor, q feed.OpportunityQuery) ([]feed.OpportunityCandidate, error) {
	f.call("opportunities")
	f.callQuery = q
	return f.calls2, f.callErr
}

func (f *fakes) OpportunityCards(_ context.Context, _ authz.Actor, rows []feed.OpportunityCandidate) ([]feed.PostView, error) {
	f.call("opportunity_cards")
	out := make([]feed.PostView, len(rows))
	f.callCards = f.callCards[:0]
	for i, c := range rows {
		f.callCards = append(f.callCards, c.PostID)
		out[i] = feed.PostView{Post: feed.Post{ID: c.PostID, OrgID: c.OrgID, Kind: feed.KindOpportunityCall},
			Org: orgs.OrgSummary{ID: c.OrgID, Kind: orgs.KindCompany, Name: "Şirket", Status: orgs.StatusActive}}
	}
	return out, nil
}

func (f *fakes) ContactedOpportunityPostIDs(_ context.Context, user uuid.UUID, since time.Time, limit int) ([]uuid.UUID, error) {
	f.call("contacted")
	f.contactQ = contactQuery{user, since, limit}
	return f.contacted, f.contactE
}

func newService(t *testing.T, f *fakes) recommend.Service {
	t.Helper()
	mod, err := recommend.New(recommend.Deps{Students: f, Events: f, Clubs: f, Follows: f, Opportunities: f,
		ContactRequests: f, Clock: clock.NewFake(t0), Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	return mod.Service
}

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func row(org uuid.UUID, cat events.Category, at time.Time, confirmed int) events.Event {
	return events.Event{ID: uuid.NewV7(), OrgID: org, Title: "Etkinlik", Category: cat, StartsAt: at, EndsAt: at.Add(time.Hour),
		Status: events.StatusPublished, Visibility: events.VisibilityPublic, ConfirmedCount: confirmed}
}

func card(org uuid.UUID, name string) directory.ClubCard {
	return directory.ClubCard{Row: directory.ClubRow{Club: directory.Club{OrgID: org}, Name: name, Slug: name, OrgStatus: "active"}}
}

func reasons(rs []recommend.Reason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

// TestList_VerifiedStudentsOnly (03 §5.24): the caller's own
// recommendations are a verified student's; an account without an active
// student verification, a company and platform staff are 403 before any
// read.
func TestList_VerifiedStudentsOnly(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"user_unverified", "company_member", "university_rep", "platform_admin", "dpo"} {
		f := &fakes{}
		_, err := newService(t, f).List(t.Context(), persona(t, name), recommend.Query{})
		if !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("%s: %v, want 403", name, err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("%s: read %v before the decision", name, f.calls)
		}
	}
	if _, err := newService(t, &fakes{}).List(t.Context(), authz.Anonymous(), recommend.Query{}); err == nil {
		t.Fatal("anonymous got recommendations")
	}
	for _, name := range []string{"student", "student_minor", "club_admin", "event_staff"} {
		if _, err := newService(t, &fakes{choice: students.RankingEnabled}).List(t.Context(), persona(t, name), recommend.Query{}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// TestList_OffReadsNothing (KVKK md. 11): turned off, or a minor's default,
// answers enabled=false with empty lists and reads no signal; an adult's
// default and a minor's own "enabled" are on.
func TestList_OffReadsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		persona string
		choice  students.RecommendationChoice
		on      bool
	}{
		{"student", students.RankingDefault, true},
		{"student", students.RankingDisabled, false},
		{"student_minor", students.RankingDefault, false},
		{"student_minor", students.RankingEnabled, true},
	} {
		f := &fakes{choice: tc.choice}
		r, err := newService(t, f).List(t.Context(), persona(t, tc.persona), recommend.Query{})
		if err != nil || r.Enabled != tc.on || r.Choice != tc.choice || r.Events == nil || r.Clubs == nil || r.Opportunities == nil {
			t.Fatalf("%s/%s = %+v, %v", tc.persona, tc.choice, r, err)
		}
		if !tc.on && !slices.Equal(f.calls, []string{"choice"}) {
			t.Fatalf("%s/%s off read %v", tc.persona, tc.choice, f.calls)
		}
		if got := recommend.Enabled(tc.choice, tc.persona == "student_minor"); got != tc.on {
			t.Fatalf("Enabled(%s) = %v", tc.choice, got)
		}
	}
}

// TestList_EventRules (BE-174): the candidates are asked for the student's
// non-sensitive followed and member orgs, the university's orgs and the
// top three attended categories in the next 30 days; each item carries the
// rules that hold, strongest first (starting_soon last), ranked by weight
// then by start, cut at the limit.
func TestList_EventRules(t *testing.T) {
	t.Parallel()
	a := persona(t, "club_member") // member of club_a and club_sensitive, student of uni_a
	clubA, sens, followed, uniOrg := authztest.ID("club_a"), authztest.ID("club_sensitive"), uuid.NewV7(), uuid.NewV7()
	f := &fakes{
		choice:    students.RankingDefault,
		follows:   []uuid.UUID{followed, sens},
		sensitive: map[uuid.UUID]bool{sens: true},
		uniOrgs:   []uuid.UUID{uniOrg, clubA},
		signals: events.RecommendationSignals{
			Categories: []events.Category{events.CategoryWorkshop, events.CategoryTalk, events.CategoryCareer, events.CategorySports},
		},
	}
	soon, later := t0.Add(24*time.Hour), t0.Add(10*24*time.Hour)
	followedWorkshop := row(followed, events.CategoryWorkshop, later, 0) // 4 + 2
	clubAPopular := row(clubA, events.CategoryCulture, later, 12)        // 4 + 2 (university) + 1
	uniSoon := row(uniOrg, events.CategorySocial, soon, 0)               // 2 + 1 (soon)
	popularOnly := row(uuid.NewV7(), events.CategoryTrip, later, events.PopularThreshold)
	sportsOnly := row(uuid.NewV7(), events.CategorySports, soon, 0) // 4th category: no rule holds
	f.rows = []events.Event{sportsOnly, popularOnly, uniSoon, followedWorkshop, clubAPopular}

	r, err := newService(t, f).List(t.Context(), a, recommend.Query{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	q := f.query
	if !q.From.Equal(t0) || !q.To.Equal(t0.Add(recommend.Horizon)) || q.Limit != recommend.Candidates ||
		!slices.Equal(q.Categories, []events.Category{events.CategoryWorkshop, events.CategoryTalk, events.CategoryCareer}) {
		t.Fatalf("query = %+v", q)
	}
	if slices.Contains(q.OrgIDs, sens) || !slices.Contains(q.OrgIDs, followed) || !slices.Contains(q.OrgIDs, clubA) ||
		!slices.Contains(q.OrgIDs, uniOrg) {
		t.Fatalf("query orgs = %v (the sensitive club is never a signal)", q.OrgIDs)
	}
	want := []struct {
		id      uuid.UUID
		reasons []string
	}{
		{clubAPopular.ID, []string{"followed_org", "same_university", "popular"}},
		{followedWorkshop.ID, []string{"followed_org", "attended_category"}},
		{uniSoon.ID, []string{"same_university", "starting_soon"}},
	}
	if len(r.Events) != len(want) || len(f.carded) != len(want) {
		t.Fatalf("events = %d (cards asked for %d), want %d: only the chosen ones become cards", len(r.Events), len(f.carded), len(want))
	}
	for i, w := range want {
		if r.Events[i].View.Event.ID != w.id || !slices.Equal(reasons(r.Events[i].Reasons), w.reasons) {
			t.Fatalf("event %d = %s %v, want %s %v", i, r.Events[i].View.Event.ID, r.Events[i].Reasons, w.id, w.reasons)
		}
	}
	r, err = newService(t, f).List(t.Context(), a, recommend.Query{Limit: 10})
	if err != nil || len(r.Events) != 4 || !slices.Equal(reasons(r.Events[3].Reasons), []string{"popular"}) {
		t.Fatalf("limit 10 = %+v, %v (popular last, the sports event without a rule left out)", r.Events, err)
	}
}

// TestList_ClubRules (BE-174): the clubs are the organisers of the
// student's events and the clubs popular in the department and the
// university, without the student's own (followed, member, sensitive
// included), ranked by weight then by the order the rules named them;
// directory keeps the recommendable ones.
func TestList_ClubRules(t *testing.T) {
	t.Parallel()
	a := persona(t, "club_member")
	mine, organiser, dept, uni, sens := authztest.ID("club_a"), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), authztest.ID("club_sensitive")
	f := &fakes{
		choice:    students.RankingEnabled,
		sensitive: map[uuid.UUID]bool{sens: true},
		signals:   events.RecommendationSignals{Organisers: []uuid.UUID{mine, organiser}},
		popular:   students.PopularClubs{Department: []uuid.UUID{dept, organiser, sens}, University: []uuid.UUID{uni, dept}},
		cards:     map[uuid.UUID]directory.ClubCard{organiser: card(organiser, "a"), uni: card(uni, "c"), dept: card(dept, "b")},
	}
	r, err := newService(t, f).List(t.Context(), a, recommend.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.asked, mine) || slices.Contains(f.asked, sens) {
		t.Fatalf("asked for the student's own clubs: %v", f.asked)
	}
	if !slices.Equal(f.asked, []uuid.UUID{organiser, dept, uni}) {
		t.Fatalf("asked = %v, want organiser (3+2), dept (2+1), uni (1)", f.asked)
	}
	if len(r.Clubs) != 3 || !slices.Equal(reasons(r.Clubs[0].Reasons), []string{"attended_organiser", "popular_in_department"}) ||
		!slices.Equal(reasons(r.Clubs[1].Reasons), []string{"popular_in_department", "popular_in_university"}) ||
		!slices.Equal(reasons(r.Clubs[2].Reasons), []string{"popular_in_university"}) {
		t.Fatalf("clubs = %+v", r.Clubs)
	}
}

// candidate is an opportunity call of org published at, with the flags and
// deadline feed computed.
func candidate(org uuid.UUID, at time.Time, deadline string, forYear, popular bool) feed.OpportunityCandidate {
	return feed.OpportunityCandidate{PostID: uuid.NewV7(), OrgID: org, PublishedAt: at, ForYourYear: forYear, Popular: popular,
		Opportunity: feed.Opportunity{Type: "internship", ApplyVia: "contact_request", Deadline: deadline}}
}

// TestList_OpportunityRules (BE-191): the opportunity candidates are asked
// for the student's non-sensitive followed and member orgs, the attended
// organisers and the university's orgs, published in the last 60 days;
// each item carries the rules that hold, strongest first (deadline_soon
// last, never alone), ranked by weight then by the newest, cut at the
// limit; only the chosen ones become cards.
func TestList_OpportunityRules(t *testing.T) {
	t.Parallel()
	a := persona(t, "club_member") // member of club_a and club_sensitive, student of uni_a
	clubA, sens, followed, organiser, uniOrg := authztest.ID("club_a"), authztest.ID("club_sensitive"), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	at := func(h int) time.Time { return t0.Add(-time.Duration(h) * time.Hour) }
	soon, later, today := "2026-11-08", "2026-11-20", "2026-11-05" // t0 is 2026-11-05 in Istanbul
	c1 := candidate(followed, at(9), soon, true, false)            // 4 + 2 + 1 (soon)
	c2 := candidate(organiser, at(8), later, false, true)          // 3 + 1
	c3 := candidate(clubA, at(7), "", false, false)                // 4 (member) + 2 (university)
	c4 := candidate(uniOrg, at(6), "", false, false)               // 2
	c5 := candidate(uuid.NewV7(), at(5), today, false, true)
	onlySensitive := candidate(sens, at(4), "", false, false)      // the sensitive membership is no rule
	onlySoon := candidate(uuid.NewV7(), at(3), soon, false, false) // deadline_soon never alone
	nothing := candidate(uuid.NewV7(), at(2), later, false, false) // no rule
	f := &fakes{
		choice:    students.RankingDefault,
		follows:   []uuid.UUID{followed, sens},
		sensitive: map[uuid.UUID]bool{sens: true},
		uniOrgs:   []uuid.UUID{uniOrg, clubA},
		signals:   events.RecommendationSignals{Organisers: []uuid.UUID{organiser}},
		calls2:    []feed.OpportunityCandidate{nothing, onlySoon, onlySensitive, c5, c4, c3, c2, c1},
	}
	r, err := newService(t, f).List(t.Context(), a, recommend.Query{Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	q := f.callQuery
	if !q.Since.Equal(t0.Add(-recommend.OpportunityWindow)) || q.Limit != recommend.OpportunityCandidates {
		t.Fatalf("query = %+v", q)
	}
	for _, id := range []uuid.UUID{followed, organiser, uniOrg, clubA} {
		if !slices.Contains(q.OrgIDs, id) {
			t.Fatalf("query orgs = %v, missing %s", q.OrgIDs, id)
		}
	}
	if slices.Contains(q.OrgIDs, sens) {
		t.Fatalf("query orgs = %v (the sensitive club is never a signal)", q.OrgIDs)
	}
	want := []struct {
		id      uuid.UUID
		reasons []string
	}{
		{c1.PostID, []string{"followed_org", "eligible_year", "deadline_soon"}},
		{c3.PostID, []string{"followed_org", "same_university"}},
		{c2.PostID, []string{"attended_organiser", "popular"}},
		{c5.PostID, []string{"popular", "deadline_soon"}},
	}
	if len(r.Opportunities) != len(want) || len(f.callCards) != len(want) {
		t.Fatalf("opportunities = %d (cards asked for %d), want %d: only the chosen ones become cards",
			len(r.Opportunities), len(f.callCards), len(want))
	}
	for i, w := range want {
		if got := r.Opportunities[i]; got.View.Post.ID != w.id || !slices.Equal(reasons(got.Reasons), w.reasons) {
			t.Fatalf("opportunity %d = %s %v, want %s %v", i, got.View.Post.ID, got.Reasons, w.id, w.reasons)
		}
	}
	r, err = newService(t, f).List(t.Context(), a, recommend.Query{Limit: 10})
	if err != nil || len(r.Opportunities) != 5 || r.Opportunities[4].View.Post.ID != c4.PostID ||
		!slices.Equal(reasons(r.Opportunities[4].Reasons), []string{"same_university"}) {
		t.Fatalf("limit 10 = %+v, %v (same score as c5, older: after it; the calls without a rule left out)", r.Opportunities, err)
	}
	dto := recommend.ToDTOForTest(r)
	if len(dto.Opportunities) != 5 || dto.Opportunities[0].Post.Kind != "opportunity_call" ||
		!slices.Equal(dto.Opportunities[0].Reasons, want[0].reasons) {
		t.Fatalf("dto = %+v", dto.Opportunities)
	}
}

// TestList_LeavesOutContactedCalls (BE-198): the calls a company already
// contacted the student about are read from the student's own requests
// (the caller's user ID, the same 60-day window, bounded) and handed to
// feed to leave out; an error of that read is returned.
func TestList_LeavesOutContactedCalls(t *testing.T) {
	t.Parallel()
	a := persona(t, "student")
	contacted := []uuid.UUID{uuid.NewV7(), uuid.NewV7()}
	kept := candidate(uuid.NewV7(), t0.Add(-time.Hour), "", false, true)
	f := &fakes{choice: students.RankingEnabled, contacted: contacted, calls2: []feed.OpportunityCandidate{kept}}
	r, err := newService(t, f).List(t.Context(), a, recommend.Query{})
	if err != nil {
		t.Fatal(err)
	}
	want := contactQuery{a.UserID, t0.Add(-recommend.OpportunityWindow), feed.MaxExcludedOpportunities}
	if f.contactQ != want {
		t.Fatalf("contact read = %+v, want %+v", f.contactQ, want)
	}
	if !slices.Equal(f.callQuery.Exclude, contacted) || !f.callQuery.Since.Equal(want.since) {
		t.Fatalf("query = %+v, want Exclude %v", f.callQuery, contacted)
	}
	if len(r.Opportunities) != 1 || r.Opportunities[0].View.Post.ID != kept.PostID {
		t.Fatalf("opportunities = %+v", r.Opportunities)
	}

	broken := &fakes{choice: students.RankingEnabled, contactE: errors.New("messaging down")}
	if r, err := newService(t, broken).List(t.Context(), a, recommend.Query{}); err == nil {
		t.Fatalf("List without the contact read = %+v, want an error", r)
	}
	if slices.Contains(broken.calls, "opportunities") {
		t.Fatal("the candidates were read after the contact read failed")
	}

	off := &fakes{choice: students.RankingDisabled}
	if _, err := newService(t, off).List(t.Context(), a, recommend.Query{}); err != nil || slices.Contains(off.calls, "contacted") {
		t.Fatalf("recommendations off: %v, calls %v (no signal is read)", err, off.calls)
	}
}

// TestList_OpportunityReadFailsClosed: an error of the opportunity read
// (the sensitivity of its authors included) is returned, nothing is
// recommended.
func TestList_OpportunityReadFailsClosed(t *testing.T) {
	t.Parallel()
	f := &fakes{choice: students.RankingEnabled, callErr: errors.New("directory down")}
	if r, err := newService(t, f).List(t.Context(), persona(t, "student"), recommend.Query{}); err == nil {
		t.Fatalf("List without the opportunity read = %+v, want an error", r)
	}
}

// TestList_FailClosedOnSensitivity (03 Y2): without the clubs' sensitivity
// nothing is recommended: the error is returned.
func TestList_FailClosedOnSensitivity(t *testing.T) {
	t.Parallel()
	f := &fakes{choice: students.RankingEnabled, follows: []uuid.UUID{uuid.NewV7()}, sensErr: errors.New("directory down")}
	if r, err := newService(t, f).List(t.Context(), persona(t, "student"), recommend.Query{}); err == nil {
		t.Fatalf("List without the sensitivity = %+v, want an error", r)
	}
}

// TestNew_NeedsEveryDependency: a missing port stops the start.
func TestNew_NeedsEveryDependency(t *testing.T) {
	t.Parallel()
	if _, err := recommend.New(recommend.Deps{}); err == nil {
		t.Fatal("New without dependencies succeeded")
	}
}
