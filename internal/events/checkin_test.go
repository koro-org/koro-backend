package events_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/platform/crypto"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// doorStart is the clock at the door: ten minutes before the test events
// start (scanned_at must be near the server clock).
var doorStart = start.Add(-10 * time.Minute)

// doorEvent publishes a club_a event with check-in on (capacity 10, the
// motivation form); persona event_staff scans it, helper event_manager_a
// manages its staff role.
func (e *env) doorEvent(t *testing.T, mut ...func(*events.EventInput)) events.EventView {
	t.Helper()
	ev := e.regEvent(t, func(in *events.EventInput) {
		in.CheckinEnabled = true
		n := 10
		in.Capacity = &n
		for _, m := range mut {
			m(in)
		}
	}, motivationField)
	for name, role := range map[string]events.StaffRole{"event_staff": events.StaffScanner, "event_manager_a": events.StaffManager} {
		if _, err := e.svc.AddStaff(t.Context(), persona(t, "club_admin"), ev.Event.ID, name+"@example.test", role); err != nil {
			t.Fatalf("AddStaff(%s): %v", name, err)
		}
	}
	return ev
}

// ticketOf registers a for the event and returns the registration and the
// QR payload of its ticket (getMyTicket).
func (e *env) ticketOf(t *testing.T, a authz.Actor, eventID uuid.UUID) (events.RegistrationView, events.TicketView) {
	t.Helper()
	r := e.mustRegister(t, a, eventID, motivation)
	tk, err := e.svc.GetMyTicket(t.Context(), a, r.Registration.ID)
	if err != nil {
		t.Fatalf("GetMyTicket: %v", err)
	}
	return r, tk
}

// scan checks a QR payload in as the actor with a fresh client_scan_id.
func (e *env) scan(t *testing.T, a authz.Actor, eventID uuid.UUID, payload string) events.CheckinResult {
	t.Helper()
	res, err := e.svc.CreateCheckin(t.Context(), a, eventID, qrInput(payload, uuid.NewV7(), e.clk.Now()))
	if err != nil {
		t.Fatalf("CreateCheckin: %v", err)
	}
	return res
}

func qrInput(payload string, scan uuid.UUID, at time.Time) events.CheckinInput {
	return events.CheckinInput{Method: events.MethodQR, Token: payload, ClientScanID: scan, ScannedAt: at, DeviceID: "ipad-kapi-1"}
}

// withTicketKeys is the env's service over the same store and fakes with
// another CRYPTO_TOKEN_KEYS ring (keys[0] active): key rotation.
func (e *env) withTicketKeys(t *testing.T, keys ...crypto.Key) events.Service {
	t.Helper()
	d, err := crypto.NewDeriver(keys)
	if err != nil {
		t.Fatal(err)
	}
	idem := idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: e.clk, Log: obs.Discard()})
	mod, err := events.New(events.Deps{
		Store: e.store, Tx: e.tx, Outbox: e.out, Audit: e.audit, Orgs: e.orgs, Catalog: fakeCatalog{},
		Idempotency: idem, Clock: e.clk, Log: obs.Discard(), Users: e.users, Notices: e.notices,
		Consents: e.consent, Sealer: testEnvelope(), Tickets: d, Sender: e.sender,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mod.Service
}

// adultStudent is persona student (a verified adult of uni_a, "Ayla Y.").
func adultStudent(t *testing.T) authz.Actor { return persona(t, "student") }

// TestCheckin_SameQRTwice_SecondReturnsAlreadyCheckedIn: the first scan
// checks the attendee in (200 ok, "Ayla Y."); the same QR again answers
// already_checked_in with the first scan's time and scanner, and writes no
// second row (04 §E.5, T-70).
func TestCheckin_SameQRTwice_SecondReturnsAlreadyCheckedIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.clk.Set(doorStart)
	staff := persona(t, "event_staff")
	first := e.scan(t, staff, ev.Event.ID, tk.Payload)
	if first.Result != events.ResultOK || first.CheckinID == nil || first.Attendee != "Ayla Y." || first.CheckedInAt == nil ||
		first.Previous != nil {
		t.Fatalf("first scan = %+v", first)
	}
	e.clk.Advance(time.Minute)
	second := e.scan(t, persona(t, "event_manager_a"), ev.Event.ID, tk.Payload)
	if second.Result != events.ResultAlreadyCheckedIn || second.CheckinID != nil || second.Attendee != "Ayla Y." ||
		second.Previous == nil || !second.Previous.CheckedInAt.Equal(*first.CheckedInAt) || second.Previous.StaffDisplay != "Ayla Y." {
		t.Fatalf("second scan = %+v", second)
	}
	if n := len(e.mem.Checkins(r.Registration.ID)); n != 1 {
		t.Fatalf("%d check-in rows", n)
	}
	if got := e.reg(t, adultStudent(t), r.Registration.ID); got.Registration.Status != events.RegCheckedIn ||
		got.Registration.CheckedInAt == nil || e.seats(t, ev.Event.ID) != 1 {
		t.Fatalf("registration %s, seats %d", got.Registration.Status, e.seats(t, ev.Event.ID))
	}
	rec := e.eventsOf(domainevent.TopicCheckinRecorded)
	if len(rec) != 1 {
		t.Fatalf("%d checkin.recorded", len(rec))
	}
	p, err := domainevent.Decode[domainevent.CheckinRecorded](rec[0])
	if err != nil || p.CheckinID != *first.CheckinID || p.UserID != adultStudent(t).UserID || p.StaffUserID != staff.UserID ||
		p.EventID != ev.Event.ID || p.Sensitive {
		t.Fatalf("checkin.recorded = %+v, %v", p, err)
	}
	// Every scan is audited, never with the token.
	scans := e.audit.of(authz.EventsCheckinCreate)
	if len(scans) != 2 || scans[0].Metadata["result"] != "ok" || scans[1].Metadata["result"] != "already_checked_in" {
		t.Fatalf("audit = %+v", scans)
	}
	for _, a := range scans {
		for _, v := range a.Metadata {
			if s, ok := v.(string); ok && strings.Contains(tk.Payload, s) && len(s) > 20 {
				t.Fatalf("the token reached the audit: %v", a.Metadata)
			}
		}
	}
}

