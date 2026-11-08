package recommend_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/recommend"
	"koro/internal/students"
)

// TestInterests_MapOntoTheModulesEnums (BE-204): every interest maps onto
// event categories and opportunity types of the owning modules' enums and
// onto the club category of its own name, which is never a sensitive one
// (religious, political, identity: special-category data is never a
// signal, KVKK md. 6).
func TestInterests_MapOntoTheModulesEnums(t *testing.T) {
	t.Parallel()
	all := recommend.InterestMatchesForTest()
	if len(all) != len(students.Interests()) {
		t.Fatalf("%d interests mapped, %d interests", len(all), len(students.Interests()))
	}
	for _, i := range students.Interests() {
		m, ok := all[i]
		if !ok {
			t.Fatalf("interest %s is not mapped", i)
		}
		if c := directory.Category(i); !c.Valid() || c.Sensitive() || c == directory.CategoryOther {
			t.Fatalf("interest %s is not a non-sensitive club category", i)
		}
		if len(m.Events) == 0 {
			t.Fatalf("interest %s matches no event category", i)
		}
		for _, c := range m.Events {
			if !c.Valid() {
				t.Fatalf("interest %s: %s is no event category", i, c)
			}
		}
		for _, o := range m.Opportunities {
			if !slices.Contains(feed.OpportunityTypes(), o) {
				t.Fatalf("interest %s: %s is no opportunity type", i, o)
			}
		}
	}
	for _, c := range directory.Categories {
		if c.Sensitive() && students.Interest(c).Valid() {
			t.Fatalf("the sensitive club category %s is an interest", c)
		}
	}
}

// TestDTO_ReasonEnumsMatchTheRules (BE-204): the OpenAPI enum of each
// item's reasons lists exactly the codes of its rules, in their order.
func TestDTO_ReasonEnumsMatchTheRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		typ   reflect.Type
		rules []recommend.Reason
	}{
		{reflect.TypeFor[recommend.RecommendedEventDTO](), recommend.EventReasons},
		{reflect.TypeFor[recommend.RecommendedClubDTO](), recommend.ClubReasons},
		{reflect.TypeFor[recommend.RecommendedOpportunityDTO](), recommend.OpportunityReasons},
	} {
		f, _ := tc.typ.FieldByName("Reasons")
		if got := strings.Split(f.Tag.Get("enum"), ","); !slices.Equal(got, reasons(tc.rules)) {
			t.Fatalf("%s reasons enum = %v, rules %v", tc.typ.Name(), got, tc.rules)
		}
	}
}

