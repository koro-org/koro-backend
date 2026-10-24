// Package realtime is the WebSocket hub (01 §1.3, 04 §W, 05 §10.7, §10.9):
// server → client streams over github.com/coder/websocket, fanned out
// between API replicas by Redis pub/sub (Bus). MVP's only stream is the
// live check-in board of events (BE-054).
//
// Rules the hub enforces:
//
//   - The handshake is a protected GET: the Origin is checked before
//     anything else (CrossOriginProtection never sees a GET, so a missing
//     check is Cross-Site WebSocket Hijacking; a request without Origin is
//     a native client and passes), then the credential (session cookie or
//     bearer header; never the query string), then the stream's own
//     authorisation. A refusal is a plain RFC 9457 answer (401/403/404,
//     429 over the limits, 503 while shutting down): nothing is upgraded.
//   - Limits: at most MaxPerTopic connections per topic and MaxPerUser per
//     user across every replica when a shared Counter is configured (Redis;
//     T-325), each replica's own count otherwise; a client sends nothing but
//     ping/pong (any data message closes with 1008, frames above
//     MaxMessageBytes with 1009); a client that does not answer a ping
//     within PingTimeout or cannot keep up with SendBuffer messages is
//     dropped.
//   - Re-authorisation: every connection re-validates its session and its
//     authorisation every Recheck (at most 60 seconds) and before a message
//     when its last check is older than Fresh; a lost session closes with
//     4401, a lost authorisation with 4403.
//   - Shutdown: http.Server.Shutdown neither waits for nor closes hijacked
//     connections; CloseAll (registered with RegisterOnShutdown) closes
//     every connection with 1001 and refuses new ones.
//
// Messages are opaque JSON produced by the publishing module; the hub
// never logs them (they may carry an attendee's display name).
package realtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/coder/websocket"

	"koro/internal/platform/clock"
	"koro/internal/platform/problem"
)

// Close codes of a revoked connection (04 §W).
const (
	// CloseUnauthenticated: the session ended (expired, revoked, logout).
	CloseUnauthenticated websocket.StatusCode = 4401
	// CloseForbidden: the authorisation was lost (e.g. the staff member was
	// removed from the event).
	CloseForbidden websocket.StatusCode = 4403
)

// Limits and timings (04 §W). Config overrides them (tests shorten them).
const (
	DefaultMaxPerTopic  = 50
	DefaultMaxPerUser   = 3
	MaxRecheck          = 60 * time.Second // the longest Recheck interval allowed
	DefaultFresh        = 10 * time.Second
	DefaultPingInterval = 25 * time.Second
	DefaultPingTimeout  = 10 * time.Second
	DefaultWriteTimeout = 10 * time.Second
	DefaultSendBuffer   = 32
	// MaxMessageBytes bounds a client frame: clients send only ping/pong.
	MaxMessageBytes = 512
	// closeWait bounds CloseAll's wait for the close handshakes.
	closeWait = 6 * time.Second
	// DefaultLeaseTTL is how long a connection's record in the shared
	// Counter outlives its last heartbeat (every LeaseTTL/3): the records of
	// a replica that dies free their slots within it.
	DefaultLeaseTTL = 45 * time.Second
	// counterTimeout bounds one call to the shared Counter.
	counterTimeout = 2 * time.Second
)

// ErrTooManyConnections answers a handshake over MaxPerTopic or MaxPerUser
// (429 rate_limited).
var ErrTooManyConnections = problem.ErrRateLimited

// Config configures a Hub. Bus, Clock and Log are required; zero limits
// and timings take the defaults above.
type Config struct {
	Bus   Bus
	Clock clock.Clock
	Log   *slog.Logger
	// TrustedOrigins are the browser origins (scheme://host[:port]) allowed
	// besides the API's own host: TRUSTED_ORIGINS, as for
	// CrossOriginProtection (05 §10.7).
	TrustedOrigins []string

	MaxPerTopic int
	MaxPerUser  int
	// Counter shares the limits between the replicas (T-325); nil counts
	// per replica (a warning when the hub starts listening). When the Counter fails, the
	// handshake falls back to the replica's own count (fail open, logged):
	// the limits protect capacity, the board must not go down with Redis.
	Counter Counter
	// LeaseTTL is the lifetime of a record in Counter without a heartbeat
	// (default DefaultLeaseTTL; the heartbeat runs every LeaseTTL/3).
	LeaseTTL time.Duration
	// Recheck is the re-authorisation period (default and maximum
	// MaxRecheck).
	Recheck time.Duration
	// Fresh: a message to a connection checked longer ago re-checks first.
	Fresh        time.Duration
	PingInterval time.Duration
	PingTimeout  time.Duration
	WriteTimeout time.Duration
	SendBuffer   int
}

