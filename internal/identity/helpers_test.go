package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/passwd"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/ratelimit"
)

// goodPassword passes the policy (15+ characters, not common).
const goodPassword = "Mavi-Kayik-Bogazda-1923"

var sharedHasher = sync.OnceValue(func() *passwd.Hasher {
	h, err := passwd.New([]byte("identity-unit-test-pepper-identity-unit"), 8)
	if err != nil {
		panic(err)
	}
	return h
})

// countingHasher counts the argon2id work of each path (05 §14.1: unknown
// and known accounts cost the same).
type countingHasher struct {
	h                   *passwd.Hasher
	hash, verify, dummy atomic.Int64
}

func (c *countingHasher) Hash(ctx context.Context, pw string) (string, error) {
	c.hash.Add(1)
	return c.h.Hash(ctx, pw)
}

func (c *countingHasher) Verify(ctx context.Context, pw, enc string) (bool, bool, error) {
	c.verify.Add(1)
	return c.h.Verify(ctx, pw, enc)
}

func (c *countingHasher) VerifyDummy(ctx context.Context, pw string) error {
	c.dummy.Add(1)
	return c.h.VerifyDummy(ctx, pw)
}

// argon2 returns the number of argon2id verifications so far.
func (c *countingHasher) argon2() int64 { return c.verify.Load() + c.dummy.Load() }

// auditRec is the real audit service over its memory store that also
// remembers the accepted events.
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

// fakeRecorder records consents (and the transaction they ran in).
type fakeRecorder struct {
	mu    sync.Mutex
	calls []consent.RecordInput
	txs   []any
	err   error
}

func (f *fakeRecorder) Record(ctx context.Context, in consent.RecordInput) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	f.txs = append(f.txs, ctx.Value(txKey{}))
	return uuid.NewV7(), f.err
}

// notices is the fake consent notice lookup: one current privacy notice
// for students, one consent text of commercial_email, the current terms of
// use (audience all) and a retired version of the terms.
type notices struct {
	privacy, ccEmail, terms, oldTerms identity.NoticeInfo
}

const ccText = "Ticari ileti onayı metni"

func newNotices() notices {
	sum := consent.BodyDigest(ccText)
	return notices{
		privacy: identity.NoticeInfo{ID: uuid.NewV7(), Kind: consent.KindPrivacyNotice, Audience: consent.AudienceStudent, Published: true},
		ccEmail: identity.NoticeInfo{ID: uuid.NewV7(), Kind: consent.KindConsentText, Purpose: consent.PurposeCommercialEmail,
			Audience: consent.AudienceAll, BodySHA256: sum, Published: true},
		terms: identity.NoticeInfo{ID: uuid.NewV7(), Kind: consent.KindTermsOfUse, Audience: consent.AudienceAll, Version: "2026.10.01",
			BodySHA256: consent.BodyDigest("Kullanım koşulları"), Published: true},
		oldTerms: identity.NoticeInfo{ID: uuid.NewV7(), Kind: consent.KindTermsOfUse, Audience: consent.AudienceAll, Version: "2026.09.01",
			BodySHA256: consent.BodyDigest("Eski kullanım koşulları")},
	}
}

func (n notices) NoticeByID(_ context.Context, id uuid.UUID) (identity.NoticeInfo, error) {
	for _, x := range []identity.NoticeInfo{n.privacy, n.ccEmail, n.terms, n.oldTerms} {
		if x.ID == id {
			return x, nil
		}
	}
	return identity.NoticeInfo{}, problem.ErrNotFound
}

// fakeTerms is consent's terms ledger over the fake notices: the current
// terms of use are current (any audience); acceptances in memory, one per
// (user, version), with the transaction each call ran in.
type fakeTerms struct {
	mu       sync.Mutex
	current  identity.NoticeInfo
	calls    []consent.TermsInput
	txs      []any
	accepted []consent.TermsAcceptance
	err      error
}

func (f *fakeTerms) CheckTerms(_ context.Context, id uuid.UUID, _ consent.Audience) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != f.current.ID || !f.current.Published {
		return consent.ErrTermsNotCurrent
	}
	return nil
}

