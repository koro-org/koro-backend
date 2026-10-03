//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"koro/internal/platform/testutil"
)

// redisURL is the REDIS_URL of a testutil Redis client (its logical DB).
func redisURL(c *redis.Client) string {
	return "redis://" + c.Options().Addr + "/" + strconv.Itoa(c.Options().DB)
}

// TestWorker_StartRelayScheduleAndShutdown runs `koro worker` in-process:
// it starts, schedules the platform jobs (Europe/Istanbul), relays an
// outbox row, answers its probes and shuts down cleanly on cancellation.
func TestWorker_StartRelayScheduleAndShutdown(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	rdb := testutil.NewRedis(t)
	environ := map[string]string{
		"APP_ENV":                 "dev",
		"DATABASE_URL":            testutil.AppURL(t, d),
		"DB_MAX_CONNS":            "4",
		"REDIS_URL":               redisURL(rdb),
		"WORKER_HEALTH_ADDR":      "127.0.0.1:0",
		"WORKER_SHUTDOWN_TIMEOUT": "2s",
		"OUTBOX_RELAY_IDLE":       "50ms",
		// BE-003 secrets (.env.example development placeholders).
		"CRYPTO_KEKS":            "dev1:ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGU=",
		"CRYPTO_BLIND_INDEX_KEY": "aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWQ=",
		"CRYPTO_TOKEN_KEYS":      "dev1:dG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG8=",
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runWorker(ctx, cancel, environ, pw)
		_ = pw.Close()
	}()

	var scheduled []string
	healthAddr := ""
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		switch rec["msg"] {
		case "job scheduled":
			scheduled = append(scheduled, rec["job"].(string)+" "+rec["cron"].(string))
		case "worker health listening":
			healthAddr, _ = rec["addr"].(string)
		}
		if rec["msg"] == "worker started" {
			break
		}
	}
	if healthAddr == "" {
		t.Fatalf("worker did not start: %v", <-done)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	for _, want := range []string{"outbox.cleanup 10 4 * * *", "idempotency.cleanup 5 * * * *", "webhook_inbox.purge_payloads 20 4 * * *"} {
		if !slices.Contains(scheduled, want) {
			t.Errorf("job %q not scheduled (got %q)", want, scheduled)
		}
	}

	// The scheduler is visible in Redis (asynqmon / inspector).
	insp := asynq.NewInspectorFromRedisClient(rdb)
	waitFor(t, func() bool {
		entries, err := insp.SchedulerEntries()
		if err != nil {
			return false
		}
		for _, e := range entries {
			if e.Task.Type() == "outbox.cleanup" && e.Spec == "10 4 * * *" {
				return true
			}
		}
		return false
	})

	// The relay publishes a row whose topic has no subscriber yet.
	if _, err := d.Pool.Exec(t.Context(), `INSERT INTO outbox (topic, aggregate_type, aggregate_id, payload)
		VALUES ('file.uploaded', 'file', uuidv7(), '{"file_id":"0192f5c4-0000-7000-8000-000000000001"}')`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var n int
		err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox WHERE published_at IS NOT NULL`).Scan(&n)
		return err == nil && n == 1
	})

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	get := func(path string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+healthAddr+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d", code)
	}
	if code := get("/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz = %d", code)
	}
	// `koro healthcheck -url` is the worker's Docker HEALTHCHECK.
	if code := run([]string{"healthcheck", "-url", "http://" + healthAddr + "/healthz"}, nil, io.Discard, io.Discard); code != 0 {
		t.Fatalf("koro healthcheck on the worker = %d", code)
	}

	cancel() // SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWorker() error = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("worker did not stop")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
