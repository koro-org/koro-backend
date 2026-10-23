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
// cancelled and waitlisted registrations do not count); unknown IDs are
// missing.
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
