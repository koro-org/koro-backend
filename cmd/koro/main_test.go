package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRun_Dispatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		wantCode  int
		wantInErr string
	}{
		{"no command", nil, 2, "usage: koro"},
		{"unknown command", []string{"serve"}, 2, `unknown command "serve"`},
		{"help", []string{"help"}, 0, ""},
		{"worker not yet", []string{"worker"}, 1, "not implemented yet (BE-006)"},
		{"migrate not yet", []string{"migrate", "up"}, 1, "not implemented yet (BE-002)"},
		{"seed not yet", []string{"seed"}, 1, "not implemented yet"},
		{"api without config", []string{"api"}, 1, `required environment variable "APP_ENV" is not set`},
		{"healthcheck bad flag", []string{"healthcheck", "-nope"}, 2, "usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if got := run(tt.args, map[string]string{}, &stdout, &stderr); got != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", got, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantInErr) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.wantInErr)
			}
		})
	}
}

func TestHealthcheck_ExitCodes(t *testing.T) {
	t.Parallel()
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	if got := run([]string{"healthcheck", "-url", srv.URL + "/healthz"}, nil, io.Discard, io.Discard); got != 0 {
		t.Fatalf("healthy exit code = %d", got)
	}
	if got := run([]string{"healthcheck", "-url", srv.URL + "/missing"}, nil, io.Discard, io.Discard); got != 1 {
		t.Fatalf("unhealthy exit code = %d", got)
	}
	if got := run([]string{"healthcheck"}, map[string]string{"HTTP_ADDR": "127.0.0.1:1"}, io.Discard, io.Discard); got != 1 {
		t.Fatalf("unreachable exit code = %d", got)
	}
}

func TestHealthURL(t *testing.T) {
	t.Parallel()
	tests := []struct{ addr, want string }{
		{":8080", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:9000", "http://127.0.0.1:9000/healthz"},
		{"[::]:8080", "http://127.0.0.1:8080/healthz"},
		{"10.1.2.3:8080", "http://10.1.2.3:8080/healthz"},
	}
	for _, tt := range tests {
		got, err := healthURL(tt.addr)
		if err != nil || got != tt.want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", tt.addr, got, err, tt.want)
		}
	}
	if _, err := healthURL("8080"); err == nil {
		t.Error("healthURL(8080) must fail")
	}
}

// TestAPI_StartServeAndShutdown runs `koro api` in-process: it starts,
// answers /healthz and /readyz, then drains on cancellation and returns nil.
func TestAPI_StartServeAndShutdown(t *testing.T) {
	t.Parallel()
	environ := map[string]string{
		"APP_ENV":          "dev",
		"HTTP_ADDR":        "127.0.0.1:0",
		"PUBLIC_BASE_URL":  "http://localhost:3000",
		"TRUSTED_ORIGINS":  "http://localhost:3000",
		"SHUTDOWN_DELAY":   "1ms",
		"SHUTDOWN_TIMEOUT": "5s",
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runAPI(ctx, cancel, environ, pw)
		_ = pw.Close()
	}()

	// Wait for the "api listening" log line and read the bound address.
	var addr string
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec["msg"] == "api listening" {
			addr, _ = rec["addr"].(string)
			break
		}
	}
	if addr == "" {
		t.Fatalf("api did not start: %v", <-done)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()

	// No keep-alive: a spare pooled connection that never sends a request
	// would make Shutdown wait (net/http treats it as busy for 5 s).
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusOK, "/api/v1/nothing": http.StatusNotFound} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}

	cancel() // SIGTERM
	if err := <-done; err != nil {
		t.Fatalf("runAPI() error = %v", err)
	}
}
