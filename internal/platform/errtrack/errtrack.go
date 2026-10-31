// Package errtrack reports errors to an error tracker (OPS-014; teknik rapor
// §3.1, §7.7: GlitchTip, which speaks the Sentry protocol).
//
// The process log is the only source: NewHandler wraps the slog handler of
// koro api / koro worker and turns every record at ERROR or above into one
// Event. Panics recovered by the HTTP stack and the task mux, 5xx failures
// and alarms are already logged once at ERROR, so nothing is captured twice
// and no call site imports this package. ReportPanic covers a panic that
// reaches the process entry point.
//
// KVKK (05 §11.2): an Event carries no more than the log line. Attribute
// values pass obs.RedactAttr and obs.Scrub; network and request data (IP,
// User-Agent, URL, query, body, headers, cookies) are dropped by key; the
// user is the actor ID only. The Sentry adapter strips the same fields once
// more in its BeforeSend hook (scrubEvent), whatever the SDK added.
//
// Adapters (ERRORS_PROVIDER, config.ErrTrack): Noop (off; the default
// while SENTRY_DSN is empty) and Sentry (GlitchTip or Sentry).
package errtrack

import (
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"koro/internal/platform/obs"
)

// LevelFatal marks an Event of a panic that ends the process.
const LevelFatal = slog.LevelError + 4

// Event is one reported error. Every string is already redacted.
type Event struct {
	// Time is the log record's time; zero lets the adapter stamp it.
	Time    time.Time
	Level   slog.Level
	Message string
	// Error is the redacted text of the record's "err" (or "error",
	// "panic") attribute; empty when the record has none.
	Error string
	// Attrs are the remaining attributes (groups flattened as "a.b").
	Attrs map[string]string
	// Correlation with the log line (05 §11.1). ActorID is the acting
	// user's UUID: the only user field an Event ever carries.
	RequestID, TaskID, TraceID, ActorID string
	// Stack holds the program counters of the logging call, innermost
	// first (runtime.Callers); nil when unknown.
	Stack []uintptr
}

// Reporter is the error tracker port. Report must not block on the
// network; Flush waits up to timeout for queued events and reports whether
// all were sent.
type Reporter interface {
	Report(e Event)
	Flush(timeout time.Duration) bool
}

// Noop is the disabled tracker (ERRORS_PROVIDER=noop, SENTRY_DSN empty).
type Noop struct{}

// Report drops e.
func (Noop) Report(Event) {}

// Flush has nothing to wait for.
func (Noop) Flush(time.Duration) bool { return true }

// ReportPanic is deferred by a process entry point: a panic that reaches it
// is reported as fatal, flushed for up to timeout and then continues, so
// the process still crashes with its stack on stderr.
func ReportPanic(r Reporter, timeout time.Duration) {
	v := recover()
	if v == nil {
		return
	}
	r.Report(Event{
		Level:   LevelFatal,
		Message: "panic",
		Error:   obs.Scrub(fmt.Sprint(v)),
		Stack:   callers(),
	})
	r.Flush(timeout)
	panic(v)
}

// maxFrames bounds the captured stack.
const maxFrames = 64

// callers returns the stack of its caller's caller, innermost first.
func callers() []uintptr {
	pcs := make([]uintptr, maxFrames)
	return pcs[:runtime.Callers(3, pcs)]
}
