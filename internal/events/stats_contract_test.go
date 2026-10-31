package events_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
)

func TestStatsStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	statsContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// statsContract is the event counts of the panels (BE-163): upcoming,
// held and cancelled events with the places kept and the check-ins of the
// held ones (deltas: a fresh database may hold seed rows); one org's
// events of a range with their figures, drafts and other orgs left out,
// latest first and bounded.
func statsContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	from, to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	setup := func(t *testing.T, s events.Store, w world, org uuid.UUID) (held events.Event) {
		t.Helper()
		ctx := t.Context()
		insert := func(e events.Event) events.Event {
			t.Helper()
			got, ok, err := s.InsertEvent(ctx, e)
			if err != nil || !ok {
				t.Fatalf("InsertEvent: %v %v", ok, err)
			}
			return got
		}
		by := w.user(t)
		past := row(org, by, "Geçmiş", time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC))
		past.CheckinEnabled = true
		held = insert(past)
		insert(row(org, by, "Gelecek", time.Date(2026, 10, 25, 15, 0, 0, 0, time.UTC)))
		cancelled := row(org, by, "İptal", time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC))
		cancelled.Status, cancelled.CancelledAt = events.StatusCancelled, &t0
		insert(cancelled)
		draft := row(org, by, "Taslak", time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC))
		draft.Status, draft.PublishedAt = events.StatusDraft, nil
		insert(draft)
		insert(row(org, by, "Eski", time.Date(2026, 8, 1, 15, 0, 0, 0, time.UTC)))
		// Four places kept (one stays confirmed, two check in through a ticket
		// as the door does, one is a no-show) and one waitlisted registration.
		at, staff := t0, w.user(t)
		for i, st := range []events.RegStatus{events.RegConfirmed, events.RegCheckedIn, events.RegCheckedIn, events.RegNoShow,
			events.RegWaitlisted} {
			in := st
			if st == events.RegCheckedIn {
				in = events.RegConfirmed
			}
			r := newReg(held, w.user(t), in)
			if st == events.RegNoShow {
				r.ConfirmedAt = &at
			}
			r, err := s.InsertRegistration(ctx, r)
			if err != nil {
				t.Fatalf("InsertRegistration %s: %v", st, err)
			}
			if st != events.RegCheckedIn {
				continue
			}
			if ok, err := s.ClaimSeat(ctx, org, held.ID); err != nil || !ok {
				t.Fatalf("ClaimSeat: %v %v", ok, err)
			}
			tk, ok, err := s.InsertTicket(ctx, org, newTicket(r, 1, "STAT000"+string(rune('0'+i))))
			if err != nil || !ok {
				t.Fatalf("InsertTicket: %v %v", ok, err)
			}
			if _, ok, err := s.CheckIn(ctx, org, events.Checkin{ID: uuid.NewV7(), EventID: held.ID, OrgID: org, RegistrationID: r.ID,
				TicketID: tk.ID, StaffUserID: staff, Method: events.MethodQR, ClientScanID: uuid.NewV7(), ScannedAt: at,
				ReceivedAt: at}); err != nil || !ok {
				t.Fatalf("CheckIn: %v %v", ok, err)
			}
		}
		return held
	}

	t.Run("event stats", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		before, err := s.EventStats(ctx, from, to, now)
		if err != nil {
			t.Fatal(err)
		}
		setup(t, s, w, w.org(t))
		after, err := s.EventStats(ctx, from, to, now)
		if err != nil {
			t.Fatal(err)
		}
		d := events.EventStats{Upcoming: after.Upcoming - before.Upcoming, Held: after.Held - before.Held,
			Cancelled: after.Cancelled - before.Cancelled, Registered: after.Registered - before.Registered,
			CheckedIn: after.CheckedIn - before.CheckedIn}
		if want := (events.EventStats{Upcoming: 1, Held: 1, Cancelled: 1, Registered: 4, CheckedIn: 2}); d != want {
			t.Fatalf("event stats delta = %+v, want %+v", d, want)
		}
	})

	t.Run("org event figures", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other := w.org(t), w.org(t)
		held := setup(t, s, w, org)
		setup(t, s, w, other)
		got, err := s.OrgEventFigures(ctx, []uuid.UUID{org}, from, to, 10)
		if err != nil {
			t.Fatal(err)
		}
		titles := make([]string, len(got))
		for i, f := range got {
			titles[i] = f.Title
			if f.OrgID != org {
				t.Fatalf("figures of another org: %+v", f)
			}
		}
		if want := []string{"Gelecek", "İptal", "Geçmiş"}; len(titles) != 3 || titles[0] != want[0] || titles[1] != want[1] || titles[2] != want[2] {
			t.Fatalf("titles = %v, want %v", titles, want)
		}
		h := got[2]
		if h.EventID != held.ID || h.Registered != 4 || h.CheckedIn != 2 || h.NoShow != 1 || h.Status != events.StatusPublished {
			t.Fatalf("held figures = %+v", h)
		}
		if two, err := s.OrgEventFigures(ctx, []uuid.UUID{org, other}, from, to, 4); err != nil || len(two) != 4 {
			t.Fatalf("bounded = %d, %v", len(two), err)
		}
		if none, err := s.OrgEventFigures(ctx, []uuid.UUID{uuid.NewV7()}, from, to, 10); err != nil || len(none) != 0 {
			t.Fatalf("unknown org = %v, %v", none, err)
		}
	})

	t.Run("org event totals", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, quiet := w.org(t), w.org(t), w.org(t)
		setup(t, s, w, org)
		setup(t, s, w, other)
		// A sensitive-category event of org counts nowhere.
		sens := row(org, w.user(t), "Hassas", time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC))
		sens.IsSensitiveCategory = true
		got, ok, err := s.InsertEvent(ctx, sens)
		if err != nil || !ok {
			t.Fatalf("InsertEvent: %v %v", ok, err)
		}
		if _, err := s.InsertRegistration(ctx, newReg(got, w.user(t), events.RegConfirmed)); err != nil {
			t.Fatal(err)
		}
		totals, err := s.OrgEventTotals(ctx, []uuid.UUID{org, quiet}, from, to, now)
		if err != nil {
			t.Fatal(err)
		}
		// Past and upcoming count as events; the cancelled, the draft, the old and the sensitive do not;
		// the places are the past event's (the upcoming one has none).
		if len(totals) != 1 || totals[org] != (events.EventTotals{Events: 2, Registered: 4}) {
			t.Fatalf("totals = %+v", totals)
		}
	})
}
