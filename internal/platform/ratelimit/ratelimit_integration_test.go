//go:build integration

package ratelimit_test

import (
	"os"
	"testing"
	"time"

	"koro/internal/platform/ratelimit"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// TestRedisLimiter_GCRA: the redis_rate limiter allows the burst, then
// rejects with a Retry-After; keys and rules are independent (R-002 §2.2).
func TestRedisLimiter_GCRA(t *testing.T) {
	t.Parallel()
	l := ratelimit.NewRedis(testutil.NewRedis(t))
	ctx := t.Context()
	rule := ratelimit.Rule{Name: "auth_login", Rate: 5, Per: time.Minute, Strict: true, Key: ratelimit.KeyIP}
	for i := range 5 {
		res, err := l.Allow(ctx, rule, "ip:203.0.113.7")
		if err != nil || !res.Allowed {
			t.Fatalf("request %d: %+v, %v", i+1, res, err)
		}
	}
	res, err := l.Allow(ctx, rule, "ip:203.0.113.7")
	if err != nil || res.Allowed || res.RetryAfter <= 0 || res.RetryAfter > 13*time.Second {
		t.Fatalf("6th request: %+v, %v; want rejected with RetryAfter ≈ 12s", res, err)
	}
	if res, err := l.Allow(ctx, rule, "ip:203.0.113.8"); err != nil || !res.Allowed {
		t.Fatalf("other key: %+v, %v", res, err)
	}
	other := rule
	other.Name = "auth_register"
	if res, err := l.Allow(ctx, other, "ip:203.0.113.7"); err != nil || !res.Allowed {
		t.Fatalf("other rule: %+v, %v", res, err)
	}
}
