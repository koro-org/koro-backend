package kvkk_test

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
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/kvkk"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// testPassword is the proof the fake re-authentication verifier accepts.
const testPassword = "kvkk-test-password"

const dpoMailbox = "kvkk@koro.test"

var errBadProof = problem.ErrForbidden.Wrap(errors.New("fake reauth: wrong proof"))

type fakeReauth struct{}

func (fakeReauth) Verify(_ context.Context, _ uuid.UUID, p kvkk.ReauthProof) (string, error) {
	if p.Password != testPassword {
		return "", errBadProof
	}
	return "password", nil
}

var proof = kvkk.ReauthProof{Password: testPassword}

// auditRec is the real audit service over its in-memory store that also
// remembers what it accepted.
type auditRec struct {
	svc    audit.Logger
	mu     sync.Mutex
	events []audit.Event
}

func (a *auditRec) Log(ctx context.Context, e audit.Event) error {
	if err := a.svc.Log(ctx, e); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return nil
}

func (a *auditRec) of(action string) []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.Event
	for _, e := range a.events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// fakeUsers is identity: live users have a contact; erase turns them into
// tombstones (they still exist).
type fakeUsers struct {
	mu     sync.Mutex
	live   map[uuid.UUID]kvkk.Contact
	erased map[uuid.UUID]bool
}

func (u *fakeUsers) add(id uuid.UUID) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.live[id] = kvkk.Contact{Email: "kisi-" + id.String()[:8] + "@example.test", FirstName: "Ada", Locale: "tr"}
}

func (u *fakeUsers) erase(id uuid.UUID) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.live[id]; !ok {
		return false
	}
	delete(u.live, id)
	u.erased[id] = true
	return true
}

func (u *fakeUsers) Exists(_ context.Context, id uuid.UUID) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, ok := u.live[id]
	return ok || u.erased[id], nil
}

func (u *fakeUsers) Contact(_ context.Context, id uuid.UUID) (kvkk.Contact, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	c, ok := u.live[id]
	return c, ok, nil
}

// fakeMailer records e-mails, deduplicated by key like notify.
type fakeMailer struct {
	mu   sync.Mutex
	sent []notify.Message
	keys map[string]bool
}

func (m *fakeMailer) Send(_ context.Context, msg notify.Message) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.keys[msg.DedupeKey] {
		m.keys[msg.DedupeKey] = true
		m.sent = append(m.sent, msg)
	}
	return uuid.NewV7(), nil
}

func (m *fakeMailer) of(template string) []notify.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []notify.Message
	for _, x := range m.sent {
		if x.Template == template {
			out = append(out, x)
		}
	}
	return out
}

