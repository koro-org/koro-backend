package students_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// t0 is the test clock's start (2026-10-04 09:00 UTC, 12:00 in Istanbul).
var t0 = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

// txKey marks the context of an open spyTx transaction; its value is the
// transaction's number.
type txKey struct{}

// spyTx is db.NopTx that numbers its transactions in the context (the
// marker is gone in AfterCommit hooks: they get the caller's context).
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

// spyStore records the transaction of every privacy update.
type spyStore struct {
	*students.MemStore
	mu        sync.Mutex
	privacyTx []int
}

func (s *spyStore) UpdatePrivacy(ctx context.Context, p students.Privacy) (students.Privacy, error) {
	s.mu.Lock()
	s.privacyTx = append(s.privacyTx, txOf(ctx))
	s.mu.Unlock()
	return s.MemStore.UpdatePrivacy(ctx, p)
}

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

func (l *auditLog) of(action string) []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []audit.Event
	for _, e := range l.events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// consents is consent.Recorder + Checker: the ledger of mirrored
// purposes, the transaction each record was written in, an optional
// failure.
type consents struct {
	mu      sync.Mutex
	rows    []consent.RecordInput
	tx      []int
	fail    error
	removed map[uuid.UUID]bool // FilterActive ignores these (consent withdrawn elsewhere)
}

func (c *consents) Record(ctx context.Context, in consent.RecordInput) (uuid.UUID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return uuid.Nil(), c.fail
	}
	if in.Action == consent.Granted && (in.NoticeVersionID == nil || len(in.TextSHA256) != 32) {
		return uuid.Nil(), consent.ErrProofRequired
	}
	c.rows = append(c.rows, in)
	c.tx = append(c.tx, txOf(ctx))
	return uuid.NewV7(), nil
}

func (c *consents) FilterActive(_ context.Context, ids []uuid.UUID, purpose consent.Purpose) ([]uuid.UUID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := map[uuid.UUID]bool{}
	for _, r := range c.rows {
		if r.Purpose == purpose {
			state[r.UserID] = r.Action == consent.Granted
		}
	}
	var out []uuid.UUID
	for _, id := range ids {
		if state[id] && !c.removed[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (c *consents) records() []consent.RecordInput {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.rows)
}

// catalog is directory: two universities, their departments, skills and
// sensitive clubs.
type catalog struct {
	mu        sync.Mutex
	unis      map[uuid.UUID]directory.University
	depts     map[uuid.UUID]directory.Department
	skills    map[uuid.UUID]bool
	sensitive map[uuid.UUID]bool
}

func (c *catalog) GetUniversity(_ context.Context, id uuid.UUID) (directory.University, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.unis[id]
	if !ok {
		return directory.University{}, directory.ErrNotFound
	}
	return u, nil
}

func (c *catalog) GetDepartment(_ context.Context, id uuid.UUID) (directory.Department, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.depts[id]
	if !ok {
		return directory.Department{}, directory.ErrNotFound
	}
	return d, nil
}

func (c *catalog) SkillsExist(_ context.Context, ids []uuid.UUID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		if !c.skills[id] {
			return directory.ErrUnknownSkill
		}
	}
	return nil
}

func (c *catalog) SensitiveOrgIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if c.sensitive[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (c *catalog) setSensitive(id uuid.UUID, v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sensitive[id] = v
}

// orgsFake is orgs: the active memberships of users.
type orgsFake struct {
	mu sync.Mutex
	ms map[uuid.UUID][]authz.Membership
}

func (o *orgsFake) MembershipsOf(_ context.Context, userID uuid.UUID) ([]authz.Membership, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.ms[userID]), nil
}

func (o *orgsFake) ActiveMemberIDs(_ context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []uuid.UUID
	for u, ms := range o.ms {
		if slices.ContainsFunc(ms, func(m authz.Membership) bool { return m.OrgID == orgID }) {
			out = append(out, u)
		}
	}
	return out, nil
}

func (o *orgsFake) join(user, club uuid.UUID, role authz.MemberRole) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ms[user] = append(o.ms[user], authz.Membership{OrgID: club, OrgKind: authz.OrgClub, Role: role})
}

