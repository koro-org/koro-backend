package messaging_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/entitlements"
	"koro/internal/feed"
	"koro/internal/identity"
	"koro/internal/messaging"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/students"
)

var t0 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// ---- transactions

// txKey carries the number of the transaction a call runs in.
type txKey struct{}

func fakeTxOf(ctx context.Context) int {
	n, _ := ctx.Value(txKey{}).(int)
	return n
}

// rollbacker is a fake whose records of a transaction go when it rolls
// back.
type rollbacker interface{ rollback(tx int) }

// txFake numbers the outermost transactions (a nested WithTx joins), and on
// an error restores the in-memory store and drops the fakes' records of
// that transaction: the unit tests see a rollback as PostgreSQL would.
type txFake struct {
	mu    sync.Mutex
	n     int
	store *messaging.MemStore
	fakes []rollbacker
}

func (t *txFake) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if fakeTxOf(ctx) > 0 {
		return fn(ctx)
	}
	t.mu.Lock()
	t.n++
	id := t.n
	t.mu.Unlock()
	snap := t.store.Snapshot()
	err := db.NopTx{}.WithTx(context.WithValue(ctx, txKey{}, id), fn)
	if err != nil {
		t.store.Restore(snap)
		for _, f := range t.fakes {
			f.rollback(id)
		}
	}
	return err
}

func (t *txFake) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	return t.WithTx(ctx, fn)
}

// ---- fakes

type consumption struct {
	tx    int
	org   uuid.UUID
	ref   entitlements.Ref
	usage uuid.UUID
}

// entitlementsFake is the contact_request quota: units per org; a ref
// consumes once (entitlement_usages_ref_uq).
type entitlementsFake struct {
	mu    sync.Mutex
	units map[uuid.UUID]int
	used  []consumption
}

func (e *entitlementsFake) Consume(ctx context.Context, org uuid.UUID, f entitlements.Feature, qty int, ref entitlements.Ref) (uuid.UUID, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f != entitlements.FeatureContactRequest || qty != 1 || ref.Type != entitlements.RefContactRequest {
		return uuid.Nil(), errors.New("unexpected consumption")
	}
	if fakeTxOf(ctx) == 0 {
		return uuid.Nil(), errors.New("Consume outside a transaction")
	}
	for _, c := range e.used {
		if c.ref == ref {
			return c.usage, nil
		}
	}
	if e.units[org] < 1 {
		return uuid.Nil(), entitlements.ErrNoEntitlement
	}
	e.units[org]--
	c := consumption{tx: fakeTxOf(ctx), org: org, ref: ref, usage: uuid.NewV7()}
	e.used = append(e.used, c)
	return c.usage, nil
}

func (e *entitlementsFake) rollback(tx int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.used = slices.DeleteFunc(e.used, func(c consumption) bool {
		if c.tx == tx {
			e.units[c.org]++
			return true
		}
		return false
	})
}

func (e *entitlementsFake) consumed() []consumption {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.used)
}

func (e *entitlementsFake) left(org uuid.UUID) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.units[org]
}

type grant struct {
	tx               int
	student, company uuid.UUID
	src              students.GrantSource
	srcID            uuid.UUID
}

type share struct {
	tx      int
	student uuid.UUID
	company uuid.UUID
	proof   students.ConsentProof
	channel consent.Channel
	request uuid.UUID
}

// studentsFake is students' level 2 contract, AccessGrants and
// ContactSharing.
type studentsFake struct {
	mu           sync.Mutex
	discoverable map[uuid.UUID]bool
	grants       []grant
	failGrant    error
	shares       []share
	failShare    error
}

func (s *studentsFake) ShareContactWithCompany(ctx context.Context, a authz.Actor, company uuid.UUID, proof students.ConsentProof,
	ch consent.Channel, request uuid.UUID,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fakeTxOf(ctx) == 0 {
		return errors.New("ShareContactWithCompany outside a transaction")
	}
	s.shares = append(s.shares, share{tx: fakeTxOf(ctx), student: a.UserID, company: company, proof: proof, channel: ch,
		request: request})
	return s.failShare
}

