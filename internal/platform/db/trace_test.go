package db_test

import (
	"strings"
	"testing"
	"uuid"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"koro/internal/platform/db"
)

// TestDB_Tracing_RecordsSQLTemplateWithoutValues: the GORM plugin records
// one client span per statement, child of the caller's span, named by
// operation and table; bound values never reach the span.
func TestDB_Tracing_RecordsSQLTemplateWithoutValues(t *testing.T) {
	t.Parallel()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	orm := dryRun(t)
	if err := orm.Use(db.Tracing(tp)); err != nil {
		t.Fatalf("Use(Tracing) error = %v", err)
	}

	ctx, parent := tp.Tracer("test").Start(t.Context(), "events.Register")
	tx := orm.WithContext(ctx)
	id := uuid.NewV7()
	var e event
	tx.Where("title = ?", "ayse@ogrenci.edu.tr").Take(&e)
	tx.Create(&event{ID: id, Title: "Ayşe Yılmaz"})
	tx.Exec("UPDATE events SET title = ? WHERE id = ?", "ayse@ogrenci.edu.tr", id)
	parent.End()

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		byName[s.Name()] = s
	}
	for _, name := range []string{"SELECT events", "INSERT events", "UPDATE"} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("no span %q; got %v", name, keys(byName))
		}
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("span %q is not a child of the caller's span", name)
		}
		var query string
		for _, kv := range s.Attributes() {
			v := kv.Value.String()
			if strings.Contains(v, "ayse") || strings.Contains(v, "Ayşe") || strings.Contains(v, id.String()) {
				t.Errorf("span %q leaks a bound value in %s=%q", name, kv.Key, v)
			}
			if kv.Key == "db.query.text" {
				query = v
			}
		}
		if !strings.Contains(query, "$1") {
			t.Errorf("span %q query = %q, want the placeholder template", name, query)
		}
	}
	// Statements on the same handle are siblings, not nested.
	if byName["INSERT events"].Parent().SpanID() != byName["SELECT events"].Parent().SpanID() {
		t.Error("the second statement became a child of the first")
	}
}

func keys(m map[string]sdktrace.ReadOnlySpan) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
