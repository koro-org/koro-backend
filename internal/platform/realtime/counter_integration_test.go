//go:build integration

package realtime_test

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/redis/go-redis/v9"

	"koro/internal/platform/realtime"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// redisRecords counts the live records of the shared counter in Redis
// (members of the per-user sets; every record is in exactly one).
func redisRecords(t *testing.T, rdb *redis.Client) func() int {
	return func() int {
		keys, err := rdb.Keys(context.Background(), realtime.CounterPrefix+"u:*").Result()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, k := range keys {
			n += int(rdb.ZCard(context.Background(), k).Val())
		}
		return n
	}
}

// TestRedisCounter_TwoReplicas: two hubs over one real Redis (pub/sub bus
// and shared counter, as two API replicas): 3 connections per user and
// MaxPerTopic per topic hold across both (T-325), a freed slot is reusable
// on the other replica, and nothing is left in Redis afterwards.
func TestRedisCounter_TwoReplicas(t *testing.T) {
	t.Parallel()
	rdb := testutil.NewRedis(t)
	limits := func(c *realtime.Config) { c.MaxPerTopic = 4 }
	a := newReplica(t, realtime.NewRedisBus(rdb), realtime.NewRedisCounter(rdb), limits)
	b := newReplica(t, realtime.NewRedisBus(rdb), realtime.NewRedisCounter(rdb), limits)
	runSharedLimits(t, a, b)
	waitRecords(t, redisRecords(t, rdb), 0)
}

// TestRedisCounter_CrashedReplicaExpires: a record whose replica stopped
// refreshing it (crashed) frees its slot after the TTL; a live replica's
// heartbeat keeps its own record beyond the TTL.
func TestRedisCounter_CrashedReplicaExpires(t *testing.T) {
	t.Parallel()
	rdb := testutil.NewRedis(t)
	counter := realtime.NewRedisCounter(rdb)
	const ttl = 600 * time.Millisecond
	one := func(c *realtime.Config) { c.MaxPerUser, c.LeaseTTL = 1, ttl }
	a := newReplica(t, realtime.NewRedisBus(rdb), counter, one)
	b := newReplica(t, realtime.NewRedisBus(rdb), counter, one)

	crashed := uuid.NewV7()
	dead := realtime.Slot{Topic: "board", User: crashed, ID: "crashed-replica:1"}
	if ok, err := counter.Acquire(t.Context(), dead, 50, 1, ttl); !ok || err != nil {
		t.Fatalf("Acquire = %v, %v", ok, err)
	}
	tooMany(t, "the crashed replica's user, before the TTL", a.url, "board", crashed)
	c := eventually(t, "the crashed replica's user, after the TTL", a.url, "board", crashed)

	time.Sleep(3 * ttl) // the heartbeat (every TTL/3) keeps c's record
	tooMany(t, "a live connection's user on the other replica", b.url, "board", crashed)
	_ = c.CloseNow()
	waitRecords(t, redisRecords(t, rdb), 0)
}

// TestRedisCounter_ConcurrentAcquire: admissions racing on several
// replicas never exceed the limit (one script checks and records).
func TestRedisCounter_ConcurrentAcquire(t *testing.T) {
	t.Parallel()
	rdb := testutil.NewRedis(t)
	counter := realtime.NewRedisCounter(rdb)
	const limit, tries = 5, 40
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := range tries {
		wg.Go(func() {
			s := realtime.Slot{Topic: "board", User: uuid.NewV7(), ID: "r" + strconv.Itoa(i%3) + ":" + strconv.Itoa(i)}
			ok, err := counter.Acquire(context.Background(), s, limit, 3, time.Minute)
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if n := admitted.Load(); n != limit {
		t.Fatalf("admitted %d of %d, want %d", n, tries, limit)
	}
	// Refresh keeps only present records; Release is idempotent.
	gone := realtime.Slot{Topic: "board", User: uuid.NewV7(), ID: "gone:1"}
	if err := counter.Refresh(t.Context(), []realtime.Slot{gone}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := counter.Release(t.Context(), gone); err != nil {
		t.Fatal(err)
	}
	if n := redisRecords(t, rdb)(); n != limit {
		t.Fatalf("records = %d, want %d (a refresh must not create one)", n, limit)
	}
}
