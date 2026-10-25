package ratelimit_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
	"koro/internal/platform/ratelimit"
)

// Synthetic test values, not keys.
const (
	webKey    = "test-web-key-0123456789abcdefghijklmnopqrstuvwxyz"
	webKeyOld = "test-web-key-old-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	ipBucket  = "global_ip|ip:192.0.2.1" // httptest's peer address
	webCalls  = "web_internal|web"
)

// seen is what a public handler observed of the request.
type seen struct {
	Body struct {
		SawKey bool `json:"saw_key"`
		User   bool `json:"user"`
	}
}

// newWebApp is the API chain of `koro api` with koro-web's keys: GlobalIP
// (gin) and the class middleware (huma) on one limiter, and four
// operations: a public read (read_default), a public search, a public
// strict read (token_confirm) and a protected read.
func newWebApp(t *testing.T, l ratelimit.Limiter, log *slog.Logger, m *obs.Metrics, keys ...string) http.Handler {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: log, Metrics: m, Actors: userResolver{id: uuid.NewV7()},
		Middleware: []gin.HandlerFunc{ratelimit.GlobalIP(l, log, "urn:koro:problem:", ratelimit.NewWebKeys(keys))}})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(ratelimit.Middleware(app.API, l, log))
	type in struct {
		Slug string `path:"slug"`
		Key  string `header:"X-Koro-Web-Key"`
	}
	look := func(ctx context.Context, i *in) (*seen, error) {
		out := &seen{}
		out.Body.SawKey, out.Body.User = i.Key != "", authz.ActorOrAnonymous(ctx).IsUser()
		return out, nil
	}
	reg := func(id, path string, m op.Meta) {
		huma.Register(app.V1, op.Koro(huma.Operation{OperationID: id, Method: http.MethodGet, Path: path, Tags: []string{"directory"}}, m), look)
	}
	reg("getClub", "/clubs/{slug}", op.Meta{Action: "directory.club.read", Surface: op.SurfacePublic})
	reg("searchOrgs", "/orgs/{slug}", op.Meta{Action: "directory.org.search", Surface: op.SurfacePublic, RateLimit: op.RateSearch})
	reg("confirmThing", "/confirm/{slug}", op.Meta{Action: "directory.thing.confirm", Surface: op.SurfacePublic, RateLimit: op.RateTokenConfirm})
	reg("listMyThings", "/me/{slug}", op.Meta{Action: "directory.thing.list", Surface: op.SurfaceSelf})
	return app.Handler()
}