func (c *Config) defaults() error {
	if c.Bus == nil || c.Clock == nil || c.Log == nil {
		return errors.New("realtime: Config.Bus, Clock and Log are required")
	}
	if c.Recheck > MaxRecheck {
		return fmt.Errorf("realtime: Recheck %s exceeds %s (04 §W)", c.Recheck, MaxRecheck)
	}
	setInt := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	setDur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	setInt(&c.MaxPerTopic, DefaultMaxPerTopic)
	setInt(&c.MaxPerUser, DefaultMaxPerUser)
	setInt(&c.SendBuffer, DefaultSendBuffer)
	setDur(&c.Recheck, MaxRecheck)
	setDur(&c.Fresh, DefaultFresh)
	setDur(&c.PingInterval, DefaultPingInterval)
	setDur(&c.PingTimeout, DefaultPingTimeout)
	setDur(&c.WriteTimeout, DefaultWriteTimeout)
	setDur(&c.LeaseTTL, DefaultLeaseTTL)
	return nil
}

// Hub holds the WebSocket connections of one API process.
type Hub struct {
	cfg      Config
	origins  map[string]bool // lower-case scheme://host of TrustedOrigins
	patterns []string        // the same as websocket.AcceptOptions.OriginPatterns

	mu      sync.Mutex // closing, the counters, the leases and the clients
	closing bool
	topicN  map[string]int
	userN   map[uuid.UUID]int
	clients map[string]map[*client]struct{}
	running sync.WaitGroup
	// replica names this hub's records in the shared Counter; seq numbers
	// them; shared holds the leases recorded there (the heartbeat's).
	replica string
	seq     uint64
	shared  map[*Lease]struct{}

	// listenMu guards the bus subscription; it is never held with mu (the
	// bus delivers under its own locks into deliver, which takes mu).
	listenMu   sync.Mutex
	listening  bool
	stopListen context.CancelFunc
}

// NewHub returns a hub; nothing runs until the first connection.
func NewHub(cfg Config) (*Hub, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	h := &Hub{cfg: cfg, origins: map[string]bool{}, topicN: map[string]int{}, userN: map[uuid.UUID]int{},
		clients: map[string]map[*client]struct{}{}, replica: uuid.NewV7().String(), shared: map[*Lease]struct{}{}}
	for _, o := range cfg.TrustedOrigins {
		o = strings.TrimSuffix(strings.TrimSpace(o), "/")
		u, err := url.Parse(o)
		if o == "" || err != nil || u.Host == "" || u.Scheme == "" {
			return nil, fmt.Errorf("realtime: trusted origin %q is not scheme://host[:port]", o)
		}
		key := strings.ToLower(u.Scheme + "://" + u.Host)
		if !h.origins[key] {
			h.origins[key] = true
			h.patterns = append(h.patterns, escapePattern(key))
		}
	}
	return h, nil
}

// escapePattern quotes path.Match's metacharacters ("[::1]" in a host).
func escapePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(s)
}

// CheckOrigin is the Cross-Site WebSocket Hijacking check of a handshake
// (05 §10.7): no Origin (a native client), the API's own host or a trusted
// origin pass; anything else is 403.
func (h *Hub) CheckOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return problem.ErrForbidden
	}
	if strings.EqualFold(u.Host, r.Host) || h.origins[strings.ToLower(u.Scheme+"://"+u.Host)] {
		return nil
	}
	return problem.ErrForbidden
}

// Publish sends msg to every connection of topic on every replica.
func (h *Hub) Publish(ctx context.Context, topic string, msg []byte) error {
	return h.cfg.Bus.Publish(ctx, ChannelPrefix+topic, msg)
}

