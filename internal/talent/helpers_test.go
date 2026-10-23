package talent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/hibiken/asynq"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/students"
	"koro/internal/talent"
)

// t0 is the test clock: a Tuesday noon in Istanbul (09:00 UTC).
var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// fakeStudents is the students port: profiles (with the minor flag) and
// ranking choices.
type fakeStudents struct {
	mu      sync.Mutex
	cohorts map[uuid.UUID]students.Cohort
	choices map[uuid.UUID]students.RankingChoice
}

func (f *fakeStudents) Cohorts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]students.Cohort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]students.Cohort{}
	for _, id := range ids {
		if c, ok := f.cohorts[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

func (f *fakeStudents) RankingChoice(_ context.Context, id uuid.UUID) (students.RankingChoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.choices[id]; ok {
		return c, nil
	}
	return students.RankingDefault, nil
}

func (f *fakeStudents) setChoice(id uuid.UUID, c students.RankingChoice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.choices[id] = c
}

// fakeDirectory reports the sensitive clubs.
type fakeDirectory struct{ sensitive map[uuid.UUID]bool }

func (f fakeDirectory) SensitiveOrgIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if f.sensitive[id] {
			out[id] = true
		}
	}
	return out, nil
}

// fakeAccounts is identity's account age: the creation instants.
type fakeAccounts struct {
	mu      sync.Mutex
	created map[uuid.UUID]time.Time
}

func (f *fakeAccounts) AccountAge(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]time.Time{}
	for _, id := range ids {
		if c, ok := f.created[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

func (f *fakeAccounts) set(id uuid.UUID, created time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created[id] = created
}

// fakeEvents is the events participation read: the event of every source,
// the figures of every event, and how often the figures were read.
type fakeEvents struct {
	mu        sync.Mutex
	of        map[uuid.UUID]uuid.UUID
	facts     map[uuid.UUID]talent.EventFacts
	factReads int
}

func (f *fakeEvents) EventsOf(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]uuid.UUID{}
	for _, id := range ids {
		if ev, ok := f.of[id]; ok {
			out[id] = ev
		}
	}
	return out, nil
}

func (f *fakeEvents) EventFacts(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]talent.EventFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.factReads++
	out := map[uuid.UUID]talent.EventFacts{}
	for _, id := range ids {
		if x, ok := f.facts[id]; ok {
			out[id] = x
		}
	}
	return out, nil
}

// event registers an event of org with its figures and returns its ID.
func (f *fakeEvents) event(org uuid.UUID, confirmed, distinct int) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.facts[id] = talent.EventFacts{OrgID: org, Confirmed: confirmed, DistinctCheckins: distinct}
	return id
}

// source maps a registration or check-in ID to its event and returns it.
func (f *fakeEvents) source(event uuid.UUID) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.of[id] = event
	return id
}

// fakeMemberships is orgs' MembershipsOf.
type fakeMemberships struct {
	mu sync.Mutex
	m  map[uuid.UUID][]authz.Membership
}

func (f *fakeMemberships) MembershipsOf(_ context.Context, id uuid.UUID) ([]authz.Membership, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[id], nil
}

func (f *fakeMemberships) add(user, org uuid.UUID, kind authz.OrgKind, role authz.MemberRole) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[user] = append(f.m[user], authz.Membership{OrgID: org, OrgKind: kind, Role: role})
}

// fakeAudit records the events it is given.
type fakeAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (f *fakeAudit) Log(_ context.Context, e audit.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeAudit) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	for i, e := range f.events {
		out[i] = e.Action
	}
	return out
}

// reauthPassword is the proof the fake re-authentication accepts.
const reauthPassword = "right-password-123"

// fakeReauth is identity's Verify: no proof → reauth_required, a wrong
// password → reauth_failed.
type fakeReauth struct{}

func (fakeReauth) Verify(_ context.Context, _ uuid.UUID, p identity.ReauthProof) (string, error) {
	switch {
	case p.Password == "" && p.TOTPCode == "":
		return "", identity.ErrReauthRequired
	case p.Password != reauthPassword:
		return "", identity.ErrReauthFailed
	}
	return "password", nil
}

// goodProof is the accepted proof.
var goodProof = identity.ReauthProof{Password: reauthPassword}

// env is the module over the memory store and fake ports.
type env struct {
	mod      talent.Module
	store    *talent.MemStore
	students *fakeStudents
	dir      fakeDirectory
	accounts *fakeAccounts
	events   *fakeEvents
	members  *fakeMemberships
	audit    *fakeAudit
	reauth   *audittest.LateReauth[identity.ReauthProof]
	clk      *clock.Fake
	subs     *outbox.Registry
	cfg      talent.ConfigVersion
}

func newEnv(t *testing.T, enabled bool) *env { t.Helper(); return buildEnv(t, enabled, true) }

// buildEnv builds the environment with the auditor API closed; withConfig
// inserts a config version in force since the day before t0.
func buildEnv(t *testing.T, enabled, withConfig bool) *env {
	t.Helper()
	return buildEnvWith(t, envOptions{collection: enabled, config: withConfig})
}

// envOptions are the switches of an environment.
type envOptions struct {
	collection bool // talent.default_enabled
	auditorAPI bool // talent.auditor_api_enabled
	config     bool // a config version in force since the day before t0
	noReauth   bool // no re-authentication port (503)
}

