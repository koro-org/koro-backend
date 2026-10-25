package kvkk_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/kvkk"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// BE-072: on an overloaded server identity cannot check a re-authentication
// proof within the request budget (httpx, 05 §10.6): the password hasher or
// a store call is still waiting when the request context ends, and Verify
// returns the bare context error (BE-070; identity's
// TestPasswordPaths_PastRequestDeadline_503NoLockout pins that answer).
// That is no verdict on the proof: kvkk audits nothing, logs nothing,
// writes nothing and returns the error unchanged, which httpx answers with
// 503 unavailable and one WARN line (BE-071). Before BE-072 kvkk wrote a
// denied reauth_failed row — with the ended context, so the write failed
// and logged ERROR "reauth failure not audited" — and a data subject's
// erasure request looked like a failed re-authentication in the audit log.

// overloadReauth is identity's verifier behind kvkk. While late is set it
// answers only once the request context has ended, with what the
// overloaded identity returns (the context error, or verdict when set);
// otherwise it is the test verifier. Use it inside a synctest bubble.
type overloadReauth struct {
	late    atomic.Bool
	verdict error // answered at the deadline instead of the context error
	fail    error // answered at once (late unset) instead of checking
}

func (r *overloadReauth) Verify(ctx context.Context, userID uuid.UUID, p kvkk.ReauthProof) (string, error) {
	switch {
	case r.late.Load():
		<-ctx.Done()
		if r.verdict != nil {
			return "", r.verdict
		}
		return "", ctx.Err()
	case r.fail != nil:
		return "", r.fail
	}
	return fakeReauth{}.Verify(ctx, userID, p)
}

// pgAudit is the audit logger over PostgreSQL: an insert with an ended
// context fails as pgx's does. Every attempt is recorded, written or not.
type pgAudit struct {
	next  kvkk.AuditLogger
	mu    sync.Mutex
	tried []audit.Event
}

func (a *pgAudit) Log(ctx context.Context, e audit.Event) error {
	a.mu.Lock()
	a.tried = append(a.tried, e)
	a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return a.next.Log(ctx, e)
}

func (a *pgAudit) attempts(action authz.Action) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.tried {
		if e.Action == string(action) {
			n++
		}
	}
	return n
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Reset drops what was logged so far (the setup's lines).
func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// overloadEnv is a kvkk module whose verifier can be overloaded, whose
// audit fails on an ended context and whose log is captured.
type overloadEnv struct {
	*env
	reauth *overloadReauth
	pg     *pgAudit
	logs   *syncBuffer
}

func newOverloadEnv(t *testing.T, r *overloadReauth) *overloadEnv {
	t.Helper()
	o := &overloadEnv{reauth: r, pg: &pgAudit{}, logs: &syncBuffer{}}
	o.env = newEnv(t, func(d *kvkk.Deps) {
		o.pg.next = d.Audit
		d.Audit, d.Reauth = o.pg, r
		d.Log = obs.NewLogger(o.logs, obs.LoggerOptions{Level: slog.LevelDebug})
	})
	return o
}

// request is a request context with the production budget.
func request(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), httpx.DefaultRequestTimeout)
	t.Cleanup(cancel)
	return ctx
}

// wantOverload asserts the answer to an overloaded request: the request
// deadline itself, after the whole budget, inside no *problem.Error (one
// would take precedence in httpx: T-315), so that httpx answers 503.
func wantOverload(t *testing.T, err error, start time.Time) {
	t.Helper()
	problemtest.WantErr(t, err, context.DeadlineExceeded)
	if pe, ok := errors.AsType[*problem.Error](err); ok {
		t.Fatalf("overload became problem %q; httpx maps the bare deadline to 503", pe.Code())
	}
	if waited := time.Since(start); waited != httpx.DefaultRequestTimeout {
		t.Fatalf("answered after %v, want the whole budget %v", waited, httpx.DefaultRequestTimeout)
	}
}

