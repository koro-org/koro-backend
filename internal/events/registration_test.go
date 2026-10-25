package events_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/events"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// sensitiveReason is the lint override of a form with a health field.
const sensitiveReason = "Etkinlikte yemek verilecek; alerji bilgisi güvenlik için gerekli."

// regEvent publishes an event of club_a configured by mut; fields (JSON
// field objects) become its published form version (a warn_special field
// is published with sensitiveReason), none: an empty form version.
func (e *env) regEvent(t *testing.T, mut func(*events.EventInput), fields ...string) events.EventView {
	t.Helper()
	in := input("Kayıt Etkinliği " + uuid.NewV7().String()[:8])
	if mut != nil {
		mut(&in)
	}
	v := e.create(t, "club_admin", "club_a", in)
	if len(fields) == 0 {
		if err := e.mem.SetFormVersion(v.Event.ID, uuid.NewV7()); err != nil {
			t.Fatal(err)
		}
	} else {
		d := e.putDraft(t, "club_admin", v.Event.ID, formJSON(fields...))
		why := ""
		if d.Lint.NeedsOverride() {
			why = sensitiveReason
		}
		e.publishForm(t, v.Event.ID, why)
	}
	p, err := e.svc.PublishEvent(t.Context(), persona(t, "club_admin"), v.Event.ID)
	if err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	return p
}

func capacity(n int, waitlist bool) func(*events.EventInput) {
	return func(in *events.EventInput) { in.Capacity, in.WaitlistEnabled = &n, waitlist }
}

// newStudent is a verified adult student of uni_a without memberships.
func newStudent() authz.Actor {
	u := authztest.UniversityID("uni_a")
	return authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), SessionID: uuid.NewV7(),
		StudentStatus: authz.StudentVerified, UniversityID: &u}
}

// register registers a with answers (no Idempotency-Key).
func (e *env) register(t *testing.T, a authz.Actor, eventID uuid.UUID, answers map[string]any) (events.RegistrationView, error) {
	t.Helper()
	return e.svc.CreateRegistration(t.Context(), a, eventID, events.RegistrationInput{Answers: answers})
}

func (e *env) mustRegister(t *testing.T, a authz.Actor, eventID uuid.UUID, answers map[string]any) events.RegistrationView {
	t.Helper()
	v, err := e.register(t, a, eventID, answers)
	if err != nil {
		t.Fatalf("CreateRegistration: %v %v", err, fieldsOf(err))
	}
	return v
}

