package ratelimit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
	"koro/internal/platform/ratelimit"
)

func TestRateLimit_RulesCoverEveryClass(t *testing.T) {
	t.Parallel()
	rules := ratelimit.Rules()
	strict := []string{}
	for _, c := range op.RateLimits() {
		r, ok := rules[c]
		if !ok || r.Name != string(c) || r.Rate <= 0 || r.Per <= 0 || r.Key == "" {
			t.Errorf("class %s: rule %+v", c, r)
		}
		if r.Strict {
			strict = append(strict, r.Name)
		}
	}
	if len(rules) != len(op.RateLimits()) {
		t.Errorf("%d rules for %d classes", len(rules), len(op.RateLimits()))
	}
	// 04 §1.9 "Strict (Redis yoksa 503)" column.
	want := []string{"auth_login", "auth_register", "auth_password_reset", "auth_email", "token_confirm", "reauth", "export", "break_glass"}
	slices.Sort(strict)
	slices.Sort(want)
	if !slices.Equal(strict, want) {
		t.Fatalf("strict classes %v, want %v", strict, want)
	}
	if r := rules[op.RateAuthLogin]; r.Rate != 10 || r.Per != time.Minute || r.Key != ratelimit.KeyIP {
		t.Fatalf("auth_login = %+v", r)
	}
	if r := rules[op.RateReauth]; r.Rate != 5 || r.Per != 15*time.Minute || r.Key != ratelimit.KeyActor {
		t.Fatalf("reauth = %+v", r)
	}
}

type userResolver struct{ id uuid.UUID }

func (r userResolver) Resolve(ctx context.Context, _ httpx.Credential) (context.Context, error) {
	return authz.WithActor(ctx, authz.Actor{Kind: authz.KindUser, UserID: r.id}), nil
}

type empty struct{}

// newApp registers four operations with different classes behind the
// middleware.
func newApp(t *testing.T, l ratelimit.Limiter, user uuid.UUID) http.Handler {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: userResolver{id: user}})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(ratelimit.Middleware(app.API, l, obs.Discard()))
	h := func(context.Context, *empty) (*empty, error) { return &empty{}, nil }
	type orgIn struct {
		OrgID string `path:"org_id"`
	}
	reg := func(id, method, path string, m op.Meta) {
		huma.Register(app.V1, op.Koro(huma.Operation{OperationID: id, Method: method, Path: path, Tags: []string{"identity"}}, m), h)
	}
	reg("createSession", http.MethodPost, "/auth/sessions", op.Meta{Action: "identity.session.create", Surface: op.SurfacePublic,
		RateLimit: op.RateAuthLogin, Idempotency: op.IdempotencyOptional})
	reg("listThings", http.MethodGet, "/me/things", op.Meta{Action: "identity.thing.list", Surface: op.SurfaceSelf})
	reg("adminReadThing", http.MethodGet, "/admin/things", op.Meta{Action: "identity.thing.read", Surface: op.SurfaceAdmin, BreakGlass: true})
	huma.Register(app.V1, op.Koro(huma.Operation{OperationID: "countAudience", Method: http.MethodPost, Path: "/orgs/{org_id}/audience/count",
		Tags: []string{"outreach"}}, op.Meta{Action: "outreach.audience.count", Surface: op.SurfaceOrg, RateLimit: op.RateAudienceCount,
		Idempotency: op.IdempotencyOptional}),
		func(context.Context, *orgIn) (*empty, error) { return &empty{}, nil })
	return app.Handler()
}