// checkinRace is one backend of the concurrency test: an event with one
// ticket and its staff member.
type checkinRace struct {
	svc     events.Service
	event   uuid.UUID
	payload string
	staff   authz.Actor
	rows    func(t *testing.T) int // event_checkins rows of the event
	status  func(t *testing.T) events.RegStatus
	seats   func(t *testing.T) int
	events  func() int // committed checkin.recorded
	at      time.Time  // scanned_at
}

// checkinBackends: in memory; the integration build adds PostgreSQL.
var checkinBackends = []struct {
	name  string
	setup func(t *testing.T) checkinRace
}{{"mem", memCheckinRace}}

func addCheckinBackend(name string, setup func(t *testing.T) checkinRace) bool {
	checkinBackends = append(checkinBackends, struct {
		name  string
		setup func(t *testing.T) checkinRace
	}{name, setup})
	return true
}

func memCheckinRace(t *testing.T) checkinRace {
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.clk.Set(doorStart)
	return checkinRace{
		svc: e.svc, event: ev.Event.ID, payload: tk.Payload, staff: persona(t, "event_staff"), at: doorStart,
		rows: func(*testing.T) int { return len(e.mem.Checkins(r.Registration.ID)) },
		status: func(t *testing.T) events.RegStatus {
			return e.reg(t, adultStudent(t), r.Registration.ID).Registration.Status
		},
		seats:  func(t *testing.T) int { return e.seats(t, ev.Event.ID) },
		events: func() int { return len(e.eventsOf(domainevent.TopicCheckinRecorded)) },
	}
}

// TestCheckin_ConcurrentSameToken_OneRow: 12 devices scan the same QR at
// once (each its own client_scan_id and Idempotency-Key): exactly one is
// ok, the others already_checked_in; one event_checkins row, one
// checkin.recorded, the seat count unchanged (05 §13.6). In memory and,
// with the integration tag, in PostgreSQL (the conditional UPDATE under
// the row lock, the partial unique index behind it).
func TestCheckin_ConcurrentSameToken_OneRow(t *testing.T) {
	t.Parallel()
	const n = 12
	for _, b := range checkinBackends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			r := b.setup(t)
			seats := r.seats(t)
			results := make([]events.CheckinResult, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Go(func() {
					scan := uuid.NewV7()
					ctx := idempotency.WithKey(t.Context(), idempotency.Key{
						Scope: r.staff.UserID.String() + ":POST /events/{event_id}/check-ins", Value: "scan-" + scan.String(), Status: 200,
					})
					results[i], errs[i] = r.svc.CreateCheckin(ctx, r.staff, r.event, qrInput(r.payload, scan, r.at))
				})
			}
			wg.Wait()
			ok, already := 0, 0
			for i, res := range results {
				if errs[i] != nil {
					t.Fatalf("scan %d: %v", i, errs[i])
				}
				switch res.Result {
				case events.ResultOK:
					ok++
				case events.ResultAlreadyCheckedIn:
					already++
				default:
					t.Fatalf("scan %d = %+v", i, res)
				}
			}
			if ok != 1 || already != n-1 {
				t.Fatalf("ok %d, already_checked_in %d", ok, already)
			}
			if rows := r.rows(t); rows != 1 || r.status(t) != events.RegCheckedIn || r.seats(t) != seats || r.events() != 1 {
				t.Fatalf("rows %d, status %s, seats %d→%d, checkin.recorded %d", rows, r.status(t), seats, r.seats(t), r.events())
			}
		})
	}
}

// TestCheckin_SameClientScanID_SameResponse: a retry with the same
// client_scan_id (a new Idempotency-Key, or none) returns the first
// answer, even after another scan came between; the same id from another
// staff member or for another event is 409 and never that answer (T-137).
func TestCheckin_SameClientScanID_SameResponse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	_, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.clk.Set(doorStart)
	staff, scan := persona(t, "event_staff"), uuid.NewV7()
	in := qrInput(tk.Payload, scan, doorStart)
	first, err := e.svc.CreateCheckin(t.Context(), staff, ev.Event.ID, in)
	if err != nil || first.Result != events.ResultOK {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if again := e.scan(t, staff, ev.Event.ID, tk.Payload); again.Result != events.ResultAlreadyCheckedIn {
		t.Fatalf("a new scan = %+v", again)
	}
	ctx := idempotency.WithKey(t.Context(), idempotency.Key{Scope: staff.UserID.String() + ":POST /events/{event_id}/check-ins",
		Value: "retry-0000001", Status: 200})
	retry, err := e.svc.CreateCheckin(ctx, staff, ev.Event.ID, in)
	if err != nil || retry.Result != events.ResultOK || *retry.CheckinID != *first.CheckinID ||
		!retry.CheckedInAt.Equal(*first.CheckedInAt) || retry.Attendee != first.Attendee {
		t.Fatalf("retry = %+v, %v; first %+v", retry, err, first)
	}
	// Another staff member's scan with the same id is not a retry: the same
	// ticket answers as any new scan (already_checked_in), never the first
	// scan's answer...
	mgr := persona(t, "event_manager_a")
	if res, err := e.svc.CreateCheckin(t.Context(), mgr, ev.Event.ID, in); err != nil || res.Result != events.ResultAlreadyCheckedIn ||
		res.CheckinID != nil {
		t.Fatalf("another staff member, same id = %+v, %v", res, err)
	}
	// ... and an id taken by another scan cannot record a new check-in
	// (409 conflict, rolled back): another ticket at this door, or another
	// event's door.
	e.clk.Set(t0)
	_, tk2 := e.ticketOf(t, persona(t, "club_member"), ev.Event.ID)
	other := e.doorEvent(t)
	_, tk3 := e.ticketOf(t, persona(t, "other_club_admin"), other.Event.ID)
	e.clk.Set(doorStart)
	_, err = e.svc.CreateCheckin(t.Context(), mgr, ev.Event.ID, qrInput(tk2.Payload, scan, doorStart))
	wantCode(t, err, 409, "conflict")
	_, err = e.svc.CreateCheckin(t.Context(), staff, other.Event.ID, qrInput(tk3.Payload, scan, doorStart))
	wantCode(t, err, 409, "conflict")
	if got := e.scan(t, mgr, ev.Event.ID, tk2.Payload); got.Result != events.ResultOK {
		t.Fatalf("the refused scan left state: %+v", got)
	}
	if got := e.scan(t, staff, other.Event.ID, tk3.Payload); got.Result != events.ResultOK {
		t.Fatalf("the refused scan left state: %+v", got)
	}
}

