package main

import (
	"bytes"
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
		{"migrate without command", []string{"migrate"}, 2, "missing command"},
		{"migrate unknown command", []string{"migrate", "sideways"}, 2, `unknown migrate command "sideways"`},
		{"migrate down without N", []string{"migrate", "down"}, 2, "migrate down takes 1 argument"},
		{"migrate down zero", []string{"migrate", "down", "0"}, 2, "positive step count"},
		{"migrate without config", []string{"migrate", "up"}, 1, `required environment variable "MIGRATE_DATABASE_URL" is not set`},
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

func TestMigrate_FreezeNeedsNoDatabase(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if got := run([]string{"migrate", "freeze", "9"}, map[string]string{}, &stdout, &stderr); got != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), "floor 000009") || !strings.Contains(stdout.String(), "000009_create_files.up.sql") {
		t.Fatalf("manifest = %s", stdout.String())
	}
	if got := run([]string{"migrate", "freeze", "x"}, map[string]string{}, &stdout, &stderr); got != 2 {
		t.Fatalf("freeze x exit code = %d", got)
	}
}

// TestAPI_StartupNeedsDatabase: `koro api` fails fast without a reachable
// database (05 §12.2); the full start/serve/shutdown cycle runs against a
// real database in api_integration_test.go.
func TestAPI_StartupNeedsDatabase(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"APP_ENV":         "dev",
		"HTTP_ADDR":       "127.0.0.1:0",
		"PUBLIC_BASE_URL": "http://localhost:3000",
		"TRUSTED_ORIGINS": "http://localhost:3000",
		// BE-003 secrets (.env.example development placeholders).
		"PASSWORD_PEPPER":        "devpepperdevpepperdevpepperdevpepper",
		"CRYPTO_KEKS":            "dev1:ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGU=",
		"CRYPTO_BLIND_INDEX_KEY": "aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWQ=",
		"CRYPTO_TOKEN_KEYS":      "dev1:dG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG8=",
	}
	var stderr bytes.Buffer
	if got := run([]string{"api"}, base, io.Discard, &stderr); got != 1 || !strings.Contains(stderr.String(), `"DATABASE_URL" is not set`) {
		t.Fatalf("without DATABASE_URL: exit %d, stderr %q", got, stderr.String())
	}
	unreachable := map[string]string{"DATABASE_URL": "postgres://app_rw:s3cr3t-pw@127.0.0.1:1/koro?sslmode=disable"}
	for k, v := range base {
		unreachable[k] = v
	}
	stderr.Reset()
	if got := run([]string{"api"}, unreachable, io.Discard, &stderr); got != 1 || !strings.Contains(stderr.String(), "db: ping") {
		t.Fatalf("unreachable database: exit %d, stderr %q", got, stderr.String())
	}
	if strings.Contains(stderr.String(), "s3cr3t-pw") {
		t.Fatalf("stderr leaks the database password: %q", stderr.String())
	}
}
