package errtrack_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"koro/internal/platform/errtrack"
	"koro/internal/platform/obs"
)

// glitchtip is a fake Sentry-protocol ingest endpoint that keeps the bodies
// it receives.
type glitchtip struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newGlitchtip(t *testing.T) *glitchtip {
	t.Helper()
	g := &glitchtip{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.bodies = append(g.bodies, r.URL.Path+"\n"+string(b))
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *glitchtip) dsn() string {
	return strings.Replace(g.srv.URL, "http://", "http://examplepublickey@", 1) + "/7"
}

func (g *glitchtip) received() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.bodies, "\n---\n")
}

func newSentry(t *testing.T, g *glitchtip) *errtrack.Sentry {
	t.Helper()
	s, err := errtrack.NewSentry(errtrack.SentryOptions{DSN: g.dsn(), Environment: "staging", Release: "abc123", Service: "koro-api"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewSentry_RejectsBadDSN(t *testing.T) {
	t.Parallel()
	for _, dsn := range []string{"", "not a dsn", "https://glitchtip.koro.example/3"} {
		if _, err := errtrack.NewSentry(errtrack.SentryOptions{DSN: dsn}); err == nil {
			t.Errorf("NewSentry(%q) accepted", dsn)
		} else if dsn != "" && strings.Contains(err.Error(), dsn) {
			t.Errorf("error echoes the DSN: %v", err)
		}
	}
}

// TestSentry_SendsRedactedEvent: an ERROR log line reaches the server as
// one event with the release, environment, service, request ID and the
// user's ID; no personal data, request data or credential is in the body.
func TestSentry_SendsRedactedEvent(t *testing.T) {
	t.Parallel()
	g := newGlitchtip(t)
	s := newSentry(t, g)
	log := slog.New(errtrack.NewHandler(slog.DiscardHandler, s))
	ctx := obs.WithActorID(obs.WithRequestID(t.Context(), "req-0123456789"), actorID)
	log.ErrorContext(ctx, "internal error",
		slog.Any("err", errors.New("pq: duplicate key for "+piiEmail+" token="+piiToken)),
		slog.String("client_ip", piiIP), slog.String("user_agent", piiUA), slog.String("cookie", piiCookie),
		slog.String("route", "/v1/me"), slog.Int("status", 500))
	if !s.Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	body := g.received()
	if !strings.Contains(body, "/api/7/envelope/") {
		t.Fatalf("not an envelope request:\n%s", body)
	}
	for _, want := range []string{`"internal error"`, "req-0123456789", actorID, `"environment":"staging"`, `"release":"abc123"`,
		`"server_name":"koro-api"`, `"/v1/me"`, "duplicate key"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s:\n%s", want, body)
		}
	}
	for _, leak := range []string{piiEmail, piiToken, piiIP, piiUA, "s3ss10nvalue", `"request":`, `"breadcrumbs"`, `"ip_address"`, `"email"`} {
		if strings.Contains(body, leak) {
			t.Errorf("body leaks %s:\n%s", leak, body)
		}
	}
}

// TestSentry_ScrubEventStripsRequestAndUser: the BeforeSend hook removes
// what the SDK or a future integration could add (request with headers,
// cookies, query and body; user fields; breadcrumbs) and redacts the rest.
func TestSentry_ScrubEventStripsRequestAndUser(t *testing.T) {
	t.Parallel()
	e := sentry.NewEvent()
	e.Message = "failed for " + piiEmail
	e.Transaction = "/abonelikten-cik/" + piiToken
	e.Request = &sentry.Request{
		URL: "https://koro.example/abonelikten-cik/" + piiToken, QueryString: "token=" + piiToken,
		Cookies: piiCookie, Data: `{"password":"` + piiPass + `"}`, Headers: map[string]string{"Authorization": "Bearer x", "User-Agent": piiUA},
	}
	e.User = sentry.User{ID: actorID, Email: piiEmail, IPAddress: piiIP, Username: "ayse", Name: "Ayşe Yılmaz", Data: map[string]string{"phone": piiPhone}}
	e.Breadcrumbs = []*sentry.Breadcrumb{{Message: "GET /abonelikten-cik/" + piiToken}}
	e.Tags["user_email"] = piiEmail
	e.Tags["detail"] = "reach " + piiEmail
	e.Tags["request_id"] = "req-1"
	e.Contexts["log"] = sentry.Context{"password": piiPass, "client_ip": piiIP, "nested": map[string]any{"email": piiEmail, "note": "x " + piiEmail}, "n": 3}
	e.Exception = []sentry.Exception{{Type: "boom", Value: "Bearer abcdef " + piiEmail,
		Stacktrace: &sentry.Stacktrace{Frames: []sentry.Frame{{Function: "f", Vars: map[string]any{"pw": piiPass}}}}}}

	out := errtrack.ScrubEvent(e)
	if out.Request != nil || out.Breadcrumbs != nil || out.Transaction != "" {
		t.Fatalf("request data kept: %+v", out)
	}
	if out.User.ID != actorID || out.User.Email != "" || out.User.IPAddress != "" || out.User.Username != "" || out.User.Name != "" || out.User.Data != nil {
		t.Fatalf("user = %+v", out.User)
	}
	if out.Tags["request_id"] != "req-1" || out.Contexts["log"]["n"] != 3 {
		t.Fatalf("correlation lost: tags %v ctx %v", out.Tags, out.Contexts)
	}
	if _, ok := out.Tags["user_email"]; ok {
		t.Fatal("user_email tag kept")
	}
	dump := fmt.Sprint(out.Message, out.Tags, out.Contexts, out.Exception[0].Value, out.Exception[0].Stacktrace.Frames[0].Vars)
	for _, leak := range []string{piiEmail, piiPass, piiIP, "abcdef"} {
		if strings.Contains(dump, leak) {
			t.Errorf("scrubbed event leaks %q: %s", leak, dump)
		}
	}
	if errtrack.ScrubEvent(nil) != nil {
		t.Fatal("nil event")
	}
}

// TestSentry_PanicGroupsAtThePanickingFunction: the stack of a recovered
// panic starts at the function that panicked, not in the recovery code.
func TestSentry_PanicGroupsAtThePanickingFunction(t *testing.T) {
	t.Parallel()
	rep := &recorder{}
	log := slog.New(errtrack.NewHandler(slog.DiscardHandler, rep))
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic recovered", slog.Any("panic", r))
			}
		}()
		panicker()
	}()
	evs := rep.all()
	if len(evs) != 1 {
		t.Fatalf("%d events", len(evs))
	}
	ev := errtrack.ToSentry(evs[0])
	frames := ev.Exception[0].Stacktrace.Frames
	if top := frames[len(frames)-1]; top.Function != "panicker" {
		t.Fatalf("top frame = %s.%s", top.Module, top.Function)
	}
	if ev.Exception[0].Value != "kaboom" || ev.Exception[0].Type != "panic recovered" {
		t.Fatalf("exception = %+v", ev.Exception[0])
	}
}

func panicker() { panic("kaboom") }