func (s *studentsFake) shared() []share {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.shares)
}

func (s *studentsFake) IsDiscoverable(_ context.Context, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discoverable[id], nil
}

func (s *studentsFake) Grant(ctx context.Context, student, company uuid.UUID, src students.GrantSource, srcID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fakeTxOf(ctx) == 0 {
		return errors.New("Grant outside a transaction")
	}
	s.grants = append(s.grants, grant{tx: fakeTxOf(ctx), student: student, company: company, src: src, srcID: srcID})
	return s.failGrant
}

func (s *studentsFake) rollback(tx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants = slices.DeleteFunc(s.grants, func(g grant) bool { return g.tx == tx })
	s.shares = slices.DeleteFunc(s.shares, func(c share) bool { return c.tx == tx })
}

func (s *studentsFake) granted() []grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.grants)
}

func (s *studentsFake) set(id uuid.UUID, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoverable[id] = ok
}

// orgsFake is orgs' directory: summaries, active members and their roles.
type orgsFake struct {
	mu      sync.Mutex
	orgs    map[uuid.UUID]orgs.OrgSummary
	members map[uuid.UUID]map[uuid.UUID]authz.MemberRole
}

func (o *orgsFake) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.orgs[id]
	if !ok {
		return orgs.OrgSummary{}, orgs.ErrNotFound
	}
	return s, nil
}

func (o *orgsFake) GetMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[uuid.UUID]orgs.OrgSummary{}
	for _, id := range ids {
		if s, ok := o.orgs[id]; ok {
			out[id] = s
		}
	}
	return out, nil
}

func (o *orgsFake) ActiveMemberIDs(_ context.Context, org uuid.UUID) ([]uuid.UUID, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []uuid.UUID
	for id := range o.members[org] {
		out = append(out, id)
	}
	return out, nil
}

func (o *orgsFake) HasRole(_ context.Context, org, user uuid.UUID, roles ...authz.MemberRole) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, ok := o.members[org][user]
	return ok && slices.Contains(roles, r), nil
}

func (o *orgsFake) status(id uuid.UUID, st orgs.Status) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := o.orgs[id]
	s.Status = st
	o.orgs[id] = s
}

func (o *orgsFake) leave(org, user uuid.UUID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.members[org], user)
}

// pseudonymsFake derives a stable 16-character pseudonym per (company,
// student), different across companies.
type pseudonymsFake struct{}

func (pseudonymsFake) AnonID(company, user uuid.UUID) (string, error) {
	sum := sha256.Sum256(append(company[:], user[:]...))
	return hex.EncodeToString(sum[:])[:16], nil
}

func anonOf(company, user uuid.UUID) string {
	a, _ := pseudonymsFake{}.AnonID(company, user)
	return a
}

// resolver is hooks.anonResolver over the pseudonyms of the known students.
type resolver struct {
	students *studentsFake
	known    []uuid.UUID
}

func (r *resolver) resolve(_ context.Context, company uuid.UUID, anon string) (uuid.UUID, bool, error) {
	for _, id := range r.known {
		if anonOf(company, id) == anon {
			return id, true, nil
		}
	}
	return uuid.Nil(), false, nil
}

type postsFake struct {
	mu    sync.Mutex
	posts map[uuid.UUID]feed.PostSummary
}

func (p *postsFake) Get(_ context.Context, id uuid.UUID) (feed.PostSummary, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.posts[id]
	if !ok {
		return feed.PostSummary{}, feed.ErrNotFound
	}
	return s, nil
}

type event struct {
	tx  int
	env domainevent.Envelope
}

// outboxFake records the events of mechanism O; Add outside a
// transaction fails like the real outbox.
type outboxFake struct {
	mu     sync.Mutex
	events []event
}

