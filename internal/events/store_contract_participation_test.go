package events_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
)

func TestStore_ParticipationContract_Mem(t *testing.T) {
	t.Parallel()
	participationContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// participationContract is the participation read (BE-085; talent's gaming
// thresholds): registrations and check-ins map to their event; an event's
// figures count the confirmed, checked-in and no-show registrations and
// the distinct users with a valid check-in (a voided one does not count;
// cancelled and waitlisted registrations do not count); only a cancelled
// registration and a voided check-in are undone (BE-090); unknown IDs are
// missing. The targetable participations (BE-161) are the held places of
// public, non-sensitive-category events in the window.
func participationContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	t.Run("participation: events of sources, confirmed and distinct check-ins", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, staff := w.org(t), w.user(t)
		ev := row(org, w.user(t), "Katılım Sayımı", start)
		ev.CheckinEnabled = true
		e, ok, err := s.InsertEvent(ctx, ev)
		if err != nil || !ok {
			t.Fatalf("InsertEvent: %v %v", ok, err)
		}
		var regs []events.Registration
		var tickets []events.Ticket
		for i := range 6 {
			r, err := s.InsertRegistration(ctx, newReg(e, w.user(t), events.RegConfirmed))
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.ClaimSeat(ctx, org, e.ID); err != nil || !ok {
				t.Fatalf("ClaimSeat: %v %v", ok, err)
			}
			tk, ok, err := s.InsertTicket(ctx, org, newTicket(r, 1, "PRT00"+string(rune('A'+i))+"00"))
			if err != nil || !ok {
				t.Fatalf("InsertTicket: %v %v", ok, err)
			}
			regs, tickets = append(regs, r), append(tickets, tk)
		}
		checkIn := func(i int, at time.Time) events.Checkin {
			t.Helper()
			c, ok, err := s.CheckIn(ctx, org, events.Checkin{ID: uuid.NewV7(), EventID: e.ID, OrgID: org, RegistrationID: regs[i].ID,
				TicketID: tickets[i].ID, StaffUserID: staff, Method: events.MethodQR, ClientScanID: uuid.NewV7(), ScannedAt: at, ReceivedAt: at})
			if err != nil || !ok {
				t.Fatalf("CheckIn(%d): %v %v", i, ok, err)
			}
			return c
		}
		// 0 and 2 checked in; 1 checked in then voided (confirmed again);
		// 3 no-show; 4 cancelled; 5 confirmed.
		c0 := checkIn(0, t0)
		voided := checkIn(1, t0)
		if _, _, err := s.VoidCheckin(ctx, org, e.ID, voided.ID, staff, "Yanlış kişi", t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		checkIn(2, t0)
		noShow := regs[3]
		noShow.Status = events.RegNoShow
		if _, err := s.UpdateRegistration(ctx, noShow); err != nil {
			t.Fatal(err)
		}
		cancelled, by := regs[4], events.ByUser
		cancelled.Status, cancelled.CancelledAt, cancelled.CancelledByKind = events.RegCancelled, new(t0), &by
		if _, err := s.UpdateRegistration(ctx, cancelled); err != nil {
			t.Fatal(err)
		}
		if _, err := s.InsertRegistration(ctx, newReg(e, w.user(t), events.RegWaitlisted)); err != nil {
			t.Fatal(err)
		}

		// Another org's event with one confirmed registration; an event
		// without registrations.
		other := insertEvent(t, s, row(w.org(t), w.user(t), "Başka Etkinlik", start))
		otherReg, err := s.InsertRegistration(ctx, newReg(other, w.user(t), events.RegConfirmed))
		if err != nil {
			t.Fatal(err)
		}
		empty := insertEvent(t, s, row(org, w.user(t), "Boş Etkinlik", start))

		unknown := uuid.NewV7()
		of, err := s.EventsOf(ctx, []uuid.UUID{regs[0].ID, c0.ID, voided.ID, otherReg.ID, unknown})
		if err != nil {
			t.Fatal(err)
		}
		want := map[uuid.UUID]uuid.UUID{regs[0].ID: e.ID, c0.ID: e.ID, voided.ID: e.ID, otherReg.ID: other.ID}
		if len(of) != len(want) {
			t.Fatalf("EventsOf = %v, want %v", of, want)
		}
		for id, ev := range want {
			if of[id] != ev {
				t.Fatalf("EventsOf[%s] = %s, want %s", id, of[id], ev)
			}
		}
		if got, err := s.EventsOf(ctx, nil); err != nil || len(got) != 0 {
			t.Fatalf("EventsOf(nil) = %v, %v", got, err)
		}

		facts, err := s.ParticipationFacts(ctx, []uuid.UUID{e.ID, other.ID, empty.ID, unknown})
		if err != nil {
			t.Fatal(err)
		}
		wantFacts := map[uuid.UUID]events.ParticipationFacts{
			e.ID:     {OrgID: org, Confirmed: 5, DistinctCheckins: 2}, // 0, 1, 2, 3 and 5
			other.ID: {OrgID: other.OrgID, Confirmed: 1},
			empty.ID: {OrgID: org},
		}
		if len(facts) != len(wantFacts) {
			t.Fatalf("ParticipationFacts = %+v, want %+v", facts, wantFacts)
		}
		for id, f := range wantFacts {
			if facts[id] != f {
				t.Errorf("ParticipationFacts[%s] = %+v, want %+v", id, facts[id], f)
			}
		}

		// Undone (BE-090): the cancelled registration and the voided
		// check-in; a confirmed, checked-in, no-show or other org's row is
		// not, an unknown ID is missing.
		undone, err := s.Undone(ctx, []uuid.UUID{regs[0].ID, regs[1].ID, regs[3].ID, cancelled.ID, c0.ID, voided.ID, otherReg.ID, unknown})
		if err != nil {
			t.Fatal(err)
		}
		if len(undone) != 2 || !undone[cancelled.ID] || !undone[voided.ID] {
			t.Fatalf("Undone = %v, want the cancelled registration %s and the voided check-in %s", undone, cancelled.ID, voided.ID)
		}
		if got, err := s.Undone(ctx, nil); err != nil || len(got) != 0 {
			t.Fatalf("Undone(nil) = %v, %v", got, err)
		}
	})

	// BE-161: the held places of the asked users in published or
	// completed public events, not of a sensitive category, started at or
	// after since; ordered by user, newest start first. The organiser's
	// sensitivity is the service's (directory).
	t.Run("targetable participations: held places of public events since", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t), w.user(t)
		since := t0
		insert := func(title string, at time.Time, edit func(*events.Event)) events.Event {
			t.Helper()
			ev := row(org, by, title, at)
			edit(&ev)
			return insertEvent(t, s, ev)
		}
		reg := func(ev events.Event, user uuid.UUID, st events.RegStatus) {
			t.Helper()
			r := newReg(ev, user, st)
			switch st {
			case events.RegCheckedIn, events.RegNoShow:
				r.ConfirmedAt = new(t0)
				if st == events.RegCheckedIn {
					r.CheckedInAt = new(t0)
				}
			case events.RegCancelled:
				by := events.ByUser
				r.CancelledAt, r.CancelledByKind = new(t0), &by
			}
			if _, err := s.InsertRegistration(ctx, r); err != nil {
				t.Fatalf("InsertRegistration(%s, %s): %v", ev.Title, st, err)
			}
		}
		u1, u2, u3 := w.user(t), w.user(t), w.user(t)
		later := insert("Sonraki", start, func(*events.Event) {})
		earlier := insert("Önceki", t0.Add(24*time.Hour), func(ev *events.Event) { ev.Status = events.StatusCompleted })
		reg(later, u1, events.RegConfirmed)
		reg(earlier, u1, events.RegCheckedIn)
		reg(later, u2, events.RegNoShow)
		reg(earlier, u2, events.RegCancelled)
		reg(later, u3, events.RegConfirmed) // not asked for
		for _, ev := range []events.Event{
			insert("Üyelere Özel", start, func(ev *events.Event) { ev.Visibility = events.VisibilityMembers }),
			insert("Hassas Kategori", start, func(ev *events.Event) { ev.IsSensitiveCategory = true }),
			insert("İptal", start, func(ev *events.Event) { ev.Status, ev.CancelledAt = events.StatusCancelled, new(t0) }),
			insert("Eski", since.Add(-time.Hour), func(*events.Event) {}),
		} {
			reg(ev, u1, events.RegConfirmed)
		}
		waiting := insert("Bekleme", start, func(*events.Event) {})
		reg(waiting, u2, events.RegWaitlisted)

		got, err := s.TargetableParticipations(ctx, []uuid.UUID{u1, u2, uuid.NewV7()}, since)
		if err != nil {
			t.Fatal(err)
		}
		want := map[uuid.UUID][]events.TargetableRow{
			u1: {{UserID: u1, EventID: later.ID, OrgID: org}, {UserID: u1, EventID: earlier.ID, OrgID: org, Attended: true}},
			u2: {{UserID: u2, EventID: later.ID, OrgID: org}},
		}
		if len(got) != 3 {
			t.Fatalf("TargetableParticipations = %+v, want %+v", got, want)
		}
		for i := 1; i < len(got); i++ {
			if got[i-1].UserID.Compare(got[i].UserID) > 0 {
				t.Fatalf("rows not ordered by user: %+v", got)
			}
		}
		by2 := map[uuid.UUID][]events.TargetableRow{}
		for _, r := range got {
			by2[r.UserID] = append(by2[r.UserID], r)
		}
		for u, rows := range want {
			if len(by2[u]) != len(rows) {
				t.Fatalf("rows of %s = %+v, want %+v", u, by2[u], rows)
			}
			for i := range rows {
				if by2[u][i] != rows[i] {
					t.Errorf("row %d of %s = %+v, want %+v", i, u, by2[u][i], rows[i])
				}
			}
		}
		if got, err := s.TargetableParticipations(ctx, nil, since); err != nil || len(got) != 0 {
			t.Fatalf("TargetableParticipations(nil) = %v, %v", got, err)
		}
	})
}

// insertEvent writes e and fails the test otherwise.
func insertEvent(t *testing.T, s events.Store, e events.Event) events.Event {
	t.Helper()
	got, ok, err := s.InsertEvent(t.Context(), e)
	if err != nil || !ok {
		t.Fatalf("InsertEvent: %v %v", ok, err)
	}
	return got
}