// seats is the event's confirmed_count.
func (e *env) seats(t *testing.T, eventID uuid.UUID) int {
	t.Helper()
	ev, err := e.mem.GetEvent(t.Context(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	return ev.ConfirmedCount
}

// reg reads a registration as its owner sees it (status, position).
func (e *env) reg(t *testing.T, a authz.Actor, id uuid.UUID) events.RegistrationView {
	t.Helper()
	v, err := e.svc.GetMyRegistration(t.Context(), a, id)
	if err != nil {
		t.Fatalf("GetMyRegistration: %v", err)
	}
	return v
}

// eventsOf returns the committed outbox events of topic.
func (e *env) eventsOf(topic string) []domainevent.Envelope {
	var out []domainevent.Envelope
	for _, ev := range e.out.Events() {
		if ev.Topic == topic {
			out = append(out, ev)
		}
	}
	return out
}

// deliver runs the module's subscriptions for the committed outbox events
// of topic after skip (the worker's part).
func (e *env) deliver(t *testing.T, topic string, skip int) {
	t.Helper()
	reg := outbox.NewRegistry()
	e.mod.Subscribe(reg)
	if err := reg.Err(); err != nil {
		t.Fatal(err)
	}
	for _, ev := range e.eventsOf(topic)[skip:] {
		for _, s := range reg.For(topic) {
			if err := s.Handle(t.Context(), ev); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
}

// job runs one scheduled job of the module.
func (e *env) job(t *testing.T, name string) {
	t.Helper()
	for _, j := range e.mod.Jobs() {
		if j.Name == name {
			if err := j.Handle(t.Context()); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return
		}
	}
	t.Fatalf("no job %s", name)
}

var motivation = map[string]any{"motivation": "Veri bilimi kariyerimi ilerletmek istiyorum."}

// TestRegister_CapacityFull_GoesToWaitlist: auto mode confirms while seats
// last (one ticket each), then waitlists in order with a position; without
// a waiting list (or once the event has started) a full event is 409.
func TestRegister_CapacityFull_GoesToWaitlist(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, capacity(2, true), motivationField)
	var confirmed, waiting []events.RegistrationView
	students := []authz.Actor{newStudent(), newStudent(), newStudent(), newStudent()}
	for i, s := range students {
		e.clk.Advance(time.Second)
		v := e.mustRegister(t, s, ev.Event.ID, motivation)
		if i < 2 {
			confirmed = append(confirmed, v)
		} else {
			waiting = append(waiting, v)
		}
	}
	for _, v := range confirmed {
		r := v.Registration
		if r.Status != events.RegConfirmed || r.ConfirmedAt == nil || !v.TicketAvailable || v.WaitlistPosition != nil ||
			r.FormVersionID == nil {
			t.Fatalf("confirmed = %+v", v)
		}
		ts := e.mem.Tickets(r.ID)
		if len(ts) != 1 || ts[0].Version != 1 || ts[0].Status != events.TicketActive || len(ts[0].TokenLookup) != 32 ||
			len(ts[0].ManualCode) != 8 || ts[0].KeyID != "t1" || ts[0].EventID != ev.Event.ID {
			t.Fatalf("tickets = %+v", ts)
		}
		token, err := events.TicketToken(testDeriver(), "t1", r.ID, 1)
		if err != nil || len(token) != 26 || !bytes.Equal(events.TicketLookup(token), ts[0].TokenLookup) {
			t.Fatalf("token %q does not match the stored lookup: %v", token, err)
		}
	}
	for i, v := range waiting {
		if v.Registration.Status != events.RegWaitlisted || v.Registration.WaitlistedAt == nil || v.TicketAvailable ||
			v.WaitlistPosition == nil || *v.WaitlistPosition != i+1 || len(e.mem.Tickets(v.Registration.ID)) != 0 {
			t.Fatalf("waitlisted %d = %+v", i, v)
		}
	}
	if n := e.seats(t, ev.Event.ID); n != 2 {
		t.Fatalf("confirmed_count = %d", n)
	}
	if got := e.out.Topics(); !slices.Equal(got, []string{domainevent.TopicEventPublished, domainevent.TopicRegistrationConfirmed, domainevent.TopicRegistrationConfirmed,
		domainevent.TopicRegistrationWaitlisted, domainevent.TopicRegistrationWaitlisted}) {
		t.Fatalf("topics = %v", got)
	}
	c, err := domainevent.Decode[domainevent.RegistrationConfirmed](e.eventsOf(domainevent.TopicRegistrationConfirmed)[0])
	if err != nil || c.TicketVersion != 1 || c.Promoted || c.UserID != students[0].UserID || c.OrgID != orgID("club_a") || c.Sensitive {
		t.Fatalf("confirmed payload = %+v, %v", c, err)
	}
	if got := e.audit.Of(authz.EventsRegistrationCreate); len(got) != 4 || got[0].Metadata["status"] != "confirmed" {
		t.Fatalf("audit = %+v", got)
	}

	// Without a waiting list a full event refuses (nothing written).
	full := e.regEvent(t, capacity(1, false), motivationField)
	e.mustRegister(t, newStudent(), full.Event.ID, motivation)
	late := newStudent()
	_, err = e.register(t, late, full.Event.ID, motivation)
	problemtest.WantProblem(t, err, 409, "events.capacity_full")
	if n := e.seats(t, full.Event.ID); n != 1 {
		t.Fatalf("confirmed_count after the refusal = %d", n)
	}
	page, err := e.svc.ListMyRegistrations(ctx, late, events.MyRegistrationQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("the refused student has %+v, %v", page.Items, err)
	}
	// Once the event has started the waiting list takes nobody.
	e.clk.Set(start.Add(time.Minute))
	_, err = e.register(t, newStudent(), ev.Event.ID, motivation)
	problemtest.WantProblem(t, err, 409, "events.capacity_full")
}

// raceEnv is one backend of the concurrency test.
type raceEnv struct {
	svc      events.Service
	event    uuid.UUID
	students []authz.Actor
	seats    func(t *testing.T) int
	statuses func(t *testing.T) map[events.RegStatus]int
}

// raceBackends build a published event with capacity seats (waiting list
// as asked) and n verified students; the integration build adds PostgreSQL.
var raceBackends = []struct {
	name  string
	setup func(t *testing.T, seats, n int, waitlist bool) raceEnv
}{{"mem", memRace}}

func addRaceBackend(name string, setup func(t *testing.T, seats, n int, waitlist bool) raceEnv) bool {
	raceBackends = append(raceBackends, struct {
		name  string
		setup func(t *testing.T, seats, n int, waitlist bool) raceEnv
	}{name, setup})
	return true
}

func memRace(t *testing.T, seats, n int, waitlist bool) raceEnv {
	e := newEnv(t)
	ev := e.regEvent(t, capacity(seats, waitlist), motivationField)
	r := raceEnv{svc: e.svc, event: ev.Event.ID, seats: func(t *testing.T) int { return e.seats(t, ev.Event.ID) }}
	for range n {
		r.students = append(r.students, newStudent())
	}
	r.statuses = func(t *testing.T) map[events.RegStatus]int {
		out := map[events.RegStatus]int{}
		for _, s := range r.students {
			p, err := e.svc.ListMyRegistrations(t.Context(), s, events.MyRegistrationQuery{})
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range p.Items {
				out[v.Registration.Status]++
			}
		}
		return out
	}
	return r
}

// TestRegister_ConcurrentLastSeat_ExactlyCapacityConfirmed: N students race
// for K seats (05 §13.6): exactly K are confirmed and confirmed_count is K,
// whatever the interleaving; the rest are waitlisted (or refused with 409
// events.capacity_full without a waiting list). In memory and, with the
// integration tag, in PostgreSQL with the Idempotency-Key in each
// transaction.
func TestRegister_ConcurrentLastSeat_ExactlyCapacityConfirmed(t *testing.T) {
	t.Parallel()
	const seats, n = 5, 24
	for _, b := range raceBackends {
		for _, waitlist := range []bool{true, false} {
			name := b.name + "/full"
			if waitlist {
				name = b.name + "/waitlist"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := b.setup(t, seats, n, waitlist)
				errs := make([]error, n)
				var wg sync.WaitGroup
				for i, s := range r.students {
					wg.Go(func() {
						ctx := idempotency.WithKey(t.Context(), idempotency.Key{
							Scope: s.UserID.String() + ":POST /events/{event_id}/registrations", Value: "race-" + s.UserID.String(),
							Status: 201,
						})
						_, errs[i] = r.svc.CreateRegistration(ctx, s, r.event, events.RegistrationInput{Answers: motivation})
					})
				}
				wg.Wait()
				full := 0
				for _, err := range errs {
					switch p, ok := errors.AsType[*problem.Error](err); {
					case err == nil:
					case ok && p.Code() == "events.capacity_full" && !waitlist:
						full++
					default:
						t.Fatalf("unexpected error: %v", err)
					}
				}
				st := r.statuses(t)
				if st[events.RegConfirmed] != seats || r.seats(t) != seats {
					t.Fatalf("confirmed %d, confirmed_count %d; want %d (statuses %v)", st[events.RegConfirmed], r.seats(t), seats, st)
				}
				if waitlist && (st[events.RegWaitlisted] != n-seats || full != 0) {
					t.Fatalf("waitlisted %d, refused %d; want %d, 0", st[events.RegWaitlisted], full, n-seats)
				}
				if !waitlist && (full != n-seats || st[events.RegWaitlisted] != 0) {
					t.Fatalf("refused %d, waitlisted %d; want %d, 0", full, st[events.RegWaitlisted], n-seats)
				}
			})
		}
	}
}

// TestRegister_SameIdempotencyKey_ReplaysSameRegistration: a retry with the
// same key and body returns the first registration, writing nothing
// (06 H-5); the same key with another body is 422; a new key is a new
// request (409, already registered).
func TestRegister_SameIdempotencyKey_ReplaysSameRegistration(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.regEvent(t, capacity(10, false), motivationField)
	a := persona(t, "student")
	key := func(v string) context.Context {
		return idempotency.WithKey(t.Context(), idempotency.Key{Scope: a.UserID.String() + ":POST /events/{event_id}/registrations",
			Value: v, Status: 201})
	}
	in := events.RegistrationInput{Answers: motivation}
	first, err := e.svc.CreateRegistration(key("reg-7b1d4c2e9a"), a, ev.Event.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CreateRegistration(key("reg-7b1d4c2e9a"), a, ev.Event.ID, in)
	if err != nil || again.Registration.ID != first.Registration.ID || again.Registration.Status != events.RegConfirmed {
		t.Fatalf("replay = %+v, %v", again.Registration, err)
	}
	if n := len(e.audit.Of(authz.EventsRegistrationCreate)); n != 1 || e.seats(t, ev.Event.ID) != 1 ||
		len(e.eventsOf(domainevent.TopicRegistrationConfirmed)) != 1 || len(e.mem.Tickets(first.Registration.ID)) != 1 {
		t.Fatalf("the replay wrote again: %d audits, %d seats", n, e.seats(t, ev.Event.ID))
	}
	_, err = e.svc.CreateRegistration(key("reg-7b1d4c2e9a"), a, ev.Event.ID,
		events.RegistrationInput{Answers: map[string]any{"motivation": "Başka bir gövde"}})
	problemtest.WantProblem(t, err, 422, "idempotency.key_reused")
	_, err = e.svc.CreateRegistration(key("reg-another-key"), a, ev.Event.ID, in)
	problemtest.WantProblem(t, err, 409, "events.already_registered")
}

// TestRegister_DuplicateUser_409: one live registration per user and event
// (event_registrations_active_uq); after a cancellation the user may
// register again.
func TestRegister_DuplicateUser_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.regEvent(t, capacity(5, true), motivationField)
	a := persona(t, "student")
	v := e.mustRegister(t, a, ev.Event.ID, motivation)
	_, err := e.register(t, a, ev.Event.ID, motivation)
	problemtest.WantProblem(t, err, 409, "events.already_registered")
	if n := e.seats(t, ev.Event.ID); n != 1 {
		t.Fatalf("confirmed_count = %d after the duplicate", n)
	}
	if _, err := e.svc.CancelMyRegistration(t.Context(), a, v.Registration.ID, ""); err != nil {
		t.Fatal(err)
	}
	again := e.mustRegister(t, a, ev.Event.ID, motivation)
	if again.Registration.ID == v.Registration.ID || again.Registration.Status != events.RegConfirmed {
		t.Fatalf("second registration = %+v", again.Registration)
	}
}

// TestRegister_UnverifiedUser_403: only verified students register (T-54,
// k-uv); a university event only its university's students; an event the
// caller does not see is 404.
func TestRegister_UnverifiedUser_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.regEvent(t, nil, motivationField)
	_, err := e.register(t, persona(t, "user_unverified"), ev.Event.ID, motivation)
	problemtest.WantProblem(t, err, 403, "students.verification_required")
	// Accounts without a student verification hold no grant; like
	// joinClub (orgs) they are told what is missing.
	for _, p := range []string{"university_rep", "company_member", "company_admin", "support", "platform_admin", "dpo"} {
		_, err := e.register(t, persona(t, p), ev.Event.ID, motivation)
		problemtest.WantProblem(t, err, 403, "students.verification_required")
	}
	_, err = e.register(t, authz.Anonymous(), ev.Event.ID, motivation)
	wantForbidden(t, err)
	// Verified students, minors and the club's own members register.
	for _, p := range []string{"student", "student_minor", "club_member", "club_admin", "event_staff", "other_club_admin"} {
		e.mustRegister(t, persona(t, p), ev.Event.ID, motivation)
	}
	// A members event is invisible to a student outside the club.
	members := e.regEvent(t, func(in *events.EventInput) { in.Visibility = events.VisibilityMembers }, motivationField)
	_, err = e.register(t, persona(t, "student"), members.Event.ID, motivation)
	wantNotFound(t, err)
	// A university event: uni_b's student does not see it; a club_a member
	// studying at uni_b sees it but registers only as a uni_a student.
	uni := authzUniversity("uni_a")
	uniEvent := e.regEvent(t, func(in *events.EventInput) {
		in.Visibility, in.VisibilityUniversityID = events.VisibilityUniversity, &uni
	}, motivationField)
	_, err = e.register(t, persona(t, "other_club_admin"), uniEvent.Event.ID, motivation)
	wantNotFound(t, err)
	stranger := persona(t, "other_club_admin")
	stranger.Memberships = append(stranger.Memberships, authz.Membership{OrgID: orgID("club_a"), OrgKind: authz.OrgClub,
		Role: authz.MemberMember})
	_, err = e.register(t, stranger, uniEvent.Event.ID, motivation)
	wantForbidden(t, err)
	e.mustRegister(t, persona(t, "student"), uniEvent.Event.ID, motivation)
	if n := len(e.audit.Of(authz.EventsRegistrationCreate)); n != 7 {
		t.Fatalf("%d registrations audited, want 7", n)
	}
}

// TestRegister_InvalidAnswers_422: the answers are checked against the
// current version (ValidateAnswers); an older version is 409; a minor
// cannot answer the sponsor consent field; nothing is written.
func TestRegister_InvalidAnswers_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, capacity(5, false), motivationField, tshirtField, sponsorField)
	a := persona(t, "student")
	_, err := e.register(t, a, ev.Event.ID, map[string]any{"tshirt": "m"})
	wantField(t, err, 422, "validation_failed", "body.answers.motivation")
	_, err = e.register(t, a, ev.Event.ID, map[string]any{"motivation": "x", "tshirt": "xxl"})
	wantField(t, err, 422, "validation_failed", "body.answers.tshirt")
	_, err = e.register(t, a, ev.Event.ID, map[string]any{"motivation": "x", "unknown_key": "y"})
	wantField(t, err, 422, "validation_failed", "body.answers.unknown_key")
	_, err = e.register(t, persona(t, "student_minor"), ev.Event.ID, map[string]any{"motivation": "x", "share_sponsor": true})
	wantField(t, err, 422, "validation_failed", "body.answers.share_sponsor")
	stale := uuid.NewV7()
	_, err = e.svc.CreateRegistration(ctx, a, ev.Event.ID, events.RegistrationInput{FormVersionID: &stale, Answers: motivation})
	problemtest.WantProblem(t, err, 409, "events.form_version_stale")
	if n := e.seats(t, ev.Event.ID); n != 0 || len(e.audit.Of(authz.EventsRegistrationCreate)) != 0 {
		t.Fatalf("refused answers wrote: %d seats", n)
	}
	// The current version by ID is accepted; the stored answers are typed
	// and cleaned.
	f, err := e.svc.GetForm(ctx, a, ev.Event.ID, consent.LocaleTR)
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.CreateRegistration(ctx, a, ev.Event.ID, events.RegistrationInput{FormVersionID: &f.Version.ID,
		Answers: map[string]any{"motivation": "  Öğrenmek istiyorum  ", "tshirt": "s"}})
	if err != nil {
		t.Fatal(err)
	}
	got := e.reg(t, a, v.Registration.ID)
	if got.Answers["motivation"] != "Öğrenmek istiyorum" || got.Answers["tshirt"] != "s" || len(got.AnswersSensitive) != 0 {
		t.Fatalf("stored answers = %v", got.Answers)
	}
	// A minor registers without the consent field (T-22).
	e.mustRegister(t, persona(t, "student_minor"), ev.Event.ID, motivation)
}

