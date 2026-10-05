package directory_test

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
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// goodPassword is the re-authentication password every test user has.
const goodPassword = "dogru-parola-12345"

// auditLog records the events the service writes; fail makes Log fail.
type auditLog struct {
	mu     sync.Mutex
	events []audit.Event
	fail   error
}

func (l *auditLog) Log(_ context.Context, e audit.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return l.fail
	}
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

func (l *auditLog) setFail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail = err
}

// reauth accepts goodPassword.
type reauth struct{}

func (reauth) Verify(_ context.Context, _ uuid.UUID, p identity.ReauthProof) (string, error) {
	switch p.Password {
	case "":
		return "", identity.ErrReauthRequired
	case goodPassword:
		return "password", nil
	}
	return "", identity.ErrReauthFailed
}

// txKey marks the context of an open spyTx transaction.
type txKey struct{}

// spyTx is db.NopTx that marks its transaction in the context (value: the
// transaction's number), so a test can tell which calls share it.
type spyTx struct {
	mu sync.Mutex
	n  int
}

func (s *spyTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if ctx.Value(txKey{}) != nil {
		return fn(ctx)
	}
	s.mu.Lock()
	s.n++
	n := s.n
	s.mu.Unlock()
	return db.NopTx{}.WithTx(ctx, func(inner context.Context) error {
		return fn(context.WithValue(inner, txKey{}, n))
	})
}

func (s *spyTx) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	return s.WithTx(ctx, fn)
}

func txOf(ctx context.Context) int {
	n, _ := ctx.Value(txKey{}).(int)
	return n
}

// spyOrgs wraps the orgs service and records the transaction of each
// Create.
type spyOrgs struct {
	orgs.Service
	mu        sync.Mutex
	creates   []int
	grants    []int // transaction of each GrantRole (BE-022)
	failGrant error
}

func (s *spyOrgs) Create(ctx context.Context, in orgs.CreateOrg) (uuid.UUID, error) {
	s.mu.Lock()
	s.creates = append(s.creates, txOf(ctx))
	s.mu.Unlock()
	return s.Service.Create(ctx, in)
}

// spyStore records the transaction of each InsertClub and can fail it.
type spyStore struct {
	directory.Store
	mu           sync.Mutex
	clubInserts  []int
	failClub     error
	claimUpdates []int // transaction of each UpdateClaim (BE-022)
}

func (s *spyStore) InsertClub(ctx context.Context, c directory.Club) (directory.Club, error) {
	s.mu.Lock()
	s.clubInserts = append(s.clubInserts, txOf(ctx))
	fail := s.failClub
	s.mu.Unlock()
	if fail != nil {
		return directory.Club{}, fail
	}
	return s.Store.InsertClub(ctx, c)
}

// counter is a MemberCounter with fixed counts.
type counter struct {
	mu     sync.Mutex
	counts map[uuid.UUID]int
}

func (c *counter) set(id uuid.UUID, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[id] = n
}

func (c *counter) count(_ context.Context, id uuid.UUID) (int, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.counts[id]
	return n, ok, nil
}

// sender records the notify messages of the worker task.
type sender struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (s *sender) Send(_ context.Context, m notify.Message) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return uuid.NewV7(), nil
}

// env is a directory module over in-memory stores and a real orgs module.
type env struct {
	clk     *clock.Fake
	tx      *spyTx
	orgsMem *orgs.MemStore
	orgs    *spyOrgs
	store   *spyStore
	mem     *directory.MemStore
	out     *outbox.Mem
	queue   *queue.Mem
	audit   *auditLog
	members *counter
	sender  *sender
	admins  []uuid.UUID
	ver     *verifications
	users   *userDir
	files   *fakeFiles
	removed *removedFiles
	mod     directory.Module
	svc     directory.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{
		clk: clk, tx: &spyTx{}, orgsMem: orgs.NewMemStore(clk.Now), out: outbox.NewMem(clk), queue: queue.NewMem(),
		audit: &auditLog{}, members: &counter{counts: map[uuid.UUID]int{}}, sender: &sender{},
		admins: []uuid.UUID{uuid.NewV7(), uuid.NewV7()},
		ver:    &verifications{m: map[uuid.UUID]identity.EduVerification{}}, users: &userDir{m: map[uuid.UUID]identity.UserSummary{}},
		removed: &removedFiles{},
	}
	e.files = &fakeFiles{m: map[uuid.UUID]files.FileMeta{}, removed: e.removed}
	idem := idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	om, err := orgs.New(orgs.Deps{
		Store: e.orgsMem, Tx: e.tx, Outbox: e.out, Audit: e.audit, Idempotency: idem, Clock: clk, Log: obs.Discard(),
		Students: e.ver, Users: e.users,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.orgs = &spyOrgs{Service: om.Service}
	e.mem = directory.NewMemStore(clk.Now, func(ctx context.Context, id uuid.UUID) (directory.OrgRef, bool) {
		o, err := e.orgsMem.GetOrg(ctx, id)
		if err != nil {
			return directory.OrgRef{}, false
		}
		return directory.OrgRef{Name: o.Name, Slug: o.Slug, Status: string(o.Status)}, true
	})
	e.store = &spyStore{Store: e.mem}
	mod, err := directory.New(directory.Deps{
		Store: e.store, Tx: e.tx, Outbox: e.out, Queue: e.queue, Audit: e.audit, Orgs: e.orgs, Idempotency: idem,
		Clock: clk, Log: obs.Discard(), Reauth: reauth{}, MemberCount: e.members.count, Sender: e.sender,
		PlatformAdmins: func(context.Context) ([]uuid.UUID, error) { return e.admins, nil },
		Students:       e.ver, Users: e.users, Files: e.files,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

// university imports one university (with its org) and returns it.
func (e *env) university(t *testing.T, code, name, city string) directory.University {
	t.Helper()
	if _, err := e.svc.ImportReference(t.Context(), directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: code, Name: name, City: city, Type: directory.UniversityState}},
	}); err != nil {
		t.Fatalf("import university: %v", err)
	}
	u, err := e.mem.UniversityByYokCode(t.Context(), code)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// club creates a club as the platform admin.
func (e *env) club(t *testing.T, uni directory.University, name string, cat directory.Category) directory.ClubView {
	t.Helper()
	in := directory.ClubInput{UniversityID: uni.ID, Name: name, Category: cat, JoinPolicy: directory.JoinApproval}
	if cat.Sensitive() {
		in.IsSensitive, in.SensitiveReason = true, "religion"
	}
	v, err := e.svc.AdminCreateClub(t.Context(), pad(t), in)
	if err != nil {
		t.Fatalf("AdminCreateClub(%s): %v", name, err)
	}
	return v
}

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func pad(t *testing.T) authz.Actor { return persona(t, "platform_admin") }

// clubAdmin is a verified, adult student holding club_admin of org.
func clubAdmin(org uuid.UUID) authz.Actor {
	return authz.Actor{
		Kind: authz.KindUser, UserID: uuid.NewV7(), SessionID: uuid.NewV7(), StudentStatus: authz.StudentVerified,
		Memberships: []authz.Membership{{OrgID: org, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}},
	}
}

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

func names(page directory.ClubPage) []string {
	out := make([]string, len(page.Items))
	for i, c := range page.Items {
		out[i] = c.Row.Name
	}
	slices.Sort(out)
	return out
}
