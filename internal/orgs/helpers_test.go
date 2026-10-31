package orgs_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db/dbtest"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// goodPassword is the re-authentication password every test user has.
const goodPassword = "dogru-parola-12345"

// reauth accepts goodPassword and counts the calls.
type reauth struct {
	mu    sync.Mutex
	calls int
}

func (r *reauth) Verify(_ context.Context, _ uuid.UUID, p identity.ReauthProof) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	switch p.Password {
	case "":
		return "", identity.ErrReauthRequired
	case goodPassword:
		return "password", nil
	}
	return "", identity.ErrReauthFailed
}

// revoker records RevokeSessions calls and whether a transaction was still
// open at the time (it must not be: the revocation follows the commit).
type revoker struct {
	mu      sync.Mutex
	users   []uuid.UUID
	inTx    bool
	failure error
}

func (r *revoker) revoke(ctx context.Context, userID uuid.UUID) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = append(r.users, userID)
	r.inTx = r.inTx || dbtest.TxOf(ctx) != 0
	return 1, r.failure
}

func (r *revoker) revoked() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.users)
}

// env is an orgs module over the in-memory store.
type env struct {
	clk      *clock.Fake
	store    *orgs.MemStore
	tx       *dbtest.SpyTx
	out      *outbox.Mem
	audit    *audittest.Recorder
	reauth   *reauth
	late     *audittest.LateReauth[identity.ReauthProof] // reauth with an overload switch
	revoker  *revoker
	users    *users
	students *students
	clubs    *clubs
	mailer   *mailer
	open     []orgs.OpenCommerceCheck
	avatars  *avatars
	svc      orgs.Service
	mod      orgs.Module
}

func newEnv(t *testing.T, opts ...func(*orgs.Deps)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{
		clk: clk, store: orgs.NewMemStore(clk.Now), tx: &dbtest.SpyTx{}, out: outbox.NewMem(clk), audit: &audittest.Recorder{},
		reauth: &reauth{}, revoker: &revoker{}, users: newUsers(), students: &students{ok: map[uuid.UUID]bool{}},
		clubs: &clubs{facts: map[uuid.UUID]orgs.ClubFacts{}}, mailer: &mailer{},
		avatars: &avatars{urls: map[uuid.UUID]string{}},
	}
	e.late = &audittest.LateReauth[identity.ReauthProof]{Next: e.reauth}
	d := orgs.Deps{
		Store: e.store, Tx: e.tx, Outbox: e.out, Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Idempotency:    idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()}),
		Reauth:         e.late,
		RevokeSessions: e.revoker.revoke,
		OpenCommerce:   func() []orgs.OpenCommerceCheck { return e.open },
		Users:          e.users, Students: e.students, Clubs: e.clubs.info, Sender: e.mailer, Avatars: e.avatars.of,
		Deriver: testDeriver(t), LinkBaseURL: "https://app.koro.test",
	}
	for _, o := range opts {
		o(&d)
	}
	mod, err := orgs.New(d)
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

// avatars is students' AvatarURLs (late bound in cmd/koro).
type avatars struct {
	mu   sync.Mutex
	urls map[uuid.UUID]string
	fail error
}

func (a *avatars) of(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return nil, a.fail
	}
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		if u, ok := a.urls[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func testDeriver(t *testing.T) *crypto.Deriver {
	t.Helper()
	d, err := crypto.NewDeriver([]crypto.Key{{ID: "test1", Secret: []byte("orgs-test-token-key-orgs-test-token-key-0123")}})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// users is identity's user directory: accounts with an address, names and
// an age.
type users struct {
	mu   sync.Mutex
	byID map[uuid.UUID]identity.UserContact
	sum  map[uuid.UUID]identity.UserSummary
}

func newUsers() *users {
	return &users{byID: map[uuid.UUID]identity.UserContact{}, sum: map[uuid.UUID]identity.UserSummary{}}
}

func (u *users) add(id uuid.UUID, email string, minor bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.byID[id] = identity.UserContact{ID: id, Email: email, FirstName: "Ada", LastName: "Yılmaz", Locale: "en"}
	u.sum[id] = identity.UserSummary{ID: id, Status: identity.StatusActive, FirstName: "Ada", LastName: "Yılmaz", IsMinor: minor}
}

func (u *users) Get(_ context.Context, id uuid.UUID) (identity.UserSummary, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.sum[id]
	if !ok {
		return identity.UserSummary{}, identity.ErrNotFound
	}
	return s, nil
}

func (u *users) GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error) {
	out := map[uuid.UUID]identity.UserSummary{}
	for _, id := range ids {
		if s, err := u.Get(ctx, id); err == nil {
			out[id] = s
		}
	}
	return out, nil
}

func (u *users) Contact(_ context.Context, id uuid.UUID) (identity.UserContact, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	c, ok := u.byID[id]
	if !ok {
		return identity.UserContact{}, identity.ErrNotFound
	}
	return c, nil
}

func (u *users) FindByEmail(_ context.Context, email string) (uuid.UUID, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, c := range u.byID {
		if strings.EqualFold(c.Email, strings.TrimSpace(email)) {
			return id, true, nil
		}
	}
	return uuid.Nil(), false, nil
}

// students is identity's student verification.
type students struct {
	mu sync.Mutex
	ok map[uuid.UUID]bool
}

func (s *students) set(id uuid.UUID, verified bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ok[id] = verified
}

func (s *students) Active(_ context.Context, id uuid.UUID) (identity.EduVerification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ok[id] {
		return identity.EduVerification{UniversityID: uniA, ExpiresAt: t0.Add(365 * 24 * time.Hour)}, true, nil
	}
	return identity.EduVerification{}, false, nil
}

func (s *students) ActiveMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.EduVerification, error) {
	out := map[uuid.UUID]identity.EduVerification{}
	for _, id := range ids {
		if v, ok, _ := s.Active(ctx, id); ok {
			out[id] = v
		}
	}
	return out, nil
}