// TestList_MatchesInterest (BE-204, Q-57): the profile's interests ask
// events for their event categories (after the attended ones), directory
// for the clubs of their categories in the student's university and feed
// for their opportunity types; an item they match carries matches_interest
// (weight 2), a club another rule picked too when its category is one of
// them. Without interests nothing more is asked.
func TestList_MatchesInterest(t *testing.T) {
	t.Parallel()
	a := persona(t, "student") // verified student of uni_a
	interestClub, popularArts, popularOther := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	later := t0.Add(10 * 24 * time.Hour)
	sportsEvent := row(uuid.NewV7(), events.CategorySports, later, 0)
	careerEvent := row(uuid.NewV7(), events.CategoryCareer, later, 0)
	otherEvent := row(uuid.NewV7(), events.CategoryTrip, later, 0) // no rule
	volunteerCall := candidate(uuid.NewV7(), t0.Add(-time.Hour), "", false, false)
	volunteerCall.Opportunity.Type = "volunteer"
	internshipCall := candidate(uuid.NewV7(), t0.Add(-2*time.Hour), "", false, false) // no rule: internship is no interest's here
	arts := card(popularArts, "Resim")
	arts.Row.Category = directory.CategoryArts
	f := &fakes{
		choice:    students.RankingDefault,
		interests: []students.Interest{students.InterestSports, students.InterestSocialResponsibility, students.InterestArts},
		signals:   events.RecommendationSignals{Categories: []events.Category{events.CategoryCareer}},
		rows:      []events.Event{otherEvent, careerEvent, sportsEvent},
		inClubs:   []uuid.UUID{interestClub},
		popular:   students.PopularClubs{University: []uuid.UUID{popularOther, popularArts}},
		cards: map[uuid.UUID]directory.ClubCard{
			interestClub: card(interestClub, "Koşu"), popularArts: arts, popularOther: card(popularOther, "Diğer"),
		},
		calls2: []feed.OpportunityCandidate{internshipCall, volunteerCall},
	}
	r, err := newService(t, f).List(t.Context(), a, recommend.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []events.Category{events.CategoryCareer, events.CategorySports, events.CategoryVolunteering, events.CategoryCulture}; !slices.Equal(f.query.Categories, want) {
		t.Fatalf("event categories asked = %v, want %v", f.query.Categories, want)
	}
	if len(r.Events) != 2 || r.Events[0].View.Event.ID != sportsEvent.ID && r.Events[0].View.Event.ID != careerEvent.ID ||
		!slices.Equal(reasons(eventReasons(r, sportsEvent.ID)), []string{"matches_interest"}) ||
		!slices.Equal(reasons(eventReasons(r, careerEvent.ID)), []string{"attended_category"}) {
		t.Fatalf("events = %+v", r.Events)
	}
	wantCats := []directory.Category{directory.CategorySports, directory.CategorySocialResponsibility, directory.CategoryArts}
	if a.UniversityID == nil || f.inAsked.uni != *a.UniversityID || !slices.Equal(f.inAsked.cats, wantCats) || f.inAsked.limit != 20 {
		t.Fatalf("clubs asked = %+v, want the student's university and %v", f.inAsked, wantCats)
	}
	if len(r.Clubs) != 3 || r.Clubs[0].Card.Row.OrgID != popularArts ||
		!slices.Equal(reasons(r.Clubs[0].Reasons), []string{"matches_interest", "popular_in_university"}) ||
		r.Clubs[1].Card.Row.OrgID != interestClub || !slices.Equal(reasons(r.Clubs[1].Reasons), []string{"matches_interest"}) ||
		!slices.Equal(reasons(r.Clubs[2].Reasons), []string{"popular_in_university"}) {
		t.Fatalf("clubs = %+v (the arts club named by popularity carries its category's interest and ranks first)", r.Clubs)
	}
	if want := []string{"talent_call", "volunteer"}; !slices.Equal(f.callQuery.Types, want) {
		t.Fatalf("opportunity types asked = %v, want %v", f.callQuery.Types, want)
	}
	if len(r.Opportunities) != 1 || r.Opportunities[0].View.Post.ID != volunteerCall.PostID ||
		!slices.Equal(reasons(r.Opportunities[0].Reasons), []string{"matches_interest"}) {
		t.Fatalf("opportunities = %+v", r.Opportunities)
	}

	none := &fakes{choice: students.RankingDefault}
	if _, err := newService(t, none).List(t.Context(), a, recommend.Query{}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(none.calls, "interest_clubs") || len(none.query.Categories) != 0 || len(none.callQuery.Types) != 0 {
		t.Fatalf("without interests: calls %v, categories %v, types %v", none.calls, none.query.Categories, none.callQuery.Types)
	}
}

// TestList_InterestsOffOrMinorDefault (BE-204, BE-202): the interests are a
// signal like the others: recommendations off (a minor's default, which
// identity.MinorProtected extends to an unknown age) read no interest.
func TestList_InterestsOffOrMinorDefault(t *testing.T) {
	t.Parallel()
	f := &fakes{choice: students.RankingDefault, interests: []students.Interest{students.InterestSports}}
	r, err := newService(t, f).List(t.Context(), persona(t, "student_minor"), recommend.Query{})
	if err != nil || r.Enabled || slices.Contains(f.calls, "interests") {
		t.Fatalf("minor default = %+v, %v, calls %v", r, err, f.calls)
	}
}

func eventReasons(r recommend.Recommendations, id uuid.UUID) []recommend.Reason {
	for _, e := range r.Events {
		if e.View.Event.ID == id {
			return e.Reasons
		}
	}
	return nil
}
