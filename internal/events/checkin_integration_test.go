//go:build integration

package events_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/testutil"
)

func TestStore_CheckinContract_Gorm(t *testing.T) {
	t.Parallel()
	checkinContract(t, func(t *testing.T) (events.Store, world) {
		d := testutil.NewDB(t)
		return events.NewGormStore(d.ORM), pgWorld(d)
	})
}

// The check-in concurrency test also runs against PostgreSQL: the
// conditional UPDATE under the registration row lock, the partial unique
// index event_checkins_one_active_uq behind it, each scan in its own
// transaction with its Idempotency-Key.
var _ = addCheckinBackend("postgres", pgCheckinRace)

// doorEvent is a published event of the env's org with check-in on and a
// scanner; it returns the event and the scanner.
func (e *pgEnv) doorEvent(t *testing.T) (events.Event, authz.Actor) {
	t.Helper()
	ev := e.event(t, 10, false)
	if _, err := e.d.Pool.Exec(t.Context(), `UPDATE events SET checkin_enabled = true WHERE id = $1`, ev.ID); err != nil {
		t.Fatal(err)
	}
	staff := authz.Actor{Kind: authz.KindUser, UserID: e.w.user(t), SessionID: uuid.NewV7()}
	if _, err := e.store.InsertStaff(t.Context(), events.Staff{EventID: ev.ID, UserID: staff.UserID, OrgID: e.org,
		Role: events.StaffManager, InvitedBy: e.w.user(t)}); err != nil {
		t.Fatal(err)
	}
	return ev, staff
}

