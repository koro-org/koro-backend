package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/token"
)

// BE-072 over HTTP, with identity's real re-authentication behind consent
// and kvkk: when identity cannot check the proof within the request budget
// (hasher slots taken, a store call past the deadline), the R operation
// answers 503 unavailable after the whole budget, logs one WARN line and
// no ERROR, writes no denied audit row and counts no failed attempt.
// Before BE-072 consent and kvkk tried a denied reauth_failed row with the
// ended context: the insert failed and ERROR "reauth failure not audited"
// fired on every overloaded R request.

// verifyLateHasher is the shared test hasher on an overloaded server:
// while late is set, Verify reaches the real hasher only after the request
// context has ended, so identity sees platform/passwd's own context error.
type verifyLateHasher struct {
	identity.PasswordHasher
	late atomic.Bool
}

func (h *verifyLateHasher) Verify(ctx context.Context, pw, encoded string) (bool, bool, error) {
	if h.late.Load() {
		<-ctx.Done()
	}
	return h.PasswordHasher.Verify(ctx, pw, encoded)
}

// slowTOTPStore is the identity store on an overloaded database for the
// TOTP read of a re-authentication (only Verify reads it): while slow is
// set it answers once the request context has ended, with the context
// error wrapped like a store error.
type slowTOTPStore struct {
	identity.Store
	slow atomic.Bool
}

func (s *slowTOTPStore) TOTP(ctx context.Context, userID uuid.UUID) (identity.TOTP, bool, error) {
	if s.slow.Load() {
		<-ctx.Done()
		return identity.TOTP{}, false, fmt.Errorf("identity: totp: %w", ctx.Err())
	}
	return s.Store.TOTP(ctx, userID)
}

// pgAuditStore is the audit store over PostgreSQL: an insert with an
// ended context fails as pgx's does. Every attempted row is recorded.
type pgAuditStore struct {
	audit.Store
	mu    sync.Mutex
	tried []audit.Record
}

func (s *pgAuditStore) Insert(ctx context.Context, r audit.Record) error {
	s.mu.Lock()
	s.tried = append(s.tried, r)
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return s.Store.Insert(ctx, r)
}

// of returns the attempted rows of action.
func (s *pgAuditStore) of(action authz.Action) []audit.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []audit.Record
	for _, r := range s.tried {
		if r.Action == string(action) {
			out = append(out, r)
		}
	}
	return out
}

// reauthBudgetEnv is the wired application with identity's real verifier
// bound to consent and kvkk, a PostgreSQL-like audit store and one log
// sink for the modules and httpx.
type reauthBudgetEnv struct {
	c      *container
	mem    *identity.MemStore
	audits *pgAuditStore
	logs   *lockedBuffer
	mark   int // length of the logs before the request under test
	app    *httpx.App
}

func newReauthBudgetEnv(t *testing.T, store func(*identity.MemStore) identity.Store, hasher identity.PasswordHasher) *reauthBudgetEnv {
	t.Helper()
	c := newTestContainer(t)
	e := &reauthBudgetEnv{c: c, mem: identity.NewMemStore(clock.Real().Now), logs: &lockedBuffer{}}
	e.audits = &pgAuditStore{Store: c.audit.store}
	c.audit.store = e.audits
	c.identity.store, c.identity.hasher = store(e.mem), hasher
	c.consent.reauth, c.kvkk.reauth = nil, nil // identity binds its own Verify (BE-014, BE-049)
	log := obs.NewLogger(e.logs, obs.LoggerOptions{Level: slog.LevelInfo})
	c.infra.log = log
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	app, err := httpx.New(httpx.Deps{Log: log, Actors: c.actors}) // default request budget, as the matrix
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	e.app = app
	return e
}

func (e *reauthBudgetEnv) do(ctx context.Context, tok, method, path, body string) (int, string, string) {
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	req.Header.Set("X-Koro-Client", "mobile")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.app.Handler().ServeHTTP(rec, req)
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p.Code, rec.Body.String()
}

