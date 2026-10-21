package realtime_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/coder/websocket"

	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/platform/realtime"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func newHub(t *testing.T, mut ...func(*realtime.Config)) *realtime.Hub {
	t.Helper()
	cfg := realtime.Config{Bus: realtime.NewMemBus(), Clock: clock.NewFake(t0), Log: obs.Discard(),
		TrustedOrigins: []string{"https://app.koro.test"}}
	for _, m := range mut {
		m(&cfg)
	}
	h, err := realtime.NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// hubHandler admits the X-User header's user on the X-Topic header's
// topic and serves the stream returned by stream.
func hubHandler(h *realtime.Hub, stream func(r *http.Request) realtime.Stream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := uuid.Parse(r.Header.Get("X-User"))
		if err != nil {
			user = uuid.NewV7()
		}
		l, err := h.Admit(r.Context(), r.Header.Get("X-Topic"), user)
		if err != nil {
			pe, _ := errors.AsType[*problem.Error](err)
			w.WriteHeader(pe.Status())
			return
		}
		h.Serve(w, r, l, stream(r))
	}
}

func snapshot(s string) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) { return []byte(s), nil }
}

func dial(t *testing.T, url, topic string, user uuid.UUID) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	h.Set("X-Topic", topic)
	h.Set("X-User", user.String())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(url, "http"), &websocket.DialOptions{HTTPHeader: h})
	if c != nil {
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, resp, err
}

func mustDial(t *testing.T, url, topic string, user uuid.UUID) *websocket.Conn {
	t.Helper()
	c, _, err := dial(t, url, topic, user)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func read(t *testing.T, c *websocket.Conn) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	return string(b), err
}

func mustRead(t *testing.T, c *websocket.Conn, want string) {
	t.Helper()
	got, err := read(t, c)
	if err != nil || got != want {
		t.Fatalf("read = %q, %v; want %q", got, err, want)
	}
}

