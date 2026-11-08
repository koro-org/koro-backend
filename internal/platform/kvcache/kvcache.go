// Package kvcache is a short-lived read-through value cache (BE-197): a
// small Cache port with a Redis adapter (the API's Redis, shared by the
// replicas) and an in-memory one (tests, a single process). It caches
// derived, non-personal results only — aggregates any caller may see,
// such as the popular clubs of a university — never a user's own data;
// the consumer owns the key, the encoding and the TTL.
//
// Failure policy is the consumer's: a cache is an optimisation, so the
// consumers fail open (a Get or Set error logs a warning and the value is
// read from its source).
package kvcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"koro/internal/platform/clock"
)

// Prefix namespaces every key of the Redis adapter.
const Prefix = "koro:cache:"

// Cache stores byte values under keys for a bounded time.
type Cache interface {
	// Get returns the value of key; found is false for a missing or
	// expired key.
	Get(ctx context.Context, key string) (val []byte, found bool, err error)
	// Set stores val under key for ttl (> 0), replacing any value.
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
}

// ErrInvalidTTL: Set needs a positive TTL (a cache entry never lives
// forever).
var ErrInvalidTTL = errors.New("kvcache: ttl must be positive")

// NewRedis returns the cache on rdb; keys are Prefix + key and expire in
// Redis.
func NewRedis(rdb redis.UniversalClient) Cache { return redisCache{rdb: rdb} }

type redisCache struct{ rdb redis.UniversalClient }

func (r redisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := r.rdb.Get(ctx, Prefix+key).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("kvcache: get: %w", err)
	}
	return v, true, nil
}

func (r redisCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return ErrInvalidTTL
	}
	if err := r.rdb.Set(ctx, Prefix+key, val, ttl).Err(); err != nil {
		return fmt.Errorf("kvcache: set: %w", err)
	}
	return nil
}

// Mem is the in-memory cache of tests and of a single process. Fail
// simulates an unavailable Redis.
type Mem struct {
	clock clock.Clock
	mu    sync.Mutex
	items map[string]memItem
	fail  error
}

type memItem struct {
	val      []byte
	deadline time.Time
}

// NewMem returns an empty in-memory cache on clk.
func NewMem(clk clock.Clock) *Mem { return &Mem{clock: clk, items: map[string]memItem{}} }

// Fail makes every call return err (nil restores it).
func (m *Mem) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

// Keys returns the number of live keys (tests).
func (m *Mem) Keys() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now()
	n := 0
	for _, it := range m.items {
		if it.deadline.After(now) {
			n++
		}
	}
	return n
}

// Get implements Cache.
func (m *Mem) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, false, m.fail
	}
	it, ok := m.items[key]
	if !ok {
		return nil, false, nil
	}
	if !it.deadline.After(m.clock.Now()) {
		delete(m.items, key)
		return nil, false, nil
	}
	return append([]byte(nil), it.val...), true, nil
}

// Set implements Cache.
func (m *Mem) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return ErrInvalidTTL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.items[key] = memItem{val: append([]byte(nil), val...), deadline: m.clock.Now().Add(ttl)}
	return nil
}