func buildEnvWith(t *testing.T, o envOptions) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{store: talent.NewMemStore(clk.Now), clk: clk, audit: &fakeAudit{}, subs: outbox.NewRegistry(),
		students: &fakeStudents{cohorts: map[uuid.UUID]students.Cohort{}, choices: map[uuid.UUID]students.RankingChoice{}},
		dir:      fakeDirectory{sensitive: map[uuid.UUID]bool{}}, reauth: &audittest.LateReauth[identity.ReauthProof]{Next: fakeReauth{}}}
	e.newPorts()
	d := talent.Deps{Store: e.store, Tx: db.NopTx{}, Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Students: e.students, Directory: e.dir, Accounts: e.accounts, Events: e.events, Memberships: e.members,
		CollectionEnabled: o.collection, AuditorAPIEnabled: o.auditorAPI}
	if !o.noReauth {
		d.Reauth = e.reauth
	}
	mod, err := talent.New(d)
	if err != nil {
		t.Fatal(err)
	}
	e.mod = mod
	mod.Subscribe(e.subs)
	if err := e.subs.Err(); err != nil {
		t.Fatal(err)
	}
	if o.config {
		e.cfg = e.config(t, t0.Add(-24*time.Hour))
	}
	return e
}

// newPorts sets empty identity, events and orgs fakes (BE-085).
func (e *env) newPorts() {
	e.accounts = &fakeAccounts{created: map[uuid.UUID]time.Time{}}
	e.events = &fakeEvents{of: map[uuid.UUID]uuid.UUID{}, facts: map[uuid.UUID]talent.EventFacts{}}
	e.members = &fakeMemberships{m: map[uuid.UUID][]authz.Membership{}}
}

// isPermanent reports a task error that asynq does not retry.
func isPermanent(err error) bool { return errors.Is(err, asynq.SkipRetry) }

// inTalentTx runs fn on the store inside a talent transaction.
func (e *env) inTalentTx(t *testing.T, fn func(ctx context.Context) error) {
	t.Helper()
	err := db.NopTx{}.WithTx(t.Context(), func(ctx context.Context) error {
		ctx, err := e.store.Enter(ctx)
		if err != nil {
			return err
		}
		return fn(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// config inserts a config version effective from at (BE-052 creates them).
func (e *env) config(t *testing.T, at time.Time) talent.ConfigVersion {
	t.Helper()
	var v talent.ConfigVersion
	e.inTalentTx(t, func(ctx context.Context) error {
		var err error
		v, err = e.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: at,
			Params: json.RawMessage(`{"shrink_k":5}`), CreatedBy: uuid.NewV7()})
		return err
	})
	return v
}

// student adds an adult student.
func (e *env) student() uuid.UUID {
	id := uuid.NewV7()
	e.students.mu.Lock()
	defer e.students.mu.Unlock()
	e.students.cohorts[id] = students.Cohort{UniversityID: uuid.NewV7()}
	return id
}

// minor adds a student under 18.
func (e *env) minor() uuid.UUID {
	id := e.student()
	e.students.mu.Lock()
	defer e.students.mu.Unlock()
	c := e.students.cohorts[id]
	c.IsMinor = true
	e.students.cohorts[id] = c
	return id
}

// envelope builds the envelope of p at at with a fresh ID.
func envelope(t *testing.T, p domainevent.Payload, at time.Time) domainevent.Envelope {
	t.Helper()
	ev, err := domainevent.New(p, "test", uuid.NewV7())
	if err != nil {
		t.Fatal(err)
	}
	ev.ID, ev.OccurredAt = uuid.NewV7(), at
	return ev
}

// deliver runs every talent subscription of the envelope's topic.
func (e *env) deliver(t *testing.T, ev domainevent.Envelope) {
	t.Helper()
	if err := e.deliverErr(t, ev); err != nil {
		t.Fatalf("%s: %v", ev.Topic, err)
	}
}

func (e *env) deliverErr(t *testing.T, ev domainevent.Envelope) error {
	t.Helper()
	subs := e.subs.For(ev.Topic)
	if len(subs) != 1 {
		t.Fatalf("%s: %d talent subscriptions, want 1", ev.Topic, len(subs))
	}
	return subs[0].Handle(t.Context(), ev)
}

// send builds and delivers p at at.
func (e *env) send(t *testing.T, p domainevent.Payload, at time.Time) domainevent.Envelope {
	t.Helper()
	ev := envelope(t, p, at)
	e.deliver(t, ev)
	return ev
}

// points returns the ledger rows of user.
func (e *env) points(user uuid.UUID) []talent.PointEvent {
	var out []talent.PointEvent
	for _, p := range e.store.AllPoints() {
		if p.UserID == user {
			out = append(out, p)
		}
	}
	return out
}

// total sums the points of user's ledger.
func (e *env) total(user uuid.UUID, signal string) int {
	n := 0
	for _, p := range e.points(user) {
		if signal == "" || p.Signal == signal {
			n += p.PointsCP
		}
	}
	return n
}

// count returns koro_talent_ingest_total{signal, outcome}.
func (e *env) count(signal, outcome string) uint64 {
	for _, c := range e.mod.Counters() {
		if c.Signal == signal && c.Outcome == outcome {
			return c.Value
		}
	}
	return 0
}

// active returns the user's active neutralisations.
func (e *env) active(user uuid.UUID) []talent.Neutralization {
	var out []talent.Neutralization
	for _, n := range e.store.AllNeutralizations() {
		if n.UserID == user && n.Active() {
			out = append(out, n)
		}
	}
	return out
}

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("no persona %q", name)
	}
	return p.Actor
}