// callLog records provider calls in order.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, s)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// fakeProvider is a module's privacy provider: it records every call, and
// whether the caller wrapped it in a transaction; fail makes the next
// erase fail once.
type fakeProvider struct {
	name     string
	log      *callLog
	items    []map[string]any
	retained int
	reason   string
	onErase  func(uuid.UUID)

	mu      sync.Mutex
	fail    bool
	inTx    bool
	erasing int
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Export(_ context.Context, _ uuid.UUID) (privacy.ExportSection, error) {
	p.log.add("export:" + p.name)
	return privacy.ExportSection{Module: p.name, Items: p.items}, nil
}

func (p *fakeProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	p.log.add("erase:" + p.name)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inTx = p.inTx || db.InTx(ctx)
	if sub, ok := privacy.SubjectFrom(ctx); !ok || sub.UserID != userID {
		return privacy.EraseResult{}, errors.New("no subject in context")
	}
	if p.fail {
		p.fail = false
		return privacy.EraseResult{}, errors.New("transient failure")
	}
	p.erasing++
	if p.onErase != nil {
		p.onErase(userID)
	}
	res := privacy.EraseResult{Module: p.name, Retained: p.retained, RetainedReason: p.reason}
	if p.erasing == 1 {
		res.Deleted = len(p.items)
	}
	return res, nil
}

// fakeTalent is hooks.talentAuditor.
type fakeTalent struct{ calls int }

func (*fakeTalent) Available() bool { return true }

func (f *fakeTalent) Report(_ context.Context, _, userID uuid.UUID, _ string) (json.RawMessage, error) {
	f.calls++
	return json.RawMessage(`{"user_id":"` + userID.String() + `","band":"hidden"}`), nil
}

// env is a kvkk module over in-memory stores (05 §13.2) with the real
// files service over in-memory object storage.
type env struct {
	clk       *clock.Fake
	store     *kvkk.MemStore
	records   *kvkk.MemRecords
	metrics   *kvkk.MemMetrics
	sweepers  []privacy.RetentionSweeper
	revoked   *callLog // RevokeSessions calls ("revoke:<user>")
	admins    []uuid.UUID
	out       *outbox.Mem
	subs      *outbox.Registry
	audit     *auditRec
	users     *fakeUsers
	mail      *fakeMailer
	files     files.Service
	zips      *capFiles
	filesMem  *storage.Mem
	log       *callLog
	providers []privacy.Provider
	mod       kvkk.Module
	svc       kvkk.Service
}

func newEnv(t *testing.T, opts ...func(*kvkk.Deps)) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	e := &env{clk: clk, store: kvkk.NewMemStore(clk.Now), records: kvkk.NewMemRecords(clk.Now), metrics: kvkk.NewMemMetrics(),
		revoked: &callLog{},
		admins:  []uuid.UUID{authztest.ID("platform_admin")}, out: outbox.NewMem(clk), subs: outbox.NewRegistry(),
		users: &fakeUsers{live: map[uuid.UUID]kvkk.Contact{}, erased: map[uuid.UUID]bool{}},
		mail:  &fakeMailer{keys: map[string]bool{}}, log: &callLog{}, filesMem: storage.NewMem(clk)}
	auditSvc := audit.New(audit.Deps{Store: audit.NewMemStore(), Tx: db.NopTx{}, Clock: clk, Log: obs.Discard()}).Service
	e.audit = &auditRec{svc: auditSvc}
	keeper := func() *idempotency.Keeper {
		return idempotency.New(idempotency.Deps{Tx: db.NopTx{}, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	}
	fm, err := files.New(files.Deps{
		Store: files.NewMemStore(clk.Now), Objects: e.filesMem, Tx: db.NopTx{}, Outbox: e.out, Queue: queue.NewMem(),
		Idempotency: keeper(), Clock: clk, Log: obs.Discard(), Buckets: files.Buckets{Files: "files", Quarantine: "quarantine"},
		Audit: auditSvc,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.files = fm.Service
	e.zips = &capFiles{Service: fm.Service, content: map[uuid.UUID][]byte{}}
	for _, s := range authztest.Personas() {
		if s.Actor.IsUser() {
			e.users.add(s.Actor.UserID)
		}
	}
	// Default providers: two modules and identity (whose erase tombstones
	// the fake user).
	e.providers = []privacy.Provider{
		&fakeProvider{name: "events", log: e.log, items: []map[string]any{{"registration_id": "r1"}}},
		&fakeProvider{name: "consent", log: e.log, items: []map[string]any{{"consent_id": "c1"}}, retained: 1, reason: "consent proof"},
		&fakeProvider{name: kvkk.IdentityProvider, log: e.log, items: []map[string]any{{"type": "account"}},
			onErase: func(id uuid.UUID) { e.users.erase(id) }},
	}
	seedPolicies(e.records, clk.Now())
	d := kvkk.Deps{
		Store: e.store, Records: e.records, Metrics: e.metrics, Tx: db.NopTx{}, Outbox: e.out, Audit: e.audit, Idempotency: keeper(), Clock: clk,
		Log: obs.Discard(), Reauth: fakeReauth{}, Users: e.users, Files: e.zips,
		Mailer: e.mail, Providers: func() []privacy.Provider { return e.providers }, DPOEmail: dpoMailbox,
		Sweepers: func() []privacy.RetentionSweeper { return e.sweepers },
		RevokeSessions: func(_ context.Context, userID uuid.UUID) (int, error) {
			e.revoked.add("revoke:" + userID.String())
			return 1, nil
		},
		Admins: func(context.Context) ([]uuid.UUID, error) { return e.admins, nil },
	}
	for _, o := range opts {
		o(&d)
	}
	mod, err := kvkk.New(d)
	if err != nil {
		t.Fatalf("kvkk.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	e.providers = append(e.providers, mod.Privacy)
	mod.Subscribe(e.subs)
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

// workerCtx is the context of a worker task.
func workerCtx(t *testing.T) context.Context { return obs.WithTaskID(t.Context(), "task-1") }

// deliver runs every subscription of every outbox event written since
// from, in order, like the relay (at-least-once: tests may deliver twice).
// It returns the index to continue from.
func (e *env) deliver(t *testing.T, from int) (int, error) {
	t.Helper()
	evs := e.out.Events()
	for _, ev := range evs[from:] {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range e.subs.For(ev.Topic) {
			if err := s.HandleTask(workerCtx(t), b); err != nil {
				return len(evs), err
			}
		}
	}
	return len(evs), nil
}

// deliverTopic runs the subscriptions of the events of topic.
func (e *env) deliverTopic(t *testing.T, topic string) error {
	t.Helper()
	for _, ev := range e.out.Events() {
		if ev.Topic != topic {
			continue
		}
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range e.subs.For(ev.Topic) {
			if err := s.HandleTask(workerCtx(t), b); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *env) job(t *testing.T, name string) queue.Job {
	t.Helper()
	for _, j := range e.mod.Jobs() {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("no job %s", name)
	return queue.Job{}
}

// topics lists the outbox topics written so far.
func (e *env) topics() []string { return e.out.Topics() }

// completeAs answers id as dpo with the proof.
func (e *env) completeAs(t *testing.T, id uuid.UUID) kvkk.DSR {
	t.Helper()
	d, err := e.svc.Complete(t.Context(), actor(t, "dpo"), id, "Başvurunuz yanıtlandı.", proof)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return d
}

func decodeCompleted(t *testing.T, ev domainevent.Envelope) domainevent.DSRCompleted {
	t.Helper()
	p, err := domainevent.Decode[domainevent.DSRCompleted](ev)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// capFiles is the files service that also keeps the generated contents.
type capFiles struct {
	files.Service
	mu      sync.Mutex
	content map[uuid.UUID][]byte
}

func (c *capFiles) PutGenerated(ctx context.Context, in files.GeneratedFile) (uuid.UUID, error) {
	id, err := c.Service.PutGenerated(ctx, in)
	if err == nil {
		c.mu.Lock()
		c.content[id] = in.Content
		c.mu.Unlock()
	}
	return id, err
}

func (c *capFiles) of(id uuid.UUID) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.content[id]
}

// seedPolicies seeds the retention policies the tests use, as 000090 /
// 000121 do: none legally approved.
func seedPolicies(r *kvkk.MemRecords, at time.Time) {
	for _, p := range []kvkk.Policy{
		{Code: kvkk.ExportPolicyCode, OwnerModule: "kvkk", Target: "files:dsr_export", Period: kvkk.Period{Days: 7},
			Anchor: "created_at", Method: privacy.MethodFileDelete, LegalBasis: "Teslim süresi", Enabled: true},
		{Code: kvkk.ErasurePolicyCode, OwnerModule: "kvkk", Target: "users", Anchor: "created_at",
			Method: privacy.MethodHardDelete, LegalBasis: "Hesap silme", Enabled: true},
		{Code: "notification_delivery", OwnerModule: "notify", Target: "notification_deliveries", Period: kvkk.Period{Years: 1},
			Anchor: "created_at", Method: privacy.MethodHardDelete, LegalBasis: "R-003 §5.4", Enabled: true},
		{Code: "post_removed", OwnerModule: "feed", Target: "posts", Period: kvkk.Period{Years: 1},
			Anchor: "closed_at", Method: privacy.MethodHardDelete, LegalBasis: "Moderasyon itirazı", Enabled: true},
		{Code: "outreach_dispatch", OwnerModule: "outreach", Target: "outreach_dispatches", Period: kvkk.Period{Years: 2},
			Anchor: "created_at", Method: privacy.MethodHardDelete, LegalBasis: "Hak kullanımı ispatı", Enabled: true},
		{Code: "destruction_log", OwnerModule: "kvkk", Target: "destruction_logs", Period: kvkk.Period{Years: 3},
			Anchor: "created_at", Method: privacy.MethodHardDelete, LegalBasis: "Teknik rapor §6.2", Enabled: true},
		{Code: "dormant_account", OwnerModule: "identity", Target: "users", Period: kvkk.Period{Years: 100},
			Anchor: "last_seen_at", Method: privacy.MethodAnonymize, LegalBasis: "Hukukçu kararına kadar kapalı", Enabled: false},
	} {
		p.DescriptionTR, p.CreatedAt, p.UpdatedAt = "Test", at, at
		r.PutPolicy(p)
	}
}
