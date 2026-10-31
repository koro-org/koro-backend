package errtrack_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"koro/internal/platform/errtrack"
	"koro/internal/platform/obs"
)

// recorder is an in-memory Reporter.
type recorder struct {
	mu     sync.Mutex
	events []errtrack.Event
}

func (r *recorder) Report(e errtrack.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (*recorder) Flush(time.Duration) bool { return true }

func (r *recorder) all() []errtrack.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]errtrack.Event(nil), r.events...)
}

func newLogger(rep errtrack.Reporter) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	base := obs.NewLogger(&buf, obs.LoggerOptions{Level: slog.LevelInfo, Service: "koro-api", Env: "dev"})
	return slog.New(errtrack.NewHandler(base.Handler(), rep)), &buf
}

// Personal data and credentials of the KVKK test (synthetic).
const (
	piiEmail  = "ayse.yilmaz@ogrenci.edu.tr"
	piiIP     = "203.0.113.77"
	piiUA     = "Mozilla/5.0 (Ayse's laptop)"
	piiPass   = "hunter2hunter2hunter2"
	piiCookie = "koro_session=s3ss10nvalue"
	piiToken  = "unsubTok3nXyZ"
	piiPhone  = "0532 123 45 67"
	actorID   = "01920000-0000-7000-8000-000000000001"
)

// TestHandler_ReportsErrorsOnly: below ERROR nothing is reported; every
// record is still logged exactly once.
func TestHandler_ReportsErrorsOnly(t *testing.T) {
	t.Parallel()
	rep := &recorder{}
	log, buf := newLogger(rep)
	ctx := t.Context()
	log.DebugContext(ctx, "debug line")
	log.InfoContext(ctx, "info line")
	log.WarnContext(ctx, "warn line")
	if n := len(rep.all()); n != 0 {
		t.Fatalf("%d events below ERROR", n)
	}
	log.ErrorContext(ctx, "error line", slog.String("route", "/v1/events/{id}"))
	evs := rep.all()
	if len(evs) != 1 || evs[0].Message != "error line" || evs[0].Attrs["route"] != "/v1/events/{id}" || evs[0].Level != slog.LevelError {
		t.Fatalf("events = %+v", evs)
	}
	out := buf.String()
	if strings.Contains(out, "debug line") || strings.Count(out, "info line") != 1 || strings.Count(out, "error line") != 1 {
		t.Fatalf("log output changed:\n%s", out)
	}
}

// TestHandler_EventCarriesNoPersonalData (KVKK): the event holds the
// correlation IDs and the user's ID only; credentials, contact data,
// network data, request data and the query string never reach it.
func TestHandler_EventCarriesNoPersonalData(t *testing.T) {
	t.Parallel()
	rep := &recorder{}
	log, _ := newLogger(rep)
	ctx := obs.WithActorID(obs.WithRequestID(t.Context(), "req-0123456789"), actorID)
	log.With(slog.String("component", "identity")).WithGroup("http").ErrorContext(ctx, "sign-in failed for "+piiEmail,
		slog.Any("err", fmt.Errorf("smtp: Bearer abc.def rejected %s (cookie %s)", piiEmail, piiCookie)),
		slog.String("email", piiEmail),
		slog.Any("user_email", obs.Email(piiEmail)),
		slog.String("password", piiPass),
		slog.String("client_ip", piiIP),
		slog.String("user_agent", piiUA),
		slog.String("query", "token="+piiToken),
		slog.String("url", "/abonelikten-cik/"+piiToken),
		slog.String("body", `{"password":"`+piiPass+`"}`),
		slog.String("detail", "call me on "+piiPhone),
		slog.Group("headers", slog.String("Authorization", "Bearer "+piiToken), slog.String("Cookie", piiCookie)),
		slog.Int("status", 500),
	)
	evs := rep.all()
	if len(evs) != 1 {
		t.Fatalf("%d events", len(evs))
	}
	e := evs[0]
	if e.RequestID != "req-0123456789" || e.ActorID != actorID {
		t.Fatalf("correlation: request %q actor %q", e.RequestID, e.ActorID)
	}
	if !strings.Contains(e.Error, "Bearer "+obs.Redacted) || e.Attrs["component"] != "identity" || e.Attrs["http.status"] != "500" {
		t.Fatalf("event = %+v", e)
	}
	dump := fmt.Sprintf("%+v", e)
	for _, leak := range []string{piiEmail, piiIP, piiUA, piiPass, "s3ss10nvalue", piiToken, "abc.def", piiPhone, "123 45 67"} {
		if strings.Contains(dump, leak) {
			t.Errorf("event leaks %q: %s", leak, dump)
		}
	}
	for _, key := range []string{"http.client_ip", "http.user_agent", "http.query", "http.url", "http.body", "http.email", "http.user_email", "http.headers.Cookie"} {
		if _, ok := e.Attrs[key]; ok {
			t.Errorf("attribute %q reached the tracker", key)
		}
	}
}

// TestHandler_StackStartsAtTheLoggingCall: slog and the handler are not
// part of the reported stack.
func TestHandler_StackStartsAtTheLoggingCall(t *testing.T) {
	t.Parallel()
	rep := &recorder{}
	log, _ := newLogger(rep)
	log.ErrorContext(t.Context(), "boom")
	evs := rep.all()
	if len(evs) != 1 || len(evs[0].Stack) == 0 {
		t.Fatalf("events = %+v", evs)
	}
	f, _ := runtime.CallersFrames(evs[0].Stack).Next()
	if !strings.HasSuffix(f.Function, "TestHandler_StackStartsAtTheLoggingCall") {
		t.Fatalf("top frame = %s", f.Function)
	}
}

// TestHandler_RespectsNextLevel: a record next does not log is still
// reported when it is an error (the tracker does not depend on LOG_LEVEL).
func TestHandler_RespectsNextLevel(t *testing.T) {
	t.Parallel()
	rep := &recorder{}
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError + 8})
	log := slog.New(errtrack.NewHandler(base, rep))
	log.ErrorContext(context.Background(), "quiet error")
	if buf.Len() != 0 || len(rep.all()) != 1 {
		t.Fatalf("log %q, events %d", buf.String(), len(rep.all()))
	}
}

// TestReportPanic_ReportsAndRepanics: a panic at the entry point is reported
// as fatal with a scrubbed value and keeps crashing.
func TestReportPanic_ReportsAndRepanics(t *testing.T) {
	t.Parallel()
	rep := &recorder{}
	var got any
	func() {
		defer func() { got = recover() }()
		defer errtrack.ReportPanic(rep, time.Second)
		panic(errors.New("nil map for " + piiEmail))
	}()
	if got == nil {
		t.Fatal("panic was swallowed")
	}
	evs := rep.all()
	if len(evs) != 1 || evs[0].Level != errtrack.LevelFatal || strings.Contains(evs[0].Error, piiEmail) || !strings.Contains(evs[0].Error, "nil map") {
		t.Fatalf("events = %+v", evs)
	}
	func() {
		defer errtrack.ReportPanic(rep, time.Second) // no panic: nothing happens
	}()
	if len(rep.all()) != 1 {
		t.Fatal("ReportPanic reported without a panic")
	}
}
