package httpx

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"
	"uuid"

	"github.com/gin-gonic/gin"

	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// RequestIDHeader is echoed on every response (04 §1.5).
const RequestIDHeader = "X-Request-ID"

var reqIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// injectLogger stores the App logger in the request context (see loggerFrom).
func injectLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(withLogger(c.Request.Context(), log))
		c.Next()
	}
}

// requestID accepts a well-formed incoming X-Request-ID or generates a
// UUIDv7, echoes it and stores it in the context (05 §10.8).
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !reqIDRe.MatchString(id) {
			id = uuid.NewV7().String()
		}
		c.Header(RequestIDHeader, id)
		c.Request = c.Request.WithContext(obs.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

// accessLog writes one line per request with the route template, never the
// raw path, query values, bodies or credential headers (05 §11.1).
func accessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now() //nolint:forbidigo // latency measurement (monotonic), not a business timestamp
		c.Next()
		status := c.Writer.Status()
		route := c.FullPath()
		if (route == healthzPath || route == readyzPath) && status < http.StatusInternalServerError {
			return
		}
		if route == "" {
			route = "unmatched"
		}
		ctx := c.Request.Context()
		log.LogAttrs(ctx, slog.LevelInfo, "http request",
			slog.String("method", c.Request.Method),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Int("bytes", c.Writer.Size()),
			slog.String("client_ip", ClientIPFrom(ctx)),
		)
	}
}

// recoverProblem turns panics into a 500 problem+json and logs them once.
func recoverProblem(log *slog.Logger, base string) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, rec any) {
		ctx := c.Request.Context()
		log.LogAttrs(ctx, slog.LevelError, "panic recovered",
			slog.String("panic", fmt.Sprint(rec)),
			slog.String("stack", string(debug.Stack())),
		)
		respondProblem(c, problem.ForStatus(http.StatusInternalServerError, "", base))
	})
}

// securityHeaders sets the response headers of 05 §10.8. Every response is
// no-store: v1 has no cacheable API responses (04 §1.13: no conditional GET).
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		c.Next()
	}
}

type clientIPKey struct{}

// clientIP stores gin's ClientIP (X-Forwarded-For honoured only from
// TRUSTED_PROXIES) in the context; raw X-Forwarded-For is never read
// elsewhere (05 §10.1).
func clientIP() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), clientIPKey{}, c.ClientIP()))
		c.Next()
	}
}

// ClientIPFrom returns the client IP stored by the middleware or "".
func ClientIPFrom(ctx context.Context) string {
	v, _ := ctx.Value(clientIPKey{}).(string)
	return v
}

// crossOrigin applies http.CrossOriginProtection (05 §10.7); rejections are
// 403 csrf_rejected problems.
func crossOrigin(cop *http.CrossOriginProtection, base string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := cop.Check(c.Request); err != nil {
			respondProblem(c, problem.ForCode(problem.CodeCSRFRejected, http.StatusForbidden, "", base))
			return
		}
		c.Next()
	}
}

// requestTimeout bounds every request context (05 §10.6). WebSocket
// upgrades are exempt: they are long-lived by design.
func requestTimeout(d time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d <= 0 || strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
			c.Next()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), d)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
