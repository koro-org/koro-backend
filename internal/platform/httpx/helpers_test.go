package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
)

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

type testApp struct {
	*httpx.App
	logs *syncBuffer
}

func newTestApp(t *testing.T, mod func(*httpx.Deps)) *testApp {
	t.Helper()
	logs := &syncBuffer{}
	d := httpx.Deps{
		Log:            obs.NewLogger(logs, obs.LoggerOptions{Level: slog.LevelDebug}),
		TrustedOrigins: []string{"https://app.koro.test"},
	}
	if mod != nil {
		mod(&d)
	}
	app, err := httpx.New(d)
	if err != nil {
		t.Fatalf("httpx.New() error = %v", err)
	}
	return &testApp{App: app, logs: logs}
}

func (a *testApp) do(t *testing.T, method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

// problemBody decodes an application/problem+json response.
func problemBody(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) map[string]any {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json; body = %s", ct, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not JSON: %v; %s", err, rec.Body.String())
	}
	if m["status"] != float64(wantStatus) {
		t.Fatalf("body status = %v, want %d", m["status"], wantStatus)
	}
	code, _ := m["code"].(string)
	if m["type"] != "urn:koro:problem:"+code {
		t.Fatalf("type = %v, want urn:koro:problem:%s", m["type"], code)
	}
	if rid := rec.Header().Get(httpx.RequestIDHeader); rid == "" || m["instance"] != "urn:koro:request:"+rid {
		t.Fatalf("instance = %v, X-Request-ID = %q", m["instance"], rid)
	}
	return m
}

type emptyInput struct{}

type okOutput struct {
	Body struct {
		Status string `json:"status"`
	}
}

func ok() *okOutput {
	o := &okOutput{}
	o.Body.Status = "ok"
	return o
}

// registerGET registers a public GET test operation whose handler is h.
func registerGET(api huma.API, id, path string, h func(context.Context, *emptyInput) (*okOutput, error)) {
	huma.Register(api, op.Koro(huma.Operation{
		OperationID: id, Method: http.MethodGet, Path: path, Tags: []string{"testonly"},
	}, op.Meta{Action: "testonly.thing.read", Surface: op.SurfacePublic}), h)
}