func do(t *testing.T, h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	req.Header.Set("Authorization", "Bearer persona")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRateLimit_ClassFromExtension(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	mem := ratelimit.NewMem(clk)
	user := uuid.NewV7()
	h := newApp(t, mem, user)
	org := uuid.NewV7()

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/auth/sessions"},
		{http.MethodGet, "/api/v1/me/things"},
		{http.MethodPost, "/api/v1/orgs/" + org.String() + "/audience/count"},
	} {
		if rec := do(t, h, c.method, c.path, nil); rec.Code >= 300 {
			t.Fatalf("%s %s = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/admin/things", map[string]string{ratelimit.AccessReasonHeader: "ticket 42"}); rec.Code >= 300 {
		t.Fatalf("break glass = %d", rec.Code)
	}
	want := []string{
		"auth_login|ip:192.0.2.1",            // x-koro-rate-limit: auth_login, keyed by IP
		"read_default|user:" + user.String(), // GET without a class → read_default, keyed by actor
		"audience_count|org:" + org.String(), // keyed by the org path parameter
		"read_default|user:" + user.String(), // the B operation's own class…
		"break_glass|user:" + user.String(),  // …plus break_glass when the reason header is sent
	}
	if got := mem.Calls(); !slices.Equal(got, want) {
		t.Fatalf("limiter calls:\n got  %q\n want %q", got, want)
	}

	// auth_login allows 10 per minute per IP; the 11th is 429 with Retry-After.
	for range 9 {
		if rec := do(t, h, http.MethodPost, "/api/v1/auth/sessions", nil); rec.Code >= 300 {
			t.Fatalf("within limit: %d", rec.Code)
		}
	}
	clk.Advance(20 * time.Second)
	rec := do(t, h, http.MethodPost, "/api/v1/auth/sessions", nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "40" ||
		!strings.Contains(rec.Body.String(), `"code":"rate_limited"`) {
		t.Fatalf("over limit: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	clk.Advance(41 * time.Second)
	if rec := do(t, h, http.MethodPost, "/api/v1/auth/sessions", nil); rec.Code >= 300 {
		t.Fatalf("next window: %d", rec.Code)
	}
}

func TestRateLimit_StrictWithoutRedis_503(t *testing.T) {
	t.Parallel()
	// A Redis that is not there: every Allow fails.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	h := newApp(t, ratelimit.NewRedis(rdb), uuid.NewV7())

	strict := do(t, h, http.MethodPost, "/api/v1/auth/sessions", nil) // auth_login: Strict
	if strict.Code != http.StatusServiceUnavailable || !strings.Contains(strict.Body.String(), `"code":"unavailable"`) {
		t.Fatalf("strict class without Redis = %d %s, want 503 unavailable", strict.Code, strict.Body)
	}
	open := do(t, h, http.MethodGet, "/api/v1/me/things", nil) // read_default: fail open
	if open.Code >= 300 {
		t.Fatalf("non-strict class without Redis = %d %s, want success (fail open)", open.Code, open.Body)
	}

	// The same through the in-memory fake.
	mem := ratelimit.NewMem(clock.Real())
	mem.Fail(ratelimit.ErrUnavailable)
	h = newApp(t, mem, uuid.NewV7())
	if rec := do(t, h, http.MethodGet, "/api/v1/admin/things", map[string]string{ratelimit.AccessReasonHeader: "x"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("break_glass (strict) with a failing limiter = %d", rec.Code)
	}
}

func TestRateLimit_GlobalIP(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	mem := ratelimit.NewMem(clk)
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Middleware: []gin.HandlerFunc{ratelimit.GlobalIP(mem, obs.Discard(), "urn:koro:problem:", ratelimit.WebKeys{})}})
	if err != nil {
		t.Fatal(err)
	}
	h := app.Handler()
	for range 300 {
		if rec := do(t, h, http.MethodGet, "/api/v1/nothing", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("within limit: %d", rec.Code)
		}
	}
	rec := do(t, h, http.MethodGet, "/api/v1/nothing", map[string]string{"X-Request-ID": "req-abcdefgh"})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), `"instance":"urn:koro:request:req-abcdefgh"`) {
		t.Fatalf("over the global limit: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Fatalf("health probe limited: %d", rec.Code)
	}
}