// TestRegister_ConsentFieldRecordedSameTx: a consent field answered yes is
// recorded in consent's ledger (scope: the registration) in the
// registration's transaction (01 §6.3), with its proof; the snapshot keeps
// the answer; a proof without a yes, or a yes without a proof, is 422.
func TestRegister_ConsentFieldRecordedSameTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, nil, motivationField, sponsorField)
	f, err := e.svc.GetForm(ctx, persona(t, "student"), ev.Event.ID, consent.LocaleTR)
	if err != nil || len(f.Notices) != 1 {
		t.Fatalf("form = %+v, %v", f, err)
	}
	proof := events.ConsentProof{Purpose: consent.PurposeEventSponsor, NoticeVersionID: f.Notices[0].ID,
		TextSHA256: f.Notices[0].BodySHA256}
	yes := map[string]any{"motivation": "x", "share_sponsor": true}
	a := persona(t, "student")
	_, err = e.svc.CreateRegistration(ctx, a, ev.Event.ID, events.RegistrationInput{Answers: yes})
	wantField(t, err, 422, "validation_failed", "body.consents")
	_, err = e.svc.CreateRegistration(ctx, a, ev.Event.ID, events.RegistrationInput{Answers: motivation,
		Consents: []events.ConsentProof{proof}})
	wantField(t, err, 422, "validation_failed", "body.consents[0]")
	v, err := e.svc.CreateRegistration(ctx, a, ev.Event.ID, events.RegistrationInput{Answers: yes,
		Consents: []events.ConsentProof{proof}, Source: events.SourceMobile})
	if err != nil {
		t.Fatal(err)
	}
	recs, txs := e.consent.records()
	if len(recs) != 1 {
		t.Fatalf("consents = %+v", recs)
	}
	r := recs[0]
	if r.UserID != a.UserID || r.Purpose != consent.PurposeEventSponsor || r.Action != consent.Granted ||
		r.ScopeID == nil || *r.ScopeID != v.Registration.ID || *r.NoticeVersionID != f.Notices[0].ID ||
		!bytes.Equal(r.TextSHA256, f.Notices[0].BodySHA256) || r.Channel != consent.ChannelMobile || r.Minor {
		t.Fatalf("consent = %+v", r)
	}
	if confirmed := e.out.txs[domainevent.TopicRegistrationConfirmed]; len(confirmed) != 1 || txs[0] == 0 || txs[0] != confirmed[0] {
		t.Fatalf("consent tx %v, registration.confirmed tx %v: not one transaction", txs, confirmed)
	}
	if got := e.reg(t, a, v.Registration.ID); !got.ConsentSnapshot["event_sponsor_share"] || got.Answers["share_sponsor"] != nil ||
		got.Registration.Source != events.SourceMobile {
		t.Fatalf("snapshot = %v, answers = %v", got.ConsentSnapshot, got.Answers)
	}
	// Answered no: no consent row, the snapshot says false.
	b := persona(t, "club_member")
	nv := e.mustRegister(t, b, ev.Event.ID, map[string]any{"motivation": "x", "share_sponsor": false})
	if recs, _ := e.consent.records(); len(recs) != 1 {
		t.Fatalf("a no was recorded: %+v", recs)
	}
	if got := e.reg(t, b, nv.Registration.ID); got.ConsentSnapshot["event_sponsor_share"] {
		t.Fatalf("snapshot of a no = %v", got.ConsentSnapshot)
	}
}