func (o *outboxFake) Add(ctx context.Context, e domainevent.Envelope) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if fakeTxOf(ctx) == 0 {
		return errors.New("outbox: Add outside a transaction")
	}
	o.events = append(o.events, event{tx: fakeTxOf(ctx), env: e})
	return nil
}

func (o *outboxFake) AfterCommit(context.Context, domainevent.Envelope) error {
	return errors.New("messaging uses mechanism O only")
}

func (o *outboxFake) rollback(tx int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = slices.DeleteFunc(o.events, func(e event) bool { return e.tx == tx })
}

func (o *outboxFake) of(topic string) []event {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []event
	for _, e := range o.events {
		if e.env.Topic == topic {
			out = append(out, e)
		}
	}
	return out
}

type logged struct {
	tx int
	ev audit.Event
}

// auditFake records the audit events; every one must be in a transaction.
type auditFake struct {
	mu     sync.Mutex
	events []logged
}

func (a *auditFake) Log(ctx context.Context, e audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if fakeTxOf(ctx) == 0 {
		return errors.New("audit outside a transaction")
	}
	a.events = append(a.events, logged{tx: fakeTxOf(ctx), ev: e})
	return nil
}

func (a *auditFake) rollback(tx int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = slices.DeleteFunc(a.events, func(l logged) bool { return l.tx == tx })
}

func (a *auditFake) of(action authz.Action) []logged {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []logged
	for _, l := range a.events {
		if l.ev.Action == string(action) {
			out = append(out, l)
		}
	}
	return out
}

// senderFake records the notices.
type senderFake struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (s *senderFake) Send(_ context.Context, m notify.Message) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.sent {
		if o.DedupeKey == m.DedupeKey {
			return uuid.NewV7(), nil
		}
	}
	s.sent = append(s.sent, m)
	return uuid.NewV7(), nil
}

func (s *senderFake) all() []notify.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

// on returns the notices of one channel.
func (s *senderFake) on(ch notify.Channel) []notify.Message {
	return slices.DeleteFunc(s.all(), func(m notify.Message) bool { return m.Channel != ch })
}

// usersFake is identity's address lookup: every persona has an address
// and a language; erased users are not found.
type usersFake struct {
	mu     sync.Mutex
	erased map[uuid.UUID]bool
}

func (u *usersFake) Contact(_ context.Context, id uuid.UUID) (identity.UserContact, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.erased[id] {
		return identity.UserContact{}, identity.ErrNotFound
	}
	return identity.UserContact{ID: id, Email: id.String() + "@example.test", FirstName: "Ada", Locale: "en"}, nil
}

func (u *usersFake) erase(id uuid.UUID) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.erased[id] = true
}

// linkBase is the e-mail link origin of the tests.
const linkBase = "https://koro.example.test"

// ---- environment

type env struct {
	clk      *clock.Fake
	store    *messaging.MemStore
	tx       *txFake
	ent      *entitlementsFake
	students *studentsFake
	orgs     *orgsFake
	posts    *postsFake
	outbox   *outboxFake
	audit    *auditFake
	sender   *senderFake
	users    *usersFake
	resolver *resolver
	mod      messaging.Module
	svc      messaging.Service

	companyA, companyB, clubA uuid.UUID
}