// TestCheckin_WrongEvent_Result: a genuine ticket of another event is
// wrong_event without the attendee (another event's attendee does not
// leak, 04 §E.5); a ticket of another org's event alike; its manual code
// is simply not found here.
func TestCheckin_WrongEvent_Result(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	here := e.doorEvent(t)
	there := e.doorEvent(t)
	_, tk := e.ticketOf(t, adultStudent(t), there.Event.ID)
	e.clk.Set(doorStart)
	res := e.scan(t, persona(t, "event_staff"), here.Event.ID, tk.Payload)
	if res.Result != events.ResultWrongEvent || res.Attendee != "" || res.CheckinID != nil || res.Previous != nil {
		t.Fatalf("wrong event = %+v", res)
	}
	if dto := events.ToCheckinResultDTO(res); dto.Attendee != nil {
		t.Fatalf("DTO carries an attendee: %+v", dto)
	}
	manual, err := e.svc.CreateCheckin(t.Context(), persona(t, "event_staff"), here.Event.ID, events.CheckinInput{
		Method: events.MethodManualCode, ManualCode: tk.Ticket.ManualCode, ClientScanID: uuid.NewV7(), ScannedAt: doorStart})
	if err != nil || manual.Result != events.ResultNotFound || manual.Attendee != "" {
		t.Fatalf("manual code of another event = %+v, %v", manual, err)
	}
	// The ticket still works at its own door.
	if ok := e.scan(t, persona(t, "event_staff"), there.Event.ID, tk.Payload); ok.Result != events.ResultOK {
		t.Fatalf("own door = %+v", ok)
	}
	// An active ticket of a registration without a seat is not_confirmed
	// (defensive: the flows revoke the ticket with the seat).
	e.clk.Set(t0)
	pr, ptk := e.ticketOf(t, persona(t, "club_member"), here.Event.ID)
	cur := pr.Registration
	cur.Status = events.RegPending
	if _, err := e.mem.UpdateRegistration(t.Context(), cur); err != nil {
		t.Fatal(err)
	}
	e.clk.Set(doorStart)
	if res := e.scan(t, persona(t, "event_staff"), here.Event.ID, ptk.Payload); res.Result != events.ResultNotConfirmed ||
		res.Attendee != "Ayla Y." {
		t.Fatalf("not confirmed = %+v", res)
	}
	// The manual code (case and separators ignored).
	e.clk.Set(t0)
	_, mtk := e.ticketOf(t, persona(t, "other_club_admin"), here.Event.ID)
	e.clk.Set(doorStart)
	code := strings.ToLower(mtk.Ticket.ManualCode[:4] + "-" + mtk.Ticket.ManualCode[4:])
	got, err := e.svc.CreateCheckin(t.Context(), persona(t, "club_admin"), here.Event.ID, events.CheckinInput{
		Method: events.MethodManualCode, ManualCode: code, ClientScanID: uuid.NewV7(), ScannedAt: doorStart})
	if err != nil || got.Result != events.ResultOK {
		t.Fatalf("manual code %q = %+v, %v", code, got, err)
	}
}

// TestCheckin_ReissuedOldToken_Revoked: after a reissue the old QR answers
// revoked (with the attendee, so the door can say "show your new ticket");
// the new QR checks in.
func TestCheckin_ReissuedOldToken_Revoked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, old := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	nw, err := e.svc.ReissueMyTicket(t.Context(), adultStudent(t), r.Registration.ID)
	if err != nil || nw.Ticket.Version != 2 || nw.Payload == old.Payload {
		t.Fatalf("reissue = %+v, %v", nw, err)
	}
	e.clk.Set(doorStart)
	staff := persona(t, "event_staff")
	if res := e.scan(t, staff, ev.Event.ID, old.Payload); res.Result != events.ResultRevoked || res.Attendee != "Ayla Y." {
		t.Fatalf("old QR = %+v", res)
	}
	if res := e.scan(t, staff, ev.Event.ID, nw.Payload); res.Result != events.ResultOK {
		t.Fatalf("new QR = %+v", res)
	}
	// A cancelled registration's QR is revoked too.
	e.clk.Set(t0)
	c, ctk := e.ticketOf(t, persona(t, "club_member"), ev.Event.ID)
	if _, err := e.svc.CancelMyRegistration(t.Context(), persona(t, "club_member"), c.Registration.ID, ""); err != nil {
		t.Fatal(err)
	}
	e.clk.Set(doorStart)
	if res := e.scan(t, staff, ev.Event.ID, ctk.Payload); res.Result != events.ResultRevoked {
		t.Fatalf("cancelled QR = %+v", res)
	}
}

