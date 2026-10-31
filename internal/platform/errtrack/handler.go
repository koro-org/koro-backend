package errtrack

import (
	"context"
	"log/slog"
	"runtime"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"koro/internal/platform/obs"
)

// maxAttrValue bounds one attribute value in an Event (a panic stack is
// the longest).
const maxAttrValue = 4096

// droppedKeys never reach the tracker, not even redacted: network and
// request data (KVKK; the ticket's list: no body, header, query string,
// cookie, credential) and direct identifiers. A key matches when it is one
// of these or ends with "_" + one of them ("client_ip", "user_email").
var droppedKeys = []string{
	"ip", "ip_address", "user_agent", "ua", "url", "uri", "query", "raw_query",
	"body", "headers", "header", "cookie", "cookies", "email", "phone", "iban",
	"tckn", "address", "full_name", "first_name", "last_name", "display_name", "username",
}

// Handler is a slog.Handler that passes every record to next and reports
// the records at or above ERROR to a Reporter. The log line stays the
// source of truth: the tracker gets a redacted copy, never more.
type Handler struct {
	next  slog.Handler
	rep   Reporter
	attrs []slog.Attr // from WithAttrs, keys already qualified by groups
	group string      // "a.b." prefix from WithGroup
}

// NewHandler wraps next. A nil rep reports nothing.
func NewHandler(next slog.Handler, rep Reporter) *Handler {
	if rep == nil {
		rep = Noop{}
	}
	return &Handler{next: next, rep: rep}
}

// Enabled reports whether next logs l or l is reported.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelError || h.next.Enabled(ctx, l)
}

// Handle logs r through next (when next is enabled for it) and reports it
// when it is at ERROR or above. The error of next is returned.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.next.Enabled(ctx, r.Level) {
		err = h.next.Handle(ctx, r.Clone())
	}
	if r.Level >= slog.LevelError {
		h.rep.Report(h.event(ctx, r))
	}
	return err
}

// WithAttrs returns a handler whose records carry as.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	c := *h
	c.next = h.next.WithAttrs(as)
	c.attrs = slices.Clone(h.attrs)
	for _, a := range as {
		c.attrs = append(c.attrs, slog.Attr{Key: h.group + a.Key, Value: a.Value})
	}
	return &c
}

// WithGroup returns a handler that qualifies later attributes with name.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.next = h.next.WithGroup(name)
	c.group = h.group + name + "."
	return &c
}

// event builds the redacted Event of r.
func (h *Handler) event(ctx context.Context, r slog.Record) Event {
	e := Event{
		Time:      r.Time,
		Level:     r.Level,
		Message:   obs.Scrub(r.Message),
		Attrs:     map[string]string{},
		RequestID: obs.RequestID(ctx),
		TaskID:    obs.TaskID(ctx),
		ActorID:   obs.ActorID(ctx),
		Stack:     stackFrom(r.PC),
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		e.TraceID = sc.TraceID().String()
	}
	for _, a := range h.attrs {
		collect(e.Attrs, "", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		collect(e.Attrs, h.group, a)
		return true
	})
	for _, k := range []string{"err", "error", "panic"} {
		if v, ok := e.Attrs[h.group+k]; ok {
			e.Error = v
			delete(e.Attrs, h.group+k)
			break
		}
	}
	// The record's own correlation fields win over a stray attribute.
	for _, k := range []string{"request_id", "task_id", "actor_id", "trace_id", "span_id"} {
		delete(e.Attrs, k)
	}
	return e
}

// collect adds the redacted leaves of a to dst under prefix.
func collect(dst map[string]string, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			collect(dst, p, g)
		}
		return
	}
	if a.Key == "" || dropped(a.Key) {
		return
	}
	ra := obs.RedactAttr(a)
	v := ra.Value.String()
	if ra.Value.Kind() != slog.KindString {
		v = obs.Scrub(v) // structs, maps: Scrub is the last resort
	}
	if len(v) > maxAttrValue {
		v = v[:maxAttrValue] + "…"
	}
	dst[prefix+a.Key] = v
}

// dropped reports whether the attribute key k is one of droppedKeys.
func dropped(k string) bool {
	k = strings.ToLower(k)
	for _, d := range droppedKeys {
		if k == d || strings.HasSuffix(k, "_"+d) {
			return true
		}
	}
	return false
}

// stackFrom returns the current stack starting at the logging call pc
// (slog's Record.PC), so that slog and this handler are not part of it.
// When pc is not on the stack (a record built elsewhere), it is nil.
func stackFrom(pc uintptr) []uintptr {
	if pc == 0 {
		return nil
	}
	pcs := make([]uintptr, maxFrames)
	pcs = pcs[:runtime.Callers(1, pcs)]
	if i := slices.Index(pcs, pc); i >= 0 {
		return pcs[i:]
	}
	return nil
}
