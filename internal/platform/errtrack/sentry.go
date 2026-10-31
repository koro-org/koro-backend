package errtrack

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"time"

	"github.com/getsentry/sentry-go"

	"koro/internal/platform/obs"
)

// SentryOptions configures NewSentry.
type SentryOptions struct {
	// DSN is the project's Sentry-protocol DSN (GlitchTip or Sentry):
	// https://<public key>@<host>/<project id>.
	DSN string
	// Environment (APP_ENV), Release (the binary version) and Service
	// (koro-api, koro-worker; sent as the server name, never the host name).
	Environment, Release, Service string
	// SampleRate is the share of events sent, in (0, 1]; 0 means 1.
	SampleRate float64
}

// Sentry reports to a Sentry-protocol server through sentry-go. It owns its
// client: the SDK's global hub is never touched, and no SDK integration that
// reads requests (sentryhttp, sentrygin) is installed.
type Sentry struct {
	client *sentry.Client
}

// NewSentry builds the adapter. Sending is asynchronous; Flush waits.
func NewSentry(o SentryOptions) (*Sentry, error) {
	if o.DSN == "" {
		return nil, errors.New("errtrack: sentry adapter needs a DSN")
	}
	if _, err := sentry.NewDsn(o.DSN); err != nil {
		return nil, errors.New("errtrack: SENTRY_DSN is not a valid DSN") // the DSN holds the key: not echoed
	}
	c, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:         o.DSN,
		Environment: o.Environment,
		Release:     o.Release,
		ServerName:  o.Service,
		SampleRate:  o.SampleRate,
		// Automatic collection off: no user info, cookies, headers, bodies
		// or query parameters, should an integration ever read a request.
		DataCollection: &sentry.DataCollection{
			UserInfo:    sentry.Set(false),
			Cookies:     off(),
			HTTPHeaders: &sentry.HeaderCollectionConfig{Request: off(), Response: off()},
			HTTPBodies:  []sentry.BodyType{},
			QueryParams: off(),
		},
		MaxBreadcrumbs: -1, // no breadcrumbs: they would replay earlier log lines and requests
		BeforeSend: func(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			return scrubEvent(e)
		},
		// Modules and the runtime/OS context only; nothing that reads
		// requests or the environment of the process.
		Integrations: func(in []sentry.Integration) []sentry.Integration {
			return slices.DeleteFunc(in, func(i sentry.Integration) bool {
				return i.Name() != "Modules" && i.Name() != "Environment"
			})
		},
	})
	if err != nil {
		return nil, fmt.Errorf("errtrack: sentry client: %w", err)
	}
	return &Sentry{client: c}, nil
}

// off is a collection behaviour that collects nothing.
func off() *sentry.KeyValueCollectionBehavior {
	return &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff}
}

// Report queues e.
func (s *Sentry) Report(e Event) {
	s.client.CaptureEvent(toSentry(e), nil, nil)
}

// Flush waits up to timeout for queued events.
func (s *Sentry) Flush(timeout time.Duration) bool { return s.client.Flush(timeout) }

// toSentry maps an Event onto the Sentry protocol: the message is the
// exception type (the issue title, grouped with the stack), the error text
// its value; correlation IDs are tags; the attributes are one context.
func toSentry(e Event) *sentry.Event {
	ev := sentry.NewEvent()
	ev.Timestamp = e.Time
	ev.Level = sentry.LevelError
	if e.Level >= LevelFatal {
		ev.Level = sentry.LevelFatal
	}
	ev.Logger = "slog"
	ev.Message = e.Message
	if e.Error != "" || len(e.Stack) > 0 {
		ev.Exception = []sentry.Exception{{
			Type:       e.Message,
			Value:      e.Error,
			Stacktrace: stacktrace(e.Stack),
		}}
	}
	for k, v := range map[string]string{
		"request_id": e.RequestID, "task_id": e.TaskID, "trace_id": e.TraceID,
	} {
		if v != "" {
			ev.Tags[k] = v
		}
	}
	if e.ActorID != "" {
		ev.User = sentry.User{ID: e.ActorID}
	}
	if len(e.Attrs) > 0 {
		attrs := make(sentry.Context, len(e.Attrs))
		for k, v := range e.Attrs {
			attrs[k] = v
		}
		ev.Contexts["log"] = attrs
	}
	return ev
}

// stacktrace turns program counters (innermost first) into Sentry frames
// (outermost first). Inside a recovered panic the frames from
// runtime.gopanic inwards (the recovery code) are cut, so the panicking
// function is the top frame.
func stacktrace(pcs []uintptr) *sentry.Stacktrace {
	if len(pcs) == 0 {
		return nil
	}
	var frames []runtime.Frame
	it := runtime.CallersFrames(pcs)
	for {
		f, more := it.Next()
		frames = append(frames, f)
		if !more {
			break
		}
	}
	if i := slices.IndexFunc(frames, func(f runtime.Frame) bool { return f.Function == "runtime.gopanic" }); i >= 0 {
		frames = frames[i+1:]
	}
	out := make([]sentry.Frame, 0, len(frames))
	for _, f := range slices.Backward(frames) {
		out = append(out, sentry.NewFrame(f))
	}
	return &sentry.Stacktrace{Frames: out}
}

// scrubEvent is the BeforeSend hook: the last gate before the network,
// whatever built the event. It removes what Koro never sends (the request
// with its URL, query, headers, cookies and body; breadcrumbs; attachments;
// every user field but the ID; frame variables) and passes the message,
// exception texts, tags and contexts through the log redaction once more.
func scrubEvent(e *sentry.Event) *sentry.Event {
	if e == nil {
		return nil
	}
	e.Request = nil
	e.Breadcrumbs = nil
	e.User = sentry.User{ID: e.User.ID}
	e.Transaction = "" // may be a raw path
	e.Attachments = nil
	e.Message = obs.Scrub(e.Message)
	for i := range e.Exception {
		e.Exception[i].Type = obs.Scrub(e.Exception[i].Type)
		e.Exception[i].Value = obs.Scrub(e.Exception[i].Value)
		if st := e.Exception[i].Stacktrace; st != nil {
			for j := range st.Frames {
				st.Frames[j].Vars = nil
			}
		}
	}
	for k, v := range e.Tags {
		if dropped(k) {
			delete(e.Tags, k)
			continue
		}
		e.Tags[k] = redactString(k, v)
	}
	for name, c := range e.Contexts {
		e.Contexts[name] = scrubContext(c)
	}
	return e
}

// scrubContext redacts a context's values by key, recursively.
func scrubContext(c sentry.Context) sentry.Context {
	for k, v := range c {
		if dropped(k) {
			delete(c, k)
			continue
		}
		switch x := v.(type) {
		case string:
			c[k] = redactString(k, x)
		case map[string]any:
			c[k] = scrubContext(x)
		case nil, bool, int, int64, uint64, float64:
		default:
			c[k] = redactString(k, fmt.Sprint(x))
		}
	}
	return c
}

// redactString applies the log rules for the key k to v.
func redactString(k, v string) string {
	return obs.RedactAttr(slog.String(k, v)).Value.String()
}