// TestCheckin_NonStaff_403: only the event's staff and the organiser's
// managers scan (03 §5.6): a club member who sees the event is 403, a
// stranger of a members-only event 404, anonymous 403 (401 over HTTP);
// staff of another event get nothing; nothing is read or written.
func TestCheckin_NonStaff_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.clk.Set(doorStart)
	for _, name := range []string{"club_member", "student", "university_rep", "support", "platform_admin", "company_admin"} {
		_, err := e.svc.CreateCheckin(t.Context(), persona(t, name), ev.Event.ID, qrInput(tk.Payload, uuid.NewV7(), doorStart))
		wantForbidden(t, err)
	}
	_, err := e.svc.CreateCheckin(t.Context(), authz.Anonymous(), ev.Event.ID, qrInput(tk.Payload, uuid.NewV7(), doorStart))
	if !errors.Is(err, authz.ErrForbidden) && !errors.Is(err, problem.ErrUnauthenticated) {
		t.Fatalf("anonymous: %v", err)
	}
	e.clk.Set(t0)
	members := e.doorEvent(t, func(in *events.EventInput) { in.Visibility = events.VisibilityMembers })
	e.clk.Set(doorStart)
	_, err = e.svc.CreateCheckin(t.Context(), persona(t, "student"), members.Event.ID, qrInput(tk.Payload, uuid.NewV7(), doorStart))
	wantNotFound(t, err)
	// The staff of this event cannot scan at another event's door.
	e.clk.Set(t0)
	unstaffed := e.regEvent(t, func(in *events.EventInput) { in.CheckinEnabled = true }, motivationField)
	e.clk.Set(doorStart)
	_, err = e.svc.CreateCheckin(t.Context(), persona(t, "event_staff"), unstaffed.Event.ID, qrInput(tk.Payload, uuid.NewV7(), doorStart))
	wantForbidden(t, err)
	if len(e.mem.Checkins(r.Registration.ID)) != 0 || len(e.audit.of(authz.EventsCheckinCreate)) != 0 {
		t.Fatal("a refused scan left a trace")
	}
	// The body and the event state.
	staff := persona(t, "event_staff")
	_, err = e.svc.CreateCheckin(t.Context(), staff, ev.Event.ID, qrInput(tk.Payload, uuid.NewV7(), doorStart.Add(-25*time.Hour)))
	wantCode(t, err, 422, "validation_failed")
	_, err = e.svc.CreateCheckin(t.Context(), staff, ev.Event.ID, events.CheckinInput{Method: events.MethodQR,
		ManualCode: "7KQ2M9TX", ClientScanID: uuid.NewV7(), ScannedAt: doorStart})
	wantCode(t, err, 422, "validation_failed")
	e.clk.Set(t0)
	off := e.regEvent(t, nil, motivationField)
	if _, err := e.svc.AddStaff(t.Context(), persona(t, "club_admin"), off.Event.ID, "event_staff@example.test", events.StaffScanner); err != nil {
		t.Fatal(err)
	}
	e.clk.Set(doorStart)
	_, err = e.svc.CreateCheckin(t.Context(), staff, off.Event.ID, qrInput(tk.Payload, uuid.NewV7(), doorStart))
	wantCode(t, err, 409, "events.invalid_state") // check-in is off
}

// TestQR_BadMAC_Rejected: a well-formed token that is not a derived one is
// not_found, and so is a ticket row planted in the database with the hash
// of a token the key never derived (DB write without the key is not
// enough: the token is re-derived and compared in constant time).
func TestQR_BadMAC_Rejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.clk.Set(doorStart)
	staff := persona(t, "event_staff")
	forged := events.TicketQRPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	if res := e.scan(t, staff, ev.Event.ID, forged); res.Result != events.ResultNotFound || res.Attendee != "" {
		t.Fatalf("forged = %+v", res)
	}
	// Tamper: a second registration whose ticket row carries the hash of a
	// token chosen by the attacker.
	e.clk.Set(t0)
	v, _ := e.ticketOf(t, persona(t, "club_member"), ev.Event.ID)
	planted := "MFRGGZDFMZTWQ2LKNNWG23TPOA"
	for _, x := range e.mem.Tickets(v.Registration.ID) {
		if err := e.mem.RewriteTicketLookup(x.ID, events.TicketLookup(planted)); err != nil {
			t.Fatal(err)
		}
	}
	e.clk.Set(doorStart)
	if res := e.scan(t, staff, ev.Event.ID, events.TicketPayload(planted)); res.Result != events.ResultNotFound {
		t.Fatalf("planted row = %+v", res)
	}
	// Malformed payloads never reach the store.
	for _, p := range []string{"KORO1:", tk.Payload[:len(tk.Payload)-1], strings.ToLower(tk.Payload), "KORO2:" + tk.Payload[6:],
		tk.Payload + "A", "https://koro.test/t/" + tk.Payload} {
		if res := e.scan(t, staff, ev.Event.ID, p); res.Result != events.ResultNotFound {
			t.Fatalf("payload %q = %+v", p, res)
		}
	}
	if res := e.scan(t, staff, ev.Event.ID, tk.Payload); res.Result != events.ResultOK || len(e.mem.Checkins(r.Registration.ID)) != 1 {
		t.Fatalf("genuine = %+v", res)
	}
}

// TestQR_OldKid_Rejected: a ticket of a key that left CRYPTO_TOKEN_KEYS is
// rejected at the door (not_found), getMyTicket answers 409 (reissue it)
// and the reissued ticket (active key) works; a key still in the ring
// (rotation: old keys verify) keeps working.
func TestQR_OldKid_Rejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	if tk.Ticket.KeyID != "t1" {
		t.Fatalf("key %q", tk.Ticket.KeyID)
	}
	// The same store under a rotated ring: t2 active, t1 still verifying.
	rotated := e.withTicketKeys(t, crypto.Key{ID: "t2", Secret: []byte("events-test-ticket-key-2-events-test-ticket-key-2")},
		crypto.Key{ID: "t1", Secret: []byte("events-test-ticket-key-events-test-ticket-key")})
	e.clk.Set(doorStart)
	staff := persona(t, "event_staff")
	if res, err := rotated.CreateCheckin(t.Context(), staff, ev.Event.ID, qrInput(tk.Payload, uuid.NewV7(), doorStart)); err != nil ||
		res.Result != events.ResultOK {
		t.Fatalf("t1 still in the ring = %+v, %v", res, err)
	}
	// t1 retired: another ticket of t1 no longer passes.
	e.clk.Set(t0)
	m, mtk := e.ticketOf(t, persona(t, "club_member"), ev.Event.ID)
	retired := e.withTicketKeys(t, crypto.Key{ID: "t2", Secret: []byte("events-test-ticket-key-2-events-test-ticket-key-2")})
	e.clk.Set(doorStart)
	if res, err := retired.CreateCheckin(t.Context(), staff, ev.Event.ID, qrInput(mtk.Payload, uuid.NewV7(), doorStart)); err != nil ||
		res.Result != events.ResultNotFound || res.Attendee != "" {
		t.Fatalf("retired key = %+v, %v", res, err)
	}
	_, err := retired.GetMyTicket(t.Context(), persona(t, "club_member"), m.Registration.ID)
	wantCode(t, err, 409, "events.invalid_state")
	nw, err := retired.ReissueMyTicket(t.Context(), persona(t, "club_member"), m.Registration.ID)
	if err != nil || nw.Ticket.KeyID != "t2" {
		t.Fatalf("reissue under t2 = %+v, %v", nw, err)
	}
	if res, err := retired.CreateCheckin(t.Context(), staff, ev.Event.ID, qrInput(nw.Payload, uuid.NewV7(), doorStart)); err != nil ||
		res.Result != events.ResultOK {
		t.Fatalf("reissued = %+v, %v", res, err)
	}
	_ = r
}

