package errtrack_test

import (
	"log/slog"
	"testing"
	"time"

	"koro/internal/platform/errtrack"
)

// TestReporter_Contract: every ERRORS_PROVIDER adapter accepts any event
// (zero values included) without blocking or panicking, and Flush returns
// within its timeout. A new adapter is added to this table.
func TestReporter_Contract(t *testing.T) {
	t.Parallel()
	for name, build := range map[string]func(t *testing.T) errtrack.Reporter{
		"noop":   func(*testing.T) errtrack.Reporter { return errtrack.Noop{} },
		"sentry": func(t *testing.T) errtrack.Reporter { return newSentry(t, newGlitchtip(t)) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := build(t)
			for _, e := range []errtrack.Event{
				{},
				{Level: slog.LevelError, Message: "m", Error: "e", Attrs: map[string]string{"k": "v"}, RequestID: "r", TaskID: "t", TraceID: "x", ActorID: actorID},
				{Level: errtrack.LevelFatal, Message: "panic", Stack: []uintptr{0, 1}},
			} {
				r.Report(e)
			}
			start := time.Now()
			if !r.Flush(5 * time.Second) {
				t.Fatal("Flush timed out")
			}
			if d := time.Since(start); d > 6*time.Second {
				t.Fatalf("Flush took %v", d)
			}
		})
	}
}