// TestDSRReauth_PastRequestDeadline_NoAuditNoWrite reproduces BE-072 on
// every kvkk R operation (an erasure request, completing a request, a
// retention policy update): identity cannot answer within the budget, so
// the call fails with the bare deadline (503), tries no denied audit row
// (no ERROR "reauth failure not audited"), writes nothing, and the same
// call with a fresh budget goes through.
func TestDSRReauth_PastRequestDeadline_NoAuditNoWrite(t *testing.T) {
	t.Parallel()
	const policy = "notification_delivery"
	p13 := kvkk.Period{Years: 1, Months: 1}
	cases := []struct {
		name   string
		action authz.Action
		setup  func(t *testing.T, o *overloadEnv) uuid.UUID
		call   func(ctx context.Context, o *overloadEnv, id uuid.UUID) error
		check  func(t *testing.T, o *overloadEnv, id uuid.UUID) // nothing written
	}{
		{name: "createMyDataRequest erasure", action: authz.KvkkDsrCreate,
			call: func(ctx context.Context, o *overloadEnv, _ uuid.UUID) error {
				_, err := o.svc.CreateMine(ctx, studentActor(), kvkk.MyRequest{Type: kvkk.TypeErasure, Details: "Hesabımı silin.", Proof: proof})
				return err
			},
			check: func(t *testing.T, o *overloadEnv, _ uuid.UUID) {
				if rows, _ := o.store.AllOf(t.Context(), studentActor().UserID); len(rows) != 0 {
					t.Fatalf("%d requests stored by the failed request", len(rows))
				}
			}},
		{name: "adminCompleteDataRequest", action: authz.KvkkDsrComplete,
			setup: func(t *testing.T, o *overloadEnv) uuid.UUID {
				d, err := o.svc.CreateMine(t.Context(), studentActor(), kvkk.MyRequest{Type: kvkk.TypeInfo, Details: "Hangi verilerim var?"})
				if err != nil {
					t.Fatal(err)
				}
				return d.ID
			},
			call: func(ctx context.Context, o *overloadEnv, id uuid.UUID) error {
				_, err := o.svc.Complete(ctx, dpoActor(), id, "Başvurunuz yanıtlandı.", proof)
				return err
			},
			check: func(t *testing.T, o *overloadEnv, id uuid.UUID) {
				if d, _ := o.store.Get(t.Context(), id); d.Status != kvkk.StatusReceived || d.CompletedAt != nil {
					t.Fatalf("request after the failed completion: %s", d.Status)
				}
			}},
		{name: "adminUpdateRetentionPolicy", action: authz.KvkkRetentionPolicyUpdate,
			call: func(ctx context.Context, o *overloadEnv, _ uuid.UUID) error {
				cur, err := o.records.Policy(context.WithoutCancel(ctx), policy)
				if err != nil {
					return err
				}
				_, err = o.svc.UpdateRetentionPolicy(ctx, dpoActor(), policy, kvkk.ETagOf(cur.UpdatedAt), kvkk.PolicyPatch{Period: &p13}, proof)
				return err
			},
			check: func(t *testing.T, o *overloadEnv, _ uuid.UUID) {
				if cur, _ := o.records.Policy(t.Context(), policy); cur.Period == p13 || cur.UpdatedBy != nil {
					t.Fatalf("policy changed by the failed request: %+v", cur)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				o := newOverloadEnv(t, &overloadReauth{})
				var id uuid.UUID
				if tc.setup != nil {
					id = tc.setup(t, o)
				}
				events, audited := len(o.out.Events()), o.pg.attempts(tc.action)
				o.logs.Reset()

				o.reauth.late.Store(true)
				start := time.Now()
				err := tc.call(request(t), o, id)
				wantOverload(t, err, start)
				if n := o.pg.attempts(tc.action) - audited; n != 0 {
					t.Fatalf("%s: %d audit rows tried; an overload is no verdict on the proof", tc.action, n)
				}
				if out := o.logs.String(); out != "" {
					t.Fatalf("kvkk logged on an overload (httpx logs the 503 once): %s", out)
				}
				tc.check(t, o, id)
				if n := len(o.out.Events()); n != events {
					t.Fatalf("%d outbox events written by the failed request", n-events)
				}

				// The same request with a fresh budget goes through.
				o.reauth.late.Store(false)
				if err := tc.call(request(t), o, id); err != nil {
					t.Fatalf("retry: %v", err)
				}
			})
		})
	}
}

