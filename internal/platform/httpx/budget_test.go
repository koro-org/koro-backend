package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
)

// T-315 / BE-071: a request that runs out of time is answered 503
// unavailable at the single conversion point (toProblem), like Go's
// http.TimeoutHandler, instead of a 500 internal_error alarm. These tests
// drive the real httpx stack (requestTimeout, huma, the error factory,
// the logger).

// logLine is one JSON log record.
type logLine struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Err       string `json:"err"`
	RequestID string `json:"request_id"`
}

// failureLogs returns the records an error response logged (the access
// log line is left out).
func failureLogs(t *testing.T, app *testApp) []logLine {
	t.Helper()
	var out []logLine
	for raw := range strings.SplitSeq(strings.TrimSpace(app.logs.String()), "\n") {
		if raw == "" {
			continue
		}
		var l logLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("log line is not JSON: %v; %s", err, raw)
		}
		if l.Msg != "http request" {
			out = append(out, l)
		}
	}
	return out
}

// wantOneLog asserts exactly one failure record with level and msg, bound
// to the request ID of rec.
func wantOneLog(t *testing.T, app *testApp, rec *httptest.ResponseRecorder, level, msg string) logLine {
	t.Helper()
	logs := failureLogs(t, app)
	if len(logs) != 1 {
		t.Fatalf("%d failure log records, want 1: %+v", len(logs), logs)
	}
	l := logs[0]
	if l.Level != level || l.Msg != msg {
		t.Fatalf("logged %s %q, want %s %q", l.Level, l.Msg, level, msg)
	}
	if rid := rec.Header().Get(httpx.RequestIDHeader); rid == "" || l.RequestID != rid {
		t.Fatalf("log request_id = %q, X-Request-ID = %q", l.RequestID, rid)
	}
	return l
}

// wantUnavailable asserts the bare 503 body: code, title, no detail or
// field errors.
func wantUnavailable(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	m := problemBody(t, rec, http.StatusServiceUnavailable)
	if m["code"] != "unavailable" || m["title"] != "Service unavailable" {
		t.Fatalf("problem = %v, want unavailable", m)
	}
	for _, k := range []string{"detail", "errors"} {
		if _, has := m[k]; has {
			t.Fatalf("503 must not carry %s: %v", k, m)
		}
	}
}

// TestProblem_RequestBudgetExhausted_Returns503Warn: a slow handler (a
// store waiting on a lock, an overloaded dependency) outlives the request
// budget and returns the bare context error: 503 unavailable after the
// budget, logged once at WARN with the request ID, never at ERROR.
func TestProblem_RequestBudgetExhausted_Returns503Warn(t *testing.T) {
	t.Parallel()
	const budget = 50 * time.Millisecond
	app := newTestApp(t, func(d *httpx.Deps) { d.RequestTimeout = budget })
	registerGET(app.V1, "getSlow", "/slow", func(ctx context.Context, _ *emptyInput) (*okOutput, error) {
		<-ctx.Done() // a store call that never got its answer
		return nil, fmt.Errorf("load thing: %w", ctx.Err())
	})

	start := time.Now()
	rec := app.do(t, http.MethodGet, "/api/v1/slow", nil, nil)
	if took := time.Since(start); took < budget {
		t.Fatalf("answered after %v, before the %v budget", took, budget)
	}
	wantUnavailable(t, rec)
	l := wantOneLog(t, app, rec, "WARN", "request budget exhausted")
	if l.Status != http.StatusServiceUnavailable || l.Code != "unavailable" || !strings.Contains(l.Err, "context deadline exceeded") {
		t.Fatalf("log record %+v", l)
	}
}