// readClose reads until the connection closes and returns its status.
func readClose(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	for {
		if _, err := read(t, c); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func publish(t *testing.T, h *realtime.Hub, topic, msg string) {
	t.Helper()
	if err := h.Publish(t.Context(), topic, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}

// waitConns waits until the hub holds n connections of topic (a joined
// connection gets every later message).
func waitConns(t *testing.T, h *realtime.Hub, topic string, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for h.Connections(topic) != n {
		select {
		case <-deadline:
			t.Fatalf("connections of %s = %d, want %d", topic, h.Connections(topic), n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestShutdown_ClosesSockets: http.Server.Shutdown neither waits for nor
// closes hijacked connections; the hub's CloseAll, registered with
// RegisterOnShutdown (05 §10.9), closes every socket with 1001, and a
// handshake after it is refused (503) without an upgrade.
func TestShutdown_ClosesSockets(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	srv := httpx.NewServer(hubHandler(h, func(*http.Request) realtime.Stream {
		return realtime.Stream{Snapshot: snapshot(`{"type":"snapshot"}`)}
	}), t.Context(), obs.Discard())
	srv.RegisterOnShutdown(h.CloseAll)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	url := "http://" + ln.Addr().String()

	a := mustDial(t, url, "board", uuid.NewV7())
	b := mustDial(t, url, "board", uuid.NewV7())
	mustRead(t, a, `{"type":"snapshot"}`)
	mustRead(t, b, `{"type":"snapshot"}`)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for name, c := range map[string]*websocket.Conn{"a": a, "b": b} {
		if got := readClose(t, c); got != websocket.StatusGoingAway {
			t.Fatalf("%s closed with %d, want 1001", name, got)
		}
	}
	if _, err := h.Admit(t.Context(), "board", uuid.NewV7()); !errors.Is(err, problem.ErrUnavailable) {
		t.Fatalf("Admit after CloseAll: %v, want unavailable", err)
	}
}

// TestHub_ServerDeadlinesCleared: the server's read/write timeouts stay on
// a hijacked connection; the hub clears them, so a board outlives them.
func TestHub_ServerDeadlinesCleared(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	srv := httptest.NewUnstartedServer(hubHandler(h, func(*http.Request) realtime.Stream { return realtime.Stream{} }))
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = 150*time.Millisecond, 150*time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	c := mustDial(t, srv.URL, "slow", uuid.NewV7())
	waitConns(t, h, "slow", 1)
	time.Sleep(400 * time.Millisecond)
	publish(t, h, "slow", `{"type":"stats"}`)
	mustRead(t, c, `{"type":"stats"}`)
}

// TestHub_Limits: at most MaxPerUser connections per user and MaxPerTopic
// per topic (429 before the upgrade); a closed connection frees its slot.
func TestHub_Limits(t *testing.T) {
	t.Parallel()
	h := newHub(t, func(c *realtime.Config) { c.MaxPerTopic = 4 })
	srv := httptest.NewServer(hubHandler(h, func(*http.Request) realtime.Stream { return realtime.Stream{} }))
	t.Cleanup(srv.Close)

	me := uuid.NewV7()
	var mine []*websocket.Conn
	for range realtime.DefaultMaxPerUser {
		mine = append(mine, mustDial(t, srv.URL, "t1", me))
	}
	if _, resp, err := dial(t, srv.URL, "t1", me); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("4th connection of a user: %v %v", resp, err)
	}
	mustDial(t, srv.URL, "t1", uuid.NewV7()) // the topic's 4th
	if _, resp, err := dial(t, srv.URL, "t1", uuid.NewV7()); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("5th connection of a topic: %v %v", resp, err)
	}
	mustDial(t, srv.URL, "t2", uuid.NewV7()) // another topic has its own budget

	_ = mine[0].Close(websocket.StatusNormalClosure, "")
	waitConns(t, h, "t1", 3)
	deadline := time.After(5 * time.Second)
	for {
		c, _, err := dial(t, srv.URL, "t1", me)
		if err == nil {
			_ = c.CloseNow()
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the freed slot was not reusable: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestHub_ClientMessage_Closes1008: clients send only ping/pong; a data
// message closes with 1008 (policy violation).
func TestHub_ClientMessage_Closes1008(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	srv := httptest.NewServer(hubHandler(h, func(*http.Request) realtime.Stream { return realtime.Stream{} }))
	t.Cleanup(srv.Close)
	c := mustDial(t, srv.URL, "t", uuid.NewV7())
	if err := c.Write(t.Context(), websocket.MessageText, []byte(`{"type":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	if got := readClose(t, c); got != websocket.StatusPolicyViolation {
		t.Fatalf("closed with %d, want 1008", got)
	}
}

// TestHub_PingPong: the hub pings; the client's reader answers; the
// connection survives several ping periods.
func TestHub_PingPong(t *testing.T) {
	t.Parallel()
	h := newHub(t, func(c *realtime.Config) { c.PingInterval, c.PingTimeout = 30*time.Millisecond, time.Second })
	srv := httptest.NewServer(hubHandler(h, func(*http.Request) realtime.Stream { return realtime.Stream{} }))
	t.Cleanup(srv.Close)
	c := mustDial(t, srv.URL, "p", uuid.NewV7())
	waitConns(t, h, "p", 1)
	got := make(chan string, 1)
	go func() {
		_, b, _ := c.Read(t.Context()) // reads (and answers pings) until the message
		got <- string(b)
	}()
	time.Sleep(200 * time.Millisecond)
	publish(t, h, "p", "after pings")
	select {
	case s := <-got:
		if s != "after pings" {
			t.Fatalf("got %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

// TestHub_Recheck: the periodic re-check closes a lost session with 4401
// and a lost authorisation with 4403 (04 §W).
func TestHub_Recheck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want websocket.StatusCode
	}{
		{"session", problem.ErrUnauthenticated, realtime.CloseUnauthenticated},
		{"forbidden", problem.ErrForbidden, realtime.CloseForbidden},
		{"not_found", problem.ErrNotFound, realtime.CloseForbidden},
		{"infra", errors.New("db down"), websocket.StatusInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var lost atomic.Bool
			h := newHub(t, func(c *realtime.Config) { c.Recheck = 20 * time.Millisecond })
			srv := httptest.NewServer(hubHandler(h, func(*http.Request) realtime.Stream {
				return realtime.Stream{Recheck: func(context.Context) error {
					if lost.Load() {
						return tc.err
					}
					return nil
				}}
			}))
			t.Cleanup(srv.Close)
			c := mustDial(t, srv.URL, "r", uuid.NewV7())
			waitConns(t, h, "r", 1)
			time.Sleep(100 * time.Millisecond) // several successful re-checks
			publish(t, h, "r", "still here")
			mustRead(t, c, "still here")
			lost.Store(true)
			if got := readClose(t, c); got != tc.want {
				t.Fatalf("closed with %d, want %d", got, tc.want)
			}
		})
	}
}

// TestHub_StaleCheckBeforeMessage: a message to a connection checked more
// than Fresh ago re-checks first; a revoked viewer gets 4403, not the
// message.
func TestHub_StaleCheckBeforeMessage(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	var lost atomic.Bool
	h := newHub(t, func(c *realtime.Config) { c.Clock = clk })
	srv := httptest.NewServer(hubHandler(h, func(*http.Request) realtime.Stream {
		return realtime.Stream{Recheck: func(context.Context) error {
			if lost.Load() {
				return problem.ErrForbidden
			}
			return nil
		}}
	}))
	t.Cleanup(srv.Close)
	c := mustDial(t, srv.URL, "f", uuid.NewV7())
	waitConns(t, h, "f", 1)
	publish(t, h, "f", "fresh")
	mustRead(t, c, "fresh")
	lost.Store(true)
	clk.Advance(realtime.DefaultFresh)
	publish(t, h, "f", "secret")
	got, err := read(t, c)
	if err == nil || websocket.CloseStatus(err) != realtime.CloseForbidden {
		t.Fatalf("read = %q, %v; want close 4403", got, err)
	}
}

// TestHub_FanOutAndFilter: a message reaches every connection of its
// topic, through each connection's filter, and no other topic.
func TestHub_FanOutAndFilter(t *testing.T) {
	t.Parallel()
	h := newHub(t)
	srv := httptest.NewServer(hubHandler(h, func(r *http.Request) realtime.Stream {
		if r.URL.Query().Get("redact") == "" {
			return realtime.Stream{}
		}
		return realtime.Stream{Filter: func(m []byte) ([]byte, bool) {
			if strings.Contains(string(m), "drop") {
				return nil, false
			}
			return []byte(strings.ToUpper(string(m))), true
		}}
	}))
	t.Cleanup(srv.Close)
	plain := mustDial(t, srv.URL, "x", uuid.NewV7())
	redacted := mustDial(t, srv.URL+"?redact=1", "x", uuid.NewV7())
	other := mustDial(t, srv.URL, "y", uuid.NewV7())
	waitConns(t, h, "x", 2)
	waitConns(t, h, "y", 1)
	publish(t, h, "x", "drop me")
	publish(t, h, "x", "hello")
	publish(t, h, "y", "for y")
	mustRead(t, plain, "drop me")
	mustRead(t, plain, "hello")
	mustRead(t, redacted, "HELLO")
	mustRead(t, other, "for y")
}

// TestNewHub_Config: Recheck may not exceed 60 s (04 §W); a trusted origin
// must be scheme://host.
func TestNewHub_Config(t *testing.T) {
	t.Parallel()
	base := realtime.Config{Bus: realtime.NewMemBus(), Clock: clock.NewFake(t0), Log: obs.Discard()}
	over := base
	over.Recheck = realtime.MaxRecheck + time.Second
	if _, err := realtime.NewHub(over); err == nil {
		t.Fatal("Recheck over 60s accepted")
	}
	bad := base
	bad.TrustedOrigins = []string{"app.koro.test"}
	if _, err := realtime.NewHub(bad); err == nil {
		t.Fatal("origin without scheme accepted")
	}
	if _, err := realtime.NewHub(realtime.Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
}

// TestCheckOrigin: no Origin (native client), the API's own host and a
// trusted origin pass; another site, "null" and a scheme mismatch of a
// trusted origin are 403.
func TestCheckOrigin(t *testing.T) {
	t.Parallel()
	h := newHub(t, func(c *realtime.Config) {
		c.TrustedOrigins = []string{"https://app.koro.test/", "http://[::1]:3000"}
	})
	for origin, ok := range map[string]bool{
		"":                         true,
		"https://api.koro.test":    true, // same host as the request
		"https://app.koro.test":    true,
		"https://APP.koro.test":    true,
		"http://[::1]:3000":        true,
		"http://app.koro.test":     false,
		"https://evil.example":     false,
		"https://app.koro.test.ev": false,
		"null":                     false,
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.koro.test/live", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if err := h.CheckOrigin(r); (err == nil) != ok {
			t.Errorf("Origin %q: %v, want ok=%v", origin, err, ok)
		}
	}
}

// TestCredentialFrom: the bearer header, then the session cookie; a token
// in the query string is never a credential (04 §W).
func TestCredentialFrom(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/live?token=abc&access_token=abc&session=abc", nil)
	if _, ok := realtime.CredentialFrom(r); ok {
		t.Fatal("query string token accepted")
	}
	r.AddCookie(&http.Cookie{Name: httpx.SessionCookieName, Value: "cookie-token"})
	if c, ok := realtime.CredentialFrom(r); !ok || c.Transport != httpx.TransportCookie {
		t.Fatalf("cookie: %v %v", c, ok)
	}
	r.Header.Set("Authorization", "Bearer bearer-token")
	if c, ok := realtime.CredentialFrom(r); !ok || c.Transport != httpx.TransportBearer {
		t.Fatalf("bearer: %v %v", c, ok)
	}
	r.Header.Set("Authorization", "Basic x")
	if c, ok := realtime.CredentialFrom(r); !ok || c.TokenHash != [32]byte{} {
		t.Fatalf("malformed header must yield an empty hash: %v %v", c, ok)
	}
}

// TestCloseFor: the close code of a failed re-check.
func TestCloseFor(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]websocket.StatusCode{
		nil:                         0,
		problem.ErrUnauthenticated:  realtime.CloseUnauthenticated,
		problem.ErrForbidden:        realtime.CloseForbidden,
		problem.ErrNotFound:         realtime.CloseForbidden,
		context.DeadlineExceeded:    websocket.StatusInternalError,
		problem.ErrInternal:         websocket.StatusInternalError,
		problem.ErrValidation:       realtime.CloseForbidden,
		errors.New("redis is gone"): websocket.StatusInternalError,
	} {
		if got, _ := realtime.CloseFor(err); got != want {
			t.Errorf("CloseFor(%v) = %d, want %d", err, got, want)
		}
	}
}