func (o *orgsFake) leave(user, club uuid.UUID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ms[user] = slices.DeleteFunc(o.ms[user], func(m authz.Membership) bool { return m.OrgID == club })
}

// usersFake is identity: accounts and their student verifications.
type usersFake struct {
	mu  sync.Mutex
	sum map[uuid.UUID]identity.UserSummary
	ver map[uuid.UUID]identity.EduVerification
}

func (u *usersFake) GetMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[uuid.UUID]identity.UserSummary{}
	for _, id := range ids {
		if s, ok := u.sum[id]; ok {
			out[id] = s
		}
	}
	return out, nil
}

func (u *usersFake) ActiveMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.EduVerification, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[uuid.UUID]identity.EduVerification{}
	for _, id := range ids {
		if v, ok := u.ver[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (u *usersFake) setMinor(id uuid.UUID, minor bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.sum[id]
	s.IsMinor = minor
	u.sum[id] = s
}

func (u *usersFake) lapse(id uuid.UUID) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.ver, id)
}

func (u *usersFake) verify(id, uni uuid.UUID, at time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.ver[id] = identity.EduVerification{UniversityID: uni, VerifiedAt: at, ExpiresAt: at.AddDate(1, 0, 0), Method: "edu_email"}
}

// filesFake is files: avatar uploads, attachments and deletions.
type filesFake struct {
	mu       sync.Mutex
	meta     map[uuid.UUID]files.FileMeta
	attached map[uuid.UUID]files.Owner
	deleted  []uuid.UUID
	deleteTx []int
}

func (f *filesFake) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.meta[id]
	if !ok {
		return files.FileMeta{}, files.ErrNotFound
	}
	return m, nil
}

func (f *filesFake) Attach(_ context.Context, _ authz.Actor, id uuid.UUID, owner files.Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached[id] = owner
	return nil
}

func (f *filesFake) PresignGet(_ context.Context, id uuid.UUID, _ time.Duration) (files.Download, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.meta[id]; !ok {
		return files.Download{}, files.ErrNotFound
	}
	return files.Download{URL: "https://files.test/" + id.String()}, nil
}

func (f *filesFake) remove(ctx context.Context, _ authz.Actor, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	f.deleteTx = append(f.deleteTx, txOf(ctx))
	delete(f.meta, id)
	return nil
}

func (f *filesFake) upload(owner uuid.UUID, purpose files.Purpose) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.meta[id] = files.FileMeta{ID: id, Purpose: purpose, UploadedBy: &owner, OwnerUserID: &owner}
	return id
}