// FuzzParseQRPayload: the payload parser never panics, accepts only
// "KORO1:" + 26 canonical base32 characters, and what it accepts
// round-trips.
func FuzzParseQRPayload(f *testing.F) {
	for _, s := range []string{"", "KORO1:", "KORO1:N4Q7ZK2M5XWB3T6Y8C1D9F0GHA", "KORO1:AAAAAAAAAAAAAAAAAAAAAAAAAA",
		"KORO1:AAAAAAAAAAAAAAAAAAAAAAAAAB", "koro1:aaaaaaaaaaaaaaaaaaaaaaaaaa", "KORO1:AAAAAAAAAAAAAAAAAAAAAAAAA=",
		"KORO1:ÇAAAAAAAAAAAAAAAAAAAAAAAAA", "https://koro.test/KORO1:AAAAAAAAAAAAAAAAAAAAAAAAAA", "\x00KORO1:"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		tok, ok := events.ParseQRPayload(s)
		if !ok {
			if tok != "" {
				t.Fatalf("rejected %q but returned %q", s, tok)
			}
			return
		}
		if events.TicketPayload(tok) != s || len(tok) != 26 {
			t.Fatalf("accepted %q as %q", s, tok)
		}
		for _, c := range tok {
			if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
				t.Fatalf("accepted %q with %q", s, c)
			}
		}
	})
}

