// Package kvcachetest is the shared contract of kvcache.Cache adapters
// (BE-197): every adapter runs Contract.
package kvcachetest

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"koro/internal/platform/kvcache"
)

// Contract checks c: a missing key, a round trip, a replacement, an
// isolated key and the TTL rule. expire makes every entry set so far
// expire (advance a fake clock, or wait out a real TTL); ttl is the TTL
// the contract sets.
func Contract(t *testing.T, c kvcache.Cache, ttl time.Duration, expire func()) {
	t.Helper()
	ctx := t.Context()
	if _, found, err := c.Get(ctx, "missing"); err != nil || found {
		t.Fatalf("Get(missing) = %v, %v; want not found", found, err)
	}
	if err := c.Set(ctx, "k", []byte("v1"), ttl); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Set(ctx, "k", []byte("v2"), ttl); err != nil {
		t.Fatalf("Set (replace): %v", err)
	}
	if v, found, err := c.Get(ctx, "k"); err != nil || !found || !bytes.Equal(v, []byte("v2")) {
		t.Fatalf("Get(k) = %q, %v, %v; want v2", v, found, err)
	}
	if _, found, err := c.Get(ctx, "k2"); err != nil || found {
		t.Fatalf("Get(k2) = %v, %v; want not found", found, err)
	}
	for _, bad := range []time.Duration{0, -time.Second} {
		if err := c.Set(ctx, "k3", []byte("x"), bad); !errors.Is(err, kvcache.ErrInvalidTTL) {
			t.Fatalf("Set(ttl=%v) = %v; want ErrInvalidTTL", bad, err)
		}
	}
	expire()
	if _, found, err := c.Get(ctx, "k"); err != nil || found {
		t.Fatalf("Get(k) after the TTL = %v, %v; want not found", found, err)
	}
}
