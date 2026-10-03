package consent_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// testPassword is the proof the fake re-authentication verifier accepts.
const testPassword = "consent-test-password"

// errBadProof is the fake verifier's answer to a wrong proof.
var errBadProof = problem.ErrForbidden.Wrap(errors.New("fake reauth: wrong proof"))

type fakeReauth struct{}

func (fakeReauth) Verify(_ context.Context, _ uuid.UUID, p consent.ReauthProof) (string, error) {
	if p.Password != testPassword {
		return "", errBadProof
	}
	return "password", nil
}

var proof = consent.ReauthProof{Password: testPassword}

// auditRec is the real audit service over its in-memory store that also
// remembers what it accepted (tests read the events back).
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

// env is a consent module over in-memory stores (05 §13.2).
type env struct {
	clk   *clock.Fake
	store consent.Store
	out   *outbox.Mem
	audit *auditRec
	iys   *consent.FakeIYS
	mod   consent.Module
	svc   consent.Service
}

func newEnv(t *testing.T, opts ...func(*consent.Deps)) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	e := &env{clk: clk, store: consent.NewMemStore(clk.Now), out: outbox.NewMem(clk), iys: &consent.FakeIYS{}}
	e.audit = &auditRec{svc: audit.New(audit.Deps{Store: audit.NewMemStore(), Tx: db.NopTx{}, Clock: clk, Log: obs.Discard()}).Service}
	d := consent.Deps{
		Store: e.store, Tx: db.NopTx{}, Outbox: e.out, Audit: e.audit, Clock: clk, Log: obs.Discard(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: db.NopTx{}, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()}),
		Reauth:      fakeReauth{}, IYS: e.iys,
	}
	for _, o := range opts {
		o(&d)
	}
	mod, err := consent.New(d)
	if err != nil {
		t.Fatalf("consent.New: %v", err)
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

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func ptr[T any](v T) *T { return &v }

// publish creates and publishes a notice as dpo and returns it.
func (e *env) publish(t *testing.T, d consent.NoticeDraft) consent.Notice {
	t.Helper()
	n, err := e.svc.CreateNotice(t.Context(), actor(t, "dpo"), d, proof)
	if err != nil {
		t.Fatalf("CreateNotice: %v", err)
	}
	n, err = e.svc.PublishNotice(t.Context(), actor(t, "dpo"), n.ID, proof)
	if err != nil {
		t.Fatalf("PublishNotice: %v", err)
	}
	return n
}

// consentText publishes a consent text of purpose (version v) for audience all.
func (e *env) consentText(t *testing.T, purpose consent.Purpose, v string) consent.Notice {
	t.Helper()
	return e.publish(t, consent.NoticeDraft{
		Kind: consent.KindConsentText, Purpose: &purpose, Audience: consent.AudienceAll, Locale: consent.LocaleTR,
		Version: v, Title: "Rıza metni " + string(purpose), BodyMD: "Açık rıza metni: " + string(purpose) + " " + v + "\n",
	})
}

// grant records a grant of purpose for userID with the text n.
func grant(userID uuid.UUID, n consent.Notice, ch consent.Channel) consent.RecordInput {
	id := n.ID
	return consent.RecordInput{UserID: userID, Purpose: *n.PurposeCode, Action: consent.Granted,
		NoticeVersionID: &id, TextSHA256: n.BodySHA256, Channel: ch}
}

func withdraw(userID uuid.UUID, purpose consent.Purpose) consent.RecordInput {
	return consent.RecordInput{UserID: userID, Purpose: purpose, Action: consent.Withdrawn, Channel: consent.ChannelWeb}
}

// record runs Recorder.Record and advances the clock (distinct occurred_at).
func (e *env) record(t *testing.T, in consent.RecordInput) uuid.UUID {
	t.Helper()
	id, err := e.svc.Record(t.Context(), in)
	if err != nil {
		t.Fatalf("Record(%s %s): %v", in.Purpose, in.Action, err)
	}
	e.clk.Advance(time.Second)
	return id
}

// subscription returns the consent.recorded subscription of the module.
func (e *env) subscription(t *testing.T) outbox.Subscription {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	subs := r.For(domainevent.TopicConsentRecorded)
	if len(subs) != 1 || subs[0].Name != consent.SubmitIYSTaskName {
		t.Fatalf("subscriptions of consent.recorded = %+v", subs)
	}
	return subs[0]
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

// deliverAll hands every committed consent.recorded event to the subscription.
func (e *env) deliverAll(t *testing.T) []error {
	t.Helper()
	sub := e.subscription(t)
	var errs []error
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicConsentRecorded {
			errs = append(errs, sub.Handle(t.Context(), ev))
		}
	}
	return errs
}
