package orgs_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// goodPassword is the re-authentication password every test user has.
const goodPassword = "dogru-parola-12345"

// auditLog records the events the service writes.
type auditLog struct {
	mu     sync.Mutex
	events []audit.Event
}

func (l *auditLog) Log(_ context.Context, e audit.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}

func (l *auditLog) of(action authz.Action) []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []audit.Event
	for _, e := range l.events {
		if e.Action == string(action) {
			out = append(out, e)
		}
	}
	return out
}

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
	r.inTx = r.inTx || ctx.Value(txKey{}) != nil
	return 1, r.failure
}

func (r *revoker) revoked() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.users)
}

// txKey marks the context of an open spyTx transaction.
type txKey struct{}

// spyTx is db.NopTx that marks its transaction in the context (the marker
// is gone in AfterCommit hooks: they get the caller's context values,
// which never carried it).
type spyTx struct{ n int }

func (s *spyTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if ctx.Value(txKey{}) != nil {
		return fn(ctx)
	}
	s.n++
	return db.NopTx{}.WithTx(ctx, func(inner context.Context) error {
		return fn(context.WithValue(inner, txKey{}, s.n))
	})
}

func (s *spyTx) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	return s.WithTx(ctx, fn)
}

// env is an orgs module over the in-memory store.
type env struct {
	clk     *clock.Fake
	store   *orgs.MemStore
	tx      *spyTx
	out     *outbox.Mem
	audit   *auditLog
	reauth  *reauth
	revoker *revoker
	open    []orgs.OpenCommerceCheck
	svc     orgs.Service
	mod     orgs.Module
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{
		clk: clk, store: orgs.NewMemStore(clk.Now), tx: &spyTx{}, out: outbox.NewMem(clk), audit: &auditLog{},
		reauth: &reauth{}, revoker: &revoker{},
	}
	mod, err := orgs.New(orgs.Deps{
		Store: e.store, Tx: e.tx, Outbox: e.out, Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Idempotency:    idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()}),
		Reauth:         e.reauth,
		RevokeSessions: e.revoker.revoke,
		OpenCommerce:   func() []orgs.OpenCommerceCheck { return e.open },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
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

func companyInput(name string) orgs.CompanyInput {
	return orgs.CompanyInput{Name: name, SectorCode: "software", SizeBand: "11-50", HQCity: "İstanbul"}
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

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func wantCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	p, ok := errors.AsType[*problem.Error](err)
	if !ok || p.Status() != status || p.Code() != code {
		t.Fatalf("error = %v, want %d %s", err, status, code)
	}
}

var _ = domainevent.TopicMembershipStarted
