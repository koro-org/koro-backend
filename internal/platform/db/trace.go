package db

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gorm.io/gorm"
)

// tracingScope is the instrumentation scope of the GORM spans.
const tracingScope = "koro/internal/platform/db"

// Tracing returns a GORM plugin that records one client span per statement
// (05 §11.3, BE-058). Install it only when tracing is on:
//
//	database.ORM.Use(db.Tracing(tp))
//
// Spans carry the operation, the table and the SQL template with its
// placeholders; bound values are never recorded (PII, 05 §11.2). A failed
// statement records its SQLSTATE, not the error text.
func Tracing(tp trace.TracerProvider) gorm.Plugin { return tracingPlugin{tr: tp.Tracer(tracingScope)} }

type tracingPlugin struct{ tr trace.Tracer }

func (tracingPlugin) Name() string { return "koro:tracing" }

func (p tracingPlugin) Initialize(d *gorm.DB) error {
	cb := d.Callback()
	return errors.Join(
		cb.Create().Before("gorm:create").Register("koro:trace_before_create", p.before),
		cb.Create().After("gorm:create").Register("koro:trace_after_create", p.after),
		cb.Query().Before("gorm:query").Register("koro:trace_before_query", p.before),
		cb.Query().After("gorm:query").Register("koro:trace_after_query", p.after),
		cb.Update().Before("gorm:update").Register("koro:trace_before_update", p.before),
		cb.Update().After("gorm:update").Register("koro:trace_after_update", p.after),
		cb.Delete().Before("gorm:delete").Register("koro:trace_before_delete", p.before),
		cb.Delete().After("gorm:delete").Register("koro:trace_after_delete", p.after),
		cb.Row().Before("gorm:row").Register("koro:trace_before_row", p.before),
		cb.Row().After("gorm:row").Register("koro:trace_after_row", p.after),
		cb.Raw().Before("gorm:raw").Register("koro:trace_before_raw", p.before),
		cb.Raw().After("gorm:raw").Register("koro:trace_after_raw", p.after),
	)
}

// spanCtx carries the statement span and restores the caller's context
// after the statement: a transaction reuses its Statement, and the next
// statement must not become a child of this one.
type spanCtx struct {
	context.Context
	parent context.Context
}

func (p tracingPlugin) before(tx *gorm.DB) {
	parent := tx.Statement.Context
	if parent == nil {
		return
	}
	ctx, _ := p.tr.Start(parent, "db", trace.WithSpanKind(trace.SpanKindClient))
	tx.Statement.Context = spanCtx{Context: ctx, parent: parent}
}

func (p tracingPlugin) after(tx *gorm.DB) {
	sc, ok := tx.Statement.Context.(spanCtx)
	if !ok {
		return
	}
	tx.Statement.Context = sc.parent
	span := trace.SpanFromContext(sc.Context)
	defer span.End()
	query := tx.Statement.SQL.String()
	op := operation(query)
	name := op
	attrs := []attribute.KeyValue{
		attribute.String("db.system.name", "postgresql"),
		attribute.String("db.operation.name", op),
		attribute.String("db.query.text", query), // placeholders only, never values
	}
	if t := tx.Statement.Table; t != "" {
		name += " " + t
		attrs = append(attrs, attribute.String("db.collection.name", t))
	}
	span.SetName(name)
	span.SetAttributes(attrs...)
	if err := tx.Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		code := "error"
		if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
			code = pe.Code
			span.SetAttributes(attribute.String("db.response.status_code", pe.Code))
		}
		span.SetStatus(codes.Error, code)
	}
}

// operation is the first SQL keyword (SELECT, INSERT, WITH, ...), bounded.
func operation(query string) string {
	f := strings.Fields(query)
	if len(f) == 0 {
		return "SQL"
	}
	w := strings.ToUpper(f[0])
	if len(w) > 16 || strings.IndexFunc(w, func(r rune) bool { return r < 'A' || r > 'Z' }) >= 0 {
		return "SQL"
	}
	return w
}
