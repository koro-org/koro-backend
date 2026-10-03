package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

// Log output formats (LOG_FORMAT).
const (
	FormatJSON = "json"
	FormatText = "text"
)

// LoggerOptions configures NewLogger.
type LoggerOptions struct {
	Level   slog.Level
	Format  string // FormatJSON (default) or FormatText (dev only)
	Service string // koro-api, koro-worker, ...
	Env     string // dev | staging | prod
	Version string
}

// NewLogger returns a logger that writes to w, adds request/actor/task
// fields from the context and masks PII (type, key and Scrub layers).
func NewLogger(w io.Writer, o LoggerOptions) *slog.Logger {
	ho := &slog.HandlerOptions{Level: o.Level, ReplaceAttr: redactAttr}
	var h slog.Handler
	if o.Format == FormatText {
		h = slog.NewTextHandler(w, ho)
	} else {
		h = slog.NewJSONHandler(w, ho)
	}
	l := slog.New(ctxHandler{h})
	if o.Service != "" {
		l = l.With(slog.String("service", o.Service))
	}
	if o.Env != "" {
		l = l.With(slog.String("env", o.Env))
	}
	if o.Version != "" {
		l = l.With(slog.String("version", o.Version))
	}
	return l
}

// Discard returns a logger that drops everything (tests, openapi generation).
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// ctxHandler adds request_id, actor_id and task_id from the context.
// trace_id/span_id are added once OpenTelemetry is enabled (05 §11.3).
type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if v := RequestID(ctx); v != "" {
		r.AddAttrs(slog.String("request_id", v))
	}
	if v := ActorID(ctx); v != "" {
		r.AddAttrs(slog.String("actor_id", v))
	}
	if v := TaskID(ctx); v != "" {
		r.AddAttrs(slog.String("task_id", v))
	}
	if err := h.Handler.Handle(ctx, r); err != nil {
		return fmt.Errorf("obs: write log record: %w", err)
	}
	return nil
}

func (h ctxHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return ctxHandler{h.Handler.WithAttrs(a)}
}

func (h ctxHandler) WithGroup(n string) slog.Handler {
	return ctxHandler{h.Handler.WithGroup(n)}
}