// Lease is an admitted connection slot; Serve releases it.
type Lease struct {
	h     *Hub
	topic string
	user  uuid.UUID
	slot  Slot // its record in the shared Counter
	once  sync.Once
}

// Release frees the slot (idempotent): the replica's count, then the
// shared record (a failure is logged; the record expires within LeaseTTL).
// ctx's values are kept, its cancellation is not (the request has ended).
func (l *Lease) Release(ctx context.Context) {
	l.once.Do(func() {
		h := l.h
		shared := h.releaseLocal(l)
		if !shared {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), counterTimeout)
		defer cancel()
		if err := h.cfg.Counter.Release(ctx, l.slot); err != nil {
			h.cfg.Log.LogAttrs(ctx, slog.LevelWarn, "realtime counter release failed", slog.Any("err", err))
		}
	})
}

// releaseLocal frees l's slot in the replica's count; it reports whether
// l was recorded in the shared Counter.
func (h *Hub) releaseLocal(l *Lease) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.topicN[l.topic]--; h.topicN[l.topic] <= 0 {
		delete(h.topicN, l.topic)
	}
	if h.userN[l.user]--; h.userN[l.user] <= 0 {
		delete(h.userN, l.user)
	}
	_, shared := h.shared[l]
	delete(h.shared, l)
	return shared
}

// Admit reserves a slot for user on topic before the upgrade: 429 over the
// limits, 503 while shutting down or without the bus subscription. ctx's
// values (not its cancellation) carry the hub's subscription.
//
// The replica's own count is checked first (it never exceeds the shared
// one), then the shared Counter outside the hub's lock; a refusal there
// undoes the local reservation.
func (h *Hub) Admit(ctx context.Context, topic string, user uuid.UUID) (*Lease, error) {
	if err := h.listen(ctx); err != nil {
		return nil, err
	}
	l, err := h.reserve(topic, user)
	if err != nil || h.cfg.Counter == nil {
		return l, err
	}
	cctx, cancel := context.WithTimeout(ctx, counterTimeout)
	ok, err := h.cfg.Counter.Acquire(cctx, l.slot, h.cfg.MaxPerTopic, h.cfg.MaxPerUser, h.cfg.LeaseTTL)
	cancel()
	switch {
	case err != nil: // fail open on the replica's own count
		h.cfg.Log.LogAttrs(ctx, slog.LevelWarn, "realtime counter unavailable, per-replica limits apply",
			slog.Any("err", err))
		return l, nil
	case !ok:
		l.Release(ctx) // not shared yet: only the local reservation is undone
		return nil, ErrTooManyConnections
	}
	h.mu.Lock()
	h.shared[l] = struct{}{}
	h.mu.Unlock()
	return l, nil
}

// reserve takes a slot in the replica's own count.
func (h *Hub) reserve(topic string, user uuid.UUID) (*Lease, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closing:
		return nil, problem.ErrUnavailable
	case h.topicN[topic] >= h.cfg.MaxPerTopic, h.userN[user] >= h.cfg.MaxPerUser:
		return nil, ErrTooManyConnections
	}
	h.topicN[topic]++
	h.userN[user]++
	h.seq++
	id := h.replica + ":" + strconv.FormatUint(h.seq, 10)
	return &Lease{h: h, topic: topic, user: user, slot: Slot{Topic: topic, User: user, ID: id}}, nil
}

// heartbeat refreshes the replica's shared records every LeaseTTL/3 until
// ctx ends (the bus subscription's lifetime).
func (h *Hub) heartbeat(ctx context.Context) {
	t := time.NewTicker(h.cfg.LeaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		h.mu.Lock()
		slots := make([]Slot, 0, len(h.shared))
		for l := range h.shared {
			slots = append(slots, l.slot)
		}
		h.mu.Unlock()
		if len(slots) == 0 {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, counterTimeout)
		if err := h.cfg.Counter.Refresh(rctx, slots, h.cfg.LeaseTTL); err != nil {
			h.cfg.Log.LogAttrs(ctx, slog.LevelWarn, "realtime counter heartbeat failed", slog.Any("err", err))
		}
		cancel()
	}
}