// TestDSRReauth_OnlyVerdictsAreAudited (03 §6.2, BE-072): a refused proof
// of an erasure request — 403 (wrong or missing proof) or 429 (lockout,
// reauth class) — is audited as denied with reason reauth_failed, as
// before; an error that is no verdict (a dependency down: 503 or a bare
// error) is returned unchanged and audited not at all. Before BE-072 every
// error became a denied row.
func TestDSRReauth_OnlyVerdictsAreAudited(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		err     error
		audited bool
	}{
		{"wrong proof 403", errBadProof, true},
		{"locked out 429", problem.ErrRateLimited, true},
		{"limiter down 503", problem.ErrUnavailable.Wrap(errors.New("redis: connection refused")), false},
		{"store fault", fmt.Errorf("identity: user: %w", errors.New("driver: bad connection")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newOverloadEnv(t, &overloadReauth{fail: tc.err})
			_, err := o.svc.CreateMine(t.Context(), studentActor(), kvkk.MyRequest{Type: kvkk.TypeErasure, Proof: proof})
			problemtest.WantErr(t, err, tc.err)
			denied := o.audit.of(string(authz.KvkkDsrCreate))
			switch {
			case tc.audited && (len(denied) != 1 || denied[0].Outcome != audit.Denied || denied[0].Reason != "reauth_failed" ||
				denied[0].ResourceType != authz.TypeDSR):
				t.Fatalf("audit of the refused proof = %+v", denied)
			case !tc.audited && len(denied) != 0:
				t.Fatalf("audit of an error without a verdict = %+v", denied)
			}
			if rows, _ := o.store.AllOf(t.Context(), studentActor().UserID); len(rows) != 0 {
				t.Fatalf("%d requests stored by the refused request", len(rows))
			}
			if out := o.logs.String(); strings.Contains(out, `"level":"ERROR"`) {
				t.Fatalf("ERROR logged: %s", out)
			}
		})
	}
}

// TestDSRReauth_VerdictAtDeadline_UnauditedIsWarn: a verdict that arrives
// as the request budget ends cannot be audited (the insert needs the
// request's context). That is an overload signal, logged at WARN like
// httpx's 503; ERROR "reauth failure not audited" stays for a real audit
// fault on a live request.
func TestDSRReauth_VerdictAtDeadline_UnauditedIsWarn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		o := newOverloadEnv(t, &overloadReauth{verdict: errBadProof})
		o.reauth.late.Store(true)
		_, err := o.svc.CreateMine(request(t), studentActor(), kvkk.MyRequest{Type: kvkk.TypeErasure, Proof: proof})
		problemtest.WantErr(t, err, errBadProof)
		out := o.logs.String()
		if o.pg.attempts(authz.KvkkDsrCreate) != 1 || !strings.Contains(out, `"level":"WARN","msg":"reauth failure not audited"`) ||
			strings.Contains(out, `"level":"ERROR"`) {
			t.Fatalf("unaudited verdict at the deadline: attempts %d, logs %s", o.pg.attempts(authz.KvkkDsrCreate), out)
		}
	})

	// A live request whose audit write fails is a real fault: ERROR.
	o := newOverloadEnv(t, &overloadReauth{fail: errBadProof})
	o.pg.next = failingAudit{}
	_, err := o.svc.UpdateRetentionPolicy(t.Context(), dpoActor(), "notification_delivery", `"any"`, kvkk.PolicyPatch{}, proof)
	problemtest.WantErr(t, err, errBadProof)
	if out := o.logs.String(); !strings.Contains(out, `"level":"ERROR","msg":"reauth failure not audited"`) {
		t.Fatalf("audit fault on a live request must log ERROR; logs %s", out)
	}
}

// failingAudit is an audit store that is down.
type failingAudit struct{}

func (failingAudit) Log(context.Context, audit.Event) error {
	return errors.New("audit: insert: connection refused")
}

func studentActor() authz.Actor { return persona("student") }

func dpoActor() authz.Actor { return persona("dpo") }

func persona(name string) authz.Actor {
	p, ok := authztest.Get(name)
	if !ok {
		panic("unknown persona " + name)
	}
	return p.Actor
}
