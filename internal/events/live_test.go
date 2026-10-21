package events_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/coder/websocket"

	"koro/internal/authz/authztest"
	"koro/internal/events"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/platform/realtime"
)

// liveEnv is an events env whose check-ins publish on a hub, with the
// API and the live board's route behind an HTTP test server.
type liveEnv struct {
	*env
	hub    *realtime.Hub
	actors *revocable
	srv    *httptest.Server
}

// revocable is the persona resolver with sessions that can end (4401).
type revocable struct {
	*authztest.Resolver
	mu      sync.Mutex
	revoked map[[sha256.Size]byte]bool
}

func (r *revocable) revoke(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked[sha256.Sum256([]byte(token))] = true
}

func (r *revocable) Resolve(ctx context.Context, cred httpx.Credential) (context.Context, error) {
	r.mu.Lock()
	ended := r.revoked[cred.TokenHash]
	r.mu.Unlock()
	if ended {
		return nil, problem.ErrUnauthenticated
	}
	return r.Resolver.Resolve(ctx, cred)
}

func newLiveEnv(t *testing.T, mut ...func(*realtime.Config)) *liveEnv {
	t.Helper()
	cfg := realtime.Config{Bus: realtime.NewMemBus(), Clock: clock.NewFake(t0), Log: obs.Discard(),
		TrustedOrigins: []string{"https://app.koro.test"}}
	for _, m := range mut {
		m(&cfg)
	}
	hub, err := realtime.NewHub(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(d *events.Deps) { d.Live = hub })
	actors := &revocable{Resolver: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...),
		revoked: map[[sha256.Size]byte]bool{}}
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: actors})
	if err != nil {
		t.Fatal(err)
	}
	events.Register(app.V1, e.svc)
	events.RegisterLive(app.V1, e.svc, hub, actors)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		hub.CloseAll()
		srv.Close()
	})
	return &liveEnv{env: e, hub: hub, actors: actors, srv: srv}
}

func token(t *testing.T, name string) string {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Token
}

// dialLive opens the board of eventID as the persona (bearer; "" =
// anonymous) with extra headers and a raw query.
func (l *liveEnv) dialLive(t *testing.T, persona string, eventID uuid.UUID, query string, hdr map[string]string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	if persona != "" {
		h.Set("Authorization", "Bearer "+token(t, persona))
	}
	for k, v := range hdr {
		h.Set(k, v)
	}
	u := "ws" + strings.TrimPrefix(l.srv.URL, "http") + "/api/v1/events/" + eventID.String() + "/check-ins/live" + query
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if c != nil {
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, resp, err
}

func (l *liveEnv) mustDialLive(t *testing.T, persona string, eventID uuid.UUID) *websocket.Conn {
	t.Helper()
	c, resp, err := l.dialLive(t, persona, eventID, "", nil)
	if err != nil {
		t.Fatalf("dial as %s: %v (%v)", persona, err, resp)
	}
	return c
}

// rejected asserts a refused handshake: the status, a problem+json body
// with code, and no connection on the hub.
func (l *liveEnv) rejected(t *testing.T, what string, resp *http.Response, err error, status int, code string, eventID uuid.UUID) {
	t.Helper()
	if err == nil || resp == nil || resp.StatusCode != status {
		t.Fatalf("%s: %v %v, want %d", what, resp, err, status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, problem.ContentType) {
		t.Fatalf("%s: Content-Type %q", what, ct)
	}
	var p struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil || p.Code != code {
		t.Fatalf("%s: code %q (%v), want %q", what, p.Code, err, code)
	}
	if n := l.hub.Connections(events.LiveTopic(eventID)); n != 0 {
		t.Fatalf("%s: %d connections on the board", what, n)
	}
}

func readMsg(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("message %s: %v", b, err)
	}
	return m
}

func closeStatus(t *testing.T, c *websocket.Conn, within time.Duration) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatalf("not closed within %s", within)
			}
			return websocket.CloseStatus(err)
		}
	}
}