// listen subscribes the hub to the bus once (retried on the next
// connection after a failure).
func (h *Hub) listen(ctx context.Context) error {
	if h.isClosing() {
		return problem.ErrUnavailable
	}
	h.listenMu.Lock()
	defer h.listenMu.Unlock()
	if h.listening {
		return nil
	}
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := h.cfg.Bus.Subscribe(lctx, h.deliver); err != nil {
		cancel()
		h.cfg.Log.LogAttrs(ctx, slog.LevelWarn, "realtime subscribe failed", slog.Any("err", err))
		return problem.ErrUnavailable
	}
	if h.isClosing() { // CloseAll ran meanwhile: do not keep a subscription
		cancel()
		return problem.ErrUnavailable
	}
	h.listening, h.stopListen = true, cancel
	if h.cfg.Counter != nil {
		go h.heartbeat(lctx)
	} else {
		h.cfg.Log.LogAttrs(ctx, slog.LevelWarn,
			"realtime connection limits are per replica: no shared counter (Redis) configured")
	}
	return nil
}

func (h *Hub) isClosing() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closing
}

// deliver hands a bus message to the local connections of its topic; a
// connection whose buffer is full is dropped (1013), never waited for.
func (h *Hub) deliver(channel string, payload []byte) {
	topic, ok := strings.CutPrefix(channel, ChannelPrefix)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients[topic] {
		select {
		case c.send <- payload:
		default:
			c.kick(websocket.StatusTryAgainLater, "too slow")
		}
	}
}

// Stream is what a connection serves.
type Stream struct {
	// Snapshot is the first message, read after the connection joined its
	// topic (a change in between arrives after it; the boards carry
	// absolute counters). Snapshot and Filter run on the connection's own
	// goroutine, one at a time: they may share per-connection state.
	Snapshot func(ctx context.Context) ([]byte, error)
	// Filter rewrites or drops (false) a message for this connection; nil
	// sends every message as published.
	Filter func(msg []byte) ([]byte, bool)
	// Recheck re-validates the session and the authorisation:
	// problem.ErrUnauthenticated closes with 4401, any other client error
	// (403, 404) with 4403, another error with 1011.
	Recheck func(ctx context.Context) error
}

type closeReq struct {
	code   websocket.StatusCode
	reason string
}

type client struct {
	conn  *websocket.Conn
	send  chan []byte
	kicks chan closeReq
}

func (c *client) kick(code websocket.StatusCode, reason string) {
	select {
	case c.kicks <- closeReq{code, reason}:
	default:
	}
}

// Serve upgrades the admitted handshake and serves s until the connection
// ends; it releases l. A failed upgrade has been answered by the websocket
// library (it re-checks the Origin against TrustedOrigins).
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, l *Lease, s Stream) {
	defer l.Release(r.Context())
	// The server's read/write deadlines (httpx.ReadTimeout, WriteTimeout)
	// stay on a hijacked connection: clear them before the upgrade.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.patterns})
	if err != nil {
		h.cfg.Log.LogAttrs(r.Context(), slog.LevelInfo, "websocket upgrade failed", slog.Any("err", err))
		return
	}
	conn.SetReadLimit(MaxMessageBytes)
	c := &client{conn: conn, send: make(chan []byte, h.cfg.SendBuffer), kicks: make(chan closeReq, 1)}
	if !h.join(l.topic, c) {
		_ = conn.Close(websocket.StatusGoingAway, "server restarting")
		return
	}
	defer h.leave(l.topic, c)
	// After the upgrade the request context lives as long as this handler;
	// keep its values (request ID, logger), drop its cancellation.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	h.run(ctx, c, s)
}

func (h *Hub) join(topic string, c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	if h.clients[topic] == nil {
		h.clients[topic] = map[*client]struct{}{}
	}
	h.clients[topic][c] = struct{}{}
	h.running.Add(1)
	return true
}

func (h *Hub) leave(topic string, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients[topic], c)
	if len(h.clients[topic]) == 0 {
		delete(h.clients, topic)
	}
	h.running.Done()
}

// errDataMessage: the client sent a data message (only ping/pong allowed).
var errDataMessage = errors.New("realtime: data message from client")

