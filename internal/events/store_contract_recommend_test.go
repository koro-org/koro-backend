package events_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
)

func TestStore_RecommendContract_Mem(t *testing.T) {
	t.Parallel()
	recommendContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// recommendContract is the event reads of the recommendations (BE-174):
// the signals are the categories and organisers of the user's own held
// places (confirmed, checked in) in published or completed events, not of a
// sensitive category, since a date; the candidates are published events
// the viewer may see in a start window, not of a sensitive category, that
// the user has no registration of, matching an organiser, a category or
// the popularity floor.
func recommendContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	ids := func(es []events.Event) []uuid.UUID {
		out := make([]uuid.UUID, len(es))
		for i, e := range es {
			out[i] = e.ID
		}
		return out
	}
	t.Run("signals: own held places of non-sensitive events since", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		orgA, orgB, by, u, v := w.org(t), w.org(t), w.user(t), w.user(t), w.user(t)
		ev := func(org uuid.UUID, cat events.Category, at time.Time, edit func(*events.Event)) events.Event {
			t.Helper()
			e := row(org, by, "Sinyal "+string(cat), at)
			e.Category = cat
			if edit != nil {
				edit(&e)
			}
			return insertEvent(t, s, e)
		}
		reg := func(e events.Event, user uuid.UUID, st events.RegStatus) {
			t.Helper()
			first := events.RegConfirmed
			if st == events.RegWaitlisted {
				first = st
			}
			r, err := s.InsertRegistration(ctx, newReg(e, user, first))
			if err != nil {
				t.Fatal(err)
			}
			if st == events.RegNoShow {
				r.Status = st
				if _, err := s.UpdateRegistration(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
		}
		since := t0.Add(-30 * 24 * time.Hour)
		reg(ev(orgA, events.CategoryTalk, start, nil), u, events.RegConfirmed)
		reg(ev(orgA, events.CategoryWorkshop, start, nil), u, events.RegConfirmed)
		b := ev(orgB, events.CategoryWorkshop, start, nil)
		reg(b, u, events.RegConfirmed)
		reg(b, v, events.RegConfirmed)                                                           // another user's place is not u's signal
		reg(ev(orgA, events.CategorySports, since.Add(-time.Hour), nil), u, events.RegConfirmed) // before since
		reg(ev(orgB, events.CategoryCulture, start, func(e *events.Event) { e.IsSensitiveCategory = true }), u, events.RegConfirmed)
		reg(ev(orgB, events.CategorySocial, start, nil), u, events.RegNoShow)
		reg(ev(orgA, events.CategoryTrip, start, nil), u, events.RegWaitlisted)

		got, err := s.RecommendSignals(ctx, u, since)
		if err != nil {
			t.Fatal(err)
		}
		workshops := []uuid.UUID{orgA, orgB}
		slices.SortFunc(workshops, func(a, b uuid.UUID) int { return a.Compare(b) })
		want := []events.SignalRow{
			{Category: events.CategoryTalk, OrgID: orgA, Events: 1},
			{Category: events.CategoryWorkshop, OrgID: workshops[0], Events: 1},
			{Category: events.CategoryWorkshop, OrgID: workshops[1], Events: 1},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("RecommendSignals = %+v, want %+v", got, want)
		}
		reg(ev(orgB, events.CategoryWorkshop, start.Add(time.Hour), nil), u, events.RegConfirmed)
		if got, err := s.RecommendSignals(ctx, u, since); err != nil || len(got) != 3 ||
			got[0] != (events.SignalRow{Category: events.CategoryWorkshop, OrgID: orgB, Events: 2}) {
			t.Fatalf("most events first = %+v, %v", got, err)
		}
		if got, err := s.RecommendSignals(ctx, w.user(t), since); err != nil || len(got) != 0 {
			t.Fatalf("a user without registrations = %+v, %v", got, err)
		}
	})

	t.Run("recommendable: window, visibility, sensitivity, registration, matchers", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		orgA, orgB, member, by, u := w.org(t), w.org(t), w.org(t), w.user(t), w.user(t)
		uni, otherUni := w.university(t), w.university(t)
		from, to := t0, t0.Add(30*24*time.Hour)
		day := func(n int) time.Time { return t0.Add(time.Duration(n) * 24 * time.Hour) }
		ev := func(org uuid.UUID, cat events.Category, at time.Time, edit func(*events.Event)) events.Event {
			t.Helper()
			e := row(org, by, "Öneri "+string(cat), at)
			e.Category = cat
			if edit != nil {
				edit(&e)
			}
			return insertEvent(t, s, e)
		}
		byOrg := ev(orgA, events.CategoryTalk, day(1), nil)
		byCat := ev(orgB, events.CategoryWorkshop, day(2), nil)
		popular := ev(orgB, events.CategoryCulture, day(3), nil)
		for range 10 {
			if ok, err := s.ClaimSeat(ctx, orgB, popular.ID); err != nil || !ok {
				t.Fatalf("ClaimSeat: %v %v", ok, err)
			}
		}
		ev(orgB, events.CategoryCulture, day(4), nil) // matches nothing
		ev(orgA, events.CategoryTalk, day(5), func(e *events.Event) { e.IsSensitiveCategory = true })
		ev(orgA, events.CategoryTalk, day(40), nil)                   // after the window
		ev(orgA, events.CategoryTalk, from.Add(-time.Hour), nil)      // before the window
		ev(orgA, events.CategoryTalk, day(6), func(e *events.Event) { // another university's event
			e.Visibility, e.VisibilityUniversityID = events.VisibilityUniversity, &otherUni
		})
		ownUni := ev(orgA, events.CategoryTalk, day(7), func(e *events.Event) {
			e.Visibility, e.VisibilityUniversityID = events.VisibilityUniversity, &uni
		})
		members := ev(member, events.CategoryWorkshop, day(8), func(e *events.Event) { e.Visibility = events.VisibilityMembers })
		ev(orgB, events.CategoryWorkshop, day(9), func(e *events.Event) { e.Visibility = events.VisibilityMembers }) // not a member
		cancelledReg := ev(orgA, events.CategoryTalk, day(10), nil)
		r, err := s.InsertRegistration(ctx, newReg(cancelledReg, u, events.RegConfirmed))
		if err != nil {
			t.Fatal(err)
		}
		kind := events.ByUser
		r.Status, r.CancelledAt, r.CancelledByKind = events.RegCancelled, new(t0), &kind
		if _, err := s.UpdateRegistration(ctx, r); err != nil {
			t.Fatal(err)
		}
		ev(orgA, events.CategoryTalk, day(11), func(e *events.Event) { e.Status, e.PublishedAt = events.StatusDraft, nil })

		f := events.RecommendFilter{
			Viewer: events.Viewer{UniversityID: &uni, MemberOrgs: []uuid.UUID{member}}, UserID: u, From: from, To: to,
			OrgIDs: []uuid.UUID{orgA}, Categories: []events.Category{events.CategoryWorkshop},
		}
		got, err := s.RecommendableEvents(ctx, f, 10)
		if want := []uuid.UUID{byOrg.ID, byCat.ID, ownUni.ID, members.ID}; err != nil || !slices.Equal(ids(got), want) {
			t.Fatalf("personal = %v, %v; want %v", ids(got), err, want)
		}
		if got, err := s.RecommendableEvents(ctx, f, 2); err != nil || !slices.Equal(ids(got), []uuid.UUID{byOrg.ID, byCat.ID}) {
			t.Fatalf("limit 2 = %v, %v", ids(got), err)
		}
		pop := events.RecommendFilter{Viewer: f.Viewer, UserID: u, From: from, To: to, MinConfirmed: 10}
		if got, err := s.RecommendableEvents(ctx, pop, 10); err != nil || !slices.Equal(ids(got), []uuid.UUID{popular.ID}) {
			t.Fatalf("popular = %v, %v", ids(got), err)
		}
		pop.MinConfirmed = 11
		if got, err := s.RecommendableEvents(ctx, pop, 10); err != nil || len(got) != 0 {
			t.Fatalf("popular from 11 = %v, %v", ids(got), err)
		}
		if got, err := s.RecommendableEvents(ctx, events.RecommendFilter{Viewer: f.Viewer, UserID: u, From: from, To: to}, 10); err != nil || len(got) != 0 {
			t.Fatalf("no matcher = %v, %v", ids(got), err)
		}
		// Another user sees the event u cancelled a registration of.
		f.UserID = w.user(t)
		if got, err := s.RecommendableEvents(ctx, f, 10); err != nil || !slices.Contains(ids(got), cancelledReg.ID) {
			t.Fatalf("another user = %v, %v", ids(got), err)
		}
	})
}