// TestLive_NonStaff_Rejected: the handshake is authorised like
// getCheckinStats (03 §5.6 events.checkin.stats): signed-in users who see
// the event but neither manage it nor scan for it are 403, an event they
// cannot see or that does not exist 404, anonymous 401 — every refusal a
// problem+json answer, nothing upgraded.
func TestLive_NonStaff_Rejected(t *testing.T) {
	t.Parallel()
	l := newLiveEnv(t)
	ev := l.doorEvent(t)
	id := ev.Event.ID
	for _, p := range []string{"student", "club_member", "other_club_admin", "university_rep", "company_admin",
		"platform_admin", "support"} {
		_, resp, err := l.dialLive(t, p, id, "", nil)
		l.rejected(t, p, resp, err, http.StatusForbidden, "forbidden", id)
	}
	_, resp, err := l.dialLive(t, "", id, "", nil)
	l.rejected(t, "anonymous", resp, err, http.StatusUnauthorized, "unauthenticated", id)
	missing := uuid.NewV7()
	_, resp, err = l.dialLive(t, "club_admin", missing, "", nil)
	l.rejected(t, "unknown event", resp, err, http.StatusNotFound, "not_found", missing)
	// The staff and the organiser's manager are let in.
	for _, p := range []string{"event_staff", "club_admin"} {
		c := l.mustDialLive(t, p, id)
		if m := readMsg(t, c); m["type"] != "snapshot" {
			t.Fatalf("%s: first message %v", p, m)
		}
	}
	// A plain GET (no upgrade) by an authorised user is 400, not upgraded.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, l.srv.URL+"/api/v1/events/"+id.String()+"/check-ins/live", nil)
	req.Header.Set("Authorization", "Bearer "+token(t, "club_admin"))
	plain, err := l.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Body.Close()
	if plain.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain GET: %d", plain.StatusCode)
	}
}

// TestLive_TokenInQuery_Rejected: the token is accepted only in the
// Authorization header or the session cookie; in the query string (where
// it would reach proxy and access logs) it authenticates nobody (04 §W).
func TestLive_TokenInQuery_Rejected(t *testing.T) {
	t.Parallel()
	l := newLiveEnv(t)
	ev := l.doorEvent(t)
	tok := token(t, "event_staff")
	for _, q := range []string{"?token=", "?access_token=", "?session=", "?authorization=Bearer%20"} {
		_, resp, err := l.dialLive(t, "", ev.Event.ID, q+tok, nil)
		l.rejected(t, q, resp, err, http.StatusUnauthorized, "unauthenticated", ev.Event.ID)
	}
	// The same token in the header is let in.
	c := l.mustDialLive(t, "event_staff", ev.Event.ID)
	if m := readMsg(t, c); m["type"] != "snapshot" {
		t.Fatalf("first message %v", m)
	}
}

// TestLive_BadOrigin_Rejected: the handshake is a GET, which
// CrossOriginProtection does not check; the Origin is checked against
// TRUSTED_ORIGINS before the credential (Cross-Site WebSocket Hijacking,
// 05 §10.7): another site is 403 even with a valid session; a trusted
// origin and a native client without Origin are let in.
func TestLive_BadOrigin_Rejected(t *testing.T) {
	t.Parallel()
	l := newLiveEnv(t)
	ev := l.doorEvent(t)
	for _, o := range []string{"https://evil.example", "null", "http://app.koro.test"} {
		_, resp, err := l.dialLive(t, "event_staff", ev.Event.ID, "", map[string]string{"Origin": o})
		l.rejected(t, o, resp, err, http.StatusForbidden, "forbidden", ev.Event.ID)
	}
	c, resp, err := l.dialLive(t, "event_staff", ev.Event.ID, "", map[string]string{"Origin": "https://app.koro.test"})
	if err != nil {
		t.Fatalf("trusted origin: %v %v", resp, err)
	}
	if m := readMsg(t, c); m["type"] != "snapshot" {
		t.Fatalf("first message %v", m)
	}
}

