//go:build integration

package main

import (
	"sync"
	"testing"
	"time"

	"koro/internal/platform/testutil"
)

// TestRedisChallenges_SingleUseAndTTL pins the Redis side of 06 H-16: a
// challenge expires with its TTL and is taken at most once, even by
// concurrent requests (GETDEL).
func TestRedisChallenges_SingleUseAndTTL(t *testing.T) {
	t.Parallel()
	rdb := testutil.NewRedis(t)
	r := redisChallenges{rdb: rdb}
	ctx := t.Context()

	if err := r.Put(ctx, "identity:mfa_challenge:k1", []byte("v1"), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if ttl := rdb.TTL(ctx, "koro:identity:mfa_challenge:k1").Val(); ttl <= 4*time.Minute || ttl > 5*time.Minute {
		t.Fatalf("TTL %s, want ≈ 5m", ttl)
	}
	var mu sync.Mutex
	got := 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if v, ok, err := r.Take(ctx, "identity:mfa_challenge:k1"); err == nil && ok && string(v) == "v1" {
				mu.Lock()
				got++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if got != 1 {
		t.Fatalf("challenge taken %d times, want once", got)
	}
	if err := r.Put(ctx, "identity:mfa_challenge:k2", []byte("v2"), time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, ok, err := r.Take(ctx, "identity:mfa_challenge:k2"); ok || err != nil {
		t.Fatalf("expired challenge: %v %v", ok, err)
	}
	if err := r.Put(ctx, "x", []byte("v"), 0); err == nil {
		t.Fatal("a challenge without TTL was stored")
	}
}
