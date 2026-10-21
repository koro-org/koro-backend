//go:build integration

package events_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/coder/websocket"

	"koro/internal/authz/authztest"
	"koro/internal/events"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/realtime"
	"koro/internal/platform/testutil"
)

// TestLive_ReceivesCountAfterCheckin runs the live board end to end over
// PostgreSQL and Redis with two API replicas: the board is open on replica
// B, the scan commits on replica A; after the commit A publishes on
// rt:event:<id>:checkins and B's viewer receives the check-in and the
// fresh counters read from PostgreSQL (04 §W).
func TestLive_ReceivesCountAfterCheckin(t *testing.T) {
	t.Parallel()
	rdb := testutil.NewRedis(t)
	replica := func() *realtime.Hub {
		h, err := realtime.NewHub(realtime.Config{Bus: realtime.NewRedisBus(rdb), Clock: clock.NewFake(t0), Log: obs.Discard()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.CloseAll)
		return h
	}
	hubA, hubB := replica(), replica()
	e := newPGEnv(t, func(d *events.Deps) { d.Live = hubA })
	ev, staff := e.doorEvent(t)
	_, payload := e.ticket(t, e.student(t), ev)

	// Replica B serves the board; the scanner authenticates with a bearer.
	const tok = "live-board-scanner-token"
	actors := authztest.NewResolver()
	actors.Add(tok, staff)
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: actors})
	if err != nil {
		t.Fatal(err)
	}
	events.RegisterLive(app.V1, e.svc, hubB, actors)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/events/"+ev.ID.String()+"/check-ins/live",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + tok}}})
	if err != nil {
		t.Fatalf("dial: %v %v", resp, err)
	}
	defer func() { _ = c.CloseNow() }()
	read := func() map[string]any {
		t.Helper()
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := read(); m["type"] != "snapshot" || m["confirmed"] != 1.0 || m["checked_in"] != 0.0 {
		t.Fatalf("snapshot %v", m)
	}

	res, err := e.svc.CreateCheckin(ctx, staff, ev.ID, qrInput(payload, uuid.NewV7(), t0))
	if err != nil || res.Result != events.ResultOK {
		t.Fatalf("check-in = %+v, %v", res, err)
	}
	if m := read(); m["type"] != "checkin" || m["checkin_id"] != res.CheckinID.String() || m["method"] != "qr" {
		t.Fatalf("checkin message %v", m)
	}
	if m := read(); m["type"] != "stats" || m["checked_in"] != 1.0 || m["confirmed"] != 1.0 {
		t.Fatalf("stats message %v", m)
	}
}
