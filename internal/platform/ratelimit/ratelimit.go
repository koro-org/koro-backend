// Package ratelimit limits request rates (05 §10.5, 04 §1.9): the Limiter
// interface with a Redis implementation (go-redis/redis_rate, GCRA) and an
// in-memory fake, the class table of 04 §1.9, the huma middleware that
// applies an operation's x-koro-rate-limit class and the gin middleware of
// the global per-IP limit, which also recognises koro-web's cookie-free
// server calls and counts them in a bucket of their own (web.go, BE-119).
//
// Failure policy: a Strict class fails closed (limiter unavailable → 503
// unavailable); the others fail open with a warning. A rejected request is
// 429 rate_limited with Retry-After (seconds).
//
// Account-level protection (failed login counter, back-off) is identity's
// database logic (05 §14.1); the IP limit does not replace it. Classes whose
// key includes request data the middleware cannot see — auth_email's e-mail
// digest, reauth's "failed attempts only" — are additionally applied by the
// owning service through Limiter.Allow (BE-014, BE-016, BE-018).
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"

	"koro/internal/platform/clock"
	"koro/internal/platform/httpx/op"
)

// KeyKind selects what a rule counts per (04 §1.9 "Anahtar").
type KeyKind string

// Key kinds.
const (
	KeyIP       KeyKind = "ip"       // client IP from a trusted proxy (httpx.ClientIPFrom)
	KeyActor    KeyKind = "actor"    // authenticated user; anonymous callers by IP
	KeyOrg      KeyKind = "org"      // path parameter org_id; actor without it
	KeyProvider KeyKind = "provider" // path parameter provider (webhooks); IP without it
	KeyWeb      KeyKind = "web"      // koro-web's cookie-free calls as one caller (WebRule, BE-119)
)

// Rule is one rate limit class.
type Rule struct {
	Name   string
	Rate   int           // requests per Per
	Per    time.Duration //
	Burst  int           // 0 → Rate
	Strict bool          // fail closed when the limiter is unavailable
	Key    KeyKind
}

func (r Rule) burst() int {
	if r.Burst > 0 {
		return r.Burst
	}
	return r.Rate
}

// Result is the outcome of one Allow.
type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration // when not allowed
}

// Limiter counts one request of key under rule.
type Limiter interface {
	Allow(ctx context.Context, rule Rule, key string) (Result, error)
}

// Rules returns every class of 04 §1.9 (initial values; tuned by
// measurement, 05 §18.2-10).
func Rules() map[op.RateLimit]Rule {
	const minute, hour = time.Minute, time.Hour
	rs := []struct {
		class  op.RateLimit
		rate   int
		per    time.Duration
		key    KeyKind
		strict bool
	}{
		{op.RateGlobalIP, 300, minute, KeyIP, false},
		{op.RateAuthLogin, 10, minute, KeyIP, true},
		{op.RateAuthRegister, 5, hour, KeyIP, true},
		{op.RateAuthPasswordReset, 5, hour, KeyIP, true},
		{op.RateAuthEmail, 3, hour, KeyIP, true},
		{op.RateTokenConfirm, 20, hour, KeyIP, true},
		{op.RateReauth, 5, 15 * minute, KeyActor, true},
		{op.RateReadDefault, 600, minute, KeyActor, false},
		{op.RateWriteDefault, 120, minute, KeyActor, false},
		{op.RateSearch, 60, minute, KeyActor, false},
		{op.RateAudienceCount, 30, minute, KeyOrg, false},
		{op.RateContactRequest, 20, hour, KeyOrg, false},
		{op.RateQRScan, 120, minute, KeyActor, false},
		{op.RateTicketResend, 3, hour, KeyActor, false},
		{op.RateExport, 5, hour, KeyActor, true},
		{op.RateBreakGlass, 30, hour, KeyActor, true},
		{op.RateUpload, 60, hour, KeyActor, false},
		{op.RateWebhook, 600, minute, KeyProvider, false},
		{op.RateMessageSend, 30, minute, KeyActor, false}, // BE-162: sendMyConversationMessage, sendOrgConversationMessage
	}
	out := make(map[op.RateLimit]Rule, len(rs))
	for _, r := range rs {
		out[r.class] = Rule{Name: string(r.class), Rate: r.rate, Per: r.per, Strict: r.strict, Key: r.key}
	}
	return out
}

// NewRedis returns the Redis (GCRA) limiter on the shared pool.
func NewRedis(rdb redis.UniversalClient) Limiter { return redisLimiter{l: redis_rate.NewLimiter(rdb)} }

type redisLimiter struct{ l *redis_rate.Limiter }

func (r redisLimiter) Allow(ctx context.Context, rule Rule, key string) (Result, error) {
	res, err := r.l.Allow(ctx, "rl:"+rule.Name+":"+key, redis_rate.Limit{Rate: rule.Rate, Burst: rule.burst(), Period: rule.Per})
	if err != nil {
		return Result{}, fmt.Errorf("ratelimit: %w", err)
	}
	return Result{Allowed: res.Allowed > 0, Remaining: res.Remaining, RetryAfter: res.RetryAfter}, nil
}

// Mem is the in-memory limiter of tests and of the wiring without Redis: a
// fixed window per (rule, key). Fail simulates an unavailable Redis.
type Mem struct {
	clock clock.Clock
	mu    sync.Mutex
	win   map[string]*window
	fail  error
	calls []string // "rule|key"
}

type window struct {
	start time.Time
	n     int
}

// NewMem returns an empty in-memory limiter.
func NewMem(clk clock.Clock) *Mem { return &Mem{clock: clk, win: map[string]*window{}} }

// Fail makes Allow return err (nil restores it).
func (m *Mem) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

// Calls returns "rule|key" of every Allow call in order.
func (m *Mem) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// Allow implements Limiter.
func (m *Mem) Allow(_ context.Context, rule Rule, key string) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, rule.Name+"|"+key)
	if m.fail != nil {
		return Result{}, m.fail
	}
	now := m.clock.Now()
	k := rule.Name + "|" + key
	w := m.win[k]
	if w == nil || !now.Before(w.start.Add(rule.Per)) {
		w = &window{start: now}
		m.win[k] = w
	}
	if w.n >= rule.burst() {
		return Result{RetryAfter: w.start.Add(rule.Per).Sub(now)}, nil
	}
	w.n++
	return Result{Allowed: true, Remaining: rule.burst() - w.n}, nil
}

// ErrUnavailable is what a test limiter returns to simulate a Redis outage.
var ErrUnavailable = errors.New("ratelimit: limiter unavailable")

// retryAfter formats Retry-After in whole seconds (at least 1).
func retryAfter(d time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(d.Seconds()))))
}