// TestRegister_SensitiveAnswersEncrypted_NotInList: a warn_special answer is
// sealed (02 §11.6 answers_sensitive_enc, bound to the registration), kept
// out of answers, purged 30 days after the event (answers 90), never in
// the organiser's list; the owner reads it back.
func TestRegister_SensitiveAnswersEncrypted_NotInList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, nil, motivationField, allergyField)
	a := persona(t, "student")
	v := e.mustRegister(t, a, ev.Event.ID, map[string]any{"motivation": "x", "allergy": "Fıstık alerjim var"})
	stored, err := e.mem.GetRegistration(ctx, ev.Event.OrgID, ev.Event.ID, v.Registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.AnswersSensitiveEnc) == 0 || bytes.Contains(stored.AnswersSensitiveEnc, []byte("Fıstık")) ||
		strings.Contains(string(stored.Answers), "allergy") || strings.Contains(string(stored.Answers), "Fıstık") {
		t.Fatalf("stored = answers %s, sealed %d bytes", stored.Answers, len(stored.AnswersSensitiveEnc))
	}
	end := ev.Event.EndsAt
	if stored.SensitivePurgeAt == nil || !stored.SensitivePurgeAt.Equal(end.Add(30*24*time.Hour)) ||
		!stored.AnswersPurgeAt.Equal(end.Add(90*24*time.Hour)) {
		t.Fatalf("purge dates = %v, %v", stored.AnswersPurgeAt, stored.SensitivePurgeAt)
	}
	// The ciphertext is bound to its registration.
	if _, err := testEnvelope().Open(ctx, events.SensitiveAnswersPurpose(uuid.NewV7()), stored.AnswersSensitiveEnc); err == nil {
		t.Fatal("the sealed answers open under another registration")
	}
	page, err := e.svc.ListEventRegistrations(ctx, persona(t, "club_admin"), ev.Event.ID, events.EventRegistrationQuery{}, events.Access{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("list = %+v, %v", page, err)
	}
	j, _ := json.Marshal(events.ToOrgDTO(page.Items[0]))
	if page.Items[0].AnswersSensitive != nil || strings.Contains(string(j), "allergy") || strings.Contains(string(j), "Fıstık") {
		t.Fatalf("the list carries the sensitive answer: %s", j)
	}
	mine := e.reg(t, a, v.Registration.ID)
	if mine.AnswersSensitive["allergy"] != "Fıstık alerjim var" || mine.Answers["allergy"] != nil {
		t.Fatalf("own view = %v / %v", mine.Answers, mine.AnswersSensitive)
	}
	if got := e.audit.Of(authz.EventsRegistrationReadOwn); len(got) != 1 ||
		!slices.Equal(got[0].Metadata["sensitive_fields"].([]string), []string{"allergy"}) {
		t.Fatalf("own sensitive read audit = %+v", got)
	}
	// Moving the event's end moves the purge dates.
	moved := start.Add(48 * time.Hour)
	cur, err := e.svc.GetEvent(ctx, persona(t, "club_admin"), ev.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateEvent(ctx, persona(t, "club_admin"), ev.Event.ID, cur.ETag, events.EventPatch{
		StartsAt: &moved, EndsAt: new(moved.Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	stored, _ = e.mem.GetRegistration(ctx, ev.Event.OrgID, ev.Event.ID, v.Registration.ID)
	if !stored.AnswersPurgeAt.Equal(moved.Add(time.Hour+90*24*time.Hour)) ||
		!stored.SensitivePurgeAt.Equal(moved.Add(time.Hour+30*24*time.Hour)) {
		t.Fatalf("purge dates after the move = %v, %v", stored.AnswersPurgeAt, stored.SensitivePurgeAt)
	}
}

// TestRegister_ManualMode_Submitted: a manual event takes applications as
// pending (registration.submitted; the applicant and the club's managers
// are notified); the organiser confirms (seat + ticket), waitlists or
// rejects; the note never reaches the student.
func TestRegister_ManualMode_Submitted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, func(in *events.EventInput) {
		in.RegistrationMode = events.ModeManual
		in.Capacity = new(1)
	}, motivationField)
	a, b, c := persona(t, "student"), persona(t, "club_member"), newStudent()
	pa := e.mustRegister(t, a, ev.Event.ID, motivation)
	pb := e.mustRegister(t, b, ev.Event.ID, motivation)
	pc := e.mustRegister(t, c, ev.Event.ID, motivation)
	if pa.Registration.Status != events.RegPending || pa.TicketAvailable || e.seats(t, ev.Event.ID) != 0 ||
		len(e.mem.Tickets(pa.Registration.ID)) != 0 {
		t.Fatalf("application = %+v", pa)
	}
	if subs := e.eventsOf(domainevent.TopicRegistrationSubmitted); len(subs) != 3 {
		t.Fatalf("submitted events = %d", len(subs))
	}
	e.deliver(t, domainevent.TopicRegistrationSubmitted, 0)
	sent := e.sender.sent()
	var toA, toAdmin int
	for _, m := range sent {
		switch {
		case *m.UserID == a.UserID && m.Template == "events.registration_submitted":
			toA++
		case *m.UserID == persona(t, "club_admin").UserID && m.Template == "events.registration_received":
			toAdmin++
		}
		if m.Channel != "in_app" || m.DedupeKey == "" {
			t.Fatalf("notice = %+v", m)
		}
	}
	if toA != 1 || toAdmin != 3 {
		t.Fatalf("notices %d to the applicant, %d to the admin: %+v", toA, toAdmin, sent)
	}
	admin := persona(t, "club_admin")
	// Members without the role and other clubs cannot decide.
	_, err := e.svc.DecideRegistration(ctx, b, ev.Event.ID, pa.Registration.ID, events.Decision{Status: events.RegConfirmed})
	wantForbidden(t, err)
	_, err = e.svc.DecideRegistration(ctx, persona(t, "other_club_admin"), ev.Event.ID, pa.Registration.ID,
		events.Decision{Status: events.RegConfirmed})
	wantForbidden(t, err)
	d, err := e.svc.DecideRegistration(ctx, admin, ev.Event.ID, pa.Registration.ID, events.Decision{Status: events.RegConfirmed,
		Note: "Motivasyonu güçlü"})
	if err != nil || d.Registration.Status != events.RegConfirmed || d.Registration.ApprovedBy == nil ||
		*d.Registration.ApprovedBy != admin.UserID || events.ToOrgDTO(d).DecisionNote != "Motivasyonu güçlü" {
		t.Fatalf("decision = %+v, %v", d.Registration, err)
	}
	if e.seats(t, ev.Event.ID) != 1 || len(e.mem.Tickets(pa.Registration.ID)) != 1 {
		t.Fatal("the confirmation took no seat or ticket")
	}
	if j, _ := json.Marshal(events.ToSelfDTO(e.reg(t, a, pa.Registration.ID))); strings.Contains(string(j), "Motivasyonu") {
		t.Fatalf("the student sees the note: %s", j)
	}
	_, err = e.svc.DecideRegistration(ctx, admin, ev.Event.ID, pa.Registration.ID, events.Decision{Status: events.RegRejected})
	problemtest.WantProblem(t, err, 409, "events.invalid_state")
	_, err = e.svc.DecideRegistration(ctx, admin, ev.Event.ID, pb.Registration.ID, events.Decision{Status: events.RegConfirmed})
	problemtest.WantProblem(t, err, 409, "events.capacity_full")
	_, err = e.svc.DecideRegistration(ctx, admin, ev.Event.ID, pb.Registration.ID, events.Decision{Status: events.RegWaitlisted})
	problemtest.WantProblem(t, err, 409, "events.invalid_state") // no waiting list
	r, err := e.svc.DecideRegistration(ctx, admin, ev.Event.ID, pb.Registration.ID, events.Decision{Status: events.RegRejected})
	if err != nil || r.Registration.Status != events.RegRejected {
		t.Fatalf("rejection = %+v, %v", r.Registration, err)
	}
	if got := e.out.Topics(); !slices.Contains(got, domainevent.TopicRegistrationRejected) ||
		len(e.eventsOf(domainevent.TopicRegistrationConfirmed)) != 1 {
		t.Fatalf("topics = %v", got)
	}
	// A rejected applicant may apply again; pc withdraws.
	if v, err := e.svc.CancelMyRegistration(ctx, c, pc.Registration.ID, ""); err != nil || v.Registration.Status != events.RegWithdrawn {
		t.Fatalf("withdrawal = %+v, %v", v.Registration, err)
	}
	e.mustRegister(t, b, ev.Event.ID, motivation)
	if got := e.audit.Of(authz.EventsRegistrationDecide); len(got) != 2 || got[0].Metadata["decision"] != "confirmed" {
		t.Fatalf("decision audit = %+v", got)
	}
}

// TestCancel_PromotesFirstWaitlisted_FIFO: a cancelled seat goes to the
// first on the waiting list in the same transaction (01 §6.3): ticket,
// registration.confirmed (Promoted); the count never moves; a withdrawal
// frees nothing; an organiser's cancellation promotes too.
func TestCancel_PromotesFirstWaitlisted_FIFO(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, capacity(2, true), motivationField)
	ss := []authz.Actor{newStudent(), newStudent(), newStudent(), newStudent(), newStudent()}
	vs := make([]events.RegistrationView, len(ss))
	for i, s := range ss {
		e.clk.Advance(time.Second)
		vs[i] = e.mustRegister(t, s, ev.Event.ID, motivation)
	}
	got, err := e.svc.CancelMyRegistration(ctx, ss[0], vs[0].Registration.ID, "Programım değişti")
	if err != nil || got.Registration.Status != events.RegCancelled || got.Registration.CancelledByKind == nil ||
		*got.Registration.CancelledByKind != events.ByUser || got.TicketAvailable {
		t.Fatalf("cancel = %+v, %v", got.Registration, err)
	}
	if ts := e.mem.Tickets(vs[0].Registration.ID); len(ts) != 1 || ts[0].Status != events.TicketRevoked ||
		*ts[0].RevokeReason != events.RevokeRegistrationCancelled {
		t.Fatalf("cancelled ticket = %+v", ts)
	}
	if p := e.reg(t, ss[2], vs[2].Registration.ID); p.Registration.Status != events.RegConfirmed || !p.TicketAvailable {
		t.Fatalf("first waitlisted = %+v", p.Registration)
	}
	for i, want := range map[int]int{3: 1, 4: 2} {
		if p := e.reg(t, ss[i], vs[i].Registration.ID); p.WaitlistPosition == nil || *p.WaitlistPosition != want {
			t.Fatalf("position of %d = %v", i, p.WaitlistPosition)
		}
	}
	if n := e.seats(t, ev.Event.ID); n != 2 {
		t.Fatalf("confirmed_count = %d", n)
	}
	cancelled, promoted := e.out.txs[domainevent.TopicRegistrationCancelled], e.out.txs[domainevent.TopicRegistrationConfirmed]
	if len(cancelled) != 1 || cancelled[0] != promoted[len(promoted)-1] {
		t.Fatalf("cancel tx %v, promotion txs %v: not one transaction", cancelled, promoted)
	}
	confirmed := e.eventsOf(domainevent.TopicRegistrationConfirmed)
	p, _ := domainevent.Decode[domainevent.RegistrationConfirmed](confirmed[len(confirmed)-1])
	if !p.Promoted || p.RegistrationID != vs[2].Registration.ID || p.TicketVersion != 1 {
		t.Fatalf("promotion payload = %+v", p)
	}
	if got := e.audit.Of(authz.EventsRegistrationPromoteWaitlist); len(got) != 1 || got[0].SystemJob != "events.promote_waitlist" {
		t.Fatalf("promotion audit = %+v", got)
	}
	// The last on the list withdraws: no seat moves, no cancellation event.
	if w, err := e.svc.CancelMyRegistration(ctx, ss[4], vs[4].Registration.ID, ""); err != nil || w.Registration.Status != events.RegWithdrawn {
		t.Fatalf("withdrawal = %+v, %v", w.Registration, err)
	}
	if n := len(e.eventsOf(domainevent.TopicRegistrationCancelled)); n != 1 {
		t.Fatalf("%d cancellation events", n)
	}
	// The organiser cancels a confirmed registration: the next one moves up.
	_, err = e.svc.CancelEventRegistration(ctx, persona(t, "club_admin"), ev.Event.ID, vs[1].Registration.ID, "")
	wantField(t, err, 422, "validation_failed", "body.cancel_reason")
	oc, err := e.svc.CancelEventRegistration(ctx, persona(t, "club_admin"), ev.Event.ID, vs[1].Registration.ID, "Kural ihlali")
	if err != nil || oc.Registration.Status != events.RegCancelled || *oc.Registration.CancelledByKind != events.ByOrg {
		t.Fatalf("org cancel = %+v, %v", oc.Registration, err)
	}
	if p := e.reg(t, ss[3], vs[3].Registration.ID); p.Registration.Status != events.RegConfirmed {
		t.Fatalf("second promotion = %+v", p.Registration)
	}
	_, err = e.svc.CancelEventRegistration(ctx, persona(t, "club_admin"), ev.Event.ID, vs[1].Registration.ID, "Kural ihlali")
	problemtest.WantProblem(t, err, 409, "events.invalid_state")
	// Promotion is idempotent: a reconciliation without drift moves no one.
	before := len(e.eventsOf(domainevent.TopicRegistrationConfirmed))
	e.job(t, events.ReconcileCountsJobName)
	if after := len(e.eventsOf(domainevent.TopicRegistrationConfirmed)); after != before || e.seats(t, ev.Event.ID) != 2 {
		t.Fatalf("reconcile moved the list: %d → %d", before, after)
	}
}

// TestCancel_AfterDeadline_409: a confirmed registration cannot be
// cancelled by its owner after cancel_deadline_at (04 §E.6); leaving the
// waiting list and the organiser's cancellation stay possible.
func TestCancel_AfterDeadline_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	deadline := start.Add(-24 * time.Hour)
	ev := e.regEvent(t, func(in *events.EventInput) {
		in.Capacity, in.WaitlistEnabled, in.CancelDeadlineAt = new(1), true, &deadline
	}, motivationField)
	a, b := persona(t, "student"), persona(t, "club_member")
	va := e.mustRegister(t, a, ev.Event.ID, motivation)
	vb := e.mustRegister(t, b, ev.Event.ID, motivation)
	e.clk.Set(deadline.Add(time.Minute))
	_, err := e.svc.CancelMyRegistration(ctx, a, va.Registration.ID, "")
	problemtest.WantProblem(t, err, 409, "events.registration_closed")
	// Someone else's registration is not found.
	_, err = e.svc.CancelMyRegistration(ctx, b, va.Registration.ID, "")
	wantNotFound(t, err)
	if w, err := e.svc.CancelMyRegistration(ctx, b, vb.Registration.ID, ""); err != nil || w.Registration.Status != events.RegWithdrawn {
		t.Fatalf("withdrawal after the deadline = %+v, %v", w.Registration, err)
	}
	if _, err := e.svc.CancelEventRegistration(ctx, persona(t, "club_admin"), ev.Event.ID, va.Registration.ID, "Etkinlik kuralı"); err != nil {
		t.Fatal(err)
	}
	if n := e.seats(t, ev.Event.ID); n != 0 {
		t.Fatalf("confirmed_count = %d", n)
	}
}

