//go:build integration

package queue_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

// TestServer_Telemetry_RecordsTaskMetricsSpansAndQueueLength: with metrics
// and a tracer the worker counts each attempt by queue, type and result,
// records one consumer span per task (no payload), and the inspector
// reports queue lengths per state.
func TestServer_Telemetry_RecordsTaskMetricsSpansAndQueueLength(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rdb := testutil.NewRedis(t)
	log := obs.Discard()
	client := queue.NewClient(rdb, log)
	t.Cleanup(func() { _ = client.Close() })

	m := obs.NewMetrics()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	mux := queue.NewMux(log)
	_ = mux.Handle("notify.send_email", func(context.Context, []byte) error { return nil })
	_ = mux.Handle("payments.capture_payment", func(context.Context, []byte) error {
		return queue.Permanent(errors.New("payer ayse@ogrenci.edu.tr unknown"))
	})
	srv := queue.NewServer(ctx, rdb, mux, queue.ServerConfig{Concurrency: 2, ShutdownTimeout: time.Second, Log: log,
		Metrics: m, Tracer: tp.Tracer("test")})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	if err := client.Enqueue(ctx, "notify.send_email", map[string]string{"to": "ayse@ogrenci.edu.tr"}, "n1:notify.send_email"); err != nil {
		t.Fatal(err)
	}
	if err := client.Enqueue(ctx, "payments.capture_payment", map[string]string{"payment_id": "p1"}, "p1:payments.capture_payment",
		queue.Queue(queue.Critical), queue.MaxRetry(queue.MoneyMaxRetry)); err != nil {
		t.Fatal(err)
	}

	scrape := func() string {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, obs.MetricsPath, nil))
		b, _ := io.ReadAll(rec.Body)
		return string(b)
	}
	want := []string{
		`koro_queue_tasks_processed_total{queue="default",result="ok",type="notify.send_email"} 1`,
		`koro_queue_tasks_processed_total{queue="critical",result="failed",type="payments.capture_payment"} 1`,
		`koro_queue_task_duration_seconds_count{queue="default",type="notify.send_email"} 1`,
	}
	waitUntil(t, func() bool {
		out := scrape()
		for _, w := range want {
			if !strings.Contains(out, w) {
				return false
			}
		}
		return true
	})
	if out := scrape(); strings.Contains(out, "ayse") {
		t.Fatalf("metrics leak the payload:\n%s", out)
	}

	waitUntil(t, func() bool { return len(spans.Ended()) >= 2 })
	for _, s := range spans.Ended() {
		if s.Name() != "asynq notify.send_email" && s.Name() != "asynq payments.capture_payment" {
			t.Errorf("span name = %q", s.Name())
		}
		for _, kv := range s.Attributes() {
			if strings.Contains(kv.Value.String(), "ayse") {
				t.Errorf("span %q leaks the payload in %s", s.Name(), kv.Key)
			}
		}
		if s.Name() == "asynq payments.capture_payment" && s.Status().Description != "task failed" {
			t.Errorf("failed task span status = %+v", s.Status())
		}
	}

	in := queue.NewInspector(rdb)
	waitUntil(t, func() bool {
		ls, err := in.Lengths()
		if err != nil || len(ls) != 15 {
			return false
		}
		for _, l := range ls {
			if l.Queue == queue.Critical && l.State == "archived" {
				return l.N == 1
			}
		}
		return false
	})
}
