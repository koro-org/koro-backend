//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"koro/internal/platform/testutil"
)

// startLogged runs a koro subcommand in-process and returns the "addr"
// fields of the wanted log lines once stop appears.
func startLogged(t *testing.T, run func(ctx context.Context, cancel context.CancelFunc, w io.Writer) error, stop string, want ...string) (map[string]string, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cancel, pw)
		_ = pw.Close()
	}()
	addrs := map[string]string{}
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		msg, _ := rec["msg"].(string)
		for _, w := range want {
			if msg == w {
				addrs[w], _ = rec["addr"].(string)
			}
		}
		if msg == stop {
			break
		}
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	for _, w := range want {
		if addrs[w] == "" {
			cancel()
			t.Fatalf("%q not logged: %v", w, <-done)
		}
	}
	return addrs, cancel, done
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func telemetryEnv(t *testing.T) map[string]string {
	t.Helper()
	d := testutil.NewDB(t)
	return map[string]string{
		"APP_ENV":                 "dev",
		"HTTP_ADDR":               "127.0.0.1:0",
		"PUBLIC_BASE_URL":         "http://localhost:3000",
		"TRUSTED_ORIGINS":         "http://localhost:3000",
		"SHUTDOWN_DELAY":          "1ms",
		"SHUTDOWN_TIMEOUT":        "5s",
		"DATABASE_URL":            testutil.AppURL(t, d),
		"DB_MAX_CONNS":            "4",
		"REDIS_URL":               redisURL(testutil.NewRedis(t)),
		"METRICS_ADDR":            "127.0.0.1:0",
		"WORKER_HEALTH_ADDR":      "127.0.0.1:0",
		"WORKER_SHUTDOWN_TIMEOUT": "2s",
		"OUTBOX_RELAY_IDLE":       "50ms",
		// BE-003 secrets (.env.example development placeholders).
		"PASSWORD_PEPPER":        "devpepperdevpepperdevpepperdevpepper",
		"CRYPTO_KEKS":            "dev1:ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGU=",
		"CRYPTO_BLIND_INDEX_KEY": "aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWQ=",
		"CRYPTO_TOKEN_KEYS":      "dev1:dG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG8=",
	}
}

// TestAPI_MetricsOnlyOnInternalListener runs `koro api` with METRICS_ADDR:
// the public listener answers 404 for /metrics, the internal listener
// serves the koro_* metrics (HTTP by route template, DB pool).
func TestAPI_MetricsOnlyOnInternalListener(t *testing.T) {
	t.Parallel()
	environ := telemetryEnv(t)
	addrs, cancel, done := startLogged(t, func(ctx context.Context, cancel context.CancelFunc, w io.Writer) error {
		return runAPI(ctx, cancel, environ, w)
	}, "api listening", "api listening", "metrics listening")
	defer cancel()
	public, internal := addrs["api listening"], addrs["metrics listening"]
	if public == internal {
		t.Fatalf("metrics share the public listener %s", public)
	}

	if code, _ := httpGet(t, "http://"+public+"/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz = %d", code)
	}
	httpGet(t, "http://"+public+"/api/v1/nothing/ayse@ogrenci.edu.tr")
	for _, path := range []string{"/metrics", "/api/v1/metrics"} {
		if code, body := httpGet(t, "http://"+public+path); code != http.StatusNotFound || strings.Contains(body, "koro_") {
			t.Fatalf("public GET %s = %d %s, want 404", path, code, body)
		}
	}
	code, body := httpGet(t, "http://"+internal+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("internal GET /metrics = %d", code)
	}
	for _, want := range []string{
		`koro_http_requests_total{method="GET",route="/healthz",status="200"} 1`,
		`koro_http_requests_total{method="GET",route="unmatched",status="404"}`,
		`koro_db_pool_connections{state="max"} 4`,
		`koro_http_in_flight_requests`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(body, "ayse") {
		t.Error("metrics leak the raw path")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runAPI() error = %v", err)
	}
}

// TestWorker_MetricsEndpoint runs `koro worker` with METRICS_ADDR: the
// internal listener reports queue length and outbox lag; the health
// listener (BE-006, 06 H-26) stays probes-only.
func TestWorker_MetricsEndpoint(t *testing.T) {
	t.Parallel()
	environ := telemetryEnv(t)
	addrs, cancel, done := startLogged(t, func(ctx context.Context, cancel context.CancelFunc, w io.Writer) error {
		return runWorker(ctx, cancel, environ, w)
	}, "worker started", "worker health listening", "metrics listening")
	defer cancel()

	if code, _ := httpGet(t, "http://"+addrs["worker health listening"]+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("health listener GET /metrics = %d, want 404", code)
	}
	code, body := httpGet(t, "http://"+addrs["metrics listening"]+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("internal GET /metrics = %d", code)
	}
	for _, want := range []string{
		`koro_queue_length{queue="critical",state="pending"}`,
		`koro_queue_length{queue="low",state="archived"}`,
		`koro_outbox_pending 0`,
		`koro_outbox_dead 0`,
		`koro_outbox_oldest_pending_seconds 0`,
		`koro_db_pool_connections{state="total"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("worker metrics lack %q", want)
		}
	}
	if strings.Contains(body, "koro_backup_last_success_age_seconds") {
		t.Error("backup age is exported although no backup hook is set")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runWorker() error = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("worker did not stop")
	}
}