// TestEventCancelled_CancelsAllRegistrations: the event.cancelled consumer
// cancels every live registration (system), revokes the tickets
// (event_cancelled), releases the seats without promoting anyone, emits
// registration.cancelled for each (the registrants are notified), and a
// second delivery changes nothing.
func TestEventCancelled_CancelsAllRegistrations(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, capacity(2, true), motivationField)
	ss := []authz.Actor{newStudent(), newStudent(), newStudent()}
	vs := make([]events.RegistrationView, len(ss))
	for i, s := range ss {
		vs[i] = e.mustRegister(t, s, ev.Event.ID, motivation)
	}
	if _, err := e.svc.CancelEvent(ctx, persona(t, "club_admin"), ev.Event.ID, "Salon kapandı"); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, domainevent.TopicEventCancelled, 0)
	for i, s := range ss {
		got := e.reg(t, s, vs[i].Registration.ID)
		if got.Registration.Status != events.RegCancelled || *got.Registration.CancelledByKind != events.BySystem {
			t.Fatalf("registration %d = %+v", i, got.Registration)
		}
	}
	if ts := e.mem.Tickets(vs[0].Registration.ID); len(ts) != 1 || *ts[0].RevokeReason != events.RevokeEventCancelled {
		t.Fatalf("ticket = %+v", ts)
	}
	cancels := e.eventsOf(domainevent.TopicRegistrationCancelled)
	if len(cancels) != 3 || e.seats(t, ev.Event.ID) != 0 || len(e.eventsOf(domainevent.TopicRegistrationConfirmed)) != 2 {
		t.Fatalf("%d cancellations, %d seats", len(cancels), e.seats(t, ev.Event.ID))
	}
	p, _ := domainevent.Decode[domainevent.RegistrationCancelled](cancels[0])
	if p.By != "system" {
		t.Fatalf("payload = %+v", p)
	}
	e.deliver(t, domainevent.TopicEventCancelled, 0)
	if n := len(e.eventsOf(domainevent.TopicRegistrationCancelled)); n != 3 {
		t.Fatalf("a second delivery cancelled again: %d", n)
	}
	e.deliver(t, domainevent.TopicRegistrationCancelled, 0)
	notices := 0
	for _, m := range e.sender.sent() {
		if m.Template == "events.registration_cancelled" {
			notices++
		}
	}
	if notices != 3 {
		t.Fatalf("%d cancellation notices", notices)
	}
	// A cancelled event takes no registration.
	_, err := e.register(t, newStudent(), ev.Event.ID, motivation)
	problemtest.WantProblem(t, err, 409, "events.registration_closed")
}