// clubs is directory's club facts (hooks.clubInfo).
type clubs struct {
	mu    sync.Mutex
	facts map[uuid.UUID]orgs.ClubFacts
}

func (c *clubs) info(_ context.Context, id uuid.UUID) (orgs.ClubFacts, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.facts[id]
	return f, ok, nil
}

// mailer records the e-mails sent.
type mailer struct {
	mu   sync.Mutex
	sent []notify.Message
}

func (m *mailer) Send(_ context.Context, msg notify.Message) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.sent {
		if x.DedupeKey == msg.DedupeKey {
			return uuid.NewV7(), nil
		}
	}
	m.sent = append(m.sent, msg)
	return uuid.NewV7(), nil
}

func (m *mailer) messages() []notify.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sent)
}

// uniA is the university of the test clubs and students.
var uniA = uuid.MustParse("0192f5c4-0000-7000-8000-00000000000a")

// club opens a club org with directory facts.
func (e *env) club(t *testing.T, name, policy string, sensitive bool) uuid.UUID {
	t.Helper()
	id, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: name})
	if err != nil {
		t.Fatalf("Create club: %v", err)
	}
	e.clubs.mu.Lock()
	e.clubs.facts[id] = orgs.ClubFacts{UniversityID: uniA, JoinPolicy: policy, Sensitive: sensitive}
	e.clubs.mu.Unlock()
	return id
}

// student is a verified student account (adult unless minor) with an
// address in identity.
func (e *env) student(minor bool) authz.Actor {
	a := user()
	u := uniA
	a.StudentStatus, a.UniversityID, a.IsMinor = authz.StudentVerified, &u, minor
	e.users.add(a.UserID, a.UserID.String()+"@ogr.example.edu.tr", minor)
	e.students.set(a.UserID, true)
	return a
}

// clubAdmin is a verified adult student holding club_admin of club.
func (e *env) clubAdmin(t *testing.T, club uuid.UUID) authz.Actor {
	t.Helper()
	a := e.student(false)
	now := e.clk.Now()
	if _, err := e.store.InsertMember(t.Context(), orgs.Membership{
		ID: uuid.NewV7(), OrgID: club, UserID: a.UserID, Role: authz.MemberClubAdmin, Status: orgs.MemberActive,
		Source: orgs.SourceClaim, JoinedAt: &now,
	}); err != nil {
		t.Fatalf("InsertMember: %v", err)
	}
	return as(a, club, orgs.KindClub, authz.MemberClubAdmin)
}

// join makes a a member of club through joinClub (open policy) and
// returns the membership with the actor holding it.
func (e *env) join(t *testing.T, club uuid.UUID, a authz.Actor) (orgs.Membership, authz.Actor) {
	t.Helper()
	m, err := e.svc.JoinClub(t.Context(), a, club, "")
	if err != nil {
		t.Fatalf("JoinClub: %v", err)
	}
	return m, as(a, club, orgs.KindClub, authz.MemberMember)
}

// user is an authenticated actor without roles.
func user() authz.Actor {
	return authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), SessionID: uuid.NewV7(), StudentStatus: authz.StudentNone}
}

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

// as returns a with an active membership (MFA-verified session: company
// roles are MFA roles).
func as(a authz.Actor, org uuid.UUID, kind orgs.Kind, role authz.MemberRole) authz.Actor {
	a.Memberships = append(slices.Clone(a.Memberships), authz.Membership{OrgID: org, OrgKind: kind, Role: role})
	a.MFAVerified = true
	return a
}

func pad(t *testing.T) authz.Actor { return persona(t, "platform_admin") }

// companyInput is a complete application: a synthetic VKN and its tax
// office are required (Q-45).
func companyInput(name string) orgs.CompanyInput {
	return orgs.CompanyInput{Name: name, SectorCode: "software", SizeBand: "11-50", HQCity: "İstanbul", TaxNo: testVKN,
		TaxOffice: "Kadıköy"}
}

// company registers a company founded by a fresh user and returns it with
// the founder as an owner actor.
func (e *env) company(t *testing.T, name string) (orgs.OrgView, authz.Actor) {
	t.Helper()
	founder := user()
	v, err := e.svc.CreateCompany(t.Context(), founder, companyInput(name))
	if err != nil {
		t.Fatalf("CreateCompany: %v", err)
	}
	return v, as(founder, v.Org.ID, orgs.KindCompany, authz.MemberCompanyOwner)
}

// member adds an active membership of a new user to org and returns the
// actor holding it.
func (e *env) member(t *testing.T, org uuid.UUID, role authz.MemberRole) authz.Actor {
	t.Helper()
	a := user()
	now := e.clk.Now()
	m, err := e.store.InsertMember(t.Context(), orgs.Membership{
		ID: uuid.NewV7(), OrgID: org, UserID: a.UserID, Role: role, Status: orgs.MemberActive, Source: orgs.SourceInvitation,
		JoinedAt: &now,
	})
	if err != nil {
		t.Fatalf("InsertMember: %v", err)
	}
	return as(a, org, m.OrgKind, role)
}

// topics returns the topics of the events emitted so far.
func (e *env) topics() []string { return e.out.Topics() }

func proof(pw string) identity.ReauthProof { return identity.ReauthProof{Password: pw} }

var _ = domainevent.TopicMembershipStarted