// TestQR_PayloadAndManualCode pins the formats (R-003 §2.5.4): the derived
// token is 26 canonical base32 characters behind KORO1:, the manual code
// reads case- and separator-insensitively with Crockford's look-alikes.
func TestQR_PayloadAndManualCode(t *testing.T) {
	t.Parallel()
	tok, err := events.TicketToken(testDeriver(), "t1", uuid.NewV7(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := events.ParseQRPayload(events.TicketPayload(tok)); !ok || got != tok {
		t.Fatalf("own payload %q rejected", tok)
	}
	if _, ok := events.ParseQRPayload("KORO1:AAAAAAAAAAAAAAAAAAAAAAAAAB"); ok {
		t.Fatal("non-canonical trailing bits accepted")
	}
	for in, want := range map[string]string{"7KQ2M9TX": "7KQ2M9TX", "7kq2-m9tx": "7KQ2M9TX", " 7KQ2 M9TX ": "7KQ2M9TX",
		"OIL0abcd": "0110ABCD"} {
		if got, ok := events.NormalizeManualCode(in); !ok || got != want {
			t.Fatalf("NormalizeManualCode(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "7KQ2M9T", "7KQ2M9TXX", "7KQ2M9TU", "7KQ2M9T!"} {
		if _, ok := events.NormalizeManualCode(in); ok {
			t.Fatalf("NormalizeManualCode(%q) accepted", in)
		}
	}
}

// TestGetMyTicket_NotConfirmed_409: only a seat holder has a ticket: a
// waitlisted, pending or cancelled registration is 409; somebody else's
// registration 404; the payload carries no personal data or URL.
func TestGetMyTicket_NotConfirmed_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t, capacity(1, true))
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	if !strings.HasPrefix(tk.Payload, "KORO1:") || len(tk.Payload) != 32 || strings.ContainsAny(tk.Payload, "@/.") ||
		tk.Ticket.Version != 1 || tk.Event.Event.ID != ev.Event.ID {
		t.Fatalf("ticket = %+v", tk)
	}
	dto := events.ToTicketDTO(tk)
	if dto.QRPayload != tk.Payload || dto.ManualCode != tk.Ticket.ManualCode || dto.Event.EventID != ev.Event.ID.String() {
		t.Fatalf("DTO = %+v", dto)
	}
	w := e.mustRegister(t, persona(t, "club_member"), ev.Event.ID, motivation)
	_, err := e.svc.GetMyTicket(t.Context(), persona(t, "club_member"), w.Registration.ID)
	wantCode(t, err, 409, "events.invalid_state")
	_, err = e.svc.GetMyTicket(t.Context(), persona(t, "club_member"), r.Registration.ID)
	wantNotFound(t, err)
	if _, err := e.svc.CancelMyRegistration(t.Context(), adultStudent(t), r.Registration.ID, ""); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.GetMyTicket(t.Context(), adultStudent(t), r.Registration.ID)
	wantCode(t, err, 409, "events.invalid_state")
	// The waitlisted one was promoted: its ticket is there now.
	if _, err := e.svc.GetMyTicket(t.Context(), persona(t, "club_member"), w.Registration.ID); err != nil {
		t.Fatalf("promoted: %v", err)
	}
}

// TestReissue_IncrementsVersion_OldRevoked: a reissue (the registrant's or
// the organiser's) revokes the active ticket (reissued) and issues version
// + 1 in one transaction with ticket.reissued; a checked-in or cancelled
// registration is 409; the organiser's path never returns the token.
func TestReissue_IncrementsVersion_OldRevoked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, v1 := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	v2, err := e.svc.ReissueMyTicket(t.Context(), adultStudent(t), r.Registration.ID)
	if err != nil || v2.Ticket.Version != 2 || v2.Ticket.ManualCode == v1.Ticket.ManualCode {
		t.Fatalf("v2 = %+v, %v", v2, err)
	}
	version, err := e.svc.ReissueRegistrationTicket(t.Context(), persona(t, "club_admin"), ev.Event.ID, r.Registration.ID)
	if err != nil || version != 3 {
		t.Fatalf("organiser reissue = %d, %v", version, err)
	}
	tickets := e.mem.Tickets(r.Registration.ID)
	active := 0
	for _, x := range tickets {
		switch {
		case x.Status == events.TicketActive:
			active++
			if x.Version != 3 {
				t.Fatalf("active version %d", x.Version)
			}
		case x.RevokeReason == nil || *x.RevokeReason != events.RevokeReissued:
			t.Fatalf("revoked ticket = %+v", x)
		}
	}
	if len(tickets) != 3 || active != 1 {
		t.Fatalf("tickets = %+v", tickets)
	}
	re := e.eventsOf(domainevent.TopicTicketReissued)
	if len(re) != 2 || len(e.out.txs[domainevent.TopicTicketReissued]) != 2 {
		t.Fatalf("ticket.reissued = %d", len(re))
	}
	p, err := domainevent.Decode[domainevent.TicketReissued](re[1])
	if err != nil || p.RegistrationID != r.Registration.ID || p.TicketVersion != 3 {
		t.Fatalf("payload = %+v, %v", p, err)
	}
	if n := len(e.audit.of(authz.EventsTicketReissue)); n != 2 {
		t.Fatalf("%d reissue audits", n)
	}
	// Others: 404 for a stranger's own path, 403 for a member, 404 for a
	// registration of another event (06 H-38).
	_, err = e.svc.ReissueMyTicket(t.Context(), persona(t, "club_member"), r.Registration.ID)
	wantNotFound(t, err)
	_, err = e.svc.ReissueRegistrationTicket(t.Context(), persona(t, "club_member"), ev.Event.ID, r.Registration.ID)
	wantForbidden(t, err)
	_, err = e.svc.ReissueRegistrationTicket(t.Context(), persona(t, "event_staff"), ev.Event.ID, r.Registration.ID)
	wantForbidden(t, err)
	other := e.doorEvent(t)
	_, err = e.svc.ReissueRegistrationTicket(t.Context(), persona(t, "club_admin"), other.Event.ID, r.Registration.ID)
	wantNotFound(t, err)
	// A checked-in registration keeps its ticket.
	cur, _ := e.svc.GetMyTicket(t.Context(), adultStudent(t), r.Registration.ID)
	e.clk.Set(doorStart)
	if res := e.scan(t, persona(t, "event_staff"), ev.Event.ID, cur.Payload); res.Result != events.ResultOK {
		t.Fatalf("scan = %+v", res)
	}
	_, err = e.svc.ReissueMyTicket(t.Context(), adultStudent(t), r.Registration.ID)
	wantCode(t, err, 409, "events.invalid_state")
}

// TestVoidCheckin_RevertsToConfirmed: a manager voids a check-in with a
// reason: the row stays (voided_at, voided_by, void_reason), the
// registration is confirmed again with its seat, checkin.voided is
// written; the same QR then checks in again. A scanner cannot void (403),
// a second void is 409, another event's check-in 404.
func TestVoidCheckin_RevertsToConfirmed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.clk.Set(doorStart)
	first := e.scan(t, persona(t, "event_staff"), ev.Event.ID, tk.Payload)
	seats := e.seats(t, ev.Event.ID)
	_, err := e.svc.VoidCheckin(t.Context(), persona(t, "event_staff"), ev.Event.ID, *first.CheckinID, "Yanlış kişi okutuldu")
	wantForbidden(t, err)
	_, err = e.svc.VoidCheckin(t.Context(), persona(t, "event_manager_a"), ev.Event.ID, *first.CheckinID, "kısa")
	wantCode(t, err, 422, "validation_failed")
	v, err := e.svc.VoidCheckin(t.Context(), persona(t, "event_manager_a"), ev.Event.ID, *first.CheckinID, "Yanlış kişi okutuldu")
	if err != nil || v.RegistrationStatus != events.RegConfirmed || v.Checkin.VoidedAt == nil {
		t.Fatalf("void = %+v, %v", v, err)
	}
	rows := e.mem.Checkins(r.Registration.ID)
	if len(rows) != 1 || rows[0].VoidedBy == nil || *rows[0].VoidedBy != persona(t, "event_manager_a").UserID ||
		rows[0].VoidReason == nil {
		t.Fatalf("rows = %+v", rows)
	}
	if got := e.reg(t, adultStudent(t), r.Registration.ID).Registration; got.Status != events.RegConfirmed ||
		got.CheckedInAt != nil || e.seats(t, ev.Event.ID) != seats {
		t.Fatalf("registration = %+v, seats %d", got, e.seats(t, ev.Event.ID))
	}
	vo := e.eventsOf(domainevent.TopicCheckinVoided)
	p, err := domainevent.Decode[domainevent.CheckinVoided](vo[0])
	if len(vo) != 1 || err != nil || p.CheckinID != *first.CheckinID || p.UserID != adultStudent(t).UserID {
		t.Fatalf("checkin.voided = %+v, %v", p, err)
	}
	_, err = e.svc.VoidCheckin(t.Context(), persona(t, "club_admin"), ev.Event.ID, *first.CheckinID, "Yanlış kişi okutuldu")
	wantCode(t, err, 409, "events.invalid_state")
	other := e.doorEvent(t)
	_, err = e.svc.VoidCheckin(t.Context(), persona(t, "club_admin"), other.Event.ID, *first.CheckinID, "Yanlış kişi okutuldu")
	wantNotFound(t, err)
	if again := e.scan(t, persona(t, "event_staff"), ev.Event.ID, tk.Payload); again.Result != events.ResultOK ||
		*again.CheckinID == *first.CheckinID {
		t.Fatalf("scan after the void = %+v", again)
	}
	// The list shows both rows (latest first), names as "Ayla Y."; the
	// counters follow.
	page, err := e.svc.ListCheckins(t.Context(), persona(t, "event_staff"), ev.Event.ID, 1, nil)
	if err != nil || len(page.Items) != 1 || page.Next == nil || page.Items[0].Attendee != "Ayla Y." ||
		page.Items[0].Checkin.VoidedAt != nil {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	page2, err := e.svc.ListCheckins(t.Context(), persona(t, "event_staff"), ev.Event.ID, 1, page.Next)
	if err != nil || len(page2.Items) != 1 || page2.Items[0].Checkin.VoidedAt == nil || page2.Next != nil {
		t.Fatalf("page 2 = %+v, %v", page2, err)
	}
	st, err := e.svc.GetCheckinStats(t.Context(), persona(t, "club_admin"), ev.Event.ID)
	if err != nil || st.Confirmed != 1 || st.CheckedIn != 1 || st.Voided != 1 || st.Waitlisted != 0 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
	_, err = e.svc.GetCheckinStats(t.Context(), persona(t, "club_member"), ev.Event.ID)
	wantForbidden(t, err)
}

// endedDoor is an event with check-in whose end has passed: the clock
// is set two hours after its end. It has three confirmed registrations,
// one of them checked in.
func (e *env) endedDoor(t *testing.T) (events.EventView, []events.RegistrationView) {
	t.Helper()
	ev := e.doorEvent(t)
	var regs []events.RegistrationView
	var first events.TicketView
	for i, name := range []string{"student", "club_member", "other_club_admin"} {
		r, tk := e.ticketOf(t, persona(t, name), ev.Event.ID)
		regs = append(regs, r)
		if i == 0 {
			first = tk
		}
	}
	e.clk.Set(doorStart)
	if res := e.scan(t, persona(t, "event_staff"), ev.Event.ID, first.Payload); res.Result != events.ResultOK {
		t.Fatalf("scan = %+v", res)
	}
	e.clk.Set(ev.Event.EndsAt.Add(2 * time.Hour))
	return ev, regs
}

// TestFinalize_BeforeEnd_409: the attendance closes only after the end
// (and only with check-in on); a scanner cannot close it.
func TestFinalize_BeforeEnd_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	e.clk.Set(ev.Event.EndsAt.Add(-time.Minute))
	_, err := e.svc.FinalizeEventAttendance(t.Context(), persona(t, "club_admin"), ev.Event.ID)
	wantCode(t, err, 409, "events.invalid_state")
	e.clk.Set(ev.Event.EndsAt)
	_, err = e.svc.FinalizeEventAttendance(t.Context(), persona(t, "event_staff"), ev.Event.ID)
	wantForbidden(t, err)
	e.clk.Set(t0)
	off := e.regEvent(t, nil, motivationField)
	e.clk.Set(off.Event.EndsAt.Add(time.Hour))
	_, err = e.svc.FinalizeEventAttendance(t.Context(), persona(t, "club_admin"), off.Event.ID)
	wantCode(t, err, 409, "events.invalid_state")
	if len(e.eventsOf(domainevent.TopicAttendanceFinalized)) != 0 {
		t.Fatal("attendance.finalized written")
	}
}

