//go:build integration

package events_test

import (
	"context"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/testutil"
)

func TestStore_RegistrationContract_Gorm(t *testing.T) {
	t.Parallel()
	registrationContract(t, func(t *testing.T) (events.Store, world) {
		d := testutil.NewDB(t)
		return events.NewGormStore(d.ORM), pgWorld(d)
	})
}

// The concurrency test also runs against PostgreSQL: the conditional seat
// UPDATE under real row locks, each registration with its Idempotency-Key
// in the same transaction (06 H-5).
var _ = addRaceBackend("postgres", pgRace)

// pgEnv is the events service over PostgreSQL (GORM store, idempotency
// keys in the database, the in-memory outbox recording committed events).
type pgEnv struct {
	d     *db.DB
	w     world
	clk   *clock.Fake
	svc   events.Service
	mod   events.Module
	out   *outbox.Mem
	store events.Store
	org   uuid.UUID
	audit *auditLog
}

func newPGEnv(t *testing.T) *pgEnv {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(t0)
	w := pgWorld(d)
	e := &pgEnv{d: d, w: w, clk: clk, out: outbox.NewMem(clk), store: events.NewGormStore(d.ORM), org: w.org(t), audit: &auditLog{}}
	fo := newFakeOrgs()
	fo.orgs[e.org] = orgs.OrgSummary{ID: e.org, Kind: orgs.KindClub, Name: "PG Kulübü", Slug: "pg-kulubu", Status: orgs.StatusActive}
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()})
	mod, err := events.New(events.Deps{
		Store: e.store, Tx: d.Tx, Outbox: e.out, Audit: e.audit, Orgs: fo, Catalog: fakeCatalog{}, Idempotency: idem,
		Clock: clk, Log: obs.Discard(), Consents: &spyRecorder{}, Sealer: testEnvelope(), Tickets: testDeriver(),
		Sender: &spySender{},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

// event inserts a published event of the env's org with capacity seats
// and a published form (one required long_text field, motivation).
func (e *pgEnv) event(t *testing.T, seats int, waitlist bool) events.Event {
	t.Helper()
	ctx := t.Context()
	ev := row(e.org, e.w.user(t), "PG Kayıt Etkinliği", start)
	ev.Capacity, ev.WaitlistEnabled = &seats, waitlist
	stored, ok, err := e.store.InsertEvent(ctx, ev)
	if err != nil || !ok {
		t.Fatalf("InsertEvent: %v %v", ok, err)
	}
	if err := e.store.InsertForm(ctx, e.org, events.Form{ID: uuid.NewV7(), EventID: ev.ID,
		DraftSchema: events.JSON(`{"schema_version":1,"fields":[]}`)}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.GetForm(ctx, e.org, ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := events.ParseFormSchema(formJSON(motivationField), "draft_schema")
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.store.InsertFormVersion(ctx, e.org, events.FormVersion{ID: uuid.NewV7(), FormID: f.ID,
		Schema: events.JSON(sc.Canonical()), SchemaSHA256: sc.SHA256(), FieldCount: 1, LintReport: events.LintReport{}.JSON(),
		PublishedBy: ev.CreatedBy, PublishedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.SetCurrentVersion(ctx, e.org, f.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	return stored
}

// student is a verified student with an account row.
func (e *pgEnv) student(t *testing.T) authz.Actor {
	t.Helper()
	u := authztest.UniversityID("uni_a")
	return authz.Actor{Kind: authz.KindUser, UserID: e.w.user(t), SessionID: uuid.NewV7(), StudentStatus: authz.StudentVerified,
		UniversityID: &u}
}

func (e *pgEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.d.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func pgRace(t *testing.T, seats, n int, waitlist bool) raceEnv {
	e := newPGEnv(t)
	ev := e.event(t, seats, waitlist)
	r := raceEnv{svc: e.svc, event: ev.ID}
	for range n {
		r.students = append(r.students, e.student(t))
	}
	r.seats = func(t *testing.T) int {
		return e.count(t, `SELECT confirmed_count FROM events WHERE id = $1`, ev.ID)
	}
	r.statuses = func(t *testing.T) map[events.RegStatus]int {
		out := map[events.RegStatus]int{}
		rows, err := e.d.Pool.Query(t.Context(), `SELECT status, count(*) FROM event_registrations WHERE event_id = $1 GROUP BY status`, ev.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var st string
			var c int
			if err := rows.Scan(&st, &c); err != nil {
				t.Fatal(err)
			}
			out[events.RegStatus(st)] = c
		}
		// Every registration committed its Idempotency-Key, its ticket (a
		// confirmed one) and its outbox event in the same transaction.
		total := out[events.RegConfirmed] + out[events.RegWaitlisted]
		if k := e.count(t, `SELECT count(*) FROM idempotency_keys WHERE status = 'completed'`); k != total {
			t.Errorf("%d completed keys for %d registrations", k, total)
		}
		if k := e.count(t, `SELECT count(*) FROM registration_tickets WHERE event_id = $1 AND status = 'active'`, ev.ID); k != out[events.RegConfirmed] {
			t.Errorf("%d active tickets for %d confirmed registrations", k, out[events.RegConfirmed])
		}
		if k := len(e.out.Events()); k != total {
			t.Errorf("%d outbox events for %d registrations", k, total)
		}
		return out
	}
	return r
}

// TestRegistrationFlow_Postgres runs the chains of BE-028 against
// PostgreSQL: cancellation with FIFO promotion (FOR UPDATE SKIP LOCKED),
// the event.cancelled consumer, the waiting list expiry and the
// reconciliation.
func TestRegistrationFlow_Postgres(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	ev := e.event(t, 2, true)
	ss := []authz.Actor{e.student(t), e.student(t), e.student(t), e.student(t)}
	ids := make([]uuid.UUID, len(ss))
	for i, s := range ss {
		e.clk.Advance(time.Second)
		v, err := e.svc.CreateRegistration(ctx, s, ev.ID, events.RegistrationInput{Answers: motivation})
		if err != nil {
			t.Fatalf("registration %d: %v", i, err)
		}
		ids[i] = v.Registration.ID
	}
	status := func(i int) events.RegStatus {
		t.Helper()
		v, err := e.svc.GetMyRegistration(ctx, ss[i], ids[i])
		if err != nil {
			t.Fatal(err)
		}
		return v.Registration.Status
	}
	if status(0) != events.RegConfirmed || status(2) != events.RegWaitlisted {
		t.Fatalf("statuses %s, %s", status(0), status(2))
	}
	if _, err := e.svc.CancelMyRegistration(ctx, ss[0], ids[0], "Programım değişti"); err != nil {
		t.Fatal(err)
	}
	if status(0) != events.RegCancelled || status(2) != events.RegConfirmed || status(3) != events.RegWaitlisted {
		t.Fatalf("after the cancellation: %s %s %s", status(0), status(2), status(3))
	}
	if n := e.count(t, `SELECT confirmed_count FROM events WHERE id = $1`, ev.ID); n != 2 {
		t.Fatalf("confirmed_count = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM registration_tickets WHERE registration_id = $1 AND status = 'revoked'
		AND revoke_reason = 'registration_cancelled'`, ids[0]); n != 1 {
		t.Fatalf("revoked tickets = %d", n)
	}
	// A drift is reconciled under the row lock.
	if _, err := e.d.Pool.Exec(ctx, `UPDATE events SET confirmed_count = 0 WHERE id = $1`, ev.ID); err != nil {
		t.Fatal(err)
	}
	runJob(t, e.mod, events.ReconcileCountsJobName)
	if n := e.count(t, `SELECT confirmed_count FROM events WHERE id = $1`, ev.ID); n != 2 {
		t.Fatalf("reconciled confirmed_count = %d", n)
	}
	// The waiting list expires once the event has started.
	e.clk.Set(start.Add(time.Minute))
	runJob(t, e.mod, events.WaitlistExpireJobName)
	if status(3) != events.RegExpired {
		t.Fatalf("waitlisted after the start: %s", status(3))
	}
	// The organiser cancels the event; the consumer cancels the rest.
	e.clk.Set(t0.Add(time.Hour))
	other := e.event(t, 5, false)
	for range 3 {
		if _, err := e.svc.CreateRegistration(ctx, e.student(t), other.ID, events.RegistrationInput{Answers: motivation}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.d.Pool.Exec(ctx, `UPDATE events SET status = 'cancelled', cancelled_at = now(), cancel_reason = 'Salon'
		WHERE id = $1`, other.ID); err != nil {
		t.Fatal(err)
	}
	env, err := domainevent.New(domainevent.EventCancelled{EventID: other.ID, OrgID: e.org}, "event", other.ID)
	if err != nil {
		t.Fatal(err)
	}
	reg := outbox.NewRegistry()
	e.mod.Subscribe(reg)
	for range 2 { // a second delivery changes nothing
		for _, s := range reg.For(domainevent.TopicEventCancelled) {
			if err := s.Handle(ctx, env); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := e.count(t, `SELECT count(*) FROM event_registrations WHERE event_id = $1 AND status = 'cancelled'
		AND cancelled_by_kind = 'system'`, other.ID); n != 3 {
		t.Fatalf("cancelled registrations = %d", n)
	}
	if n := e.count(t, `SELECT confirmed_count FROM events WHERE id = $1`, other.ID); n != 0 {
		t.Fatalf("confirmed_count of the cancelled event = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM registration_tickets WHERE event_id = $1 AND revoke_reason = 'event_cancelled'`, other.ID); n != 3 {
		t.Fatalf("revoked tickets = %d", n)
	}
	// The KVKK erasure deletes a registration with its tickets.
	if _, err := e.mod.Privacy.Erase(ctx, ss[2].UserID); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM registration_tickets WHERE registration_id = $1`, ids[2]); n != 0 {
		t.Fatalf("tickets after the erasure = %d", n)
	}
	if n := e.count(t, `SELECT confirmed_count FROM events WHERE id = $1`, ev.ID); n != 1 {
		t.Fatalf("confirmed_count after the erasure = %d", n)
	}
}

func runJob(t *testing.T, m events.Module, name string) {
	t.Helper()
	for _, j := range m.Jobs() {
		if j.Name == name {
			if err := j.Handle(context.WithoutCancel(t.Context())); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return
		}
	}
	t.Fatalf("no job %s", name)
}