func (f *fakeTerms) RecordTerms(ctx context.Context, in consent.TermsInput) (consent.TermsAcceptance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	f.txs = append(f.txs, ctx.Value(txKey{}))
	switch {
	case f.err != nil:
		return consent.TermsAcceptance{}, f.err
	case in.NoticeVersionID != f.current.ID || !f.current.Published:
		return consent.TermsAcceptance{}, consent.ErrTermsNotCurrent
	}
	for _, a := range f.accepted {
		if a.UserID == in.UserID && a.NoticeVersionID == in.NoticeVersionID {
			return a, nil
		}
	}
	a := consent.TermsAcceptance{ID: uuid.NewV7(), UserID: in.UserID, NoticeVersionID: in.NoticeVersionID,
		TextSHA256: f.current.BodySHA256, Channel: in.Channel, AcceptedAt: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)}
	f.accepted = append(f.accepted, a)
	return a, nil
}

func (f *fakeTerms) TermsStatus(_ context.Context, userID uuid.UUID, _ consent.Audience, _ string) (consent.TermsStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var st consent.TermsStatus
	if f.current.Published {
		st.Current = &consent.Notice{ID: f.current.ID, Kind: f.current.Kind, Audience: f.current.Audience, Version: f.current.Version}
	}
	for i := len(f.accepted) - 1; i >= 0; i-- {
		a := f.accepted[i]
		if a.UserID != userID {
			continue
		}
		if st.Current != nil && a.NoticeVersionID == st.Current.ID {
			st.Accepted = &a
			break
		}
		if st.Accepted == nil {
			st.Accepted = &a
		}
	}
	st.Required = st.Current != nil && (st.Accepted == nil || st.Accepted.NoticeVersionID != st.Current.ID)
	return st, nil
}

// publish makes n the current terms of use (a new version).
func (f *fakeTerms) publish(n identity.NoticeInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n.Published = true
	f.current = n
}

// mailer records the messages notify would send.
type mailer struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (m *mailer) Send(_ context.Context, msg notify.Message) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return uuid.NewV7(), nil
}

func (m *mailer) sent() []notify.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.msgs)
}

// txKey marks the transaction a call ran in (commitSpyTx).
type txKey struct{}

// commitSpyTx is db.NopTx that numbers its outermost transactions and remembers
// whether each one committed.
type commitSpyTx struct {
	n      atomic.Int64
	mu     sync.Mutex
	failed []any
}

func (s *commitSpyTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if ctx.Value(txKey{}) != nil {
		return fn(ctx)
	}
	id := s.n.Add(1)
	err := db.NopTx{}.WithTx(context.WithValue(ctx, txKey{}, id), fn)
	if err != nil {
		s.mu.Lock()
		s.failed = append(s.failed, id)
		s.mu.Unlock()
	}
	return err
}

func (s *commitSpyTx) WithSerializableTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return s.WithTx(ctx, fn)
}

// spyStore remembers the transaction InsertUser ran in.
type spyStore struct {
	*identity.MemStore
	mu       sync.Mutex
	insertTx []any
}

func (s *spyStore) InsertUser(ctx context.Context, u identity.User) error {
	s.mu.Lock()
	s.insertTx = append(s.insertTx, ctx.Value(txKey{}))
	s.mu.Unlock()
	return s.MemStore.InsertUser(ctx, u)
}

// env is an identity module over in-memory stores (05 §13.2).
type env struct {
	clk       *clock.Fake
	store     *spyStore
	tx        *commitSpyTx
	out       *outbox.Mem
	q         *queue.Mem
	lim       *ratelimit.Mem
	audit     *auditRec
	consent   *fakeRecorder
	terms     *fakeTerms
	notices   notices
	mail      *mailer
	hasher    *countingHasher
	members   map[uuid.UUID][]authz.Membership
	mod       identity.Module
	svc       identity.Service
	subs      *outbox.Registry
	delivered int
	chal      *identity.MemChallenges
}

// testEnvelope seals TOTP secrets (fixed local KEK).
var testEnvelope = sync.OnceValue(func() *crypto.Envelope {
	kek, err := crypto.NewLocalKEK([]crypto.Key{{ID: "k1", Secret: []byte("identity-unit-kek-32-bytes-long!")}})
	if err != nil {
		panic(err)
	}
	env, err := crypto.NewEnvelope(kek)
	if err != nil {
		panic(err)
	}
	return env
})