// anon sends a request without any credential (do adds a bearer token).
func anon(t *testing.T, h http.Handler, method, path string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func withKey(k string, kv ...string) http.Header {
	h := http.Header{ratelimit.WebKeyHeader: {k}}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func observed(t *testing.T, rec *httptest.ResponseRecorder) seen {
	t.Helper()
	var s seen
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &s.Body) != nil {
		t.Fatalf("public read = %d %s", rec.Code, rec.Body)
	}
	return s
}

// TestRateLimit_WebKey_OwnBucket (BE-119): koro-web's cookie-free call with
// a valid key is counted once, in web_internal (3000/min, key "web"),
// instead of global_ip and the anonymous per-IP classes; the two buckets
// are independent both ways; a refusal of the web bucket is a WARN line and
// a koro_ratelimit_rejected_total sample; the key never reaches the
// handler, the logs or the metrics.
func TestRateLimit_WebKey_OwnBucket(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC))
	mem := ratelimit.NewMem(clk)
	logs := &syncBuffer{}
	log := obs.NewLogger(logs, obs.LoggerOptions{Level: slog.LevelDebug})
	m := obs.NewMetrics()
	h := newWebApp(t, mem, log, m, webKey)

	if s := observed(t, anon(t, h, http.MethodGet, "/api/v1/clubs/kulup", withKey(webKey))); s.Body.SawKey || s.Body.User {
		t.Fatalf("handler saw key=%v user=%v; want neither", s.Body.SawKey, s.Body.User)
	}
	observed(t, anon(t, h, http.MethodGet, "/api/v1/orgs/kulup", withKey(webKey)))
	if rec := anon(t, h, http.MethodHead, "/api/v1/nothing", withKey(webKey)); rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD = %d", rec.Code)
	}
	observed(t, anon(t, h, http.MethodGet, "/api/v1/clubs/kulup", nil))
	want := []string{
		webCalls,                    // read_default skipped: counted once, by GlobalIP
		webCalls,                    // search skipped too
		webCalls,                    // HEAD is a safe method
		ipBucket,                    // without the key: koro-web's IP…
		"read_default|ip:192.0.2.1", // …and the class by IP, as before
	}
	if got := mem.Calls(); !slices.Equal(got, want) {
		t.Fatalf("limiter calls:\n got  %q\n want %q", got, want)
	}

	// The IP bucket fills up (300/min); koro-web's calls still pass.
	for range 299 {
		if rec := anon(t, h, http.MethodGet, "/api/v1/nothing", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("within global_ip: %d", rec.Code)
		}
	}
	if rec := anon(t, h, http.MethodGet, "/api/v1/nothing", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over global_ip: %d", rec.Code)
	}
	observed(t, anon(t, h, http.MethodGet, "/api/v1/clubs/kulup", withKey(webKey)))

	// The web bucket allows 3000 per minute (4 used so far), then 429.
	for i := 5; i <= 3000; i++ {
		if rec := anon(t, h, http.MethodGet, "/api/v1/nothing", withKey(webKey)); rec.Code != http.StatusNotFound {
			t.Fatalf("web call %d: %d", i, rec.Code)
		}
	}
	rec := anon(t, h, http.MethodGet, "/api/v1/clubs/kulup", withKey(webKey))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" ||
		!strings.Contains(rec.Body.String(), `"code":"rate_limited"`) {
		t.Fatalf("over web_internal: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	clk.Advance(time.Minute)
	observed(t, anon(t, h, http.MethodGet, "/api/v1/clubs/kulup", withKey(webKey)))
	observed(t, anon(t, h, http.MethodGet, "/api/v1/clubs/kulup", nil))

	var refusals []string
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == "rate limited" {
			refusals = append(refusals, r["level"].(string)+" "+r["rule"].(string)+" "+r["action"].(string))
		}
	}
	if want := []string{"WARN web_internal unknown"}; !slices.Equal(refusals, want) {
		t.Fatalf("rate limited logs %q, want %q", refusals, want)
	}
	mrec := httptest.NewRecorder()
	m.Handler().ServeHTTP(mrec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, obs.MetricsPath, nil))
	metrics, _ := io.ReadAll(mrec.Body)
	if !strings.Contains(string(metrics), `koro_ratelimit_rejected_total{action="unknown",rule="web_internal"} 1`) {
		t.Errorf("metrics lack the web_internal refusal")
	}
	if strings.Contains(logs.String(), webKey) || strings.Contains(string(metrics), webKey) {
		t.Fatal("the key reached the logs or the metrics")
	}
}

// TestRateLimit_WebKey_IgnoredOutsideItsScope (BE-119): without a valid
// key, or with one on a request that is not a cookie-free direct read
// (credential, forwarding header, unsafe method, repeated or oversized
// header), or while no key is configured, the request counts against the
// IP bucket as before; the header is removed from every request.
func TestRateLimit_WebKey_IgnoredOutsideItsScope(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		hdr    http.Header
		keys   []string
	}{
		{"no key", http.MethodGet, nil, []string{webKey}},
		{"wrong key", http.MethodGet, withKey(strings.ToUpper(webKey)), []string{webKey}},
		{"key prefix", http.MethodGet, withKey(webKey[:40]), []string{webKey}},
		{"oversized key", http.MethodGet, withKey(strings.Repeat(webKey, 6)), []string{webKey}},
		{"repeated header", http.MethodGet, http.Header{ratelimit.WebKeyHeader: {webKey, webKey}}, []string{webKey}},
		{"through a proxy (X-Forwarded-For)", http.MethodGet, withKey(webKey, "X-Forwarded-For", "203.0.113.9"), []string{webKey}},
		{"X-Real-IP", http.MethodGet, withKey(webKey, "X-Real-IP", "203.0.113.9"), []string{webKey}},
		{"Forwarded", http.MethodGet, withKey(webKey, "Forwarded", "for=203.0.113.9"), []string{webKey}},
		{"session cookie", http.MethodGet, withKey(webKey, "Cookie", httpx.SessionCookieName+"=abc"), []string{webKey}},
		{"other cookie", http.MethodGet, withKey(webKey, "Cookie", "theme=dark"), []string{webKey}},
		{"bearer", http.MethodGet, withKey(webKey, "Authorization", "Bearer persona"), []string{webKey}},
		{"unsafe method", http.MethodPost, withKey(webKey), []string{webKey}},
		{"no keys configured", http.MethodGet, withKey(webKey), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mem := ratelimit.NewMem(clock.Real())
			h := newWebApp(t, mem, obs.Discard(), nil, c.keys...)
			rec := anon(t, h, c.method, "/api/v1/clubs/kulup", c.hdr)
			if got := mem.Calls(); len(got) == 0 || got[0] != ipBucket || slices.Contains(got, webCalls) {
				t.Fatalf("limiter calls %q, want %q first and no %q", got, ipBucket, webCalls)
			}
			if c.method == http.MethodGet {
				if s := observed(t, rec); s.Body.SawKey {
					t.Fatal("the handler saw the key header")
				}
			}
		})
	}
}

