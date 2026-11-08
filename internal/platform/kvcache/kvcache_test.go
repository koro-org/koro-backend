package kvcache_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/kvcache"
	"koro/internal/platform/kvcache/kvcachetest"
)

func TestMem_Contract(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC))
	const ttl = 10 * time.Minute
	m := kvcache.NewMem(clk)
	kvcachetest.Contract(t, m, ttl, func() { clk.Advance(ttl) })
	if n := m.Keys(); n != 0 {
		t.Fatalf("Keys after the TTL = %d", n)
	}
}

func TestMem_Fail(t *testing.T) {
	t.Parallel()
	m := kvcache.NewMem(clock.Real())
	boom := errors.New("down")
	m.Fail(boom)
	if _, _, err := m.Get(context.Background(), "k"); !errors.Is(err, boom) {
		t.Fatalf("Get = %v", err)
	}
	if err := m.Set(context.Background(), "k", nil, time.Minute); !errors.Is(err, boom) {
		t.Fatalf("Set = %v", err)
	}
	m.Fail(nil)
	if err := m.Set(context.Background(), "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
}

// TestMem_ValuesAreCopies: a caller changing a slice it set or got does
// not change the cached value.
func TestMem_ValuesAreCopies(t *testing.T) {
	t.Parallel()
	m := kvcache.NewMem(clock.Real())
	ctx := context.Background()
	in := []byte("abc")
	if err := m.Set(ctx, "k", in, time.Minute); err != nil {
		t.Fatal(err)
	}
	in[0] = 'x'
	got, _, _ := m.Get(ctx, "k")
	got[1] = 'y'
	if again, _, _ := m.Get(ctx, "k"); string(again) != "abc" {
		t.Fatalf("cached = %q", again)
	}
}
