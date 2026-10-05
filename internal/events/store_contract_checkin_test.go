package events_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/problem"
)

func TestStore_CheckinContract_Mem(t *testing.T) {
	t.Parallel()
	checkinContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// checkinContract is the check-in part of the store contract (05 §13.3;
// BE-031): both stores pass it.
func checkinContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	// setup: a published event of a fresh org with check-in on, n confirmed
	// registrations (seats taken) with one active ticket each.
	type fixture struct {
		s       events.Store
		w       world
		org     uuid.UUID
		staff   uuid.UUID
		e       events.Event
		regs    []events.Registration
		tickets []events.Ticket
	}
	setup := func(t *testing.T, s events.Store, w world, n int) fixture {
		t.Helper()
		ctx := t.Context()
		f := fixture{s: s, w: w, org: w.org(t), staff: w.user(t)}
		ev := row(f.org, w.user(t), "Kapı Sözleşmesi", start)
		ev.CheckinEnabled = true
		e, ok, err := s.InsertEvent(ctx, ev)
		if err != nil || !ok {
			t.Fatalf("InsertEvent: %v %v", ok, err)
		}
		f.e = e
		for i := range n {
			r, err := s.InsertRegistration(ctx, newReg(e, w.user(t), events.RegConfirmed))
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := s.ClaimSeat(ctx, f.org, e.ID); err != nil || !ok {
				t.Fatalf("ClaimSeat: %v %v", ok, err)
			}
			tk, ok, err := s.InsertTicket(ctx, f.org, newTicket(r, 1, "CHK00"+string(rune('A'+i))+"00"))
			if err != nil || !ok {
				t.Fatalf("InsertTicket: %v %v", ok, err)
			}
			f.regs, f.tickets = append(f.regs, r), append(f.tickets, tk)
		}
		return f
	}
	checkin := func(f fixture, i int, scan uuid.UUID, at time.Time) events.Checkin {
		return events.Checkin{ID: uuid.NewV7(), EventID: f.e.ID, OrgID: uuid.NewV7(), RegistrationID: f.regs[i].ID,
			TicketID: f.tickets[i].ID, StaffUserID: f.staff, Method: events.MethodQR, ClientScanID: scan, ScannedAt: at,
			ReceivedAt: at}
	}

	t.Run("check-in: confirmed only, once, scoped; client_scan_id unique; event and org from the registration", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		f := setup(t, s, w, 2)
		at := t0.Add(time.Hour)
		if _, ok, err := s.CheckIn(ctx, w.org(t), checkin(f, 0, uuid.NewV7(), at)); err != nil || ok {
			t.Fatalf("CheckIn(other org) = %v, %v", ok, err)
		}
		scan := uuid.NewV7()
		c, ok, err := s.CheckIn(ctx, f.org, checkin(f, 0, scan, at))
		if err != nil || !ok || c.OrgID != f.org || c.EventID != f.e.ID || c.VoidedAt != nil || !c.ScannedAt.Equal(at) {
			t.Fatalf("CheckIn = %+v, %v, %v", c, ok, err)
		}
		r, err := s.GetRegistration(ctx, f.org, f.e.ID, f.regs[0].ID)
		if err != nil || r.Status != events.RegCheckedIn || r.CheckedInAt == nil || !r.CheckedInAt.Equal(at) {
			t.Fatalf("registration = %+v, %v", r, err)
		}
		if _, ok, err := s.CheckIn(ctx, f.org, checkin(f, 0, uuid.NewV7(), at)); err != nil || ok {
			t.Fatalf("second CheckIn = %v, %v", ok, err)
		}
		_, _, err = s.CheckIn(ctx, f.org, checkin(f, 1, scan, at))
		if !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("taken client_scan_id = %v", err)
		}
		if r, _ := s.GetRegistration(ctx, f.org, f.e.ID, f.regs[1].ID); r.Status != events.RegConfirmed {
			t.Fatalf("a refused check-in changed the registration: %s", r.Status)
		}
		got, ok, err := s.CheckinByClientScan(ctx, f.org, f.e.ID, f.staff, scan)
		if err != nil || !ok || got.ID != c.ID {
			t.Fatalf("CheckinByClientScan = %+v, %v, %v", got, ok, err)
		}
		for _, x := range []struct{ org, event, staff uuid.UUID }{{w.org(t), f.e.ID, f.staff}, {f.org, uuid.NewV7(), f.staff},
			{f.org, f.e.ID, uuid.NewV7()}} {
			if _, ok, err := s.CheckinByClientScan(ctx, x.org, x.event, x.staff, scan); err != nil || ok {
				t.Fatalf("CheckinByClientScan(out of scope) = %v, %v", ok, err)
			}
		}
		if a, ok, err := s.ActiveCheckin(ctx, f.org, f.e.ID, f.regs[0].ID); err != nil || !ok || a.ID != c.ID {
			t.Fatalf("ActiveCheckin = %+v, %v, %v", a, ok, err)
		}
		if ev, err := s.GetEvent(ctx, f.e.ID); err != nil || ev.ConfirmedCount != 2 {
			t.Fatalf("seats after a check-in = %d, %v", ev.ConfirmedCount, err)
		}
	})

	t.Run("void: back to confirmed, once, scoped", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		f := setup(t, s, w, 1)
		c, _, err := s.CheckIn(ctx, f.org, checkin(f, 0, uuid.NewV7(), t0))
		if err != nil {
			t.Fatal(err)
		}
		by := w.user(t)
		if _, _, err := s.VoidCheckin(ctx, w.org(t), f.e.ID, c.ID, by, "Yanlış kişi", t0); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("VoidCheckin(other org) = %v", err)
		}
		v, r, err := s.VoidCheckin(ctx, f.org, f.e.ID, c.ID, by, "Yanlış kişi", t0.Add(time.Minute))
		if err != nil || v.VoidedAt == nil || v.VoidedBy == nil || *v.VoidedBy != by || v.VoidReason == nil ||
			r.Status != events.RegConfirmed || r.CheckedInAt != nil {
			t.Fatalf("VoidCheckin = %+v, %+v, %v", v, r, err)
		}
		if _, _, err := s.VoidCheckin(ctx, f.org, f.e.ID, c.ID, by, "Yanlış kişi", t0); !errors.Is(err, events.ErrInvalidState) {
			t.Fatalf("second void = %v", err)
		}
		if _, ok, _ := s.ActiveCheckin(ctx, f.org, f.e.ID, f.regs[0].ID); ok {
			t.Fatal("a voided check-in is active")
		}
		c2, ok, err := s.CheckIn(ctx, f.org, checkin(f, 0, uuid.NewV7(), t0.Add(2*time.Minute)))
		if err != nil || !ok {
			t.Fatalf("check-in after the void = %v, %v", ok, err)
		}
		page, err := s.ListCheckins(ctx, f.org, f.e.ID, nil, 1)
		if err != nil || len(page) != 1 || page[0].ID != c2.ID {
			t.Fatalf("ListCheckins = %+v, %v", page, err)
		}
		rest, err := s.ListCheckins(ctx, f.org, f.e.ID, &events.After{At: page[0].ScannedAt, ID: page[0].ID}, 10)
		if err != nil || len(rest) != 1 || rest[0].ID != c.ID {
			t.Fatalf("ListCheckins page 2 = %+v, %v", rest, err)
		}
		if other, err := s.ListCheckins(ctx, w.org(t), f.e.ID, nil, 10); err != nil || len(other) != 0 {
			t.Fatalf("ListCheckins(other org) = %+v, %v", other, err)
		}
		n, err := s.CheckinCounts(ctx, f.org, f.e.ID)
		if err != nil || n != (events.CheckinCounts{Confirmed: 1, CheckedIn: 1, Voided: 1}) {
			t.Fatalf("CheckinCounts = %+v, %v", n, err)
		}
		if mine, err := s.CheckinsOfRegistration(ctx, f.org, f.regs[0].ID); err != nil || len(mine) != 2 || mine[0].ID != c.ID {
			t.Fatalf("CheckinsOfRegistration = %+v, %v", mine, err)
		}
		if scans, err := s.CheckinsByStaff(ctx, f.staff); err != nil || len(scans) != 2 {
			t.Fatalf("CheckinsByStaff = %+v, %v", scans, err)
		}
		if err := s.DeleteRegistration(ctx, f.org, f.regs[0].ID); err != nil {
			t.Fatal(err)
		}
		if scans, err := s.CheckinsByStaff(ctx, f.staff); err != nil || len(scans) != 0 {
			t.Fatalf("check-ins after the registration's deletion = %+v, %v", scans, err)
		}
	})

	t.Run("tickets: by hash (any event), by manual code (scoped), by id", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		f := setup(t, s, w, 1)
		tk := f.tickets[0]
		got, ok, err := s.TicketByLookup(ctx, tk.TokenLookup)
		if err != nil || !ok || got.ID != tk.ID || got.EventID != f.e.ID {
			t.Fatalf("TicketByLookup = %+v, %v, %v", got, ok, err)
		}
		if _, ok, err := s.TicketByLookup(ctx, make([]byte, 32)); err != nil || ok {
			t.Fatalf("TicketByLookup(unknown) = %v, %v", ok, err)
		}
		if got, ok, err := s.TicketByManualCode(ctx, f.org, f.e.ID, tk.ManualCode); err != nil || !ok || got.ID != tk.ID {
			t.Fatalf("TicketByManualCode = %+v, %v, %v", got, ok, err)
		}
		for _, x := range []struct{ org, event uuid.UUID }{{w.org(t), f.e.ID}, {f.org, uuid.NewV7()}} {
			if _, ok, err := s.TicketByManualCode(ctx, x.org, x.event, tk.ManualCode); err != nil || ok {
				t.Fatalf("TicketByManualCode(out of scope) = %v, %v", ok, err)
			}
		}
		if got, err := s.GetTicket(ctx, tk.ID); err != nil || got.RegistrationID != f.regs[0].ID {
			t.Fatalf("GetTicket = %+v, %v", got, err)
		}
		if _, err := s.GetTicket(ctx, uuid.NewV7()); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("GetTicket(unknown) = %v", err)
		}
		if ev, err := s.KeyShareLockEvent(ctx, f.org, f.e.ID); err != nil || ev.ID != f.e.ID {
			t.Fatalf("KeyShareLockEvent = %+v, %v", ev, err)
		}
		if _, err := s.KeyShareLockEvent(ctx, w.org(t), f.e.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("KeyShareLockEvent(other org) = %v", err)
		}
	})

	t.Run("attendance: finalised once; no-shows in batches release their seats; due events", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		f := setup(t, s, w, 5)
		if _, _, err := s.CheckIn(ctx, f.org, checkin(f, 0, uuid.NewV7(), t0)); err != nil {
			t.Fatal(err)
		}
		end := f.e.EndsAt
		due, err := s.DueAttendance(ctx, end, uuid.Nil(), 100)
		if err != nil || !containsEvent(due, f.e.ID) {
			t.Fatalf("DueAttendance = %d events, %v", len(due), err)
		}
		if due, _ := s.DueAttendance(ctx, end.Add(-time.Second), uuid.Nil(), 100); containsEvent(due, f.e.ID) {
			t.Fatal("an event not ended yet is due")
		}
		quiet := setup(t, s, w, 1) // no check-in: never due
		if due, _ := s.DueAttendance(ctx, end, uuid.Nil(), 100); containsEvent(due, quiet.e.ID) {
			t.Fatal("an event without check-ins is due")
		}
		holders, err := s.SeatHolders(ctx, f.org, f.e.ID, uuid.Nil(), 3)
		if err != nil || len(holders) != 3 || compare(holders[0].ID, holders[1].ID) >= 0 {
			t.Fatalf("SeatHolders = %d, %v", len(holders), err)
		}
		if rest, _ := s.SeatHolders(ctx, f.org, f.e.ID, holders[2].ID, 10); len(rest) != 2 {
			t.Fatalf("SeatHolders after = %d", len(rest))
		}
		by := w.user(t)
		if ok, err := s.FinalizeAttendance(ctx, w.org(t), f.e.ID, &by, end); err != nil || ok {
			t.Fatalf("FinalizeAttendance(other org) = %v, %v", ok, err)
		}
		if ok, err := s.FinalizeAttendance(ctx, f.org, f.e.ID, &by, end); err != nil || !ok {
			t.Fatalf("FinalizeAttendance = %v, %v", ok, err)
		}
		if ok, err := s.FinalizeAttendance(ctx, f.org, f.e.ID, nil, end); err != nil || ok {
			t.Fatalf("second FinalizeAttendance = %v, %v", ok, err)
		}
		if ev, _ := s.GetEvent(ctx, f.e.ID); ev.AttendanceFinalizedAt == nil || ev.AttendanceFinalizedBy == nil || *ev.AttendanceFinalizedBy != by {
			t.Fatalf("event = %+v", ev)
		}
		if due, _ := s.DueAttendance(ctx, end, uuid.Nil(), 100); containsEvent(due, f.e.ID) {
			t.Fatal("a finalised event is due")
		}
		first, err := s.MarkNoShows(ctx, f.org, f.e.ID, 3)
		if err != nil || len(first) != 3 {
			t.Fatalf("MarkNoShows = %d, %v", len(first), err)
		}
		second, err := s.MarkNoShows(ctx, f.org, f.e.ID, 3)
		if err != nil || len(second) != 1 || second[0].Status != events.RegNoShow {
			t.Fatalf("MarkNoShows 2 = %+v, %v", second, err)
		}
		if again, _ := s.MarkNoShows(ctx, f.org, f.e.ID, 3); len(again) != 0 {
			t.Fatalf("MarkNoShows 3 = %d", len(again))
		}
		if other, _ := s.MarkNoShows(ctx, w.org(t), quiet.e.ID, 3); len(other) != 0 {
			t.Fatal("MarkNoShows crossed orgs")
		}
		ev, _ := s.GetEvent(ctx, f.e.ID)
		n, err := s.CheckinCounts(ctx, f.org, f.e.ID)
		if err != nil || ev.ConfirmedCount != 1 || n != (events.CheckinCounts{Confirmed: 1, CheckedIn: 1, NoShow: 4}) {
			t.Fatalf("confirmed_count %d, counts %+v, %v", ev.ConfirmedCount, n, err)
		}
	})
}

func containsEvent(es []events.Event, id uuid.UUID) bool {
	for _, e := range es {
		if e.ID == id {
			return true
		}
	}
	return false
}
