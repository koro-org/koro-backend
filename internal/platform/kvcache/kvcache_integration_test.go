//go:build integration

package kvcache_test

import (
	"context"
	"os"
	"testing"
	"time"

	"koro/internal/platform/kvcache"
	"koro/internal/platform/kvcache/kvcachetest"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// TestRedis_Contract: the Redis adapter keeps the contract on a real
// Redis, under the koro:cache: prefix with the TTL set in Redis.
func TestRedis_Contract(t *testing.T) {
	t.Parallel()
	rdb := testutil.NewRedis(t)
	const ttl = 300 * time.Millisecond
	kvcachetest.Contract(t, kvcache.NewRedis(rdb), ttl, func() { time.Sleep(2 * ttl) })

	if err := kvcache.NewRedis(rdb).Set(context.Background(), "p", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if d := rdb.PTTL(context.Background(), kvcache.Prefix+"p").Val(); d <= 0 || d > time.Minute {
		t.Fatalf("PTTL = %v; want (0, 1m]", d)
	}
}
