package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/inbox"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/ratelimit"
)

// testInfra is the in-memory BE-006 infrastructure of wiring and matrix
// tests: recording queue, outbox/idempotency/inbox over memory stores with
// db.NopTx, in-memory rate limiter. Modules read c.infra.* exactly as in
// production.
func testInfra(clk clock.Clock) infra {
	log := obs.Discard()
	subs, jobs, q := outbox.NewRegistry(), queue.NewRegistry(), queue.NewMem()
	return infra{
		log: log, clock: clk, version: "test",
		queue: q, subs: subs, jobs: jobs,
		outbox:      outbox.New(outbox.Deps{Store: outbox.NewMemStore(), Tx: db.NopTx{}, Subs: subs, Queue: q, Clock: clk, Log: log}),
		idempotency: idempotency.New(idempotency.Deps{Tx: db.NopTx{}, Store: idempotency.NewMemStore(), Clock: clk, Log: log}),
		inbox:       inbox.New(inbox.Deps{Store: inbox.NewMemStore(), Tx: db.NopTx{}, Queue: q, Clock: clk, Log: log}),
		limiter:     ratelimit.NewMem(clk),
	}
}

func TestWire_PlatformTasksRegistered(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
	}
	// 01 §7 rows owned by the platform.
	want := []string{"outbox.cleanup 10 4 * * * low", "idempotency.cleanup 5 * * * * low", "webhook_inbox.purge_payloads 20 4 * * * low"}
	if !slices.Equal(jobs, want) {
		t.Fatalf("platform jobs %q, want %q", jobs, want)
	}
	mux, err := c.workerMux(obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(mux.Types(), inbox.ProcessTaskName) || !slices.Contains(mux.Types(), "outbox.cleanup") {
		t.Fatalf("worker task types %v", mux.Types())
	}
}

func TestWire_BadTaskRegistrationFailsTheBuild(t *testing.T) {
	t.Parallel()
	nop := func(context.Context, domainevent.Envelope) error { return nil }
	cases := map[string]func(c *container){
		"unknown topic": func(c *container) { c.infra.subs.Subscribe("nobody.knows", "files.scan_file", nop) },
		"job without Unique": func(c *container) {
			c.infra.jobs.Schedule(queue.Job{Name: "files.orphan_cleanup", Cron: "40 4 * * *", Queue: queue.Low,
				Handle: func(context.Context) error { return nil }})
		},
		"subscription named like a job": func(c *container) {
			c.infra.subs.Subscribe(domainevent.TopicFileUploaded, "outbox.cleanup", nop)
		},
	}
	for name, reg := range cases {
		c := newTestContainer(t)
		reg(c) // before build: tasks(c) of a module would do the same
		if err := c.build(); err == nil {
			t.Errorf("%s: build() succeeded", name)
		}
	}
}

func TestWire_RetentionHookThenConfigThenDefault(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	ctx := t.Context()
	r := c.retention("outbox_published", 0, outbox.DefaultRetention)
	if d, err := r(ctx); err != nil || d != 7*24*time.Hour {
		t.Fatalf("default = %s, %v", d, err)
	}
	if d, _ := c.retention("outbox_published", 48*time.Hour, outbox.DefaultRetention)(ctx); d != 48*time.Hour {
		t.Fatalf("configured = %s", d)
	}
	c.hooks.retention = func(_ context.Context, code string) (time.Duration, bool, error) {
		if code == "webhook_row" {
			return 0, false, nil
		}
		return 72 * time.Hour, true, nil
	}
	if d, _ := r(ctx); d != 72*time.Hour {
		t.Fatalf("from retention_policies = %s", d)
	}
	if d, _ := c.retention("webhook_row", 0, inbox.DefaultRowRetention)(ctx); d != inbox.DefaultRowRetention {
		t.Fatalf("not found in retention_policies = %s", d)
	}
	c.hooks.retention = func(context.Context, string) (time.Duration, bool, error) { return 0, false, errors.New("db down") }
	if _, err := r(ctx); err == nil {
		t.Fatal("hook error swallowed")
	}
}

func TestWire_ReadyChecksIncludeRedisOnlyWithRedis(t *testing.T) {
	t.Parallel()
	for _, chk := range newTestContainer(t).readyChecks {
		if chk.Name == "redis" {
			t.Fatal("redis readiness check without a Redis client")
		}
	}
}

// TestWorker_HealthEndpoints: /healthz follows the relay heartbeat and the
// shutdown flag; /readyz runs the dependency checks (06 H-26).
func TestWorker_HealthEndpoints(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	in := testInfra(clk)
	failing := false
	w := &worker{log: obs.Discard(), clock: clk, relay: in.outbox.NewRelay(outbox.RelayConfig{Idle: time.Millisecond}),
		checks: []httpx.Check{{Name: "db", Run: func(context.Context) error {
			if failing {
				return errors.New("db down")
			}
			return nil
		}}}}
	probe := func(h http.HandlerFunc) (int, string) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	if code, _ := probe(w.healthz); code != http.StatusServiceUnavailable {
		t.Fatalf("healthz before the relay runs = %d", code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.relay.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for w.relay.Alive(clk.Now()) != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if code, body := probe(w.healthz); code != http.StatusOK || body != `{"status":"ok"}` {
		t.Fatalf("healthz = %d %s", code, body)
	}
	if code, body := probe(w.readyz); code != http.StatusOK || body != `{"status":"ready"}` {
		t.Fatalf("readyz = %d %s", code, body)
	}
	failing = true
	if code, body := probe(w.readyz); code != http.StatusServiceUnavailable || strings.Contains(body, "db") {
		t.Fatalf("readyz with a failing check = %d %s (the dependency must not be named)", code, body)
	}
	cancel()
	<-done
	clk.Advance(w.relay.MaxBeatAge() + time.Second)
	if code, _ := probe(w.healthz); code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with a stalled relay = %d", code)
	}
	w.stopping.Store(true)
	if code, _ := probe(w.readyz); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while stopping = %d", code)
	}
}