// env is a students module over the in-memory store and the fakes.
type env struct {
	clk     *clock.Fake
	store   *spyStore
	tx      *spyTx
	out     *outbox.Mem
	audit   *auditLog
	consent *consents
	catalog *catalog
	orgs    *orgsFake
	users   *usersFake
	files   *filesFake
	mod     students.Module
	svc     students.Service
	subs    *outbox.Registry

	uniA, uniB           uuid.UUID
	deptA, deptB, deptA2 uuid.UUID // deptA2 is inactive
	skill1, skill2       uuid.UUID
	clubPlain, clubSens  uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{
		clk: clk, store: &spyStore{MemStore: students.NewMemStore(clk.Now)}, tx: &spyTx{}, out: outbox.NewMem(clk),
		audit: &auditLog{}, consent: &consents{removed: map[uuid.UUID]bool{}},
		orgs:  &orgsFake{ms: map[uuid.UUID][]authz.Membership{}},
		users: &usersFake{sum: map[uuid.UUID]identity.UserSummary{}, ver: map[uuid.UUID]identity.EduVerification{}},
		files: &filesFake{meta: map[uuid.UUID]files.FileMeta{}, attached: map[uuid.UUID]files.Owner{}},
		uniA:  uuid.NewV7(), uniB: uuid.NewV7(), deptA: uuid.NewV7(), deptB: uuid.NewV7(), deptA2: uuid.NewV7(),
		skill1: uuid.NewV7(), skill2: uuid.NewV7(), clubPlain: uuid.NewV7(), clubSens: uuid.NewV7(),
	}
	e.catalog = &catalog{
		unis: map[uuid.UUID]directory.University{
			e.uniA: {ID: e.uniA, Name: "Boğaziçi Üniversitesi", IsActive: true},
			e.uniB: {ID: e.uniB, Name: "ODTÜ", IsActive: true},
		},
		depts: map[uuid.UUID]directory.Department{
			e.deptA:  {ID: e.deptA, UniversityID: e.uniA, Name: "Bilgisayar Mühendisliği", DegreeLevel: directory.DegreeBachelor, IsActive: true},
			e.deptA2: {ID: e.deptA2, UniversityID: e.uniA, Name: "Kapanan Bölüm", DegreeLevel: directory.DegreeBachelor},
			e.deptB:  {ID: e.deptB, UniversityID: e.uniB, Name: "Fizik", DegreeLevel: directory.DegreeMaster, IsActive: true},
		},
		skills:    map[uuid.UUID]bool{e.skill1: true, e.skill2: true},
		sensitive: map[uuid.UUID]bool{e.clubSens: true},
	}
	mod, err := students.New(students.Deps{
		Store: e.store, Tx: e.tx, Outbox: e.out, Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()}),
		Consent:     e.consent, Consents: e.consent, Catalog: e.catalog, Orgs: e.orgs, Users: e.users,
		Verifications: e.users, Files: e.files, RemoveFile: e.files.remove,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	e.subs = outbox.NewRegistry()
	mod.Subscribe(e.subs)
	if err := e.subs.Err(); err != nil {
		t.Fatal(err)
	}
	return e
}

// student creates an active account with an active verification at uni
// and returns its actor (as cmd/koro's ActorResolver would build it).
func (e *env) student(t *testing.T, uni uuid.UUID, minor bool) authz.Actor {
	t.Helper()
	id := uuid.NewV7()
	e.users.mu.Lock()
	e.users.sum[id] = identity.UserSummary{ID: id, Status: identity.StatusActive, FirstName: "Ada", LastName: "Yılmaz", IsMinor: minor}
	e.users.mu.Unlock()
	e.users.verify(id, uni, t0.AddDate(0, -1, 0))
	u := uni
	return authz.Actor{Kind: authz.KindUser, UserID: id, StudentStatus: authz.StudentVerified, UniversityID: &u, IsMinor: minor}
}

// unverified is an account without a student verification (usr).
func (e *env) unverified() authz.Actor {
	id := uuid.NewV7()
	e.users.mu.Lock()
	e.users.sum[id] = identity.UserSummary{ID: id, Status: identity.StatusActive}
	e.users.mu.Unlock()
	return authz.Actor{Kind: authz.KindUser, UserID: id, StudentStatus: authz.StudentNone}
}

// deliver runs the subscription name of topic with payload p (as the
// worker would).
func (e *env) deliver(t *testing.T, name string, p domainevent.Payload, aggID uuid.UUID) {
	t.Helper()
	env, err := domainevent.New(p, "test", aggID)
	if err != nil {
		t.Fatal(err)
	}
	env.ID, env.OccurredAt = uuid.NewV7(), e.clk.Now()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range e.subs.For(p.Topic()) {
		if s.Name == name {
			if err := s.HandleTask(t.Context(), body); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return
		}
	}
	t.Fatalf("no subscription %s on %s", name, p.Topic())
}

// job runs the scheduled job name.
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

// profile returns the stored profile of userID.
func (e *env) profile(t *testing.T, userID uuid.UUID) students.Profile {
	t.Helper()
	p, err := e.store.GetProfile(t.Context(), userID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	return p
}

// topics returns the committed outbox topics.
func (e *env) topics() []string { return e.out.Topics() }

// wantCode fails unless err is a problem with code.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Code() != code {
		t.Fatalf("err = %v, want problem %s", err, code)
	}
}

func ptr[T any](v T) *T { return &v }

// ym is the first day of a month.
func ym(y int, m time.Month) civil.Date {
	d, err := civil.New(y, m, 1)
	if err != nil {
		panic(err)
	}
	return d
}
