package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/trace"

	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// telemetryShutdownTimeout bounds the final span flush and the metrics
// listener shutdown.
const telemetryShutdownTimeout = 10 * time.Second

// backupLastSuccess is the "last backup age" hook (BE-058): the ticket that
// owns backups sets it from its wire file (e.g. the time of the newest
// verified dump). nil → koro_backup_last_success_age_seconds is not
// exported.
var backupLastSuccess func(ctx context.Context) (time.Time, error)

// setupTelemetry starts the exporters of one process (05 §11.3–11.4). With
// OTEL_EXPORTER_OTLP_ENDPOINT and METRICS_ADDR empty nothing is installed.
func setupTelemetry(ctx context.Context, cfg config.Obs, service, env string, log *slog.Logger) (*obs.Telemetry, error) {
	return obs.SetupTelemetry(ctx, obs.TelemetryOptions{
		OTLPEndpoint: cfg.OTLPEndpoint, MetricsAddr: cfg.MetricsAddr, SampleRatio: cfg.SampleRatio,
		Service: service, Version: version, Env: env, Log: log,
	})
}

// shutdownTelemetry flushes spans and stops the metrics listener; called
// after HTTP / the worker has drained (05 §10.9).
func shutdownTelemetry(ctx context.Context, tel *obs.Telemetry) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryShutdownTimeout)
	defer cancel()
	return tel.Shutdown(ctx)
}

// instrumentDB adds the GORM spans (tracing on) and the pool gauge
// koro_db_pool_connections{state} (metrics on).
func instrumentDB(tel *obs.Telemetry, d *db.DB) error {
	if tel.Tracing() {
		if err := d.ORM.Use(db.Tracing(tel.TracerProvider())); err != nil {
			return err
		}
	}
	return tel.Metrics.GaugeFunc("koro_db_pool_connections", "Database pool connections by state.", []string{"state"},
		func(context.Context) ([]obs.Sample, error) {
			s := d.Pool.Stat()
			return []obs.Sample{
				{Labels: []string{"acquired"}, Value: float64(s.AcquiredConns())},
				{Labels: []string{"idle"}, Value: float64(s.IdleConns())},
				{Labels: []string{"total"}, Value: float64(s.TotalConns())},
				{Labels: []string{"max"}, Value: float64(s.MaxConns())},
			}, nil
		})
}

// tracerProvider returns the provider for otelgin, nil when tracing is off
// (no middleware).
func tracerProvider(tel *obs.Telemetry) trace.TracerProvider {
	if !tel.Tracing() {
		return nil
	}
	return tel.TracerProvider()
}

// taskTracer returns the worker's task tracer, nil when tracing is off.
func taskTracer(tel *obs.Telemetry) trace.Tracer {
	if !tel.Tracing() {
		return nil
	}
	return tel.TracerProvider().Tracer("koro/internal/platform/queue")
}

// workerGauges registers the worker-only scrape-time gauges: queue length,
// outbox backlog and the last backup age hook. The worker owns the relay
// and the queues, so the API does not report them twice.
func workerGauges(tel *obs.Telemetry, d *db.DB, rdb redis.UniversalClient, clk clock.Clock) error {
	m := tel.Metrics
	if m == nil {
		return nil
	}
	in := queue.NewInspector(rdb)
	if err := m.GaugeFunc("koro_queue_length", "Background tasks by queue and state.", []string{"queue", "state"},
		func(context.Context) ([]obs.Sample, error) {
			ls, err := in.Lengths()
			if err != nil {
				return nil, err
			}
			out := make([]obs.Sample, 0, len(ls))
			for _, l := range ls {
				out = append(out, obs.Sample{Labels: []string{l.Queue, l.State}, Value: float64(l.N)})
			}
			return out, nil
		}); err != nil {
		return err
	}
	backlog := func(pick func(outbox.Backlog) float64) func(context.Context) ([]obs.Sample, error) {
		return func(ctx context.Context) ([]obs.Sample, error) {
			b, err := outbox.ReadBacklog(ctx, d.ORM, clk.Now())
			if err != nil {
				return nil, err
			}
			return []obs.Sample{{Value: pick(b)}}, nil
		}
	}
	for _, g := range []struct {
		name, help string
		pick       func(outbox.Backlog) float64
	}{
		{"koro_outbox_pending", "Unpublished outbox rows.", func(b outbox.Backlog) float64 { return float64(b.Pending) }},
		{"koro_outbox_oldest_pending_seconds", "Age of the oldest unpublished outbox row (outbox lag).",
			func(b outbox.Backlog) float64 { return b.OldestPending.Seconds() }},
		{"koro_outbox_dead", "Unpublished outbox rows that failed at least outbox.AlarmAttempts times.",
			func(b outbox.Backlog) float64 { return float64(b.Dead) }},
	} {
		if err := m.GaugeFunc(g.name, g.help, nil, backlog(g.pick)); err != nil {
			return err
		}
	}
	if hook := backupLastSuccess; hook != nil {
		return m.GaugeFunc("koro_backup_last_success_age_seconds", "Seconds since the last successful backup.", nil,
			func(ctx context.Context) ([]obs.Sample, error) {
				at, err := hook(ctx)
				if err != nil {
					return nil, err
				}
				return []obs.Sample{{Value: clk.Now().Sub(at).Seconds()}}, nil
			})
	}
	return nil
}