// newEnv builds messaging over fakes: company_a and company_b are active
// companies with 5 contact_request units each (company_member is
// company_a's recruiter and company_admin its admin; other_company_member
// company_b's recruiter); the persona student and student_granted are
// discoverable adults, student_minor is not discoverable.
func newEnv(t *testing.T, opts ...func(*messaging.Deps)) *env {
	t.Helper()
	e := &env{clk: clock.NewFake(t0)}
	e.companyA, e.companyB, e.clubA = authztest.ID("company_a"), authztest.ID("company_b"), authztest.ID("club_a")
	e.store = messaging.NewMemStore(e.clk.Now)
	e.ent = &entitlementsFake{units: map[uuid.UUID]int{e.companyA: 5, e.companyB: 5}}
	e.students = &studentsFake{discoverable: map[uuid.UUID]bool{
		authztest.ID("student"): true, authztest.ID("student_granted"): true,
	}}
	e.orgs = &orgsFake{
		orgs: map[uuid.UUID]orgs.OrgSummary{
			e.companyA: {ID: e.companyA, Kind: orgs.KindCompany, Name: "Şirket A", Status: orgs.StatusActive, Slug: "sirket-a"},
			e.companyB: {ID: e.companyB, Kind: orgs.KindCompany, Name: "Şirket B", Status: orgs.StatusActive, Slug: "sirket-b"},
			e.clubA:    {ID: e.clubA, Kind: orgs.KindClub, Name: "Kulüp A", Status: orgs.StatusActive, Slug: "kulup-a"},
		},
		members: map[uuid.UUID]map[uuid.UUID]authz.MemberRole{
			e.companyA: {
				authztest.ID("company_member"): authz.MemberRecruiter, authztest.ID("company_admin"): authz.MemberCompanyAdmin,
				authztest.ID("company_owner_a"): authz.MemberCompanyOwner,
			},
			e.companyB: {authztest.ID("other_company_member"): authz.MemberRecruiter},
		},
	}
	e.posts = &postsFake{posts: map[uuid.UUID]feed.PostSummary{}}
	e.outbox, e.audit, e.sender = &outboxFake{}, &auditFake{}, &senderFake{}
	e.users = &usersFake{erased: map[uuid.UUID]bool{}}
	e.resolver = &resolver{students: e.students, known: []uuid.UUID{
		authztest.ID("student"), authztest.ID("student_granted"), authztest.ID("student_minor"), authztest.ID("student_b"),
	}}
	e.tx = &txFake{store: e.store, fakes: []rollbacker{e.ent, e.students, e.outbox, e.audit}}
	keeper := idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: e.clk, Log: obs.Discard()})
	d := messaging.Deps{
		Store: e.store, Tx: e.tx, Outbox: e.outbox, Audit: e.audit, Idempotency: keeper, Clock: e.clk, Log: obs.Discard(),
		Entitlements: e.ent, Grants: e.students, Contacts: e.students, Students: e.students, Orgs: e.orgs,
		Pseudonyms: pseudonymsFake{}, Resolve: e.resolver.resolve, Posts: e.posts, Sender: e.sender,
		Users: e.users, LinkBaseURL: linkBase,
	}
	for _, o := range opts {
		o(&d)
	}
	mod, err := messaging.New(d)
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

// request is a valid createContactRequest to student under company's
// pseudonym.
func request(company uuid.UUID, student string) messaging.CreateInput {
	return messaging.CreateInput{AnonID: anonOf(company, authztest.ID(student)), Subject: "Yaz stajı",
		Message: "Profilinizi gördük, yaz stajı için görüşmek isteriz."}
}

// send creates a request of company_a's recruiter to student.
func (e *env) send(t *testing.T, student string) messaging.CompanyView {
	t.Helper()
	v, err := e.svc.Create(t.Context(), actor(t, "company_member"), e.companyA, request(e.companyA, student))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return v
}

// accept and decline are the plain answers (no contact sharing).
var (
	accept  = messaging.RespondInput{Decision: messaging.DecisionAccepted}
	decline = messaging.RespondInput{Decision: messaging.DecisionDeclined}
)

// withKey returns ctx carrying an Idempotency-Key of a's create call.
func withKey(ctx context.Context, a authz.Actor, key string) context.Context {
	return idempotency.WithKey(ctx, idempotency.Key{Scope: a.UserID.String() + ":POST /orgs/{org_id}/contact-requests",
		Value: key, Status: 201})
}