// TestFinalize_Twice_409: a closed attendance is final: a second close,
// a scan and a void are 409.
func TestFinalize_Twice_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev, regs := e.endedDoor(t)
	n, err := e.svc.FinalizeEventAttendance(t.Context(), persona(t, "event_manager_a"), ev.Event.ID)
	if err != nil || n != 2 {
		t.Fatalf("finalize = %d, %v", n, err)
	}
	_, err = e.svc.FinalizeEventAttendance(t.Context(), persona(t, "club_admin"), ev.Event.ID)
	wantCode(t, err, 409, "events.invalid_state")
	tk, err := e.svc.GetMyTicket(t.Context(), persona(t, "club_member"), regs[1].Registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CreateCheckin(t.Context(), persona(t, "event_staff"), ev.Event.ID, qrInput(tk.Payload, uuid.NewV7(), e.clk.Now()))
	wantCode(t, err, 409, "events.invalid_state")
	fin := e.eventsOf(domainevent.TopicAttendanceFinalized)
	p, err := domainevent.Decode[domainevent.AttendanceFinalized](fin[0])
	if len(fin) != 1 || err != nil || p.EventID != ev.Event.ID || p.FinalizedBy != "user" {
		t.Fatalf("attendance.finalized = %+v, %v", p, err)
	}
	got, err := e.mem.GetEvent(t.Context(), ev.Event.ID)
	if err != nil || got.AttendanceFinalizedAt == nil || got.AttendanceFinalizedBy == nil ||
		*got.AttendanceFinalizedBy != persona(t, "event_manager_a").UserID || got.Status != events.StatusPublished {
		t.Fatalf("event = %+v, %v", got, err)
	}
}

