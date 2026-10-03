package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/gin-gonic/gin"

	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
	"koro/internal/platform/ratelimit"
)

// apiVersion is the OpenAPI info.version of the v1 contract. It is not the
// binary version: the generated spec must not change on every commit.
const apiVersion = "1.0.0"

// runAPI runs `koro api`: config → logger → telemetry → DB → Redis →
// wiring → HTTP server (rate limit + Idempotency-Key middlewares) →
// graceful shutdown (05 §10.9).
// Deferred closers run in reverse order after HTTP has drained.
func runAPI(ctx context.Context, stop context.CancelFunc, environ map[string]string, stdout io.Writer) error {
	cfg, err := config.LoadAPI(environ)
	if err != nil {
		return err
	}
	log := obs.NewLogger(stdout, obs.LoggerOptions{
		Level: cfg.LogLevel, Format: cfg.LogFormat, Service: "koro-api", Env: cfg.Env, Version: version,
	})
	shutdownTelemetry, err := obs.SetupTelemetry(ctx, obs.TelemetryOptions{
		OTLPEndpoint: cfg.Obs.OTLPEndpoint, MetricsAddr: cfg.Obs.MetricsAddr,
		Service: "koro-api", Version: version, Env: cfg.Env,
	})
	if err != nil {
		return err
	}

	clk := clock.Real()
	database, err := openDB(ctx, cfg.Platform.DB, "koro-api", clk, log)
	if err != nil {
		return err
	}
	defer database.Close() // after HTTP has drained (deferred closers run last)
	rdb, err := queue.OpenRedis(ctx, cfg.Platform.Queue.RedisURL.Reveal(), cfg.Platform.Queue.RedisPoolSize)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	c := newContainer(newInfra(infra{log: log, clock: clk, version: version, cfg: cfg.Modules, db: database}, rdb, cfg.Platform))
	if err := c.build(); err != nil {
		return err
	}
	var global []gin.HandlerFunc
	if cfg.RateLimit.Enabled {
		//nolint:contextcheck // the middleware uses each request's context, not ctx
		global = append(global, ratelimit.GlobalIP(c.infra.limiter, log, cfg.HTTP.ProblemTypeBase))
	}

	ready := httpx.NewReady(c.readyChecks...)
	//nolint:contextcheck // New installs process-wide error factories once; request contexts come from huma, not from ctx
	app, err := httpx.New(httpx.Deps{
		Log:             log,
		Version:         apiVersion,
		ProblemTypeBase: cfg.HTTP.ProblemTypeBase,
		TrustedOrigins:  cfg.HTTP.TrustedOrigins,
		TrustedProxies:  cfg.HTTP.TrustedProxies,
		RequestTimeout:  cfg.HTTP.RequestTimeout,
		ExposeOpenAPI:   cfg.HTTP.ExposeOpenAPI,
		Actors:          c.actors,
		Ready:           ready,
		Middleware:      global,
	})
	if err != nil {
		return err
	}
	c.apiMiddleware(app.API, cfg.RateLimit.Enabled) //nolint:contextcheck // the middlewares use each request's context, not ctx
	c.registerRoutes(app.V1)

	// Requests keep the signal context's values but not its cancellation;
	// hardStop cancels them only if graceful shutdown times out.
	baseCtx, hardStop := context.WithCancel(context.WithoutCancel(ctx))
	defer hardStop()
	srv := httpx.NewServer(app.Handler(), baseCtx, log)
	for _, f := range c.shutdownHooks {
		srv.RegisterOnShutdown(f)
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.HTTP.Addr, err)
	}
	log.LogAttrs(ctx, slog.LevelInfo, "api listening", slog.String("addr", ln.Addr().String()))

	err = httpx.Serve(ctx, srv, ln, httpx.ServeOptions{
		Stop:            stop,
		Ready:           ready,
		DrainDelay:      cfg.HTTP.ShutdownDelay,
		ShutdownTimeout: cfg.HTTP.ShutdownTimeout,
		HardStop:        hardStop,
		Log:             log,
	})
	// HTTP has drained: flush telemetry now, then the deferred closers run.
	err = errors.Join(err, shutdownTelemetry(context.WithoutCancel(ctx)))
	log.LogAttrs(ctx, slog.LevelInfo, "api stopped", slog.Bool("clean", err == nil))
	return err
}
