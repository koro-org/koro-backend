package events_test

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/orgs"
)

// recommendations is the recommendations' read of the events service
// (BE-174), asserted like the other system reads.
func (e *env) recommendations(t *testing.T) events.Recommendations {
	t.Helper()
	r, ok := e.svc.(events.Recommendations)
	if !ok {
		t.Fatal("the events service does not implement Recommendations")
	}
	return r
}

// TestRecommendationSignals_NeverSensitive (BE-174, 03 Y2): the signals
// are the categories and organisers of the student's own held places, most
// events first; a sensitive club's event (and an event of a sensitive
// category, in the store) is never a signal; without the organisers'
// sensitivity nothing is answered (fail closed).
func TestRecommendationSignals_NeverSensitive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	clubA, clubB, clubSens, by, u := orgID("club_a"), orgID("club_b"), orgID("club_sensitive"), uuid.NewV7(), uuid.NewV7()
	held := func(org uuid.UUID, cat events.Category) {
		t.Helper()
		ev := row(org, by, "Sinyal", start)
		ev.Category = cat
		got, ok, err := e.mem.InsertEvent(ctx, ev)
		if err != nil || !ok {
			t.Fatalf("InsertEvent: %v %v", ok, err)
		}
		if _, err := e.mem.InsertRegistration(ctx, newReg(got, u, events.RegConfirmed)); err != nil {
			t.Fatal(err)
		}
	}
	held(clubA, events.CategoryWorkshop)
	held(clubA, events.CategoryWorkshop)
	held(clubB, events.CategoryCareer)
	held(clubSens, events.CategoryCulture) // a religious or political club's event: never a signal
	got, err := e.recommendations(t).RecommendationSignals(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Categories, []events.Category{events.CategoryWorkshop, events.CategoryCareer}) ||
		!slices.Equal(got.Organisers, []uuid.UUID{clubA, clubB}) {
		t.Fatalf("signals = %+v", got)
	}
	if got, err := e.recommendations(t).RecommendationSignals(ctx, uuid.NewV7()); err != nil || len(got.Categories) != 0 ||
		len(got.Organisers) != 0 {
		t.Fatalf("no participation = %+v, %v", got, err)
	}

	down := &atomic.Bool{}
	fe := newEnv(t, func(d *events.Deps) { d.Catalog = failingCatalog{fail: down} })
	ev, _, err := fe.mem.InsertEvent(ctx, row(clubA, by, "Dizin Yok", start))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fe.mem.InsertRegistration(ctx, newReg(ev, u, events.RegConfirmed)); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	if got, err := fe.recommendations(t).RecommendationSignals(ctx, u); err == nil {
		t.Fatalf("signals without directory = %+v, want an error (fail closed)", got)
	}
}

// TestRecommendableEvents_MatchesPopularAndLeavesOut (BE-174): the
// candidates are the events matching an organiser or a category plus the
// popular ones (10 held places), soonest first, as the student sees them
// (03 §3.6: another university's event is not visible), without the events
// of a sensitive club, of an org that is not active or that the student
// registered for; the window and the limit bound the answer.
func TestRecommendableEvents_MatchesPopularAndLeavesOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := persona(t, "student") // a verified student of uni_a
	clubA, clubB, clubSens, by := orgID("club_a"), orgID("club_b"), orgID("club_sensitive"), uuid.NewV7()
	day := func(n int) time.Time { return t0.Add(time.Duration(n) * 24 * time.Hour) }
	put := func(org uuid.UUID, cat events.Category, at time.Time, edit func(*events.Event)) events.Event {
		t.Helper()
		ev := row(org, by, "Öneri", at)
		ev.Category = cat
		if edit != nil {
			edit(&ev)
		}
		got, ok, err := e.mem.InsertEvent(ctx, ev)
		if err != nil || !ok {
			t.Fatalf("InsertEvent: %v %v", ok, err)
		}
		return got
	}
	followed := put(clubA, events.CategoryTalk, day(2), nil)
	sameCat := put(clubB, events.CategoryWorkshop, day(1), nil)
	popular := put(clubB, events.CategorySports, day(3), nil)
	for range events.PopularThreshold {
		if ok, err := e.mem.ClaimSeat(ctx, clubB, popular.ID); err != nil || !ok {
			t.Fatalf("ClaimSeat: %v %v", ok, err)
		}
	}
	put(clubB, events.CategorySports, day(4), nil)                       // matches nothing
	put(clubSens, events.CategoryWorkshop, day(5), nil)                  // a sensitive club
	put(clubA, events.CategoryTalk, day(40), nil)                        // after the window
	put(clubB, events.CategoryWorkshop, day(6), func(ev *events.Event) { // another university only
		u := orgID("uni_b")
		ev.Visibility, ev.VisibilityUniversityID = events.VisibilityUniversity, &u
	})
	registered := put(clubA, events.CategoryTalk, day(7), nil)
	if _, err := e.mem.InsertRegistration(ctx, newReg(registered, a.UserID, events.RegConfirmed)); err != nil {
		t.Fatal(err)
	}
	q := events.RecommendQuery{OrgIDs: []uuid.UUID{clubA}, Categories: []events.Category{events.CategoryWorkshop},
		From: t0, To: day(30), Limit: 10}
	got, err := e.recommendations(t).RecommendableEvents(ctx, a, q)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uuid.UUID{sameCat.ID, followed.ID, popular.ID}; !slices.Equal(rowIDs(got), want) {
		t.Fatalf("RecommendableEvents = %v, want %v", rowIDs(got), want)
	}
	if got[2].ConfirmedCount < events.PopularThreshold {
		t.Fatalf("popular row = %+v", got[2])
	}
	// The cards of the chosen rows: listEvents' views, in the given order.
	views, err := e.recommendations(t).EventCards(ctx, []events.Event{got[1], got[0]})
	if err != nil || !slices.Equal(viewIDs(views), []uuid.UUID{followed.ID, sameCat.ID}) || views[1].Org.ID != clubB {
		t.Fatalf("EventCards = %v, %v", viewIDs(views), err)
	}
	if card := events.CardOf(views[0]); card.EventID != followed.ID.String() || card.Org.Name == "" || !card.ShareableByOthers {
		t.Fatalf("CardOf = %+v", card)
	}
	if views, err := e.recommendations(t).EventCards(ctx, nil); err != nil || len(views) != 0 {
		t.Fatalf("EventCards(nil) = %v, %v", views, err)
	}

	// Popular events still come without a personal matcher.
	only, err := e.recommendations(t).RecommendableEvents(ctx, a, events.RecommendQuery{From: t0, To: day(30), Limit: 10})
	if err != nil || !slices.Equal(rowIDs(only), []uuid.UUID{popular.ID}) {
		t.Fatalf("popular only = %v, %v", rowIDs(only), err)
	}
	// An organiser that is not active is left out.
	e.orgs.setStatus(clubB, orgs.StatusSuspended)
	if got, err := e.recommendations(t).RecommendableEvents(ctx, a, q); err != nil || !slices.Equal(rowIDs(got), []uuid.UUID{followed.ID}) {
		t.Fatalf("suspended organiser = %v, %v", rowIDs(got), err)
	}
	if got, err := e.recommendations(t).RecommendableEvents(ctx, a, events.RecommendQuery{From: t0, To: t0, Limit: 10}); err != nil || len(got) != 0 {
		t.Fatalf("an empty window = %v, %v", rowIDs(got), err)
	}
}

func rowIDs(es []events.Event) []uuid.UUID {
	out := make([]uuid.UUID, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}
