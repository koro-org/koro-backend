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
const AccessReasonHeader = httpx.HeaderAccessReason

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

// check counts the request; false means a response was written. A
// non-strict class that would count koro-web's cookie-free call by IP is
// skipped: GlobalIP already counted it in WebRule (web.go).
func check(api huma.API, ctx huma.Context, l Limiter, log *slog.Logger, rule Rule) bool {
	c := ctx.Context()
	k, byIP := key(ctx, rule.Key)
	if byIP && !rule.Strict && isWebCall(c) {
		return true
	}
	res, err := l.Allow(c, rule, k)
	switch {
	case err != nil && rule.Strict:
		log.LogAttrs(c, slog.LevelError, "rate limiter unavailable (fail closed)", slog.String("rule", rule.Name), slog.String("err", err.Error()))
		_ = huma.WriteErr(api, ctx, http.StatusServiceUnavailable, "", problem.ErrUnavailable)
		return false
	case err != nil:
		log.LogAttrs(c, slog.LevelWarn, "rate limiter unavailable (fail open)", slog.String("rule", rule.Name), slog.String("err", err.Error()))
		return true
	case !res.Allowed:
		action, _ := ctx.Operation().Extensions[op.ExtAction].(string)
		refused(c, log, rule, action)
		ctx.SetHeader("Retry-After", retryAfter(res.RetryAfter))
		_ = huma.WriteErr(api, ctx, http.StatusTooManyRequests, "", problem.ErrRateLimited)
		return false
	}
	return true
}

// refused logs and counts a request refused with 429 by its operation's
// rule (class). The refusal comes before the handler, so it is no verdict
// on the request (for the reauth class: no proof was looked at) and no
// audit record is written (T-322, BE-078): the log line names the rule and
// the action (actor_id and request_id come from the context), the counter
// koro_ratelimit_rejected_total{rule,action} (05 §11.4) never the actor.
// The rules that fail closed (Strict: sign-in, tokens, reauth, export,
// break-glass) are the security-relevant ones and log at WARN; so does
// koro-web's bucket (WebRule, action "unknown": it is applied before the
// route's operation), whose refusals mean public pages fail (BE-119).
func refused(ctx context.Context, log *slog.Logger, rule Rule, action string) {
	if action == "" {
		action = "unknown"
	}
	level := slog.LevelInfo
	if rule.Strict || rule.Key == KeyWeb {
		level = slog.LevelWarn
	}
	log.LogAttrs(ctx, level, "rate limited", slog.String("rule", rule.Name), slog.String("action", action))
	httpx.MetricsFrom(ctx).RateLimitRejected(rule.Name, action)
}

// key builds the counter key of a request; it holds IDs and IPs only.
// byIP reports that the key is the client IP.
func key(ctx huma.Context, kind KeyKind) (k string, byIP bool) {
	c := ctx.Context()
	switch kind {
	case KeyOrg:
		if id, err := uuid.Parse(ctx.Param("org_id")); err == nil {
			return "org:" + id.String(), false
		}
		return key(ctx, KeyActor)
	case KeyProvider:
		if p := ctx.Param("provider"); p != "" && len(p) <= 32 {
			return "provider:" + p, false
		}
	case KeyActor:
		if a := authz.ActorOrAnonymous(c); a.IsUser() {
			return "user:" + a.UserID.String(), false
		}
	}
	return ipKey(c), true
}

func ipKey(ctx context.Context) string {
	ip := httpx.ClientIPFrom(ctx)
	if ip == "" {
		ip = "unknown"
	}
	return "ip:" + ip
}

// GlobalIP is the gin middleware of global_ip (every route except the
// health probes); it fails open. typeBase is PROBLEM_TYPE_BASE. web holds
// koro-web's keys (zero value: none): a request web recognises is counted
// in WebRule instead and marked for Middleware (web.go). WebKeyHeader is
// removed from every request here, so nothing after it sees the key.
func GlobalIP(l Limiter, log *slog.Logger, typeBase string, web WebKeys) gin.HandlerFunc {
	global, webRule := Rules()[op.RateGlobalIP], WebRule()
	return func(c *gin.Context) {
		isWeb := web.webCall(c.Request)
		c.Request.Header.Del(WebKeyHeader)
		if p := c.Request.URL.Path; p == "/healthz" || p == "/readyz" {
			c.Next()
			return
		}
		ctx := c.Request.Context()
		rule, k := global, ipKey(ctx)
		if isWeb {
			ctx = withWebCall(ctx)
			c.Request = c.Request.WithContext(ctx)
			rule, k = webRule, webBucket
		}
		res, err := l.Allow(ctx, rule, k)
		switch {
		case err != nil:
			log.LogAttrs(ctx, slog.LevelWarn, "rate limiter unavailable (fail open)", slog.String("rule", rule.Name), slog.String("err", err.Error()))
		case !res.Allowed:
			if isWeb {
				refused(ctx, log, rule, "")
			}
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
