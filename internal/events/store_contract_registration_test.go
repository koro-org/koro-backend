package events_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/problem"
)

func TestStore_RegistrationContract_Mem(t *testing.T) {
	t.Parallel()
	registrationContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// newReg is a confirmed registration of user for event e (answers purged
// 90 days after the event).
func newReg(e events.Event, user uuid.UUID, st events.RegStatus) events.Registration {
	at := t0
	r := events.Registration{ID: uuid.NewV7(), EventID: e.ID, OrgID: e.OrgID, UserID: user, Status: st, Source: events.SourceWeb,
		Answers: events.JSON(`{"motivation":"x"}`), AnswersPurgeAt: e.EndsAt.Add(90 * 24 * time.Hour)}
	switch st {
	case events.RegConfirmed:
		r.ConfirmedAt = &at
	case events.RegWaitlisted:
		r.WaitlistedAt = &at
	}
	return r
}

func newTicket(r events.Registration, version int, code string) events.Ticket {
	sum := sha256.Sum256([]byte(r.ID.String() + code))
	return events.Ticket{ID: uuid.NewV7(), RegistrationID: r.ID, Version: version, KeyID: "t1", TokenLookup: sum[:],
		ManualCode: code, IssuedAt: t0}
}

// registrationContract is the registration part of the store contract
// (05 §13.3; BE-028).
func registrationContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	event := func(t *testing.T, s events.Store, org, by uuid.UUID, capacity *int) events.Event {
		t.Helper()
		e := row(org, by, "Kayıt Sözleşmesi", start)
		e.Capacity = capacity
		got, ok, err := s.InsertEvent(t.Context(), e)
		if err != nil || !ok {
			t.Fatalf("InsertEvent: %v %v", ok, err)
		}
		return got
	}
	insert := func(t *testing.T, s events.Store, r events.Registration) events.Registration {
		t.Helper()
		got, err := s.InsertRegistration(t.Context(), r)
		if err != nil {
			t.Fatalf("InsertRegistration: %v", err)
		}
		return got
	}

	t.Run("seats: conditional claim, published only, scoped, never below zero", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		e := event(t, s, org, by, new(2))
		if ok, err := s.ClaimSeat(ctx, other, e.ID); err != nil || ok {
			t.Fatalf("ClaimSeat(other org) = %v, %v", ok, err)
		}
		for i := range 2 {
			if ok, err := s.ClaimSeat(ctx, org, e.ID); err != nil || !ok {
				t.Fatalf("seat %d = %v, %v", i, ok, err)
			}
		}
		if ok, err := s.ClaimSeat(ctx, org, e.ID); err != nil || ok {
			t.Fatalf("third seat of two = %v, %v", ok, err)
		}
		for range 3 {
			if err := s.ReleaseSeat(ctx, org, e.ID); err != nil {
				t.Fatal(err)
			}
		}
		if got, _ := s.GetEvent(ctx, e.ID); got.ConfirmedCount != 0 {
			t.Fatalf("count after releases = %d", got.ConfirmedCount)
		}
		cancelled := e
		cancelled.Status, cancelled.CancelledAt, cancelled.CancelReason = events.StatusCancelled, new(t0), new("İptal")
		if _, err := s.UpdateEvent(ctx, cancelled); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.ClaimSeat(ctx, org, e.ID); err != nil || ok {
			t.Fatalf("ClaimSeat(cancelled) = %v, %v", ok, err)
		}
		if _, err := s.ShareLockEvent(ctx, other, e.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("ShareLockEvent(other org) = %v", err)
		}
		if got, err := s.ShareLockEvent(ctx, org, e.ID); err != nil || got.Status != events.StatusCancelled {
			t.Fatalf("ShareLockEvent = %+v, %v", got, err)
		}
		if err := s.SetConfirmedCount(ctx, org, e.ID, 3); !errors.Is(err, events.ErrCapacityFull) {
			t.Fatalf("SetConfirmedCount above the capacity = %v", err)
		}
	})

	t.Run("registrations: org from the event, one live per user, scoped reads", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by, u := w.org(t), w.org(t), w.user(t), w.user(t)
		e := event(t, s, org, by, nil)
		wrong := newReg(e, u, events.RegConfirmed)
		wrong.OrgID = other
		if _, err := s.InsertRegistration(ctx, wrong); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("InsertRegistration(other org) = %v", err)
		}
		r := insert(t, s, newReg(e, u, events.RegConfirmed))
		if r.OrgID != org || r.EventID != e.ID || r.CreatedAt.IsZero() || string(r.ConsentSnapshot) != "{}" {
			t.Fatalf("stored = %+v", r)
		}
		if _, err := s.InsertRegistration(ctx, newReg(e, u, events.RegWaitlisted)); !errors.Is(err, events.ErrAlreadyRegistered) {
			t.Fatalf("second live registration = %v", err)
		}
		if _, err := s.GetRegistration(ctx, other, e.ID, r.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("GetRegistration(other org) = %v", err)
		}
		if _, err := s.LockRegistration(ctx, org, uuid.NewV7(), r.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("LockRegistration(other event) = %v", err)
		}
		if _, err := s.GetUserRegistration(ctx, by, r.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("GetUserRegistration(another user) = %v", err)
		}
		if got, err := s.GetUserRegistration(ctx, u, r.ID); err != nil || got.ID != r.ID {
			t.Fatalf("GetUserRegistration = %+v, %v", got, err)
		}
		if got, ok, err := s.ActiveRegistration(ctx, org, e.ID, u); err != nil || !ok || got.ID != r.ID {
			t.Fatalf("ActiveRegistration = %+v, %v, %v", got, ok, err)
		}
		// A cancellation needs its time and kind (CHECK).
		bad := r
		bad.Status = events.RegCancelled
		if _, err := s.UpdateRegistration(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("cancel without cancelled_at = %v", err)
		}
		by2 := events.ByUser
		bad.CancelledAt, bad.CancelledByKind = new(t0), &by2
		if got, err := s.UpdateRegistration(ctx, bad); err != nil || got.Status != events.RegCancelled {
			t.Fatalf("UpdateRegistration = %+v, %v", got, err)
		}
		if _, ok, err := s.ActiveRegistration(ctx, org, e.ID, u); err != nil || ok {
			t.Fatalf("ActiveRegistration after the cancellation = %v, %v", ok, err)
		}
		again := insert(t, s, newReg(e, u, events.RegConfirmed))
		if n, err := s.CountUserRegistrationsInOrg(ctx, org, u); err != nil || n != 1 {
			t.Fatalf("CountUserRegistrationsInOrg = %d, %v", n, err)
		}
		if n, err := s.CountSeats(ctx, org, e.ID); err != nil || n != 1 {
			t.Fatalf("CountSeats = %d, %v", n, err)
		}
		rs, err := s.RegistrationsOfUser(ctx, u)
		if err != nil || len(rs) != 2 || rs[0].ID != r.ID || rs[1].ID != again.ID {
			t.Fatalf("RegistrationsOfUser = %+v, %v", rs, err)
		}
		evs, err := s.GetEvents(ctx, []uuid.UUID{e.ID, uuid.NewV7()})
		if err != nil || len(evs) != 1 || evs[e.ID].Title != e.Title {
			t.Fatalf("GetEvents = %+v, %v", evs, err)
		}
		if err := s.DeleteRegistration(ctx, other, again.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("DeleteRegistration(other org) = %v", err)
		}
		if err := s.DeleteRegistration(ctx, org, again.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetUserRegistration(ctx, u, again.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("deleted registration = %v", err)
		}
	})

	t.Run("waiting list: FIFO, positions, expiry after the start", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t), w.user(t)
		e := event(t, s, org, by, new(1))
		var ids []uuid.UUID
		for i := range 3 {
			r := newReg(e, w.user(t), events.RegWaitlisted)
			at := t0.Add(time.Duration(i) * time.Minute)
			r.WaitlistedAt = &at
			ids = append(ids, insert(t, s, r).ID)
		}
		next, ok, err := s.NextWaitlisted(ctx, org, e.ID)
		if err != nil || !ok || next.ID != ids[0] {
			t.Fatalf("NextWaitlisted = %+v, %v, %v", next, ok, err)
		}
		for i, id := range ids {
			if n, err := s.WaitlistPosition(ctx, org, e.ID, id); err != nil || n != i+1 {
				t.Fatalf("position of %d = %d, %v", i, n, err)
			}
		}
		if _, ok, err := s.NextWaitlisted(ctx, w.org(t), e.ID); err != nil || ok {
			t.Fatalf("NextWaitlisted(other org) = %v, %v", ok, err)
		}
		locked, err := s.LockActiveRegistrations(ctx, org, e.ID, 2)
		if err != nil || len(locked) != 2 {
			t.Fatalf("LockActiveRegistrations = %d, %v", len(locked), err)
		}
		if got, err := s.ExpireWaitlisted(ctx, start.Add(-time.Minute), 10); err != nil || len(got) != 0 {
			t.Fatalf("ExpireWaitlisted before the start = %d, %v", len(got), err)
		}
		got, err := s.ExpireWaitlisted(ctx, start, 2)
		if err != nil || len(got) != 2 || got[0].Status != events.RegExpired {
			t.Fatalf("ExpireWaitlisted = %+v, %v", got, err)
		}
		rest, err := s.ExpireWaitlisted(ctx, start, 10)
		if err != nil || len(rest) != 1 || rest[0].ID != ids[2] {
			t.Fatalf("second batch = %+v, %v", rest, err)
		}
		if n, err := s.WaitlistPosition(ctx, org, e.ID, ids[0]); err != nil || n != 0 {
			t.Fatalf("position of an expired registration = %d, %v", n, err)
		}
	})

	t.Run("lists: by creation with keyset; own by the event's start", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by, u := w.org(t), w.user(t), w.user(t)
		late := event(t, s, org, by, nil)
		early := row(org, by, "Erken Etkinlik", start.Add(-24*time.Hour))
		if _, ok, err := s.InsertEvent(ctx, early); err != nil || !ok {
			t.Fatal(ok, err)
		}
		var all []events.Registration
		for range 3 {
			all = append(all, insert(t, s, newReg(late, w.user(t), events.RegConfirmed)))
		}
		mine := insert(t, s, newReg(late, u, events.RegWaitlisted))
		all = append(all, mine)
		mineEarly := insert(t, s, newReg(early, u, events.RegConfirmed))
		page1, err := s.ListEventRegistrations(ctx, org, late.ID, events.RegFilter{}, nil, 2)
		if err != nil || len(page1) != 2 || page1[0].ID != all[0].ID || page1[1].ID != all[1].ID {
			t.Fatalf("page 1 = %+v, %v", page1, err)
		}
		page2, err := s.ListEventRegistrations(ctx, org, late.ID, events.RegFilter{},
			&events.After{At: page1[1].CreatedAt, ID: page1[1].ID}, 10)
		if err != nil || len(page2) != 2 || page2[0].ID != all[2].ID || page2[1].ID != mine.ID {
			t.Fatalf("page 2 = %+v, %v", page2, err)
		}
		desc, err := s.ListEventRegistrations(ctx, org, late.ID, events.RegFilter{Desc: true,
			Statuses: []events.RegStatus{events.RegConfirmed}}, nil, 10)
		if err != nil || len(desc) != 3 || desc[0].ID != all[2].ID {
			t.Fatalf("newest confirmed = %+v, %v", desc, err)
		}
		if got, err := s.ListEventRegistrations(ctx, w.org(t), late.ID, events.RegFilter{}, nil, 10); err != nil || len(got) != 0 {
			t.Fatalf("other org's list = %+v, %v", got, err)
		}
		own, err := s.ListUserRegistrations(ctx, u, events.MyRegFilter{}, nil, 10)
		if err != nil || len(own) != 2 || own[0].ID != mineEarly.ID || own[1].ID != mine.ID {
			t.Fatalf("own list = %+v, %v", own, err)
		}
		after := start.Add(-12 * time.Hour)
		upcoming, err := s.ListUserRegistrations(ctx, u, events.MyRegFilter{EndsAfter: &after,
			Statuses: []events.RegStatus{events.RegWaitlisted}}, nil, 10)
		if err != nil || len(upcoming) != 1 || upcoming[0].ID != mine.ID {
			t.Fatalf("upcoming waitlisted = %+v, %v", upcoming, err)
		}
		next, err := s.ListUserRegistrations(ctx, u, events.MyRegFilter{}, &events.After{At: early.StartsAt, ID: mineEarly.ID}, 10)
		if err != nil || len(next) != 1 || next[0].ID != mine.ID {
			t.Fatalf("own page 2 = %+v, %v", next, err)
		}
	})

	t.Run("counts and purge dates: reconciliation reads, shift with the event end", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t), w.user(t)
		e := event(t, s, org, by, new(5))
		r := newReg(e, w.user(t), events.RegConfirmed)
		r.AnswersSensitiveEnc, r.SensitivePurgeAt = []byte{1, 2, 3}, new(e.EndsAt.Add(30*24*time.Hour))
		insert(t, s, r)
		plain := insert(t, s, newReg(e, w.user(t), events.RegWaitlisted))
		counts, err := s.SeatCounts(ctx, t0, uuid.Nil(), 1000)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(counts, func(c events.SeatCount) bool { return c.EventID == e.ID })
		if i < 0 || counts[i].Stored != 0 || counts[i].Actual != 1 || counts[i].OrgID != org {
			t.Fatalf("SeatCounts = %+v", counts)
		}
		if got, _ := s.SeatCounts(ctx, e.EndsAt, uuid.Nil(), 1000); slices.ContainsFunc(got, func(c events.SeatCount) bool { return c.EventID == e.ID }) {
			t.Fatal("an event ending before endsAfter is counted")
		}
		if err := s.SetConfirmedCount(ctx, org, e.ID, 1); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetEvent(ctx, e.ID); got.ConfirmedCount != 1 {
			t.Fatalf("confirmed_count = %d", got.ConfirmedCount)
		}
		answers, sens := t0.Add(1000*time.Hour), t0.Add(500*time.Hour)
		if err := s.ShiftPurgeDates(ctx, org, e.ID, answers, sens); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetRegistration(ctx, org, e.ID, r.ID)
		if !got.AnswersPurgeAt.Equal(answers) || got.SensitivePurgeAt == nil || !got.SensitivePurgeAt.Equal(sens) ||
			!bytes.Equal(got.AnswersSensitiveEnc, []byte{1, 2, 3}) {
			t.Fatalf("shifted = %+v", got)
		}
		got, _ = s.GetRegistration(ctx, org, e.ID, plain.ID)
		if !got.AnswersPurgeAt.Equal(answers) || got.SensitivePurgeAt != nil {
			t.Fatalf("shifted plain = %+v", got)
		}
	})

	t.Run("tickets: event from the registration, one active, manual codes per event", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t), w.org(t), w.user(t)
		e := event(t, s, org, by, nil)
		r := insert(t, s, newReg(e, w.user(t), events.RegConfirmed))
		r2 := insert(t, s, newReg(e, w.user(t), events.RegConfirmed))
		if _, _, err := s.InsertTicket(ctx, other, newTicket(r, 1, "7KQ2M9TX")); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("InsertTicket(other org) = %v", err)
		}
		t1, ok, err := s.InsertTicket(ctx, org, newTicket(r, 1, "7KQ2M9TX"))
		if err != nil || !ok || t1.EventID != e.ID || t1.Status != events.TicketActive || t1.Version != 1 {
			t.Fatalf("InsertTicket = %+v, %v, %v", t1, ok, err)
		}
		if _, ok, err := s.InsertTicket(ctx, org, newTicket(r2, 1, "7KQ2M9TX")); err != nil || ok {
			t.Fatalf("a taken manual code = %v, %v", ok, err)
		}
		if got, ok, err := s.ActiveTicket(ctx, org, r.ID); err != nil || !ok || got.ID != t1.ID {
			t.Fatalf("ActiveTicket = %+v, %v, %v", got, ok, err)
		}
		if _, ok, err := s.ActiveTicket(ctx, other, r.ID); err != nil || ok {
			t.Fatalf("ActiveTicket(other org) = %v, %v", ok, err)
		}
		if n, err := s.RevokeTickets(ctx, other, r.ID, events.RevokeRegistrationCancelled, t0); err != nil || n != 0 {
			t.Fatalf("RevokeTickets(other org) = %d, %v", n, err)
		}
		if n, err := s.RevokeTickets(ctx, org, r.ID, events.RevokeReissued, t0); err != nil || n != 1 {
			t.Fatalf("RevokeTickets = %d, %v", n, err)
		}
		if _, ok, _ := s.ActiveTicket(ctx, org, r.ID); ok {
			t.Fatal("a revoked ticket is active")
		}
		if _, ok, err := s.InsertTicket(ctx, org, newTicket(r, 2, "AB12CD34")); err != nil || !ok {
			t.Fatalf("version 2 = %v, %v", ok, err)
		}
		if n, err := s.MaxTicketVersion(ctx, org, r.ID); err != nil || n != 2 {
			t.Fatalf("MaxTicketVersion = %d, %v", n, err)
		}
		ts, err := s.TicketsOfRegistration(ctx, org, r.ID)
		if err != nil || len(ts) != 2 || ts[0].Status != events.TicketRevoked || ts[0].RevokeReason == nil ||
			*ts[0].RevokeReason != events.RevokeReissued || ts[1].Version != 2 {
			t.Fatalf("TicketsOfRegistration = %+v, %v", ts, err)
		}
		if err := s.DeleteRegistration(ctx, org, r.ID); err != nil {
			t.Fatal(err)
		}
		if ts, err := s.TicketsOfRegistration(ctx, org, r.ID); err != nil || len(ts) != 0 {
			t.Fatalf("tickets after the delete = %+v, %v", ts, err)
		}
		// The freed manual code may be used again in the event.
		if _, ok, err := s.InsertTicket(ctx, org, newTicket(r2, 1, "7KQ2M9TX")); err != nil || !ok {
			t.Fatalf("a freed manual code = %v, %v", ok, err)
		}
	})

	t.Run("retention: answers, special category answers, staff after the event (BE-057)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t), w.user(t)
		ended := func(endedAgo time.Duration) events.Event {
			t.Helper()
			e := row(org, by, "Geçmiş Etkinlik", t0.Add(-endedAgo-time.Hour))
			got, ok, err := s.InsertEvent(ctx, e)
			if err != nil || !ok {
				t.Fatalf("InsertEvent: %v %v", ok, err)
			}
			return got
		}
		old, recent := ended(120*24*time.Hour), ended(10*24*time.Hour)
		reg := func(e events.Event) events.Registration {
			t.Helper()
			r := newReg(e, w.user(t), events.RegConfirmed)
			purge := e.EndsAt.Add(30 * 24 * time.Hour)
			r.AnswersSensitiveEnc, r.SensitivePurgeAt = []byte("enc"), &purge
			return insert(t, s, r)
		}
		oldReg, recentReg := reg(old), reg(recent)
		for _, e := range []events.Event{old, recent} {
			if _, err := s.InsertStaff(ctx, events.Staff{EventID: e.ID, UserID: w.user(t), OrgID: org, Role: events.StaffScanner, InvitedBy: by}); err != nil {
				t.Fatal(err)
			}
		}
		purge := func(policy string, cutoff time.Time, limit int, dry bool) int {
			t.Helper()
			n, err := s.PurgeExpired(ctx, policy, cutoff, limit, dry)
			if err != nil {
				t.Fatalf("PurgeExpired(%s): %v", policy, err)
			}
			return n
		}
		// Special category answers: 30 days after the end.
		if n := purge(events.PolicyFormAnswersSensitive, t0.AddDate(0, 0, -30), 0, true); n != 1 {
			t.Fatalf("sensitive dry run = %d, want 1", n)
		}
		if n := purge(events.PolicyFormAnswersSensitive, t0.AddDate(0, 0, -30), 10, false); n != 1 {
			t.Fatalf("sensitive purge = %d, want 1", n)
		}
		got, _ := s.GetRegistration(ctx, org, old.ID, oldReg.ID)
		if got.AnswersSensitiveEnc != nil || string(got.Answers) == "{}" || got.AnswersPurgedAt != nil {
			t.Fatalf("after the sensitive purge = %+v", got)
		}
		// Answers: 90 days after the end; the registration stays.
		if n := purge(events.PolicyFormAnswers, t0.AddDate(0, 0, -90), 0, true); n != 1 {
			t.Fatalf("answers dry run = %d, want 1", n)
		}
		if n := purge(events.PolicyFormAnswers, t0.AddDate(0, 0, -90), 10, false); n != 1 {
			t.Fatalf("answers purge = %d, want 1", n)
		}
		got, err := s.GetRegistration(ctx, org, old.ID, oldReg.ID)
		if err != nil || string(got.Answers) != "{}" || got.AnswersPurgedAt == nil || got.Status != events.RegConfirmed {
			t.Fatalf("after the answer purge = %+v, %v", got, err)
		}
		if n := purge(events.PolicyFormAnswers, t0.AddDate(0, 0, -90), 10, false); n != 0 {
			t.Fatalf("second answer purge = %d, want 0 (idempotent)", n)
		}
		young, _ := s.GetRegistration(ctx, org, recent.ID, recentReg.ID)
		if young.AnswersSensitiveEnc == nil || young.AnswersPurgedAt != nil {
			t.Fatalf("the recent event's answers were purged: %+v", young)
		}
		// Staff: 90 days after the end.
		if n := purge(events.PolicyStaff, t0.AddDate(0, 0, -90), 1, false); n != 1 {
			t.Fatalf("staff purge = %d, want 1", n)
		}
		if st, _ := s.ListStaff(ctx, org, old.ID); len(st) != 0 {
			t.Fatalf("staff of the old event left: %+v", st)
		}
		if st, _ := s.ListStaff(ctx, org, recent.ID); len(st) != 1 {
			t.Fatalf("staff of the recent event = %+v", st)
		}
		if _, err := s.PurgeExpired(ctx, "no_such_policy", t0, 1, true); err == nil {
			t.Fatal("unknown policy accepted")
		}
	})
}
