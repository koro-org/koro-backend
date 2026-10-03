package obs_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
)

// fakeCollector is an OTLP/HTTP trace receiver.
type fakeCollector struct {
	mu     sync.Mutex
	bodies [][]byte
	paths  []string
}

func (f *fakeCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, b)
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

type userInput struct {
	ID string `path:"id"`
}

type userOutput struct {
	Body struct {
		Status string `json:"status"`
	}
}

// Synthetic test vectors only (05 §13.7).
const (
	leakEmail = "ayse.yilmaz@ogrenci.edu.tr"
	leakIP    = "203.0.113.77"
)

// TestTelemetry_Enabled_ExportsSpansAndMetricsWithoutPII runs the real
// exporters: an OTLP/HTTP collector receives the HTTP span (named by the
// route template, without raw path, query, client IP or user agent) and
// the internal listener serves koro_* metrics labelled by route template,
// including koro_authz_denied_total. The public router has no /metrics.
func TestTelemetry_Enabled_ExportsSpansAndMetricsWithoutPII(t *testing.T) {
	t.Parallel()
	col := &fakeCollector{}
	srv := httptest.NewServer(col)
	defer srv.Close()

	tel, err := obs.SetupTelemetry(t.Context(), obs.TelemetryOptions{
		OTLPEndpoint: srv.URL, MetricsAddr: "127.0.0.1:0", SampleRatio: 1,
		Service: "koro-api", Version: "test", Env: "dev",
	})
	if err != nil {
		t.Fatalf("SetupTelemetry() error = %v", err)
	}
	if !tel.Tracing() || tel.Metrics == nil || tel.MetricsAddr() == "" {
		t.Fatalf("telemetry not enabled: tracing=%v metrics=%v addr=%q", tel.Tracing(), tel.Metrics, tel.MetricsAddr())
	}

	app, err := httpx.New(httpx.Deps{
		Log: obs.Discard(), Metrics: tel.Metrics, TracerProvider: tel.TracerProvider(), Service: "koro-api",
	})
	if err != nil {
		t.Fatalf("httpx.New() error = %v", err)
	}
	denied := authz.HTTPActions()[0]
	huma.Register(app.V1, op.Koro(huma.Operation{
		OperationID: "testUser", Method: http.MethodGet, Path: "/users/{id}", Tags: []string{"testonly"},
	}, op.Meta{Action: "testonly.thing.read", Surface: op.SurfacePublic}),
		func(_ context.Context, in *userInput) (*userOutput, error) {
			if in.ID == leakEmail {
				return nil, authz.ErrForbidden.Wrap(&authz.Denial{Action: denied, Reason: "test denial"})
			}
			o := &userOutput{}
			o.Body.Status = "ok"
			return o, nil
		})

	for _, id := range []string{leakEmail, "0192f5c4-1d2e-7a3b-9c4d-5e6f708192a3"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/users/"+id+"?email="+leakEmail, nil)
		req.Header.Set("User-Agent", "probe "+leakEmail)
		req.RemoteAddr = leakIP + ":5555"
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if id == leakEmail && rec.Code != http.StatusForbidden {
			t.Fatalf("denied request status = %d", rec.Code)
		}
	}

	// /metrics is not on the public router.
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, obs.MetricsPath, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("public GET /metrics = %d, want 404", rec.Code)
	}

	// The internal listener serves the registry.
	metrics := scrape(t, "http://"+tel.MetricsAddr()+obs.MetricsPath)
	for _, want := range []string{
		`koro_http_requests_total{method="GET",route="/api/v1/users/:id",status="403"} 1`,
		`koro_http_requests_total{method="GET",route="/api/v1/users/:id",status="200"} 1`,
		`koro_http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`koro_http_request_duration_seconds_count{method="GET",route="/api/v1/users/:id"} 2`,
		`koro_http_in_flight_requests 0`,
		`koro_authz_denied_total{action="` + string(denied) + `",module="` + denied.Module() + `",reason="forbidden"} 1`,
		`go_goroutines`,
		`process_`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	for _, leak := range []string{"ayse", "ogrenci", leakIP, "0192f5c4"} {
		if strings.Contains(metrics, leak) {
			t.Errorf("metrics leak %q", leak)
		}
	}

	// Shutdown flushes the batch to the collector and closes the listener.
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.bodies) == 0 {
		t.Fatal("collector received no spans")
	}
	var names []string
	for i, b := range col.bodies {
		if col.paths[i] != "/v1/traces" {
			t.Errorf("export path = %q, want /v1/traces", col.paths[i])
		}
		for _, leak := range []string{"ayse", "ogrenci", leakIP, "probe"} {
			if bytes.Contains(b, []byte(leak)) {
				t.Errorf("exported spans leak %q", leak)
			}
		}
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			t.Fatalf("collector body is not OTLP: %v", err)
		}
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					names = append(names, s.GetName())
					keys := map[string]string{}
					for _, kv := range s.GetAttributes() {
						keys[kv.GetKey()] = kv.GetValue().GetStringValue()
					}
					for _, k := range []string{"url.path", "url.query", "client.address", "network.peer.address", "user_agent.original"} {
						if _, ok := keys[k]; ok {
							t.Errorf("span %q carries %s", s.GetName(), k)
						}
					}
					if s.GetName() == "GET /api/v1/users/:id" && keys["http.route"] != "/api/v1/users/:id" {
						t.Errorf("http.route = %q, want the template", keys["http.route"])
					}
				}
			}
		}
	}
	if !strings.Contains(strings.Join(names, ","), "GET /api/v1/users/:id") {
		t.Fatalf("span names = %v, want the route template", names)
	}
	// The listener is closed after Shutdown.
	client := &http.Client{}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+tel.MetricsAddr()+obs.MetricsPath, nil)
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("metrics listener still serving after Shutdown")
	}
}

func scrape(t *testing.T, url string) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, b)
	}
	return string(b)
}

// TestObs_Logger_AddsTraceIDs: a log record in a span's context carries
// trace_id and span_id (05 §11.1); without a span nothing is added.
func TestObs_Logger_AddsTraceIDs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := obs.NewLogger(&buf, obs.LoggerOptions{})
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4, 5, 6}, TraceFlags: trace.FlagsSampled,
	})
	log.InfoContext(trace.ContextWithSpanContext(t.Context(), sc), "with span")
	log.InfoContext(t.Context(), "without span")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %d", len(lines))
	}
	if !strings.Contains(lines[0], `"trace_id":"`+sc.TraceID().String()+`"`) || !strings.Contains(lines[0], `"span_id":"`+sc.SpanID().String()+`"`) {
		t.Errorf("span log line = %s", lines[0])
	}
	if strings.Contains(lines[1], "trace_id") {
		t.Errorf("log line without span carries a trace_id: %s", lines[1])
	}
}
