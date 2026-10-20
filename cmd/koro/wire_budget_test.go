package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// slowLockStore is the identity store on an overloaded database: while
// slow is set, LockUser answers only once the request context has ended
// (a row lock held by another transaction, a saturated pool) and returns
// the context error as pgx does, wrapped like a store error.
type slowLockStore struct {
	identity.Store
	slow atomic.Bool
}

func (s *slowLockStore) LockUser(ctx context.Context, id uuid.UUID) (identity.User, error) {
	if s.slow.Load() {
		<-ctx.Done()
		return identity.User{}, fmt.Errorf("identity: lock user: %w", ctx.Err())
	}
	return s.Store.LockUser(ctx, id)
}

// TestWire_ChangePasswordStorePastRequestBudget_503 is the wire-level
// regression of T-315 (BE-071): BE-070 made the password hasher's
// deadline a 503, but when the request budget ran out in the database
// call after the hash (LockUser in the password transaction), the bare
// context.DeadlineExceeded still became 500 internal_error. httpx now
// answers every request that ran out of time with 503 unavailable after
// the whole budget, logs it once at WARN (no ERROR alarm), nothing is
// written, and the retry with a fresh budget is 204.
func TestWire_ChangePasswordStorePastRequestBudget_503(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	mem := identity.NewMemStore(clock.Real().Now)
	store := &slowLockStore{Store: mem}
	c.identity.store, c.identity.hasher = store, testHasher()
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	e := &actorEnv{c: c, store: mem, clk: c.infra.clock}
	u := e.user(t, nil)
	tok := e.session(t, u, identity.ClientMobile)
	other := e.session(t, u, identity.ClientWeb)
	logs := &lockedBuffer{}
	app, err := httpx.New(httpx.Deps{Log: obs.NewLogger(logs, obs.LoggerOptions{Level: slog.LevelInfo}), Actors: c.actors})
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	do := func(ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
		req.Header.Set("X-Koro-Client", "mobile")
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	change := `{"current_password":"` + matrixReauthPassword + `","new_password":"Yeni-Ve-Uzun-Parola-4821"}`
	// Warm the stack up outside the bubble (lazy initialisation stays out
	// of it).
	if rec := do(t.Context(), http.MethodGet, "/api/v1/me", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /me = %d %s", rec.Code, rec.Body)
	}
	synctest.Test(t, func(t *testing.T) {
		store.slow.Store(true)
		defer store.slow.Store(false)
		start := time.Now()
		rec := do(t.Context(), http.MethodPost, "/api/v1/me/password", change)
		var p struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || rec.Code != http.StatusServiceUnavailable || p.Code != "unavailable" {
			t.Fatalf("POST /me/password with the store past the request budget = %d %s, want 503 unavailable", rec.Code, rec.Body)
		}
		if waited := time.Since(start); waited != httpx.DefaultRequestTimeout {
			t.Fatalf("answered after %v, want the whole budget %v", waited, httpx.DefaultRequestTimeout)
		}
	})
	out := logs.String()
	if !strings.Contains(out, `"level":"WARN","msg":"request budget exhausted"`) || strings.Contains(out, `"level":"ERROR"`) {
		t.Fatalf("the exhausted budget must log once at WARN, never at ERROR; logs = %s", out)
	}
	cur, err := e.store.User(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.PasswordHash != u.PasswordHash || cur.FailedLogins != 0 || cur.LockedUntil != nil {
		t.Fatalf("the failed request wrote: hash changed %v, failed_logins %d", cur.PasswordHash != u.PasswordHash, cur.FailedLogins)
	}
	if _, err := e.resolve(t.Context(), httpx.TransportCookie, other); err != nil {
		t.Fatalf("the other session was closed by a failed request: %v", err)
	}
	if rec := do(t.Context(), http.MethodPost, "/api/v1/me/password", change); rec.Code != http.StatusNoContent {
		t.Fatalf("retry = %d %s, want 204", rec.Code, rec.Body)
	}
}