// ticket registers a for ev and returns its registration and QR payload.
func (e *pgEnv) ticket(t *testing.T, a authz.Actor, ev events.Event) (uuid.UUID, string) {
	t.Helper()
	r, err := e.svc.CreateRegistration(t.Context(), a, ev.ID, events.RegistrationInput{Answers: motivation})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := e.svc.GetMyTicket(t.Context(), a, r.Registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r.Registration.ID, tk.Payload
}

func pgCheckinRace(t *testing.T) checkinRace {
	e := newPGEnv(t)
	ev, staff := e.doorEvent(t)
	reg, payload := e.ticket(t, e.student(t), ev)
	return checkinRace{
		svc: e.svc, event: ev.ID, payload: payload, staff: staff, at: t0,
		rows: func(t *testing.T) int {
			return e.count(t, `SELECT count(*) FROM event_checkins WHERE registration_id = $1`, reg)
		},
		status: func(t *testing.T) events.RegStatus {
			var st string
			if err := e.d.Pool.QueryRow(t.Context(), `SELECT status FROM event_registrations WHERE id = $1`, reg).Scan(&st); err != nil {
				t.Fatal(err)
			}
			return events.RegStatus(st)
		},
		seats: func(t *testing.T) int { return e.count(t, `SELECT confirmed_count FROM events WHERE id = $1`, ev.ID) },
		events: func() int {
			n := 0
			for _, x := range e.out.Events() {
				if x.Topic == domainevent.TopicCheckinRecorded {
					n++
				}
			}
			return n
		},
	}
}

// TestCheckinFlow_Postgres runs the BE-031 chains against PostgreSQL: a
// check-in with its audit and outbox event in one transaction, a
// client_scan_id collision rolled back whole (409, nothing written), the
// void, the reissue (the old QR revoked), the attendance close with the
// no-shows in batches, the auto close and the KVKK erasure of check-ins.
func TestCheckinFlow_Postgres(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	ev, staff := e.doorEvent(t)
	students := []authz.Actor{e.student(t), e.student(t), e.student(t), e.student(t)}
	regs := make([]uuid.UUID, len(students))
	payloads := make([]string, len(students))
	for i, s := range students {
		regs[i], payloads[i] = e.ticket(t, s, ev)
	}
	scan := uuid.NewV7()
	res, err := e.svc.CreateCheckin(ctx, staff, ev.ID, qrInput(payloads[0], scan, t0))
	if err != nil || res.Result != events.ResultOK {
		t.Fatalf("check-in = %+v, %v", res, err)
	}
	if n := e.count(t, `SELECT count(*) FROM event_checkins WHERE registration_id = $1 AND org_id = $2 AND event_id = $3`,
		regs[0], e.org, ev.ID); n != 1 {
		t.Fatalf("%d check-in rows", n)
	}
	// Another scanner sends the same client_scan_id for another ticket:
	// 409, and the whole transaction (the key with it) is rolled back.
	second := authz.Actor{Kind: authz.KindUser, UserID: e.w.user(t), SessionID: uuid.NewV7()}
	if _, err := e.store.InsertStaff(ctx, events.Staff{EventID: ev.ID, UserID: second.UserID, OrgID: e.org,
		Role: events.StaffScanner, InvitedBy: staff.UserID}); err != nil {
		t.Fatal(err)
	}
	key := idempotency.WithKey(ctx, idempotency.Key{Scope: second.UserID.String() + ":POST /events/{event_id}/check-ins",
		Value: "pg-collide-01", Status: 200})
	_, err = e.svc.CreateCheckin(key, second, ev.ID, qrInput(payloads[1], scan, t0))
	wantCode(t, err, 409, "conflict")
	if st := e.count(t, `SELECT count(*) FROM event_registrations WHERE id = $1 AND status = 'confirmed'`, regs[1]); st != 1 {
		t.Fatal("the refused scan changed the registration")
	}
	if n := e.count(t, `SELECT count(*) FROM idempotency_keys WHERE key = 'pg-collide-01'`); n != 0 {
		t.Fatal("the refused scan kept its Idempotency-Key")
	}
	// Retry, void, scan again.
	again, err := e.svc.CreateCheckin(ctx, staff, ev.ID, qrInput(payloads[0], scan, t0))
	if err != nil || again.Result != events.ResultOK || *again.CheckinID != *res.CheckinID {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	v, err := e.svc.VoidCheckin(ctx, staff, ev.ID, *res.CheckinID, "Yanlış kişi okutuldu")
	if err != nil || v.RegistrationStatus != events.RegConfirmed {
		t.Fatalf("void = %+v, %v", v, err)
	}
	if res2, err := e.svc.CreateCheckin(ctx, staff, ev.ID, qrInput(payloads[0], uuid.NewV7(), t0)); err != nil ||
		res2.Result != events.ResultOK {
		t.Fatalf("scan after the void = %+v, %v", res2, err)
	}
	if n := e.count(t, `SELECT count(*) FROM event_checkins WHERE registration_id = $1 AND voided_at IS NOT NULL
		AND void_reason = 'Yanlış kişi okutuldu'`, regs[0]); n != 1 {
		t.Fatalf("%d voided rows", n)
	}
	// Reissue: the old QR is revoked in the database.
	nw, err := e.svc.ReissueMyTicket(ctx, students[1], regs[1])
	if err != nil || nw.Ticket.Version != 2 {
		t.Fatalf("reissue = %+v, %v", nw, err)
	}
	if r, err := e.svc.CreateCheckin(ctx, staff, ev.ID, qrInput(payloads[1], uuid.NewV7(), t0)); err != nil || r.Result != events.ResultRevoked {
		t.Fatalf("old QR = %+v, %v", r, err)
	}
	if n := e.count(t, `SELECT count(*) FROM registration_tickets WHERE registration_id = $1 AND revoke_reason = 'reissued'`, regs[1]); n != 1 {
		t.Fatalf("%d reissued tickets", n)
	}
	// The ticket.reissued consumer reads the ticket across the org boundary
	// by id and mails through the registration's own event.
	mailer := &spySender{}
	mailer.mail, _, _ = realMail(t, e.clk)
	mail := e.withMail(t, mailer)
	reg := outbox.NewRegistry()
	mail.Subscribe(reg)
	for _, x := range e.out.Events() {
		if x.Topic != domainevent.TopicTicketReissued {
			continue
		}
		for _, s := range reg.For(x.Topic) {
			if err := s.Handle(ctx, x); err != nil {
				t.Fatal(err)
			}
		}
	}
	if m := mailer.emails("events.ticket_reissued"); len(m) != 1 || *m[0].UserID != students[1].UserID {
		t.Fatalf("reissue mails = %+v", m)
	}
	// The attendance closes after the end: three confirmed become no_show
	// in batches of two (their seats leave confirmed_count).
	events.SetBatch(e.mod, 2)
	e.clk.Set(ev.EndsAt.Add(time.Hour))
	n, err := e.svc.FinalizeEventAttendance(ctx, staff, ev.ID)
	if err != nil || n != 3 {
		t.Fatalf("finalize = %d, %v", n, err)
	}
	for range 2 {
		for _, x := range e.out.Events() {
			if x.Topic != domainevent.TopicAttendanceFinalized {
				continue
			}
			reg := outbox.NewRegistry()
			e.mod.Subscribe(reg)
			for _, s := range reg.For(x.Topic) {
				if err := s.Handle(ctx, x); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if k := e.count(t, `SELECT count(*) FROM event_registrations WHERE event_id = $1 AND status = 'no_show'`, ev.ID); k != 3 {
		t.Fatalf("%d no-shows", k)
	}
	if k := e.count(t, `SELECT confirmed_count FROM events WHERE id = $1`, ev.ID); k != 1 {
		t.Fatalf("confirmed_count = %d", k)
	}
	if k := e.count(t, `SELECT count(*) FROM events WHERE id = $1 AND attendance_finalized_at IS NOT NULL
		AND attendance_finalized_by = $2`, ev.ID, staff.UserID); k != 1 {
		t.Fatal("not finalised")
	}
	// The system closes another event 24 hours after its end.
	e.clk.Set(t0)
	other, otherStaff := e.doorEvent(t)
	_, p := e.ticket(t, e.student(t), other)
	if r, err := e.svc.CreateCheckin(ctx, otherStaff, other.ID, qrInput(p, uuid.NewV7(), t0)); err != nil || r.Result != events.ResultOK {
		t.Fatalf("scan = %+v, %v", r, err)
	}
	e.clk.Set(other.EndsAt.Add(24 * time.Hour))
	runJob(t, e.mod, events.AutoFinalizeJobName)
	if k := e.count(t, `SELECT count(*) FROM events WHERE id = $1 AND attendance_finalized_at IS NOT NULL
		AND attendance_finalized_by IS NULL`, other.ID); k != 1 {
		t.Fatal("the system did not close the attendance")
	}
	// The erasure takes the person's check-ins with the registration.
	if _, err := e.mod.Privacy.Erase(context.WithoutCancel(ctx), students[0].UserID); err != nil {
		t.Fatal(err)
	}
	if k := e.count(t, `SELECT count(*) FROM event_checkins WHERE registration_id = $1`, regs[0]); k != 0 {
		t.Fatalf("%d check-ins after the erasure", k)
	}
}

// withMail is the env's service with the ticket mail dependencies.
func (e *pgEnv) withMail(t *testing.T, sender *spySender) events.Module {
	t.Helper()
	idem := idempotency.New(idempotency.Deps{Tx: e.d.Tx, Store: idempotency.NewGormStore(e.d.ORM), Clock: e.clk, Log: obs.Discard()})
	fo := newFakeOrgs()
	fo.orgs[e.org] = orgs.OrgSummary{ID: e.org, Kind: orgs.KindClub, Name: "PG Kulübü", Slug: "pg-kulubu", Status: orgs.StatusActive}
	mod, err := events.New(events.Deps{
		Store: e.store, Tx: e.d.Tx, Outbox: e.out, Audit: e.audit, Orgs: fo, Catalog: fakeCatalog{}, Idempotency: idem,
		Clock: e.clk, Log: obs.Discard(), Consents: &spyRecorder{}, Sealer: testEnvelope(), Tickets: testDeriver(), Sender: sender,
		QR: events.NewQRRenderer(), Contacts: fakeUsers{erased: map[uuid.UUID]bool{}}, LinkBaseURL: "https://koro.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return mod
}
