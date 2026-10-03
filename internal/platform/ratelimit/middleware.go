package ratelimit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// AccessReasonHeader carries the justification of a break-glass request
// (03 §6.3); with it, B operations also count against break_glass.
const AccessReasonHeader = "X-Koro-Access-Reason"

// Middleware applies the operation's x-koro-rate-limit class (op.Koro
// always writes one). Register it on the root API after authentication,
// before the routes: actor keys need the resolved actor.
func Middleware(api huma.API, l Limiter, log *slog.Logger) func(huma.Context, func(huma.Context)) {
	rules := Rules()
	breakGlass := rules[op.RateBreakGlass]
	return func(ctx huma.Context, next func(huma.Context)) {
		o := ctx.Operation()
		class, _ := o.Extensions[op.ExtRateLimit].(string)
		rule, ok := rules[op.RateLimit(class)]
		if !ok || rule.Name == string(op.RateGlobalIP) {
			next(ctx)
			return
		}
		if !check(api, ctx, l, log, rule) {
			return
		}
		if bg, _ := o.Extensions[op.ExtBreakGlass].(bool); bg && ctx.Header(AccessReasonHeader) != "" {
			if !check(api, ctx, l, log, breakGlass) {
				return
			}
		}
		next(ctx)
	}
}

// check counts the request; false means a response was written.
func check(api huma.API, ctx huma.Context, l Limiter, log *slog.Logger, rule Rule) bool {
	c := ctx.Context()
	res, err := l.Allow(c, rule, key(ctx, rule.Key))
	switch {
	case err != nil && rule.Strict:
		log.LogAttrs(c, slog.LevelError, "rate limiter unavailable (fail closed)", slog.String("rule", rule.Name), slog.String("err", err.Error()))
		_ = huma.WriteErr(api, ctx, http.StatusServiceUnavailable, "", problem.ErrUnavailable)
		return false
	case err != nil:
		log.LogAttrs(c, slog.LevelWarn, "rate limiter unavailable (fail open)", slog.String("rule", rule.Name), slog.String("err", err.Error()))
		return true
	case !res.Allowed:
		ctx.SetHeader("Retry-After", retryAfter(res.RetryAfter))
		_ = huma.WriteErr(api, ctx, http.StatusTooManyRequests, "", problem.ErrRateLimited)
		return false
	}
	return true
}

// key builds the counter key of a request; it holds IDs and IPs only.
func key(ctx huma.Context, kind KeyKind) string {
	c := ctx.Context()
	switch kind {
	case KeyOrg:
		if id, err := uuid.Parse(ctx.Param("org_id")); err == nil {
			return "org:" + id.String()
		}
		return key(ctx, KeyActor)
	case KeyProvider:
		if p := ctx.Param("provider"); p != "" && len(p) <= 32 {
			return "provider:" + p
		}
		return ipKey(c)
	case KeyActor:
		if a := authz.ActorOrAnonymous(c); a.IsUser() {
			return "user:" + a.UserID.String()
		}
	}
	return ipKey(c)
}

func ipKey(ctx context.Context) string {
	ip := httpx.ClientIPFrom(ctx)
	if ip == "" {
		ip = "unknown"
	}
	return "ip:" + ip
}

// GlobalIP is the gin middleware of global_ip (every route except the
// health probes); it fails open. typeBase is PROBLEM_TYPE_BASE.
func GlobalIP(l Limiter, log *slog.Logger, typeBase string) gin.HandlerFunc {
	rule := Rules()[op.RateGlobalIP]
	return func(c *gin.Context) {
		if p := c.Request.URL.Path; p == "/healthz" || p == "/readyz" {
			c.Next()
			return
		}
		ctx := c.Request.Context()
		res, err := l.Allow(ctx, rule, ipKey(ctx))
		switch {
		case err != nil:
			log.LogAttrs(ctx, slog.LevelWarn, "rate limiter unavailable (fail open)", slog.String("rule", rule.Name), slog.String("err", err.Error()))
		case !res.Allowed:
			p := problem.FromError(problem.ErrRateLimited, typeBase)
			if rid := obs.RequestID(ctx); rid != "" {
				p.Instance = "urn:koro:request:" + rid
			}
			b, _ := json.Marshal(p)
			c.Header("Retry-After", retryAfter(res.RetryAfter))
			c.Data(p.Status, problem.ContentType, b)
			c.Abort()
			return
		}
		c.Next()
	}
}