// TestRateLimit_WebKey_RotationAcceptsBoth (BE-119): during a rotation
// both configured keys are accepted; any other value is not.
func TestRateLimit_WebKey_RotationAcceptsBoth(t *testing.T) {
	t.Parallel()
	mem := ratelimit.NewMem(clock.Real())
	h := newWebApp(t, mem, obs.Discard(), nil, webKey, webKeyOld)
	for _, k := range []string{webKey, webKeyOld, webKey + "x"} {
		observed(t, anon(t, h, http.MethodGet, "/api/v1/clubs/kulup", withKey(k)))
	}
	want := []string{webCalls, webCalls, ipBucket, "read_default|ip:192.0.2.1"}
	if got := mem.Calls(); !slices.Equal(got, want) {
		t.Fatalf("limiter calls:\n got  %q\n want %q", got, want)
	}
}

// TestRateLimit_WebKey_StrictClassKeepsIPBucket (BE-119): a Strict class
// (sign-in, tokens, reauth, export, break-glass) is never relaxed by the
// key: it still counts by IP after the web bucket.
func TestRateLimit_WebKey_StrictClassKeepsIPBucket(t *testing.T) {
	t.Parallel()
	mem := ratelimit.NewMem(clock.Real())
	h := newWebApp(t, mem, obs.Discard(), nil, webKey)
	observed(t, anon(t, h, http.MethodGet, "/api/v1/confirm/x", withKey(webKey)))
	if want, got := []string{webCalls, "token_confirm|ip:192.0.2.1"}, mem.Calls(); !slices.Equal(got, want) {
		t.Fatalf("limiter calls %q, want %q", got, want)
	}
}

// TestRateLimit_WebKey_GrantsNoIdentity (BE-119): the key is not a
// credential. A protected operation answers 401 to a request that carries
// only the key, and a public one runs anonymously (OwnBucket); the
// authorization matrix never sees the header (GlobalIP removes it).
func TestRateLimit_WebKey_GrantsNoIdentity(t *testing.T) {
	t.Parallel()
	mem := ratelimit.NewMem(clock.Real())
	h := newWebApp(t, mem, obs.Discard(), nil, webKey)
	rec := anon(t, h, http.MethodGet, "/api/v1/me/x", withKey(webKey))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"unauthenticated"`) {
		t.Fatalf("protected read with the key only = %d %s, want 401", rec.Code, rec.Body)
	}
	// The same request with a credential is the actor's, counted by user
	// (not a web call, not the web bucket).
	rec = anon(t, h, http.MethodGet, "/api/v1/me/x", withKey(webKey, "Authorization", "Bearer persona"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"user":true`) {
		t.Fatalf("protected read with a credential = %d %s", rec.Code, rec.Body)
	}
	got := mem.Calls()
	if len(got) != 3 || got[0] != webCalls || got[1] != ipBucket || !strings.HasPrefix(got[2], "read_default|user:") {
		t.Fatalf("limiter calls %q", got)
	}
}

// TestRateLimit_WebRuleIsNotAnOperationClass (BE-119): web_internal is a
// caller bucket applied by GlobalIP only: not one of the 18 classes of 04
// §1.9, fail open, larger than global_ip.
func TestRateLimit_WebRuleIsNotAnOperationClass(t *testing.T) {
	t.Parallel()
	r := ratelimit.WebRule()
	global := ratelimit.Rules()[op.RateGlobalIP]
	if r.Name != "web_internal" || r.Key != ratelimit.KeyWeb || r.Strict || r.Per != time.Minute || r.Rate != 3000 || r.Rate <= global.Rate {
		t.Fatalf("WebRule() = %+v", r)
	}
	if slices.Contains(op.RateLimits(), op.RateLimit(r.Name)) {
		t.Fatal("web_internal is an operation class")
	}
	for _, rule := range ratelimit.Rules() {
		if rule.Name == r.Name || rule.Key == ratelimit.KeyWeb {
			t.Fatalf("Rules() holds the web bucket: %+v", rule)
		}
	}
}
