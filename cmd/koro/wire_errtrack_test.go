package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"koro/internal/platform/config"
	"koro/internal/platform/errtrack"
)

// TestWire_ErrTrackOffByDefault: without SENTRY_DSN the no-op adapter is
// used and the logger is returned as is; staging and prod warn once at
// start (the name of the variable only).
func TestWire_ErrTrackOffByDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		env  string
		warn bool
	}{{config.EnvDev, false}, {config.EnvStaging, true}, {config.EnvProd, true}} {
		var buf bytes.Buffer
		base := slog.New(slog.NewJSONHandler(&buf, nil))
		log, rep, err := setupErrorTracking(t.Context(), config.ErrTrack{Provider: config.ErrorsProviderNoop, SampleRate: 1},
			config.Common{Env: tc.env}, "koro-api", base)
		if err != nil || log != base {
			t.Fatalf("%s: logger replaced or error %v", tc.env, err)
		}
		if _, ok := rep.(errtrack.Noop); !ok {
			t.Fatalf("%s: reporter = %T", tc.env, rep)
		}
		if got := strings.Contains(buf.String(), "SENTRY_DSN is unset"); got != tc.warn {
			t.Errorf("%s: warning %v, want %v: %s", tc.env, got, tc.warn, buf.String())
		}
	}
}

// TestWire_ErrTrackSentryReportsErrorLines: with a DSN the process logger
// still writes every line and sends its ERROR lines to the server.
func TestWire_ErrTrackSentryReportsErrorLines(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	dsn := strings.Replace(srv.URL, "http://", "http://examplepublickey@", 1) + "/1"
	cfg := config.ErrTrack{Provider: config.ErrorsProviderSentry, DSN: config.NewSecret(dsn), SampleRate: 1}

	var buf bytes.Buffer
	log, rep, err := setupErrorTracking(t.Context(), cfg, config.Common{Env: config.EnvDev}, "koro-worker", slog.New(slog.NewJSONHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rep.(*errtrack.Sentry); !ok {
		t.Fatalf("reporter = %T", rep)
	}
	log.InfoContext(t.Context(), "fine")
	log.ErrorContext(t.Context(), "task failed")
	if !rep.Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	if hits.Load() != 1 || !strings.Contains(buf.String(), "task failed") || !strings.Contains(buf.String(), "error tracking on") {
		t.Fatalf("hits %d, log %s", hits.Load(), buf.String())
	}
	if strings.Contains(buf.String(), "examplepublickey") {
		t.Fatalf("log leaks the DSN: %s", buf.String())
	}
}
