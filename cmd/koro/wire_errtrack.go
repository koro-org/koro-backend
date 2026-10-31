package main

import (
	"context"
	"log/slog"
	"time"

	"koro/internal/platform/config"
	"koro/internal/platform/errtrack"
)

// errorFlushTimeout bounds the wait for queued error events at shutdown
// and after a fatal panic.
const errorFlushTimeout = 2 * time.Second

// errorReporters is the ERRORS_PROVIDER selection map (OPS-014, BE-113
// adapter rule).
func errorReporters(cfg config.ErrTrack, service, env string) adapters[errtrack.Reporter] {
	return adapters[errtrack.Reporter]{
		config.ErrorsProviderNoop: func() (errtrack.Reporter, error) { return errtrack.Noop{}, nil },
		config.ErrorsProviderSentry: func() (errtrack.Reporter, error) {
			return errtrack.NewSentry(errtrack.SentryOptions{
				DSN: cfg.DSN.Reveal(), Environment: env, Release: version, Service: service, SampleRate: cfg.SampleRate,
			})
		},
	}
}

// setupErrorTracking builds the configured tracker and returns the process
// logger that also reports its ERROR records to it (errtrack.Handler; the
// log line itself is unchanged). The caller defers
// errtrack.ReportPanic(rep, errorFlushTimeout) and rep.Flush.
func setupErrorTracking(ctx context.Context, cfg config.ErrTrack, c config.Common, service string, log *slog.Logger) (*slog.Logger, errtrack.Reporter, error) {
	rep, err := errorReporters(cfg, service, c.Env).pick("ERRORS_PROVIDER", cfg.Provider)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.Enabled() {
		if c.ProdLike() {
			log.LogAttrs(ctx, slog.LevelWarn, "error tracking is off: SENTRY_DSN is unset (errors are only logged)")
		}
		return log, rep, nil
	}
	log = slog.New(errtrack.NewHandler(log.Handler(), rep))
	log.LogAttrs(ctx, slog.LevelInfo, "error tracking on", slog.String("provider", cfg.Provider))
	return log, rep, nil
}