// TestLive_StaffRemoved_DisconnectedWithin60s: the hub re-authorises
// every connection at least every 60 seconds (here shortened); once the
// organiser removes the scanner from the event, the scanner's board closes
// with 4403, while the organiser's stays open.
func TestLive_StaffRemoved_DisconnectedWithin60s(t *testing.T) {
	t.Parallel()
	if realtime.MaxRecheck > 60*time.Second {
		t.Fatalf("MaxRecheck = %s, 04 §W requires at most 60s", realtime.MaxRecheck)
	}
	l := newLiveEnv(t, func(c *realtime.Config) { c.Recheck = 50 * time.Millisecond })
	ev := l.doorEvent(t)
	staff := l.mustDialLive(t, "event_staff", ev.Event.ID)
	admin := l.mustDialLive(t, "club_admin", ev.Event.ID)
	readMsg(t, staff)
	readMsg(t, admin)
	time.Sleep(150 * time.Millisecond) // re-checks pass while the scanner is staff

	start := time.Now()
	if err := l.svc.RemoveStaff(t.Context(), persona(t, "club_admin"), ev.Event.ID, persona(t, "event_staff").UserID); err != nil {
		t.Fatal(err)
	}
	if got := closeStatus(t, staff, 5*time.Second); got != realtime.CloseForbidden {
		t.Fatalf("removed scanner closed with %d, want 4403", got)
	}
	if d := time.Since(start); d > 60*time.Second {
		t.Fatalf("closed after %s", d)
	}
	for deadline := time.Now().Add(5 * time.Second); l.hub.Connections(events.LiveTopic(ev.Event.ID)) != 1; {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections, want the organiser's only", l.hub.Connections(events.LiveTopic(ev.Event.ID)))
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A new handshake of the removed scanner is refused.
	_, resp, err := l.dialLive(t, "event_staff", ev.Event.ID, "", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("removed scanner reconnect: %v %v", resp, err)
	}
}

// TestLive_SessionEnded_4401: a session that ends (logout, revocation)
// closes its board with 4401 at the next re-check.
func TestLive_SessionEnded_4401(t *testing.T) {
	t.Parallel()
	l := newLiveEnv(t, func(c *realtime.Config) { c.Recheck = 50 * time.Millisecond })
	ev := l.doorEvent(t)
	c := l.mustDialLive(t, "event_staff", ev.Event.ID)
	readMsg(t, c)
	l.actors.revoke(token(t, "event_staff"))
	if got := closeStatus(t, c, 5*time.Second); got != realtime.CloseUnauthenticated {
		t.Fatalf("closed with %d, want 4401", got)
	}
}

// TestLive_CheckinAndVoid_Messages: the snapshot of the counters first;
// after a committed check-in its message (attendee "Ayla Y.", method)
// then the fresh counters; after a void its message then the counters
// (04 §W). No registration, user or ticket id travels.
func TestLive_CheckinAndVoid_Messages(t *testing.T) {
	t.Parallel()
	l := newLiveEnv(t)
	ev := l.doorEvent(t)
	_, tk := l.ticketOf(t, adultStudent(t), ev.Event.ID)
	c := l.mustDialLive(t, "event_staff", ev.Event.ID)
	snap := readMsg(t, c)
	if snap["type"] != "snapshot" || snap["confirmed"] != 1.0 || snap["checked_in"] != 0.0 || snap["at"] == nil {
		t.Fatalf("snapshot %v", snap)
	}

	l.clk.Set(doorStart)
	res := l.scan(t, persona(t, "event_staff"), ev.Event.ID, tk.Payload)
	ci := readMsg(t, c)
	if ci["type"] != "checkin" || ci["checkin_id"] != res.CheckinID.String() || ci["attendee_display"] != "Ayla Y." ||
		ci["method"] != "qr" || len(ci) != 5 {
		t.Fatalf("checkin message %v", ci)
	}
	st := readMsg(t, c)
	if st["type"] != "stats" || st["checked_in"] != 1.0 || st["confirmed"] != 1.0 || st["voided"] != 0.0 {
		t.Fatalf("stats after the check-in %v", st)
	}
	if _, err := l.svc.VoidCheckin(t.Context(), persona(t, "club_admin"), ev.Event.ID, *res.CheckinID, "Yanlış kişi okutuldu"); err != nil {
		t.Fatal(err)
	}
	v := readMsg(t, c)
	if v["type"] != "void" || v["checkin_id"] != res.CheckinID.String() || len(v) != 3 {
		t.Fatalf("void message %v", v)
	}
	st = readMsg(t, c)
	if st["type"] != "stats" || st["checked_in"] != 0.0 || st["voided"] != 1.0 {
		t.Fatalf("stats after the void %v", st)
	}
	// Another event's board hears nothing of this one.
	other := l.doorEvent(t)
	if n := l.hub.Connections(events.LiveTopic(other.Event.ID)); n != 0 {
		t.Fatalf("other board has %d connections", n)
	}
}

// TestLiveFilter_DropsAttendeeWithoutList: a viewer who may not list the
// check-ins gets the messages without attendee_display.
func TestLiveFilter_DropsAttendeeWithoutList(t *testing.T) {
	t.Parallel()
	if f := events.LiveFilter(events.LiveBoard{Names: true}); f != nil {
		t.Fatal("a viewer with names needs no filter")
	}
	f := events.LiveFilter(events.LiveBoard{})
	out, ok := f([]byte(`{"type":"checkin","checkin_id":"x","attendee_display":"Ayla Y.","method":"qr","at":"t"}`))
	if !ok || strings.Contains(string(out), "attendee_display") || !strings.Contains(string(out), `"checkin_id":"x"`) {
		t.Fatalf("filtered %s %v", out, ok)
	}
	if out, ok := f([]byte(`{"type":"stats","checked_in":1}`)); !ok || string(out) != `{"type":"stats","checked_in":1}` {
		t.Fatalf("stats %s %v", out, ok)
	}
	if _, ok := f([]byte(`not json`)); ok {
		t.Fatal("an unreadable message must be dropped")
	}
}
