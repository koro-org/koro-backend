package students_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
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

// auditLog records the events the service writes; fail makes every write
// fail.
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

func (l *auditLog) setFail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail = err
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

// catalog is directory: two universities, their departments, skills,
// sensitive clubs and the known clubs (GetClub).
type catalog struct {
	mu        sync.Mutex
	unis      map[uuid.UUID]directory.University
	depts     map[uuid.UUID]directory.Department
	skills    map[uuid.UUID]bool
	sensitive map[uuid.UUID]bool
	clubs     map[uuid.UUID]bool // known clubs (catalog rows)
}

func (c *catalog) GetClub(_ context.Context, orgID uuid.UUID) (directory.Club, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.clubs[orgID] {
		return directory.Club{}, directory.ErrNotFound
	}
	return directory.Club{OrgID: orgID, OrgKind: "club", IsSensitive: c.sensitive[orgID]}, nil
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

// membersFake is orgs' GetMember: memberships by ID; it records the actor
// of every call (only a club_admin of the org may reach it).
type membersFake struct {
	mu    sync.Mutex
	ms    map[uuid.UUID]orgs.MemberView
	calls []authz.Actor
}

func (m *membersFake) GetMember(_ context.Context, a authz.Actor, orgID, id uuid.UUID, _ orgs.Access) (orgs.MemberView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, a)
	v, ok := m.ms[id]
	if !ok || v.Membership.OrgID != orgID || v.Membership.Status == orgs.MemberEnded {
		return orgs.MemberView{}, orgs.ErrNotFound
	}
	return v, nil
}

// add records a membership of user in org and returns its ID.
func (m *membersFake) add(org, user uuid.UUID, role authz.MemberRole, status orgs.MemberStatus) uuid.UUID {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := uuid.NewV7()
	joined := t0.AddDate(0, -2, 0)
	m.ms[id] = orgs.MemberView{
		Membership: orgs.Membership{ID: id, OrgID: org, OrgKind: orgs.KindClub, UserID: user, Role: role, Status: status,
			Source: orgs.SourceJoinRequest, JoinedAt: &joined},
		FirstName: "Ada", LastName: "Yılmaz", AvatarURL: "https://files.test/avatar",
	}
	return id
}

// accountsFake is identity's AdminUser over the users fake.
type accountsFake struct{ users *usersFake }

func (f accountsFake) AdminUser(_ context.Context, _ authz.Actor, id uuid.UUID) (identity.AdminUser, error) {
	f.users.mu.Lock()
	defer f.users.mu.Unlock()
	s, ok := f.users.sum[id]
	if !ok {
		return identity.AdminUser{}, identity.ErrNotFound
	}
	birth := ym(2003, 4)
	out := identity.AdminUser{
		User: identity.User{ID: id, Status: s.Status, Email: "ada@example.test", FirstName: s.FirstName, LastName: s.LastName,
			BirthDate: &birth, Locale: "tr", MFAEnabled: false, CreatedAt: t0.AddDate(-1, 0, 0), PasswordHash: "$argon2id$secret"},
		StudentStatus: authz.StudentNone,
	}
	if v, ok := f.users.ver[id]; ok {
		out.Student, out.StudentStatus = &v, authz.StudentVerified
	}
	return out, nil
}

// dsrsFake is kvkk's data subject requests: open or closed, by subject.
type dsrsFake struct {
	mu   sync.Mutex
	open map[uuid.UUID]uuid.UUID // request → subject (open requests only)
}

func (d *dsrsFake) OpenSubject(_ context.Context, a authz.Actor, id uuid.UUID) (uuid.UUID, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !a.HasPlatformRole(authz.PlatformDPO) {
		return uuid.Nil(), false, nil
	}
	s, ok := d.open[id]
	return s, ok, nil
}

// regsFake is events' registration reader.
type regsFake struct {
	count map[uuid.UUID]int // user → registrations in the club
	list  map[uuid.UUID][]students.RegistrationSummary
}

func (r *regsFake) CountInOrg(_ context.Context, _, user uuid.UUID) (int, error) {
	return r.count[user], nil
}

func (r *regsFake) SummaryOf(_ context.Context, user uuid.UUID) ([]students.RegistrationSummary, error) {
	return r.list[user], nil
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
	notClean map[uuid.UUID]bool // scan pending: PresignGet refuses
	ttls     []time.Duration    // PresignGet lifetimes asked for
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

func (f *filesFake) PresignGet(_ context.Context, id uuid.UUID, ttl time.Duration) (files.Download, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.meta[id]; !ok {
		return files.Download{}, files.ErrNotFound
	}
	if f.notClean[id] {
		return files.Download{}, files.ErrNotClean
	}
	f.ttls = append(f.ttls, ttl)
	return files.Download{URL: "https://files.test/" + id.String(), ExpiresAt: t0.Add(ttl)}, nil
}

func (f *filesFake) Delete(ctx context.Context, _ authz.Actor, id uuid.UUID) error {
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
	f.meta[id] = files.FileMeta{ID: id, Purpose: purpose, UploadedBy: &owner, OwnerUserID: &owner,
		OriginalName: string(purpose) + ".pdf"}
	return id
}

func (f *filesFake) wasDeleted(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.deleted, id)
}

// contactsFake is identity's Contact: the address of every known user.
type contactsFake struct{ users *usersFake }

