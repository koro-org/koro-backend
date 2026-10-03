package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
)

func TestHTTP_NoRoute_ReturnsProblemJSON(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)

	m := problemBody(t, app.do(t, http.MethodGet, "/api/v1/does-not-exist", nil, nil), http.StatusNotFound)
	if m["code"] != "not_found" || m["title"] != "Not found" {
		t.Fatalf("unexpected 404 problem %v", m)
	}
	m = problemBody(t, app.do(t, http.MethodPost, "/healthz", nil, nil), http.StatusMethodNotAllowed)
	if m["code"] != "method_not_allowed" {
		t.Fatalf("unexpected 405 problem %v", m)
	}
}

func TestHTTP_RequestID_EchoedInResponse(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)

	rec := app.do(t, http.MethodGet, "/healthz", nil, map[string]string{httpx.RequestIDHeader: "req_ABC-12345678"})
	if got := rec.Header().Get(httpx.RequestIDHeader); got != "req_ABC-12345678" {
		t.Fatalf("valid incoming request ID not echoed: %q", got)
	}

	for _, in := range []string{"", "short", "bad id with spaces", strings.Repeat("a", 65)} {
		rec := app.do(t, http.MethodGet, "/healthz", nil, map[string]string{httpx.RequestIDHeader: in})
		got := rec.Header().Get(httpx.RequestIDHeader)
		if got == in {
			t.Fatalf("invalid request ID %q was echoed", in)
		}
		if _, err := uuid.Parse(got); err != nil {
			t.Fatalf("generated request ID %q is not a UUID: %v", got, err)
		}
	}

	rec = app.do(t, http.MethodGet, "/api/v1/nowhere", nil, map[string]string{httpx.RequestIDHeader: "trace-0001-abcd"})
	m := problemBody(t, rec, http.StatusNotFound)
	if m["instance"] != "urn:koro:request:trace-0001-abcd" {
		t.Fatalf("problem instance = %v", m["instance"])
	}
}

type uploadInput struct {
	Body struct {
		Data string `json:"data"`
	}
}

func TestHTTP_BodyOver1MB_Returns413Problem(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)
	huma.Register(app.V1, op.Koro(huma.Operation{
		OperationID: "createTestBlob", Method: http.MethodPost, Path: "/blobs", Tags: []string{"testonly"},
	}, op.Meta{Action: "testonly.blob.create", Surface: op.SurfacePublic, Idempotency: op.IdempotencyOptional}),
		func(context.Context, *uploadInput) (*okOutput, error) { return ok(), nil })

	big := `{"data":"` + strings.Repeat("x", 1<<20) + `"}`
	m := problemBody(t, app.do(t, http.MethodPost, "/api/v1/blobs", strings.NewReader(big), nil), http.StatusRequestEntityTooLarge)
	if m["code"] != "payload_too_large" {
		t.Fatalf("code = %v", m["code"])
	}

	small := `{"data":"` + strings.Repeat("x", 1000) + `"}`
	if rec := app.do(t, http.MethodPost, "/api/v1/blobs", strings.NewReader(small), nil); rec.Code != http.StatusOK {
		t.Fatalf("small body status = %d; %s", rec.Code, rec.Body.String())
	}
}

