package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Worker probe settings (06 H-26).
const (
	workerProbeTimeout   = 2 * time.Second
	workerHealthShutdown = 5 * time.Second
)

// runWorker runs `koro worker` (05 §8.5, §10.9): config → logger →
// telemetry → DB → Redis → wiring → asynq server + scheduler + outbox relay
// + health endpoint → graceful shutdown in the order of 05 §10.9:
// (1) scheduler stops producing, (2) the relay stops, (3) the server drains
// (unfinished tasks go back to the queue without spending a retry),
// (4) telemetry, Redis and DB close.
func runWorker(ctx context.Context, stop context.CancelFunc, environ map[string]string, stdout io.Writer) error {
	cfg, err := config.LoadWorker(environ)
	if err != nil {
		return err
	}
	log := obs.NewLogger(stdout, obs.LoggerOptions{
		Level: cfg.LogLevel, Format: cfg.LogFormat, Service: "koro-worker", Env: cfg.Env, Version: version,
	})
	shutdownTelemetry, err := obs.SetupTelemetry(ctx, obs.TelemetryOptions{
		OTLPEndpoint: cfg.Obs.OTLPEndpoint, MetricsAddr: cfg.Obs.MetricsAddr,
		Service: "koro-worker", Version: version, Env: cfg.Env,
	})
	if err != nil {
		return err
	}

	clk := clock.Real()
	database, err := openDB(ctx, cfg.Platform.DB, "koro-worker", clk, log)
	if err != nil {
		return err
	}
	defer database.Close()
	q := cfg.Platform.Queue
	rdb, err := queue.OpenRedis(ctx, q.RedisURL.Reveal(), q.RedisPoolSize)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()
	if err := queue.CheckPersistence(ctx, rdb); err != nil {
		log.LogAttrs(ctx, slog.LevelError, "redis is not configured for asynq", slog.Bool("alarm", true), slog.String("err", err.Error()))
	}

	c := newContainer(newInfra(infra{log: log, clock: clk, version: version, cfg: cfg.Modules, db: database}, rdb, cfg.Platform))
	if err := c.build(); err != nil {
		return err
	}
	mux, err := c.workerMux(log)
	if err != nil {
		return err
	}
	w := &worker{
		log:   log,
		clock: clk,
		srv: queue.NewServer(ctx, rdb, mux, queue.ServerConfig{
			Concurrency: q.WorkerConcurrency, ShutdownTimeout: q.ShutdownTimeout, Log: log,
		}),
		relay: c.infra.outbox.NewRelay(outbox.RelayConfig{
			Batch: cfg.Platform.Outbox.RelayBatch, Idle: cfg.Platform.Outbox.RelayIdle, BackoffMax: cfg.Platform.Outbox.RelayBackoffMax,
		}),
		checks: c.readyChecks,
	}
	if q.SchedulerEnabled {
		w.sch = queue.NewScheduler(ctx, rdb, log)
		for _, j := range c.infra.jobs.Jobs() {
			if err := w.sch.Register(j); err != nil {
				return err
			}
			log.LogAttrs(ctx, slog.LevelInfo, "job scheduled",
				slog.String("job", j.Name), slog.String("cron", j.Cron), slog.String("queue", j.Queue), slog.String("tz", "Europe/Istanbul"))
		}
	}
	err = w.run(ctx, stop, q.HealthAddr, mux.Types())
	// The worker has drained: flush telemetry, then Redis and the DB close.
	err = errors.Join(err, shutdownTelemetry(context.WithoutCancel(ctx)))
	log.LogAttrs(ctx, slog.LevelInfo, "worker stopped", slog.Bool("clean", err == nil))
	return err
}

// worker is the running `koro worker` process.
type worker struct {
	log      *slog.Logger
	clock    clock.Clock
	srv      *queue.Server
	sch      *queue.Scheduler // nil when WORKER_SCHEDULER_ENABLED=false
	relay    *outbox.Relay
	checks   []httpx.Check
	stopping atomic.Bool
}