func (h *Hub) run(ctx context.Context, c *client, s Stream) {
	defer func() { _ = c.conn.CloseNow() }()
	reads := make(chan error, 1)
	go func() {
		// Read also answers pings and handles the close handshake.
		_, _, err := c.conn.Read(ctx)
		if err == nil {
			err = errDataMessage
		}
		reads <- err
	}()

	verified := h.cfg.Clock.Now()
	if !h.firstMessage(ctx, c, s) {
		return
	}
	ping := time.NewTicker(h.cfg.PingInterval)
	defer ping.Stop()
	recheck := time.NewTicker(h.cfg.Recheck)
	defer recheck.Stop()
	for {
		select {
		case err := <-reads:
			if errors.Is(err, errDataMessage) {
				h.close(c, websocket.StatusPolicyViolation, "only ping and pong are accepted")
			}
			return
		case k := <-c.kicks:
			h.close(c, k.code, k.reason)
			return
		case <-recheck.C:
			if !h.recheck(ctx, c, s) {
				return
			}
			verified = h.cfg.Clock.Now()
		case <-ping.C:
			pctx, done := context.WithTimeout(ctx, h.cfg.PingTimeout)
			err := c.conn.Ping(pctx)
			done()
			if err != nil {
				return
			}
		case msg := <-c.send:
			if h.cfg.Clock.Now().Sub(verified) >= h.cfg.Fresh {
				if !h.recheck(ctx, c, s) {
					return
				}
				verified = h.cfg.Clock.Now()
			}
			if s.Filter != nil {
				var ok bool
				if msg, ok = s.Filter(msg); !ok {
					continue
				}
			}
			if !h.write(ctx, c, msg) {
				return
			}
		}
	}
}

func (h *Hub) firstMessage(ctx context.Context, c *client, s Stream) bool {
	if s.Snapshot == nil {
		return true
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		h.cfg.Log.LogAttrs(ctx, slog.LevelWarn, "realtime snapshot failed", slog.Any("err", err))
		h.close(c, websocket.StatusInternalError, "unavailable")
		return false
	}
	return h.write(ctx, c, snap)
}

func (h *Hub) write(ctx context.Context, c *client, msg []byte) bool {
	wctx, done := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	defer done()
	return c.conn.Write(wctx, websocket.MessageText, msg) == nil
}

// recheck runs s.Recheck and closes the connection when it fails.
func (h *Hub) recheck(ctx context.Context, c *client, s Stream) bool {
	if s.Recheck == nil {
		return true
	}
	rctx, done := context.WithTimeout(ctx, h.cfg.WriteTimeout)
	err := s.Recheck(rctx)
	done()
	code, reason := CloseFor(err)
	if code == 0 {
		return true
	}
	if code == websocket.StatusInternalError {
		h.cfg.Log.LogAttrs(ctx, slog.LevelWarn, "realtime recheck failed", slog.Any("err", err))
	}
	h.close(c, code, reason)
	return false
}

// CloseFor maps a Recheck error to the close code and reason (0: keep).
func CloseFor(err error) (websocket.StatusCode, string) {
	switch {
	case err == nil:
		return 0, ""
	case errors.Is(err, problem.ErrUnauthenticated):
		return CloseUnauthenticated, "unauthenticated"
	case problem.IsClientError(err):
		return CloseForbidden, "forbidden"
	}
	return websocket.StatusInternalError, "unavailable"
}

// close runs the close handshake (bounded by the library: 5 s per side).
func (h *Hub) close(c *client, code websocket.StatusCode, reason string) {
	_ = c.conn.Close(code, reason)
}

// CloseAll closes every connection with 1001 and refuses new ones; it
// waits up to a few seconds for the close handshakes. Register it with
// http.Server.RegisterOnShutdown (05 §10.9).
func (h *Hub) CloseAll() {
	h.mu.Lock()
	h.closing = true
	for _, cs := range h.clients {
		for c := range cs {
			c.kick(websocket.StatusGoingAway, "server restarting")
		}
	}
	h.mu.Unlock()
	h.listenMu.Lock()
	if h.stopListen != nil {
		h.stopListen()
	}
	h.listenMu.Unlock()

	done := make(chan struct{})
	go func() {
		h.running.Wait()
		close(done)
	}()
	t := time.NewTimer(closeWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// Connections returns the number of open connections of topic on this
// replica (tests, metrics).
func (h *Hub) Connections(topic string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients[topic])
}