// overloaded runs one request inside a synctest bubble and asserts the
// overload answer: 503 unavailable after the whole budget.
func (e *reauthBudgetEnv) overloaded(t *testing.T, tok, method, path, body string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		status, code, raw := e.do(t.Context(), tok, method, path, body)
		if status != http.StatusServiceUnavailable || code != "unavailable" {
			t.Fatalf("%s %s past the request budget = %d %s, want 503 unavailable", method, path, status, raw)
		}
		if waited := time.Since(start); waited != httpx.DefaultRequestTimeout {
			t.Fatalf("answered after %v, want the whole budget %v", waited, httpx.DefaultRequestTimeout)
		}
	})
}

// markLogs starts the logs of the request under test.
func (e *reauthBudgetEnv) markLogs() { e.mark = len(e.logs.String()) }

// quiet asserts the logs of an overloaded request: the 503 once at WARN,
// no ERROR (no "reauth failure not audited").
func (e *reauthBudgetEnv) quiet(t *testing.T) {
	t.Helper()
	out := e.logs.String()[e.mark:]
	if strings.Count(out, `"level":"WARN","msg":"request budget exhausted"`) != 1 || strings.Contains(out, `"level":"ERROR"`) ||
		strings.Contains(out, "not audited") {
		t.Fatalf("an exhausted budget logs once at WARN and never ERROR; logs = %s", out)
	}
}

// TestWire_ErasureReauthPastRequestBudget_503NoDeniedAudit: a student's
// erasure request (kvkk.dsr.create, R with the password, 03 k31) while
// every hasher slot stays taken until the deadline: 503, no denied audit
// row, no failed attempt, no request stored; the retry is 201.
func TestWire_ErasureReauthPastRequestBudget_503NoDeniedAudit(t *testing.T) {
	t.Parallel()
	late := &verifyLateHasher{PasswordHasher: testHasher()}
	e := newReauthBudgetEnv(t, func(m *identity.MemStore) identity.Store { return m }, late)
	ae := &actorEnv{c: e.c, store: e.mem, clk: e.c.infra.clock}
	birth, err := civil.New(2004, time.March, 9)
	if err != nil {
		t.Fatal(err)
	}
	u := ae.user(t, &birth)
	tok := ae.session(t, u, identity.ClientMobile)
	erasure := `{"type":"erasure","reauth":{"password":"` + matrixReauthPassword + `"}}`
	if status, _, raw := e.do(t.Context(), tok, http.MethodGet, "/api/v1/me", ""); status != http.StatusOK {
		t.Fatalf("GET /me = %d %s", status, raw) // warm the stack up outside the bubble
	}
	e.markLogs()

	late.late.Store(true)
	e.overloaded(t, tok, http.MethodPost, "/api/v1/me/data-requests", erasure)
	late.late.Store(false)
	e.quiet(t)
	if rows := e.audits.of(authz.KvkkDsrCreate); len(rows) != 0 {
		t.Fatalf("audit rows tried for the overloaded erasure request: %+v", rows)
	}
	if cur, _ := e.mem.User(t.Context(), u.ID); cur.FailedLogins != 0 || cur.LockedUntil != nil {
		t.Fatalf("the overload counted as a wrong password: failed_logins=%d", cur.FailedLogins)
	}
	if rows, _ := e.c.kvkk.store.AllOf(t.Context(), u.ID); len(rows) != 0 {
		t.Fatalf("%d requests stored by the failed request", len(rows))
	}

	if status, _, raw := e.do(t.Context(), tok, http.MethodPost, "/api/v1/me/data-requests", erasure); status != http.StatusCreated {
		t.Fatalf("retry = %d %s, want 201", status, raw)
	}
	if rows := e.audits.of(authz.KvkkDsrCreate); len(rows) != 1 || rows[0].Outcome != audit.Success {
		t.Fatalf("retry audit = %+v", rows)
	}
}