// run starts everything, waits for the signal and shuts down in order.
func (w *worker) run(ctx context.Context, stop context.CancelFunc, healthAddr string, types []string) error {
	if err := w.srv.Start(); err != nil {
		return err
	}
	if w.sch != nil {
		if err := w.sch.Start(); err != nil {
			w.srv.Shutdown()
			return err
		}
	}
	relayCtx, stopRelay := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRelay()
	var relayDone sync.WaitGroup
	relayDone.Go(func() { w.relay.Run(relayCtx) })

	health, healthErr := w.serveHealth(ctx, healthAddr)
	w.log.LogAttrs(ctx, slog.LevelInfo, "worker started",
		slog.Int("task_types", len(types)), slog.Bool("scheduler", w.sch != nil), slog.Bool("health", health != nil))

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-healthErr:
	}
	if stop != nil {
		stop() // a second SIGTERM/SIGINT kills the process at once
	}
	w.stopping.Store(true)
	w.log.LogAttrs(ctx, slog.LevelInfo, "worker shutdown started")
	if w.sch != nil {
		w.sch.Shutdown() // (1) stop producing scheduled tasks
	}
	stopRelay() // (2) stop the relay; an interrupted batch rolls back
	relayDone.Wait()
	w.srv.Shutdown() // (3) drain running tasks
	if health != nil {
		shCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerHealthShutdown)
		defer cancel()
		runErr = errors.Join(runErr, health.Shutdown(shCtx))
	}
	return runErr
}

// serveHealth serves the worker probes on addr (empty → off). Liveness
// (/healthz): the relay loop beat recently and shutdown has not started;
// the Docker HEALTHCHECK of the worker service runs
// `koro healthcheck -url http://127.0.0.1:8081/healthz` (06 H-26).
// Readiness (/readyz): DB, schema version and Redis answer.
func (w *worker) serveHealth(ctx context.Context, addr string) (*http.Server, <-chan error) {
	errCh := make(chan error, 1)
	if addr == "" {
		return nil, errCh
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		errCh <- fmt.Errorf("worker health listen %s: %w", addr, err)
		return nil, errCh
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", w.healthz)
	mux.HandleFunc("GET /readyz", w.readyz)
	srv := httpx.NewServer(mux, context.WithoutCancel(ctx), w.log)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("worker health: %w", err)
		}
	}()
	w.log.LogAttrs(ctx, slog.LevelInfo, "worker health listening", slog.String("addr", ln.Addr().String()))
	return srv, errCh
}

func (w *worker) healthz(rw http.ResponseWriter, r *http.Request) {
	err := w.relay.Alive(w.clock.Now())
	if w.stopping.Load() {
		err = errors.New("shutting down")
	}
	if err != nil {
		w.log.LogAttrs(r.Context(), slog.LevelWarn, "worker liveness failed", slog.String("err", err.Error()))
		writeStatus(rw, http.StatusServiceUnavailable, "unhealthy")
		return
	}
	writeStatus(rw, http.StatusOK, "ok")
}

func (w *worker) readyz(rw http.ResponseWriter, r *http.Request) {
	if w.stopping.Load() {
		writeStatus(rw, http.StatusServiceUnavailable, "not_ready")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), workerProbeTimeout)
	defer cancel()
	for _, chk := range slices.Clone(w.checks) {
		if err := chk.Run(ctx); err != nil {
			// The failing dependency is logged, never written (04 §2.0).
			w.log.LogAttrs(ctx, slog.LevelWarn, "readiness check failed", slog.String("check", chk.Name), slog.String("err", err.Error()))
			writeStatus(rw, http.StatusServiceUnavailable, "not_ready")
			return
		}
	}
	writeStatus(rw, http.StatusOK, "ready")
}

func writeStatus(rw http.ResponseWriter, code int, status string) {
	b, _ := json.Marshal(map[string]string{"status": status})
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(code)
	_, _ = rw.Write(b)
}