func newEnv(t *testing.T, opts ...func(*identity.Deps)) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	der, err := crypto.NewDeriver([]crypto.Key{{ID: "k1", Secret: []byte("identity-unit-token-key-identity-unit-token")}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		clk: clk, store: &spyStore{MemStore: identity.NewMemStore(clk.Now)}, tx: &commitSpyTx{}, out: outbox.NewMem(clk), q: queue.NewMem(),
		lim: ratelimit.NewMem(clk), consent: &fakeRecorder{}, notices: newNotices(), mail: &mailer{},
		hasher: &countingHasher{h: sharedHasher()}, members: map[uuid.UUID][]authz.Membership{}, subs: outbox.NewRegistry(),
		chal: identity.NewMemChallenges(clk),
	}
	e.terms = &fakeTerms{current: e.notices.terms}
	e.audit = &auditRec{svc: audit.New(audit.Deps{Store: audit.NewMemStore(), Tx: db.NopTx{}, Clock: clk, Log: obs.Discard()}).Service}
	d := identity.Deps{
		Store: e.store, Tx: e.tx, Outbox: e.out, Queue: e.q, Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Limiter: e.lim, Deriver: der, Consent: e.consent, Terms: e.terms, Notices: e.notices, Mailer: e.mail,
		LinkBaseURL: "https://app.koro.test", Hasher: e.hasher, Secrets: testEnvelope(), Challenges: e.chal,
		Memberships: func(_ context.Context, id uuid.UUID) ([]authz.Membership, error) { return e.members[id], nil },
	}
	for _, o := range opts {
		o(&d)
	}
	mod, err := identity.New(d)
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	mod.Subscribe(e.subs)
	return e
}

// workerCtx is a task context (notify refuses request contexts).
func workerCtx(t *testing.T) context.Context { return obs.WithTaskID(t.Context(), "task-1") }

// deliver runs the not yet delivered outbox events through identity's
// subscriptions (the worker) and returns how many it ran.
func (e *env) deliver(t *testing.T) {
	t.Helper()
	evs := e.out.Events()
	for _, ev := range evs[e.delivered:] {
		for _, s := range e.subs.For(ev.Topic) {
			b, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.HandleTask(workerCtx(t), b); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
	e.delivered = len(evs)
}

// runNotices runs the queued security notice tasks.
func (e *env) runNotices(t *testing.T) {
	t.Helper()
	task := e.mod.Tasks()[0]
	for _, x := range e.q.Tasks() {
		if x.Type == identity.SendSecurityNoticeTaskName {
			if err := task.Handle(workerCtx(t), x.Payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.q.Reset()
}

// lastToken returns the token of the last e-mail of template.
func (e *env) lastToken(t *testing.T, template string) string {
	t.Helper()
	e.deliver(t)
	msgs := e.mail.sent()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Template == template {
			return msgs[i].Data["token"].(string)
		}
	}
	t.Fatalf("no %s e-mail among %d", template, len(msgs))
	return ""
}

func (e *env) registration(email string) identity.Registration {
	b, _ := civil.New(2003, time.April, 23)
	return identity.Registration{
		Email: email, Password: goodPassword, FirstName: "Ada", LastName: "Yılmaz", BirthDate: &b,
		Persona: identity.PersonaStudent, Locale: identity.LocaleTR, PrivacyNoticeID: e.notices.privacy.ID,
		TermsNoticeID: e.notices.terms.ID, Client: identity.ClientWeb,
	}
}

// activeUser registers and verifies an account and returns it.
func (e *env) activeUser(t *testing.T, email string) identity.User {
	t.Helper()
	if err := e.svc.Register(t.Context(), authz.Anonymous(), e.registration(email)); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ConfirmEmail(t.Context(), authz.Anonymous(), e.lastToken(t, "identity.verify_email")); err != nil {
		t.Fatal(err)
	}
	id, ok, err := e.svc.FindByEmail(t.Context(), email)
	if err != nil || !ok {
		t.Fatalf("FindByEmail: %v %v", ok, err)
	}
	u, err := e.store.User(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// login opens a session and returns it with the actor the resolver would
// build.
func (e *env) login(t *testing.T, email string, client identity.ClientKind) (identity.LoginResult, authz.Actor) {
	t.Helper()
	res, err := e.svc.CreateSession(t.Context(), authz.Anonymous(), identity.Login{Email: email, Password: goodPassword, Client: client})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return res, authz.Actor{Kind: authz.KindUser, UserID: res.Session.UserID, SessionID: res.Session.ID, StudentStatus: authz.StudentNone}
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	pe, ok := errors.AsType[*problem.Error](err)
	if !ok || pe.Status() != status {
		t.Fatalf("error = %v, want status %d", err, status)
	}
}

func hasNoSecret(t *testing.T, where, s string, secrets ...string) {
	t.Helper()
	for _, x := range secrets {
		if x != "" && strings.Contains(s, x) {
			t.Fatalf("%s contains a secret", where)
		}
	}
}

var _ = domainevent.TopicIdentityEmailTokenIssued
