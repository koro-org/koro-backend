package httpx_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

func scrapeHandler(t *testing.T, m *obs.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, obs.MetricsPath, nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

// TestHTTPX_MetricsNotOnPublicRouter: with metrics and tracing on, the
// public router still answers 404 for /metrics (any method); the registry is
// served only by the obs internal listener.
func TestHTTPX_MetricsNotOnPublicRouter(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	app := newTestApp(t, func(d *httpx.Deps) {
		d.Metrics = m
		d.TracerProvider = sdktrace.NewTracerProvider()
	})
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		for _, path := range []string{"/metrics", "/metrics/", "/api/v1/metrics"} {
			rec := app.do(t, method, path, nil, nil)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, path, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "koro_http") || strings.Contains(rec.Body.String(), "go_goroutines") {
				t.Errorf("%s %s exposes metrics: %s", method, path, rec.Body.String())
			}
		}
	}
	for _, r := range app.Engine.Routes() {
		if strings.Contains(r.Path, "metrics") {
			t.Errorf("public router has route %s %s", r.Method, r.Path)
		}
	}
}

// TestHTTPX_Telemetry_RouteTemplateLabelsAndSpans: metrics and span names use
// the route template; the raw path never becomes a label.
func TestHTTPX_Telemetry_RouteTemplateLabelsAndSpans(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	rec := tracetest.NewSpanRecorder()
	app := newTestApp(t, func(d *httpx.Deps) {
		d.Metrics = m
		d.TracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	})
	registerGET(app.V1, "telItem", "/items/{id}", func(context.Context, *emptyInput) (*okOutput, error) { return ok(), nil })
	app.do(t, http.MethodGet, "/api/v1/items/ayse@ogrenci.edu.tr", nil, nil)
	app.do(t, http.MethodGet, "/healthz", nil, nil)

	out := scrapeHandler(t, m)
	if !strings.Contains(out, `koro_http_requests_total{method="GET",route="/api/v1/items/:id",status="200"} 1`) {
		t.Errorf("route-template counter missing:\n%s", out)
	}
	if strings.Contains(out, "ayse") {
		t.Errorf("metrics leak the raw path:\n%s", out)
	}
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}
	if len(names) != 1 || names[0] != "GET /api/v1/items/:id" {
		t.Fatalf("spans = %v, want one span named by the route template (health probes untraced)", names)
	}
}

// errTestGone is registered once per test binary: problem.New panics on a
// duplicate code, so registering it inside the test broke -count>1 (BE-130).
var errTestGone = problem.New(problem.NotFound, "testonly.gone", "Gone")

// TestHTTPX_AuthzDenial_LoggedAndCountedWithoutPII: a denial is logged once
// with action, module and reason (no personal data) and counted in
// koro_authz_denied_total{module,action,reason} with bounded labels.
func TestHTTPX_AuthzDenial_LoggedAndCountedWithoutPII(t *testing.T) {
	t.Parallel()
	m := obs.NewMetrics()
	app := newTestApp(t, func(d *httpx.Deps) { d.Metrics = m })
	act := authz.HTTPActions()[0]
	errs := map[string]error{
		"forbidden":   authz.ErrForbidden.Wrap(&authz.Denial{Action: act, Reason: "anonymous actor on a non-public action"}),
		"not_visible": authz.ErrNotFound.Wrap(&authz.Denial{Action: act, Reason: "resource not visible (03 §3.6)"}),
		"mfa":         authz.ErrMFARequired.Wrap(&authz.Denial{Action: act, Reason: "mfa"}),
		"four_eyes":   authz.ErrFourEyes.Wrap(&authz.Denial{Action: act, Reason: "four eyes"}),
		"internal":    authz.ErrForbidden.Wrap(&authz.Denial{Action: act, Reason: "unknown resource type", Internal: true}),
		"unknown":     authz.ErrForbidden.Wrap(&authz.Denial{Action: "x.ayse@ogrenci.edu.tr.read", Reason: "unknown action", Internal: true}),
		"rewrapped":   errTestGone.Wrap(authz.ErrNotFound.Wrap(&authz.Denial{Action: act, Reason: "nv"})),
		"plain":       problem.ErrForbidden, // not an authz denial: neither logged nor counted
	}
	for name, err := range errs {
		registerGET(app.V1, "deny"+strings.ReplaceAll(name, "_", ""), "/deny/"+name, func(context.Context, *emptyInput) (*okOutput, error) {
			return nil, err
		})
		app.do(t, http.MethodGet, "/api/v1/deny/"+name, nil, nil)
	}

	out := scrapeHandler(t, m)
	mod := act.Module()
	for _, want := range []string{
		`koro_authz_denied_total{action="` + string(act) + `",module="` + mod + `",reason="forbidden"} 1`,
		`koro_authz_denied_total{action="` + string(act) + `",module="` + mod + `",reason="not_visible"} 2`,
		`koro_authz_denied_total{action="` + string(act) + `",module="` + mod + `",reason="mfa_required"} 1`,
		`koro_authz_denied_total{action="` + string(act) + `",module="` + mod + `",reason="four_eyes"} 1`,
		`koro_authz_denied_total{action="` + string(act) + `",module="` + mod + `",reason="internal"} 1`,
		`koro_authz_denied_total{action="unknown",module="unknown",reason="internal"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(out, "ayse") {
		t.Errorf("unknown action leaked into labels:\n%s", out)
	}

	var denials int
	for line := range strings.SplitSeq(strings.TrimSpace(app.logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != "authz denied" {
			continue
		}
		denials++
		for _, k := range []string{"module", "action", "reason", "rule", "request_id"} {
			if _, ok := rec[k]; !ok {
				t.Errorf("denial log lacks %q: %s", k, line)
			}
		}
		if rec["reason"] == "internal" && rec["level"] != "ERROR" {
			t.Errorf("programmer-error denial must log at ERROR: %s", line)
		}
	}
	if denials != 7 {
		t.Errorf("denial log lines = %d, want 7", denials)
	}
	if strings.Contains(app.logs.String(), "ayse@") {
		t.Errorf("logs leak personal data:\n%s", app.logs.String())
	}
}

// TestHTTPX_TelemetryOff_AddsNoMiddleware: with telemetry env empty
// (Metrics and TracerProvider nil) the chain is exactly the pre-BE-058
// chain; each switch adds one middleware.
func TestHTTPX_TelemetryOff_AddsNoMiddleware(t *testing.T) {
	t.Parallel()
	off := len(newTestApp(t, nil).Engine.Handlers)
	// injectLogger, requestID, accessLog, recoverProblem, securityHeaders,
	// clientIP, crossOrigin, requestTimeout: the chain before BE-058.
	if off != 8 {
		t.Errorf("core chain = %d handlers, want 8", off)
	}
	metrics := len(newTestApp(t, func(d *httpx.Deps) { d.Metrics = obs.NewMetrics() }).Engine.Handlers)
	both := len(newTestApp(t, func(d *httpx.Deps) {
		d.Metrics = obs.NewMetrics()
		d.TracerProvider = sdktrace.NewTracerProvider()
	}).Engine.Handlers)
	if metrics != off+1 || both != off+2 {
		t.Errorf("handlers off/metrics/both = %d/%d/%d", off, metrics, both)
	}
}