func (c contactsFake) Contact(_ context.Context, id uuid.UUID) (identity.UserContact, error) {
	c.users.mu.Lock()
	defer c.users.mu.Unlock()
	s, ok := c.users.sum[id]
	if !ok || s.Status == identity.StatusErased {
		return identity.UserContact{}, identity.ErrNotFound
	}
	return identity.UserContact{ID: id, Email: "student-" + id.String()[:8] + "@example.test", FirstName: s.FirstName,
		LastName: s.LastName}, nil
}

// orgNamesFake is orgs' GetMany over named orgs.
type orgNamesFake struct {
	mu    sync.Mutex
	names map[uuid.UUID]string
}

func (o *orgNamesFake) GetMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[uuid.UUID]orgs.OrgSummary{}
	for _, id := range ids {
		if n, ok := o.names[id]; ok {
			out[id] = orgs.OrgSummary{ID: id, Name: n}
		}
	}
	return out, nil
}

func (o *orgNamesFake) name(id uuid.UUID, n string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.names[id] = n
}

// skillNamesFake names the catalog skills and counts its calls (BE-084:
// one per level 2 page).
type skillNamesFake struct {
	names map[uuid.UUID]string
	calls *atomic.Int32
}

func (f skillNamesFake) SkillNames(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	f.calls.Add(1)
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		if n, ok := f.names[id]; ok {
			out[id] = n
		}
	}
	return out, nil
}

// noticesFake is notify's Sender: it records every message; fail is
// returned once per Send while set (a transient delivery failure).
type noticesFake struct {
	mu   sync.Mutex
	sent []notify.Message
	fail error
}

func (n *noticesFake) Send(_ context.Context, m notify.Message) (uuid.UUID, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.fail != nil {
		return uuid.Nil(), n.fail
	}
	n.sent = append(n.sent, m)
	return uuid.NewV7(), nil
}

// messages returns the recorded messages.
func (n *noticesFake) messages() []notify.Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.sent)
}

// env is a students module over the in-memory store and the fakes.
type env struct {
	clk      *clock.Fake
	store    *spyStore
	tx       *spyTx
	out      *outbox.Mem
	audit    *auditLog
	consent  *consents
	catalog  *catalog
	orgs     *orgsFake
	users    *usersFake
	files    *filesFake
	members  *membersFake
	dsrs     *dsrsFake
	resolver *authztest.Resolver // newHTTP
	regs     *regsFake
	orgNames *orgNamesFake
	notices  *noticesFake
	skillN   *atomic.Int32 // SkillNames calls
	mod      students.Module
	svc      students.Service
	subs     *outbox.Registry

	uniA, uniB           uuid.UUID
	deptA, deptB, deptA2 uuid.UUID // deptA2 is inactive
	skill1, skill2       uuid.UUID
	clubPlain, clubSens  uuid.UUID
}

// newEnv builds e; opts adjust the module's dependencies before New
// (BE-088: the suspension window).
func newEnv(t *testing.T, opts ...func(*students.Deps)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{
		clk: clk, store: &spyStore{MemStore: students.NewMemStore(clk.Now)}, tx: &spyTx{}, out: outbox.NewMem(clk),
		audit: &auditLog{}, consent: &consents{removed: map[uuid.UUID]bool{}},
		orgs:  &orgsFake{ms: map[uuid.UUID][]authz.Membership{}},
		users: &usersFake{sum: map[uuid.UUID]identity.UserSummary{}, ver: map[uuid.UUID]identity.EduVerification{}},
		files: &filesFake{meta: map[uuid.UUID]files.FileMeta{}, attached: map[uuid.UUID]files.Owner{},
			notClean: map[uuid.UUID]bool{}},
		members: &membersFake{ms: map[uuid.UUID]orgs.MemberView{}}, dsrs: &dsrsFake{open: map[uuid.UUID]uuid.UUID{}},
		orgNames: &orgNamesFake{names: map[uuid.UUID]string{}}, notices: &noticesFake{}, skillN: &atomic.Int32{},
		uniA: uuid.NewV7(), uniB: uuid.NewV7(), deptA: uuid.NewV7(), deptB: uuid.NewV7(), deptA2: uuid.NewV7(),
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
		clubs:     map[uuid.UUID]bool{e.clubPlain: true, e.clubSens: true},
	}
	e.orgNames.name(e.clubPlain, "Satranç Kulübü")
	e.orgNames.name(e.clubSens, "İnanç Topluluğu")
	deps := students.Deps{
		Store: e.store, Tx: e.tx, Outbox: e.out, Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: e.tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()}),
		Consent:     e.consent, Consents: e.consent, Catalog: e.catalog, Orgs: e.orgs, Users: e.users,
		Verifications: e.users, Files: e.files,
		Members: e.members, Accounts: accountsFake{users: e.users}, DSRs: e.dsrs,
		Registrations: func() students.Registrations {
			if e.regs == nil {
				return nil
			}
			return e.regs
		},
		Contacts: contactsFake{users: e.users}, OrgNames: e.orgNames,
		SkillNames: skillNamesFake{names: map[uuid.UUID]string{e.skill1: "Go", e.skill2: "SQL"}, calls: e.skillN},
		Sender:     e.notices,
	}
	for _, o := range opts {
		o(&deps)
	}
	mod, err := students.New(deps)
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