// TestWire_ConsentReauthPastRequestBudget_503NoDeniedAudit: a DPO (MFA
// account, TOTP proof) switches a consent purpose off while the database
// cannot answer the TOTP read within the budget: 503, no denied audit row,
// no failed attempt, the purpose unchanged. A real wrong code afterwards
// keeps the 03 §6.2 behaviour: 403 identity.reauth_failed, one denied
// reauth_failed row, one failed attempt counted.
func TestWire_ConsentReauthPastRequestBudget_503NoDeniedAudit(t *testing.T) {
	t.Parallel()
	var slow *slowTOTPStore
	e := newReauthBudgetEnv(t, func(m *identity.MemStore) identity.Store {
		slow = &slowTOTPStore{Store: m}
		return slow
	}, testHasher())
	tok, dpo := e.mfaDPO(t)
	patch := `{"active":false,"reauth":{"totp_code":"123456"}}`
	path := "/api/v1/admin/consent-purposes/commercial_push"
	if status, _, raw := e.do(t.Context(), tok, http.MethodGet, "/api/v1/admin/consent-purposes", ""); status != http.StatusOK {
		t.Fatalf("GET purposes = %d %s", status, raw) // warm the stack up outside the bubble
	}
	e.markLogs()

	slow.slow.Store(true)
	e.overloaded(t, tok, http.MethodPatch, path, patch)
	slow.slow.Store(false)
	e.quiet(t)
	if rows := e.audits.of(authz.ConsentPurposeManage); len(rows) != 0 {
		t.Fatalf("audit rows tried for the overloaded request: %+v", rows)
	}
	if cur, _ := e.mem.User(t.Context(), dpo); cur.FailedLogins != 0 {
		t.Fatalf("the overload counted as a wrong code: failed_logins=%d", cur.FailedLogins)
	}
	if p, err := e.c.consent.store.Purpose(t.Context(), "commercial_push"); err != nil || !p.Active {
		t.Fatalf("purpose after the failed request: %+v %v", p, err)
	}

	// A wrong code is a verdict: 403, denied audit, counted (unchanged).
	status, code, raw := e.do(t.Context(), tok, http.MethodPatch, path, patch)
	if status != http.StatusForbidden || code != "identity.reauth_failed" {
		t.Fatalf("wrong TOTP code = %d %s, want 403 identity.reauth_failed", status, raw)
	}
	rows := e.audits.of(authz.ConsentPurposeManage)
	if len(rows) != 1 || rows[0].Outcome != audit.Denied || rows[0].Reason != "reauth_failed" {
		t.Fatalf("audit of the wrong code = %+v", rows)
	}
	if cur, _ := e.mem.User(t.Context(), dpo); cur.FailedLogins != 1 {
		t.Fatalf("wrong code not counted: failed_logins=%d", cur.FailedLogins)
	}
	if out := e.logs.String()[e.mark:]; strings.Contains(out, `"level":"ERROR"`) {
		t.Fatalf("ERROR logged: %s", out)
	}
}

// mfaDPO creates an active DPO account with TOTP on and an MFA-verified
// mobile session (03 §6.4: dpo is a role-level MFA role); it returns the
// session token and the user ID.
func (e *reauthBudgetEnv) mfaDPO(t *testing.T) (string, uuid.UUID) {
	t.Helper()
	now := e.c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	id := uuid.NewV7()
	u := identity.User{
		ID: id, Status: identity.StatusActive, Email: id.String() + "@dpo.koro.test", EmailNormalized: id.String() + "@dpo.koro.test",
		EmailVerifiedAt: &now, PasswordHash: testPasswordHash(), FirstName: "Deniz", LastName: "Kaya",
		Persona: identity.PersonaCompany, Locale: identity.LocaleTR, MFAEnabled: true,
	}
	if err := e.mem.InsertUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	if err := e.mem.InsertRoleAssignment(t.Context(), identity.RoleAssignment{ID: uuid.NewV7(), UserID: id, Role: authz.PlatformDPO,
		Reason: "bootstrap", Status: identity.RoleActive, RequestedAt: now, GrantedAt: &now}); err != nil {
		t.Fatal(err)
	}
	raw, hash, err := token.New()
	if err != nil {
		t.Fatal(err)
	}
	s := identity.Session{ID: uuid.NewV7(), UserID: id, TokenHash: hash, ClientKind: identity.ClientMobile, CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(30 * time.Minute), AbsoluteExpiresAt: now.Add(12 * time.Hour), MFAVerifiedAt: &now}
	if err := e.mem.InsertSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return raw, id
}