func TestHealthz_Returns200(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)
	rec := app.do(t, http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["status"] != "ok" {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	for path := range app.API.OpenAPI().Paths {
		if strings.Contains(path, "healthz") || strings.Contains(path, "readyz") {
			t.Fatalf("health route %s must stay outside OpenAPI", path)
		}
	}
}

func TestReadyz_FlagAndChecks(t *testing.T) {
	t.Parallel()
	failing := errors.New("db 10.0.0.5 unreachable")
	var fail bool
	ready := httpx.NewReady(httpx.Check{Name: "db", Run: func(context.Context) error {
		if fail {
			return failing
		}
		return nil
	}})
	app := newTestApp(t, func(d *httpx.Deps) { d.Ready = ready })

	if rec := app.do(t, http.MethodGet, "/readyz", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-yet-ready status = %d", rec.Code)
	}
	ready.Set(true)
	if rec := app.do(t, http.MethodGet, "/readyz", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ready"`) {
		t.Fatalf("ready status = %d %s", rec.Code, rec.Body.String())
	}
	fail = true
	rec := app.do(t, http.MethodGet, "/readyz", nil, nil)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "db") {
		t.Fatalf("failing check: status = %d body = %s (must not name the dependency)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(app.logs.String(), "readiness check failed") {
		t.Fatalf("failing check not logged: %s", app.logs.String())
	}
}

func TestHTTP_SecurityHeaders(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)
	rec := app.do(t, http.MethodGet, "/api/v1/missing", nil, nil)
	want := map[string]string{
		"Strict-Transport-Security": "max-age=63072000; includeSubDomains",
		"X-Content-Type-Options":    "nosniff",
		"Content-Security-Policy":   "default-src 'none'; frame-ancestors 'none'",
		"Referrer-Policy":           "no-referrer",
		"Cache-Control":             "no-store",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestHTTP_Panic_Returns500Problem(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)
	registerGET(app.V1, "getPanic", "/panic", func(context.Context, *emptyInput) (*okOutput, error) {
		panic("boom: secret@example.test")
	})
	rec := app.do(t, http.MethodGet, "/api/v1/panic", nil, nil)
	m := problemBody(t, rec, http.StatusInternalServerError)
	if m["code"] != "internal_error" || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("unexpected panic response %s", rec.Body.String())
	}
	logs := app.logs.String()
	if !strings.Contains(logs, "panic recovered") || strings.Contains(logs, "secret@example.test") {
		t.Fatalf("panic must be logged with PII scrubbed: %s", logs)
	}
}

func TestHTTP_CrossOriginProtection(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)
	post := func(path string, hdr map[string]string) int {
		huma.Register(app.V1, op.Koro(huma.Operation{
			OperationID: "createCSRF" + strings.ReplaceAll(strings.Trim(path, "/"), "/", ""), Method: http.MethodPost,
			Path: path, Tags: []string{"testonly"},
		}, op.Meta{Action: "testonly.csrf.create", Surface: op.SurfacePublic, Idempotency: op.IdempotencyOptional}),
			func(context.Context, *emptyInput) (*okOutput, error) { return ok(), nil })
		return app.do(t, http.MethodPost, "/api/v1"+path, nil, hdr).Code
	}
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}

	rec := func() int { return app.do(t, http.MethodPost, "/api/v1/csrf", nil, crossSite).Code }
	if got := post("/csrf", nil); got != http.StatusOK {
		t.Fatalf("header-less POST (mobile/curl) = %d, want 200", got)
	}
	if got := rec(); got != http.StatusForbidden {
		t.Fatalf("cross-site POST = %d, want 403", got)
	}
	m := problemBody(t, app.do(t, http.MethodPost, "/api/v1/csrf", nil, crossSite), http.StatusForbidden)
	if m["code"] != "csrf_rejected" {
		t.Fatalf("code = %v", m["code"])
	}
	trusted := map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://app.koro.test"}
	if got := app.do(t, http.MethodPost, "/api/v1/csrf", nil, trusted).Code; got != http.StatusOK {
		t.Fatalf("trusted-origin POST = %d, want 200", got)
	}
	if got := post("/webhooks/test", crossSite); got != http.StatusOK {
		t.Fatalf("webhook POST = %d, want 200 (bypass)", got)
	}
}

func TestHTTP_OpenAPIExposure(t *testing.T) {
	t.Parallel()
	hidden := newTestApp(t, nil)
	if rec := hidden.do(t, http.MethodGet, "/api/v1/openapi.json", nil, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("spec must be hidden by default, status = %d", rec.Code)
	}
	exposed := newTestApp(t, func(d *httpx.Deps) { d.ExposeOpenAPI = true })
	rec := exposed.do(t, http.MethodGet, "/api/v1/openapi.json", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"openapi":"3.1.0"`) {
		t.Fatalf("exposed spec status = %d body prefix = %.80s", rec.Code, rec.Body.String())
	}
	if _, ok := exposed.API.OpenAPI().Components.Schemas.Map()["Problem"]; !ok {
		t.Fatal("Problem schema is missing from components")
	}
}