// TestWaitlistExpire_AfterEventStart: events.waitlist_expire turns the
// waiting list of a started event into expired (system, audited); before
// the start nothing changes; confirmed registrations stay.
func TestWaitlistExpire_AfterEventStart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.regEvent(t, capacity(1, true), motivationField)
	a, b := newStudent(), newStudent()
	va := e.mustRegister(t, a, ev.Event.ID, motivation)
	vb := e.mustRegister(t, b, ev.Event.ID, motivation)
	e.job(t, events.WaitlistExpireJobName)
	if got := e.reg(t, b, vb.Registration.ID); got.Registration.Status != events.RegWaitlisted {
		t.Fatalf("before the start: %s", got.Registration.Status)
	}
	e.clk.Set(start.Add(time.Minute))
	e.job(t, events.WaitlistExpireJobName)
	if got := e.reg(t, b, vb.Registration.ID); got.Registration.Status != events.RegExpired || got.WaitlistPosition != nil {
		t.Fatalf("after the start: %+v", got)
	}
	if got := e.reg(t, a, va.Registration.ID); got.Registration.Status != events.RegConfirmed {
		t.Fatalf("confirmed = %s", got.Registration.Status)
	}
	if got := e.audit.Of(authz.EventsRegistrationExpire); len(got) != 1 || got[0].SystemJob != "events.waitlist_expire" ||
		*got[0].ResourceID != vb.Registration.ID {
		t.Fatalf("expire audit = %+v", got)
	}
	e.job(t, events.WaitlistExpireJobName)
	if n := len(e.audit.Of(authz.EventsRegistrationExpire)); n != 1 {
		t.Fatalf("expired again: %d", n)
	}
}

// TestReconcileCounts_FixesDrift: events.reconcile_counts sets
// confirmed_count to the real count; a phantom seat freed this way goes to
// the waiting list.
func TestReconcileCounts_FixesDrift(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.regEvent(t, capacity(2, true), motivationField)
	org := ev.Event.OrgID
	a, w := newStudent(), newStudent()
	now := e.clk.Now()
	ra := events.Registration{ID: uuid.NewV7(), EventID: ev.Event.ID, OrgID: org, UserID: a.UserID, Status: events.RegConfirmed,
		Source: events.SourceWeb, ConfirmedAt: &now, AnswersPurgeAt: now.Add(time.Hour)}
	rw := events.Registration{ID: uuid.NewV7(), EventID: ev.Event.ID, OrgID: org, UserID: w.UserID, Status: events.RegWaitlisted,
		Source: events.SourceWeb, WaitlistedAt: &now, AnswersPurgeAt: now.Add(time.Hour)}
	for _, r := range []events.Registration{ra, rw} {
		if err := e.mem.PutRegistration(r); err != nil {
			t.Fatal(err)
		}
	}
	// Too low: one seat is not counted.
	e.mem.SetSeatCount(ev.Event.ID, 0)
	e.job(t, events.ReconcileCountsJobName)
	if n := e.seats(t, ev.Event.ID); n != 1 {
		t.Fatalf("after fixing a low count: %d", n)
	}
	// Too high: a phantom seat kept the waiting registration out.
	e.mem.SetSeatCount(ev.Event.ID, 2)
	e.job(t, events.ReconcileCountsJobName)
	if n := e.seats(t, ev.Event.ID); n != 2 {
		t.Fatalf("after fixing a high count and promoting: %d", n)
	}
	if got := e.reg(t, w, rw.ID); got.Registration.Status != events.RegConfirmed || !got.TicketAvailable {
		t.Fatalf("the waiting registration = %+v", got.Registration)
	}
}

