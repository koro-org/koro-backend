//go:build integration

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"koro/db/migrations"
	"koro/internal/platform/migratekit"
	"koro/internal/platform/obs"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// TestAPI_StartServeAndShutdown runs `koro api` in-process against a
// migrated database: it starts, answers /healthz and /readyz, reports not
// ready while the schema is older than the binary, then drains on
// cancellation and returns nil.
func TestAPI_StartServeAndShutdown(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	environ := map[string]string{
		"APP_ENV":          "dev",
		"HTTP_ADDR":        "127.0.0.1:0",
		"PUBLIC_BASE_URL":  "http://localhost:3000",
		"TRUSTED_ORIGINS":  "http://localhost:3000",
		"SHUTDOWN_DELAY":   "1ms",
		"SHUTDOWN_TIMEOUT": "5s",
		"DATABASE_URL":     testutil.AppURL(t, d),
		"DB_MAX_CONNS":     "4",
		// BE-003 secrets (.env.example development placeholders).
		"PASSWORD_PEPPER":        "devpepperdevpepperdevpepperdevpepper",
		"CRYPTO_KEKS":            "dev1:ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGU=",
		"CRYPTO_BLIND_INDEX_KEY": "aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWQ=",
		"CRYPTO_TOKEN_KEYS":      "dev1:dG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG8=",
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
	get := func(path string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusOK, "/api/v1/nothing": http.StatusNotFound} {
		if got := get(path); got != want {
			t.Fatalf("GET %s = %d, want %d", path, got, want)
		}
	}

	// The schema falls behind the binary: not ready; migrated again: ready.
	m, err := migratekit.New(migrations.FS, testutil.OwnerURL(t, d), obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if err := m.Down(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz with an old schema = %d, want 503", got)
	}
	if err := m.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := get("/readyz"); got != http.StatusOK {
		t.Fatalf("/readyz after migrating = %d, want 200", got)
	}

	cancel() // SIGTERM
	if err := <-done; err != nil {
		t.Fatalf("runAPI() error = %v", err)
	}
}

// TestMigrateCommand runs `koro migrate` end to end on an empty database.
func TestMigrateCommand(t *testing.T) {
	t.Parallel()
	environ := map[string]string{"APP_ENV": "dev", "MIGRATE_DATABASE_URL": testutil.NewEmptyDB(t)}
	latest := strconv.Itoa(int(testutil.LatestMigration(t)))
	migrate := func(args ...string) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"migrate"}, args...), environ, &stdout, &stderr)
		if strings.Contains(stdout.String()+stderr.String(), "dev-only-change-me") {
			t.Fatalf("output leaks the database password")
		}
		if code != 0 {
			return code, stderr.String()
		}
		return code, strings.TrimSpace(lastLine(stdout.String()))
	}
	steps := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"version"}, 0, "0"},
		{[]string{"up"}, 0, ""},
		{[]string{"version"}, 0, latest},
		{[]string{"up"}, 0, ""}, // nothing to do
		{[]string{"down", latest}, 0, ""},
		{[]string{"version"}, 0, "0"},
		{[]string{"up"}, 0, ""},
		{[]string{"force", "3"}, 0, ""},
		{[]string{"version"}, 0, "3"},
		{[]string{"force", latest}, 0, ""},
		{[]string{"version"}, 0, latest},
		{[]string{"force", "x"}, 2, "force needs a number"},
	}
	for _, s := range steps {
		code, out := migrate(s.args...)
		mismatch := s.out != "" && out != s.out
		if code != 0 {
			mismatch = !strings.Contains(out, s.out)
		}
		if code != s.code || mismatch {
			t.Fatalf("koro migrate %v: exit %d, output %q; want %d, %q", s.args, code, out, s.code, s.out)
		}
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