// TestProblem_RequestEnd_ErrorChains covers the classification of
// toProblem with a live request context: a deadline or a cancelled query
// anywhere in a chain without a *problem.Error is 503; a *problem.Error
// keeps its status (its 503 logs at WARN when the cause is a deadline); a
// Canceled error while the request is still live is a server fault.
func TestProblem_RequestEnd_ErrorChains(t *testing.T) {
	t.Parallel()
	queryCanceled := &pgconn.PgError{Severity: "ERROR", Code: "57014", Message: "canceling statement due to statement timeout"}
	lockTimeout := &pgconn.PgError{Severity: "ERROR", Code: "55P03", Message: "canceling statement due to lock timeout"}
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantLevel  string // "" = nothing logged
		wantMsg    string
	}{
		{"bare deadline of a dependency", context.DeadlineExceeded, 503, "unavailable", "WARN", "request budget exhausted"},
		{"wrapped deadline", fmt.Errorf("store: lock user: %w", context.DeadlineExceeded), 503, "unavailable", "WARN", "request budget exhausted"},
		{"joined deadline", errors.Join(errors.New("rollback failed"), context.DeadlineExceeded), 503, "unavailable", "WARN", "request budget exhausted"},
		{"statement timeout 57014", fmt.Errorf("db: %w", queryCanceled), 503, "unavailable", "WARN", "request budget exhausted"},
		{"lock timeout 55P03 stays a fault", fmt.Errorf("db: %w", lockTimeout), 500, "internal_error", "ERROR", "request failed"},
		{"canceled while the request is live", fmt.Errorf("worker group: %w", context.Canceled), 500, "internal_error", "ERROR", "request failed"},
		{"plain error stays a fault", errors.New("boom"), 500, "internal_error", "ERROR", "request failed"},
		{"unavailable problem wrapping a deadline", problem.ErrUnavailable.Wrap(context.DeadlineExceeded), 503, "unavailable", "WARN", "request budget exhausted"},
		{"unavailable problem without a deadline", problem.ErrUnavailable.Wrap(errors.New("redis down")), 503, "unavailable", "ERROR", "request failed"},
		{"internal problem wrapping a deadline", problem.ErrInternal.Wrap(context.DeadlineExceeded), 500, "internal_error", "ERROR", "request failed"},
		{"module problem wrapping a deadline wins", errTestThingFull.Wrap(context.DeadlineExceeded), 409, "testonly.thing_full", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := newTestApp(t, nil)
			registerGET(app.V1, "getThing", "/thing", func(context.Context, *emptyInput) (*okOutput, error) {
				return nil, tc.err
			})
			rec := app.do(t, http.MethodGet, "/api/v1/thing", nil, nil)
			m := problemBody(t, rec, tc.wantStatus)
			if m["code"] != tc.wantCode {
				t.Fatalf("code = %v, want %s", m["code"], tc.wantCode)
			}
			if tc.wantLevel == "" {
				if logs := failureLogs(t, app); len(logs) != 0 {
					t.Fatalf("a %d logged %+v", tc.wantStatus, logs)
				}
				return
			}
			wantOneLog(t, app, rec, tc.wantLevel, tc.wantMsg)
		})
	}
}

// TestProblem_ClientGone_Returns503Info: the client disconnects (net/http
// cancels the request context; a hard shutdown cancels the base context
// the same way) and the handler returns context.Canceled. The response
// cannot reach a departed client; for the access log, the metrics and a
// shutdown-time client that is still connected it is 503 unavailable, and
// it logs at INFO: a departed caller is neither a fault nor an overload.
func TestProblem_ClientGone_Returns503Info(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)
	ctx, disconnect := context.WithCancel(t.Context())
	defer disconnect()
	registerGET(app.V1, "getAbandoned", "/abandoned", func(ctx context.Context, _ *emptyInput) (*okOutput, error) {
		disconnect() // the client goes away while the store call runs
		<-ctx.Done()
		return nil, fmt.Errorf("load thing: %w", ctx.Err())
	})
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/abandoned", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	wantUnavailable(t, rec)
	wantOneLog(t, app, rec, "INFO", "request canceled")
}