// TestListRegistrations_SupportWithoutReason_403: support and
// platform_admin list by break-glass only (X-Koro-Access-Reason; 03
// §6.3), audited with the reason; a sensitive club's event stays hidden
// (Y2 → 404); the organiser's managers list without a reason; others 403.
func TestListRegistrations_SupportWithoutReason_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, nil, motivationField)
	v := e.mustRegister(t, persona(t, "student"), ev.Event.ID, motivation)
	q := events.EventRegistrationQuery{}
	for _, p := range []string{"support", "platform_admin"} {
		_, err := e.svc.ListEventRegistrations(ctx, persona(t, p), ev.Event.ID, q, events.Access{})
		problemtest.WantProblem(t, err, 403, "authz.break_glass_reason_required")
		_, err = e.svc.ListEventRegistrations(ctx, persona(t, p), ev.Event.ID, q, events.Access{Reason: "kısa"})
		problemtest.WantProblem(t, err, 403, "authz.break_glass_reason_required")
	}
	for _, p := range []string{"club_member", "student", "dpo", "moderator", "event_staff", "other_club_admin"} {
		_, err := e.svc.ListEventRegistrations(ctx, persona(t, p), ev.Event.ID, q, events.Access{})
		wantForbidden(t, err)
	}
	if n := len(e.audit.Of(authz.EventsRegistrationList)); n != 0 {
		t.Fatalf("refused lists audited: %d", n)
	}
	why := events.Access{Reason: "Destek talebi 4711: kayıt sorunu inceleniyor", TicketRef: "4711"}
	page, err := e.svc.ListEventRegistrations(ctx, persona(t, "support"), ev.Event.ID, q, why)
	if err != nil || len(page.Items) != 1 || page.Items[0].ShowNote || page.Items[0].User == nil ||
		page.Items[0].User.LastName != "Yılmaz" {
		t.Fatalf("break-glass list = %+v, %v", page, err)
	}
	logs := e.audit.Of(authz.EventsRegistrationList)
	if len(logs) != 1 || logs[0].Reason != why.Reason || logs[0].Metadata[audit.MetaBreakGlass] != true ||
		logs[0].Metadata[audit.MetaTicketRef] != "4711" || *logs[0].ResourceID != ev.Event.ID {
		t.Fatalf("break-glass audit = %+v", logs)
	}
	if _, err := e.svc.ListEventRegistrations(ctx, persona(t, "club_admin"), ev.Event.ID, q, events.Access{}); err != nil {
		t.Fatal(err)
	}
	if logs := e.audit.Of(authz.EventsRegistrationList); len(logs) != 2 || logs[1].Metadata[audit.MetaBreakGlass] != nil {
		t.Fatalf("manager list audit = %+v", logs)
	}
	// One registration by break-glass: no sensitive answers, the subject is
	// named for notify.
	one, err := e.svc.GetEventRegistration(ctx, persona(t, "platform_admin"), ev.Event.ID, v.Registration.ID, why)
	if err != nil || one.AnswersSensitive != nil {
		t.Fatalf("break-glass read = %+v, %v", one, err)
	}
	if r := e.audit.Of(authz.EventsRegistrationRead); len(r) != 1 || r[0].Metadata[audit.MetaSubjectUserID] != v.Registration.UserID.String() {
		t.Fatalf("break-glass read audit = %+v", r)
	}
	// A sensitive club's event: hidden from break-glass.
	sens := e.create(t, "club_admin_sensitive", "club_sensitive", input("Hassas Kulüp Buluşması"))
	if err := e.mem.SetFormVersion(sens.Event.ID, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PublishEvent(ctx, persona(t, "club_admin_sensitive"), sens.Event.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"support", "platform_admin"} {
		_, err := e.svc.ListEventRegistrations(ctx, persona(t, p), sens.Event.ID, q, why)
		wantNotFound(t, err)
	}
	if _, err := e.svc.ListEventRegistrations(ctx, persona(t, "club_admin_sensitive"), sens.Event.ID, q, events.Access{}); err != nil {
		t.Fatal(err)
	}
}

// TestGetRegistration_SensitiveReadAudited: the organiser's manager gets
// the special-category answers of one registration; the read is audited
// with the keys read (never the values).
func TestGetRegistration_SensitiveReadAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, nil, motivationField, allergyField)
	v := e.mustRegister(t, persona(t, "student"), ev.Event.ID, map[string]any{"motivation": "x", "allergy": "Glüten"})
	admin := persona(t, "club_admin")
	got, err := e.svc.GetEventRegistration(ctx, admin, ev.Event.ID, v.Registration.ID, events.Access{})
	if err != nil || got.AnswersSensitive["allergy"] != "Glüten" || got.Answers["motivation"] != "x" || !got.ShowNote {
		t.Fatalf("read = %+v, %v", got, err)
	}
	logs := e.audit.Of(authz.EventsRegistrationRead)
	if len(logs) != 1 || !slices.Equal(logs[0].Metadata["sensitive_fields"].([]string), []string{"allergy"}) ||
		logs[0].Metadata[audit.MetaBreakGlass] != nil || *logs[0].ResourceID != v.Registration.ID {
		t.Fatalf("audit = %+v", logs)
	}
	if b, _ := json.Marshal(logs[0].Metadata); strings.Contains(string(b), "Glüten") {
		t.Fatalf("the audit holds the answer: %s", b)
	}
	// Another event's registration ID is not found here (06 H-38).
	other := e.regEvent(t, nil, motivationField)
	_, err = e.svc.GetEventRegistration(ctx, admin, other.Event.ID, v.Registration.ID, events.Access{})
	wantNotFound(t, err)
	// A member without the role sees nothing.
	_, err = e.svc.GetEventRegistration(ctx, persona(t, "club_member"), ev.Event.ID, v.Registration.ID, events.Access{})
	wantForbidden(t, err)
}