// TestFinalize_MarksNoShowInBatches: the attendance.finalized consumer
// turns the confirmed registrations without a check-in into no_show batch
// by batch (each with registration.no_show), takes their seats off
// confirmed_count (no drift for the reconciliation) and changes nothing on
// a second delivery.
func TestFinalize_MarksNoShowInBatches(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	events.SetBatch(e.mod, 2)
	ev := e.doorEvent(t)
	var regs []events.RegistrationView
	var checked events.TicketView
	for i := range 6 {
		r, tk := e.ticketOf(t, newStudent(), ev.Event.ID)
		regs = append(regs, r)
		if i == 0 {
			checked = tk
		}
	}
	e.clk.Set(doorStart)
	if res := e.scan(t, persona(t, "event_staff"), ev.Event.ID, checked.Payload); res.Result != events.ResultOK {
		t.Fatalf("scan = %+v", res)
	}
	e.clk.Set(ev.Event.EndsAt.Add(time.Hour))
	n, err := e.svc.FinalizeEventAttendance(t.Context(), persona(t, "club_admin"), ev.Event.ID)
	if err != nil || n != 5 {
		t.Fatalf("finalize = %d, %v", n, err)
	}
	for range 2 {
		e.deliver(t, domainevent.TopicAttendanceFinalized, 0)
	}
	ns := e.eventsOf(domainevent.TopicRegistrationNoShow)
	if len(ns) != 5 {
		t.Fatalf("%d registration.no_show", len(ns))
	}
	// Five no-shows in three batches: three transactions.
	txs := map[int]bool{}
	for _, tx := range e.out.txs[domainevent.TopicRegistrationNoShow] {
		txs[tx] = true
	}
	if len(txs) != 3 {
		t.Fatalf("no-shows in %d transactions, want 3 (batches of 2)", len(txs))
	}
	st, err := e.svc.GetCheckinStats(t.Context(), persona(t, "club_admin"), ev.Event.ID)
	if err != nil || st.NoShow != 5 || st.CheckedIn != 1 || st.Confirmed != 1 || e.seats(t, ev.Event.ID) != 1 {
		t.Fatalf("stats = %+v, seats %d, %v", st, e.seats(t, ev.Event.ID), err)
	}
	for _, r := range regs[1:] {
		p, err := e.mem.GetRegistration(t.Context(), r.Registration.OrgID, ev.Event.ID, r.Registration.ID)
		if err != nil || p.Status != events.RegNoShow {
			t.Fatalf("registration %s: %+v", r.Registration.ID, p.Status)
		}
	}
	// The reconciliation finds nothing to fix.
	before := len(e.eventsOf(domainevent.TopicRegistrationConfirmed))
	e.job(t, events.ReconcileCountsJobName)
	if e.seats(t, ev.Event.ID) != 1 || len(e.eventsOf(domainevent.TopicRegistrationConfirmed)) != before {
		t.Fatalf("reconciled to %d", e.seats(t, ev.Event.ID))
	}
}

// TestAutoFinalize_24hAfterEnd: events.auto_finalize_attendance closes an
// attendance 24 hours after the end when the event had a check-in (by the
// system), not earlier, and never an event without check-ins.
func TestAutoFinalize_24hAfterEnd(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev, _ := e.endedDoor(t)
	e.clk.Set(t0)
	quiet := e.doorEvent(t) // no check-in
	e.ticketOf(t, persona(t, "club_member"), quiet.Event.ID)
	e.clk.Set(ev.Event.EndsAt.Add(23 * time.Hour))
	e.job(t, events.AutoFinalizeJobName)
	if len(e.eventsOf(domainevent.TopicAttendanceFinalized)) != 0 {
		t.Fatal("closed before 24 hours")
	}
	e.clk.Set(ev.Event.EndsAt.Add(24 * time.Hour))
	e.job(t, events.AutoFinalizeJobName)
	e.job(t, events.AutoFinalizeJobName) // idempotent
	fin := e.eventsOf(domainevent.TopicAttendanceFinalized)
	if len(fin) != 1 {
		t.Fatalf("%d attendance.finalized", len(fin))
	}
	p, err := domainevent.Decode[domainevent.AttendanceFinalized](fin[0])
	if err != nil || p.EventID != ev.Event.ID || p.FinalizedBy != "system" {
		t.Fatalf("payload = %+v, %v", p, err)
	}
	got, _ := e.mem.GetEvent(t.Context(), ev.Event.ID)
	if got.AttendanceFinalizedAt == nil || got.AttendanceFinalizedBy != nil {
		t.Fatalf("event = %+v", got)
	}
	if q, _ := e.mem.GetEvent(t.Context(), quiet.Event.ID); q.AttendanceFinalizedAt != nil {
		t.Fatal("an event without check-ins was closed")
	}
	e.deliver(t, domainevent.TopicAttendanceFinalized, 0)
	if n := len(e.eventsOf(domainevent.TopicRegistrationNoShow)); n != 2 {
		t.Fatalf("%d no-shows", n)
	}
	var jobs []string
	for _, j := range e.mod.Jobs() {
		if j.Name == events.AutoFinalizeJobName {
			jobs = append(jobs, j.Cron)
		}
	}
	if len(jobs) != 1 || jobs[0] != "20 * * * *" {
		t.Fatalf("job = %v", jobs)
	}
}

// TestCheckin_KVKKExportAndErasure: the registrant's export lists the
// check-ins of their registrations, the staff member's export the scans
// they made; erasing the registrant deletes the check-ins with the
// registration.
func TestCheckin_KVKKExportAndErasure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.clk.Set(doorStart)
	res := e.scan(t, persona(t, "event_staff"), ev.Event.ID, tk.Payload)
	sec, err := e.mod.Privacy.Export(t.Context(), adultStudent(t).UserID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range sec.Items {
		if it["type"] == "event_registration" {
			cs, _ := it["checkins"].([]map[string]any)
			found = len(cs) == 1 && cs[0]["checkin_id"] == res.CheckinID.String()
		}
		for _, v := range it {
			if s, ok := v.(string); ok && (strings.Contains(s, tk.Payload[6:]) || s == tk.Ticket.ManualCode) {
				t.Fatalf("the export carries a ticket credential: %v", it)
			}
		}
	}
	if !found {
		t.Fatalf("registrant export = %+v", sec.Items)
	}
	staffSec, err := e.mod.Privacy.Export(t.Context(), persona(t, "event_staff").UserID)
	if err != nil {
		t.Fatal(err)
	}
	scans := 0
	for _, it := range staffSec.Items {
		if it["type"] == "event_checkin_scanned" && it["checkin_id"] == res.CheckinID.String() {
			scans++
		}
	}
	if scans != 1 {
		t.Fatalf("staff export = %+v", staffSec.Items)
	}
	if _, err := e.mod.Privacy.Erase(context.WithoutCancel(t.Context()), adultStudent(t).UserID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.mem.Checkins(r.Registration.ID)); n != 0 {
		t.Fatalf("%d check-ins after the erasure", n)
	}
}
