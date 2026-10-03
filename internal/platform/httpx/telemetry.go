package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/trace"

	"koro/internal/authz"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// unmatchedRoute is the route label of requests that matched no route
// (raw paths never become labels).
const unmatchedRoute = "unmatched"

// tracing is the otelgin server middleware (05 §11.3). Span names and the
// http.route attribute use the route template; the raw path, client IP and
// user agent otelgin records are removed by the obs exporter filter. Health
// probes are not traced.
func tracing(service string, tp trace.TracerProvider) gin.HandlerFunc {
	return otelgin.Middleware(service,
		otelgin.WithTracerProvider(tp),
		otelgin.WithPropagators(obs.Propagator()),
		otelgin.WithGinFilter(func(c *gin.Context) bool {
			p := c.FullPath()
			return p != healthzPath && p != readyzPath
		}),
	)
}

type metricsKey struct{}

// httpMetrics records koro_http_* by route template (05 §11.4) and puts m
// into the request context for the authz denial counter.
func httpMetrics(m *obs.Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now() //nolint:forbidigo // latency measurement (monotonic), not a business timestamp
		done := m.HTTPStarted()
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), metricsKey{}, m))
		c.Next()
		done()
		route := c.FullPath()
		if route == "" {
			route = unmatchedRoute
		}
		m.ObserveHTTP(c.Request.Method, route, c.Writer.Status(), time.Since(start))
	}
}

func metricsFrom(ctx context.Context) *obs.Metrics {
	if ctx == nil {
		return nil
	}
	m, _ := ctx.Value(metricsKey{}).(*obs.Metrics)
	return m
}

// Denial reasons: the reason label of koro_authz_denied_total is one of
// these fixed names (bounded cardinality; 06 H-28).
const (
	denyForbidden  = "forbidden"
	denyNotVisible = "not_visible"
	denyMFA        = "mfa_required"
	denyFourEyes   = "four_eyes"
	denyBreakGlass = "break_glass_reason"
	denyInternal   = "internal"
)

// observeDenial logs an authorization denial without personal data and
// counts it (06 H-28, T-107). The log carries the action, its module, the
// reason name and the policy's fixed rule text; actor_id and request_id
// come from the context. Programmer errors (unknown action, missing Attrs
// key) log at ERROR (03 §3.2-0).
func observeDenial(ctx context.Context, errs []error) {
	for _, err := range errs {
		d, ok := authz.DenialOf(err)
		if !ok {
			continue
		}
		module, action := "unknown", "unknown"
		if authz.KnownAction(string(d.Action)) {
			module, action = d.Action.Module(), string(d.Action)
		}
		reason := denialReason(err, d)
		level := slog.LevelInfo
		if d.Internal {
			level = slog.LevelError
		}
		loggerFrom(ctx).LogAttrs(ctx, level, "authz denied",
			slog.String("module", module), slog.String("action", action),
			slog.String("reason", reason), slog.String("rule", d.Reason))
		metricsFrom(ctx).AuthzDenied(module, action, reason)
		return
	}
}

func denialReason(err error, d *authz.Denial) string {
	switch {
	case d.Internal:
		return denyInternal
	case errors.Is(err, authz.ErrMFARequired):
		return denyMFA
	case errors.Is(err, authz.ErrFourEyes):
		return denyFourEyes
	case errors.Is(err, authz.ErrBreakGlassReason):
		return denyBreakGlass
	}
	// A module may re-wrap the denial into its own 404/403 sentinel.
	var pe *problem.Error
	if errors.As(err, &pe) && pe.Status() == http.StatusNotFound {
		return denyNotVisible
	}
	return denyForbidden
}