// TestGetEvent_MyRegistrationAndOnlineURL: the event page shows the
// caller's live registration, and online_url to a confirmed attendee only
// (03 §5.6).
func TestGetEvent_MyRegistrationAndOnlineURL(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, func(in *events.EventInput) {
		in.Format, in.OnlineURL, in.City, in.LocationName = events.FormatOnline, "https://meet.example.test/oda", "", ""
		in.Capacity, in.WaitlistEnabled = new(1), true
	}, motivationField)
	a, b := persona(t, "student"), persona(t, "club_member")
	e.mustRegister(t, a, ev.Event.ID, motivation)
	e.mustRegister(t, b, ev.Event.ID, motivation)
	va, err := e.svc.GetEvent(ctx, a, ev.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j := eventJSON(t, va); !strings.Contains(j, `"online_url":"https://meet.example.test/oda"`) ||
		!strings.Contains(j, `"my_registration":{"registration_id"`) || !strings.Contains(j, `"status":"confirmed"}`) {
		t.Fatalf("attendee view = %s", j)
	}
	vb, err := e.svc.GetEvent(ctx, b, ev.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j := eventJSON(t, vb); strings.Contains(j, "online_url") || !strings.Contains(j, `"status":"waitlisted"}`) {
		t.Fatalf("waitlisted view = %s", j)
	}
	anon, err := e.svc.GetEvent(ctx, authz.Anonymous(), ev.Event.ID)
	if err != nil || strings.Contains(eventJSON(t, anon), "my_registration") {
		t.Fatalf("anonymous view = %v", err)
	}
}

// TestUpdateEvent_RaisedCapacityPromotes: raising the capacity of a
// published event gives the new seats to the waiting list (FIFO).
func TestUpdateEvent_RaisedCapacityPromotes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, capacity(1, true), motivationField)
	ss := []authz.Actor{newStudent(), newStudent(), newStudent()}
	vs := make([]events.RegistrationView, len(ss))
	for i, s := range ss {
		e.clk.Advance(time.Second)
		vs[i] = e.mustRegister(t, s, ev.Event.ID, motivation)
	}
	cur, err := e.svc.GetEvent(ctx, persona(t, "club_admin"), ev.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateEvent(ctx, persona(t, "club_admin"), ev.Event.ID, cur.ETag, events.EventPatch{Capacity: new(2)}); err != nil {
		t.Fatal(err)
	}
	if got := e.reg(t, ss[1], vs[1].Registration.ID); got.Registration.Status != events.RegConfirmed {
		t.Fatalf("first waiting = %s", got.Registration.Status)
	}
	if got := e.reg(t, ss[2], vs[2].Registration.ID); got.Registration.Status != events.RegWaitlisted || *got.WaitlistPosition != 1 {
		t.Fatalf("second waiting = %+v", got)
	}
	// Lowering it below the confirmed count is refused.
	cur, _ = e.svc.GetEvent(ctx, persona(t, "club_admin"), ev.Event.ID)
	_, err = e.svc.UpdateEvent(ctx, persona(t, "club_admin"), ev.Event.ID, cur.ETag, events.EventPatch{Capacity: new(1)})
	wantField(t, err, 422, "validation_failed", "body.capacity")
}

// TestRegistrations_ReaderAndPrivacy: the students hook counts a member's
// registrations of a club; the admin summary leaves sensitive events out;
// the KVKK export holds the registrations with the opened answers, the
// erasure deletes them and gives the seat to the waiting list.
func TestRegistrations_ReaderAndPrivacy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, capacity(1, true), motivationField, allergyField)
	a, w := persona(t, "student"), newStudent()
	va := e.mustRegister(t, a, ev.Event.ID, map[string]any{"motivation": "x", "allergy": "Süt"})
	vw := e.mustRegister(t, w, ev.Event.ID, motivation)
	sensitive := e.regEvent(t, func(in *events.EventInput) { in.IsSensitiveCategory = true }, motivationField)
	e.mustRegister(t, a, sensitive.Event.ID, motivation)
	if n, err := e.svc.CountInOrg(ctx, orgID("club_a"), a.UserID); err != nil || n != 2 {
		t.Fatalf("CountInOrg = %d, %v", n, err)
	}
	sum, err := e.svc.SummaryOf(ctx, a.UserID)
	if err != nil || len(sum) != 1 || sum[0].EventID != ev.Event.ID || sum[0].Status != events.RegConfirmed {
		t.Fatalf("SummaryOf = %+v, %v", sum, err)
	}
	exp, err := e.mod.Privacy.Export(ctx, a.UserID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range exp.Items {
		if it["registration_id"] == va.Registration.ID.String() {
			found = true
			if it["answers_sensitive"].(map[string]any)["allergy"] != "Süt" || len(it["tickets"].([]map[string]any)) != 1 {
				t.Fatalf("export item = %+v", it)
			}
			if b, _ := json.Marshal(it); bytes.Contains(b, []byte("manual_code")) || bytes.Contains(b, []byte("token")) {
				t.Fatalf("export holds ticket credentials: %s", b)
			}
		}
	}
	if !found {
		t.Fatalf("export = %+v", exp.Items)
	}
	res, err := e.mod.Privacy.Erase(ctx, a.UserID)
	if err != nil || res.Deleted != 2 {
		t.Fatalf("erase = %+v, %v", res, err)
	}
	if _, err := e.svc.GetMyRegistration(ctx, a, va.Registration.ID); err == nil {
		t.Fatal("the erased registration is still there")
	}
	if got := e.reg(t, w, vw.Registration.ID); got.Registration.Status != events.RegConfirmed {
		t.Fatalf("the waiting registration after the erasure = %s", got.Registration.Status)
	}
	if res, err := e.mod.Privacy.Erase(ctx, a.UserID); err != nil || res.Deleted != 0 {
		t.Fatalf("second erase = %+v, %v", res, err)
	}
}

// TestRegister_ClosedWindowAndState: registration needs a published event
// with registration, inside its window (409).
func TestRegister_ClosedWindowAndState(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opens := t0.Add(24 * time.Hour)
	ev := e.regEvent(t, func(in *events.EventInput) { in.RegistrationOpensAt = &opens }, motivationField)
	_, err := e.register(t, newStudent(), ev.Event.ID, motivation)
	problemtest.WantProblem(t, err, 409, "events.registration_closed")
	e.clk.Set(opens)
	e.mustRegister(t, newStudent(), ev.Event.ID, motivation)
	e.clk.Set(start.Add(3 * time.Hour))
	_, err = e.register(t, newStudent(), ev.Event.ID, motivation)
	problemtest.WantProblem(t, err, 409, "events.registration_closed")
	e.clk.Set(t0)
	draft := e.create(t, "club_admin", "club_a", input("Taslak Kayıt"))
	_, err = e.register(t, persona(t, "club_admin"), draft.Event.ID, motivation)
	problemtest.WantProblem(t, err, 409, "events.invalid_state")
	noReg := input("Kayıtsız")
	noReg.RequiresRegistration = new(false)
	open := e.published(t, noReg)
	_, err = e.register(t, persona(t, "student"), open.Event.ID, motivation)
	problemtest.WantProblem(t, err, 409, "events.invalid_state")
}

// TestRegistrationTemplates_Exist: every notice template of the module is
// embedded in notify as an in-app template of the registration_updates
// preference (R8: events adds files only).
func TestRegistrationTemplates_Exist(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range events.RegistrationTemplates {
		m, ok := tpl.Meta(id)
		if !ok || !m.Supports(notify.ChannelInApp) || m.Preference != notify.PrefRegistrationUpdates ||
			m.Category != notify.CategoryTransactional {
			t.Fatalf("template %s = %+v, %v", id, m, ok)
		}
		for _, loc := range []string{"tr", "en"} {
			if n, ok := tpl.Notice(id, loc); !ok || n.Title == "" || n.Body == "" {
				t.Fatalf("template %s/%s = %+v, %v", id, loc, n, ok)
			}
		}
	}
}
