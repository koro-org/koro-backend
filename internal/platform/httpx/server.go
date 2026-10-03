package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// http.Server limits (05 §10.6; R-002 §3f).
const (
	ReadHeaderTimeout = 5 * time.Second  // slowloris (gosec G112)
	ReadTimeout       = 15 * time.Second // whole request incl. body
	WriteTimeout      = 30 * time.Second // long responses use http.ResponseController
	IdleTimeout       = 90 * time.Second // must exceed the proxy (Traefik) idle timeout
	MaxHeaderBytes    = 32 << 10
)

// NewServer returns an http.Server with explicit timeouts. baseCtx becomes
// the parent of every request context; cancelling it (hard stop) lets
// handlers observe ctx.Done() when graceful shutdown times out.
func NewServer(h http.Handler, baseCtx context.Context, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

// ServeOptions controls the shutdown sequence of Serve.
type ServeOptions struct {
	// Stop releases the signal handler (signal.NotifyContext's stop) so a
	// second SIGTERM/SIGINT kills the process immediately.
	Stop context.CancelFunc
	// Ready is flipped to true once serving and to false when shutdown
	// starts, so /readyz answers 503 and the proxy stops routing.
	Ready *Ready
	// DrainDelay is the wait between /readyz going 503 and closing the
	// listener (SHUTDOWN_DELAY), giving the proxy time to notice.
	DrainDelay time.Duration
	// ShutdownTimeout bounds waiting for in-flight requests (SHUTDOWN_TIMEOUT).
	ShutdownTimeout time.Duration
	// HardStop cancels the server's base context if draining times out.
	HardStop context.CancelFunc
	Log      *slog.Logger
}

// Serve serves on ln until ctx is cancelled, then shuts down gracefully
// (05 §10.9): signal → /readyz 503 → DrainDelay → Shutdown(ShutdownTimeout)
// (hijacked WebSocket connections are closed by RegisterOnShutdown hooks) →
// return. The caller then flushes telemetry and closes pools.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, o ServeOptions) error {
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if o.Ready != nil {
		o.Ready.Set(true)
	}

	select {
	case err := <-errCh:
		if o.Ready != nil {
			o.Ready.Set(false)
		}
		if err != nil {
			return fmt.Errorf("httpx: serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	if o.Stop != nil {
		o.Stop()
	}
	if o.Ready != nil {
		o.Ready.Set(false)
	}
	if o.Log != nil {
		o.Log.LogAttrs(ctx, slog.LevelInfo, "shutdown started",
			slog.String("drain_delay", o.DrainDelay.String()), slog.String("timeout", o.ShutdownTimeout.String()))
	}
	if o.DrainDelay > 0 {
		t := time.NewTimer(o.DrainDelay)
		<-t.C
	}

	shCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		if o.HardStop != nil {
			o.HardStop()
		}
		_ = srv.Close()
		return fmt.Errorf("httpx: graceful shutdown: %w", err)
	}
	<-errCh
	return nil
}
