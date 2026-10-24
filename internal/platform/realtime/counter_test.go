package realtime_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/coder/websocket"

	"koro/internal/platform/clock"
	"koro/internal/platform/realtime"
)

// replica is one API process of a two-replica test: a hub and its server.
type replica struct {
	hub *realtime.Hub
	url string
}

func newReplica(t *testing.T, bus realtime.Bus, counter realtime.Counter, mut ...func(*realtime.Config)) replica {
	t.Helper()
	h := newHub(t, append([]func(*realtime.Config){func(c *realtime.Config) {
		c.Bus, c.Counter = bus, counter
	}}, mut...)...)
	srv := httptest.NewServer(hubHandler(h, func(*http.Request) realtime.Stream { return realtime.Stream{} }))
	t.Cleanup(func() {
		h.CloseAll()
		srv.Close()
	})
	return replica{hub: h, url: srv.URL}
}

func tooMany(t *testing.T, what, url, topic string, user uuid.UUID) {
	t.Helper()
	c, resp, err := dial(t, url, topic, user)
	if err == nil {
		_ = c.CloseNow()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("%s: %v %v, want 429", what, resp, err)
	}
}

// eventually retries dial until it is admitted.
func eventually(t *testing.T, what, url, topic string, user uuid.UUID) *websocket.Conn {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		c, _, err := dial(t, url, topic, user)
		if err == nil {
			return c
		}
		select {
		case <-deadline:
			t.Fatalf("%s: never admitted: %v", what, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestHub_SharedLimits_TwoReplicas: with a shared Counter the limits hold
// for the service, not per replica (T-325): 3 per user and 50 (here 4)
// per topic over two hubs; a slot freed on A is reusable on B.
func TestHub_SharedLimits_TwoReplicas(t *testing.T) {
	t.Parallel()
	counter := realtime.NewMemCounter(clock.NewFake(t0))
	bus := realtime.NewMemBus()
	limits := func(c *realtime.Config) { c.MaxPerTopic = 4 }
	runSharedLimits(t, newReplica(t, bus, counter, limits), newReplica(t, bus, counter, limits))
	waitRecords(t, counter.Len, 0)
}

// waitRecords waits until the counter holds n records (a lease is
// released just after its connection leaves the hub).
func waitRecords(t *testing.T, records func() int, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for records() != n {
		select {
		case <-deadline:
			t.Fatalf("records = %d, want %d", records(), n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// runSharedLimits is the scenario of the two-replica tests (memory and
// Redis counters); it closes every connection it opened before returning.
func runSharedLimits(t *testing.T, a, b replica) {
	t.Helper()
	me := uuid.NewV7()
	var opened []*websocket.Conn
	open := func(r replica, topic string, user uuid.UUID) *websocket.Conn {
		c := mustDial(t, r.url, topic, user)
		opened = append(opened, c)
		return c
	}
	a1 := open(a, "board", me)
	open(a, "board", me)
	open(b, "board", me)
	tooMany(t, "user's 4th connection on A", a.url, "board", me)
	tooMany(t, "user's 4th connection on B", b.url, "board", me)
	tooMany(t, "user's 4th connection on another topic", b.url, "other", me)

	open(b, "board", uuid.NewV7()) // the topic's 4th
	tooMany(t, "topic's 5th connection on A", a.url, "board", uuid.NewV7())
	tooMany(t, "topic's 5th connection on B", b.url, "board", uuid.NewV7())
	open(a, "other", uuid.NewV7()) // another topic has its own budget

	_ = a1.Close(websocket.StatusNormalClosure, "")
	waitConns(t, a.hub, "board", 1)
	opened = append(opened, eventually(t, "the slot freed on A, reused on B", b.url, "board", me))
	for _, c := range opened {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	waitConns(t, a.hub, "board", 0)
	waitConns(t, b.hub, "board", 0)
	waitConns(t, a.hub, "other", 0)
}

// TestHub_CounterUnavailable_PerReplicaLimits: when the shared Counter
// fails, the handshake falls back to the replica's own count (fail open,
// logged) and records nothing there; the local limits still hold.
func TestHub_CounterUnavailable_PerReplicaLimits(t *testing.T) {
	t.Parallel()
	counter := realtime.NewMemCounter(clock.NewFake(t0))
	counter.Fail(errors.New("redis down"))
	r := newReplica(t, realtime.NewMemBus(), counter, func(c *realtime.Config) { c.MaxPerUser = 1 })
	me := uuid.NewV7()
	c := mustDial(t, r.url, "board", me)
	tooMany(t, "user's 2nd connection (local count)", r.url, "board", me)
	counter.Fail(nil)
	if n := counter.Len(); n != 0 {
		t.Fatalf("records of a fail-open connection = %d, want 0", n)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	waitConns(t, r.hub, "board", 0)
	eventually(t, "after the fail-open connection closed", r.url, "board", me)
}

// TestHub_Heartbeat_KeepsRecords: the replica refreshes its records every
// LeaseTTL/3, so a long connection stays counted; a record nobody
// refreshes (a crashed replica's) expires after its TTL.
func TestHub_Heartbeat_KeepsRecords(t *testing.T) {
	t.Parallel()
	counter := realtime.NewMemCounter(clock.Real())
	const ttl = 300 * time.Millisecond
	r := newReplica(t, realtime.NewMemBus(), counter, func(c *realtime.Config) { c.LeaseTTL = ttl })
	c := mustDial(t, r.url, "board", uuid.NewV7())
	waitConns(t, r.hub, "board", 1)

	dead := realtime.Slot{Topic: "board", User: uuid.NewV7(), ID: "dead-replica:1"}
	if ok, err := counter.Acquire(context.Background(), dead, 50, 3, ttl); !ok || err != nil {
		t.Fatalf("Acquire = %v, %v", ok, err)
	}
	time.Sleep(4 * ttl)
	if n := counter.Len(); n != 1 {
		t.Fatalf("records after 4 TTLs = %d, want 1 (the live connection only)", n)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	waitRecords(t, counter.Len, 0)
}
