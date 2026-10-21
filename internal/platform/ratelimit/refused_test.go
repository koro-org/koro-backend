package ratelimit_test

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
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
	"koro/internal/platform/ratelimit"
)

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

// TestRateLimit_Refused_LoggedAndCountedWithoutActor (T-322, BE-078): a
// request refused by its operation's class is no verdict on the request —
// for an R operation the proof was never looked at — so the middleware
// writes no audit record; it logs "rate limited" with the rule (class)
// and the action (WARN for the rules that fail closed, INFO for the
// others) and counts koro_ratelimit_rejected_total{rule,action} (05
// §11.4), whose labels never carry the actor. Before BE-078 the refusal left only the access log line.
func TestRateLimit_Refused_LoggedAndCountedWithoutActor(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	user := uuid.NewV7()
	logs := &syncBuffer{}
	log := obs.NewLogger(logs, obs.LoggerOptions{Level: slog.LevelInfo})
	m := obs.NewMetrics()
	app, err := httpx.New(httpx.Deps{Log: log, Metrics: m, Actors: userResolver{id: user}})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(ratelimit.Middleware(app.API, ratelimit.NewMem(clk), log))
	h := func(context.Context, *empty) (*empty, error) { return &empty{}, nil }
	huma.Register(app.V1, op.Koro(huma.Operation{OperationID: "enrollTotp", Method: http.MethodPost, Path: "/me/mfa/totp",
		Tags: []string{"identity"}}, op.Meta{Action: "identity.mfa.enroll", Surface: op.SurfaceSelf, RateLimit: op.RateReauth,
		Reauth: true, Idempotency: op.IdempotencyOptional}), h)
	huma.Register(app.V1, op.Koro(huma.Operation{OperationID: "resendMyTicket", Method: http.MethodPost, Path: "/me/tickets/resend",
		Tags: []string{"events"}}, op.Meta{Action: "events.ticket.resend", Surface: op.SurfaceSelf, RateLimit: op.RateTicketResend,
		Idempotency: op.IdempotencyOptional}), h)
	hd := app.Handler()

	for _, c := range []struct {
		path    string
		allowed int
	}{
		{"/api/v1/me/mfa/totp", 5},       // reauth: 5 per 15 minutes per actor
		{"/api/v1/me/tickets/resend", 3}, // ticket_resend: 3 per hour per actor
	} {
		for i := 1; i <= c.allowed+1; i++ {
			rec := do(t, hd, http.MethodPost, c.path, nil)
			if (rec.Code < 300) != (i <= c.allowed) || (i > c.allowed && rec.Code != http.StatusTooManyRequests) {
				t.Fatalf("%s request %d = %d %s", c.path, i, rec.Code, rec.Body)
			}
		}
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, obs.MetricsPath, nil))
	b, _ := io.ReadAll(rec.Body)
	out := string(b)
	for _, want := range []string{
		`koro_ratelimit_rejected_total{action="identity.mfa.enroll",rule="reauth"} 1`,
		`koro_ratelimit_rejected_total{action="events.ticket.resend",rule="ticket_resend"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(out, user.String()) {
		t.Errorf("the actor is a metric label:\n%s", out)
	}

	var got []string
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) != nil || r["msg"] != "rate limited" {
			continue
		}
		got = append(got, r["level"].(string)+" "+r["rule"].(string)+" "+r["action"].(string))
		if r["request_id"] == nil {
			t.Errorf("rate limited log without request_id: %s", line)
		}
	}
	if want := []string{"WARN reauth identity.mfa.enroll", "INFO ticket_resend events.ticket.resend"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rate limited logs %q, want %q", got, want)
	}
}
