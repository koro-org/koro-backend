package realtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"uuid"

	"github.com/redis/go-redis/v9"

	"koro/internal/platform/clock"
)

// CounterPrefix prefixes the Redis keys of the shared connection counters
// (T-325): the sorted sets "rt:conn:t:<topic>" and "rt:conn:u:<user_id>".
// Keys and pub/sub channels are separate Redis namespaces; the prefix only
// keeps the hub's state together.
const CounterPrefix = "rt:conn:"

// Slot is one admitted connection in the shared counters.
type Slot struct {
	Topic string
	User  uuid.UUID
	// ID names the connection across replicas: "<replica id>:<sequence>".
	ID string
}

// Counter counts the WebSocket connections of every API replica, so that
// MaxPerTopic and MaxPerUser hold for the whole service rather than per
// replica (T-325). A connection is a record with a deadline: the replica
// that holds it refreshes it every heartbeat and removes it when the
// connection ends; the records of a replica that dies stop being refreshed
// and expire after the TTL (no counter is left behind).
type Counter interface {
	// Acquire records s unless its topic already holds maxTopic or its
	// user maxUser live records (false: over a limit, nothing recorded).
	// The record lives ttl unless refreshed.
	Acquire(ctx context.Context, s Slot, maxTopic, maxUser int, ttl time.Duration) (bool, error)
	// Refresh extends the records of slots by ttl; a record that is gone
	// (released, expired) is not brought back.
	Refresh(ctx context.Context, slots []Slot, ttl time.Duration) error
	// Release removes s's record (idempotent).
	Release(ctx context.Context, s Slot) error
}

func topicKey(topic string) string     { return CounterPrefix + "t:" + topic }
func userKey(user uuid.UUID) string    { return CounterPrefix + "u:" + user.String() }
func slotKeys(s Slot) (string, string) { return topicKey(s.Topic), userKey(s.User) }

// NewRedisCounter returns the shared counter on rdb (the API's Redis).
// Each record is a member of two sorted sets (topic, user) scored by its
// deadline in Redis server time (no replica clock skew); Acquire prunes
// the expired members and checks both limits in one script, so two
// replicas admitting at once cannot both take the last slot. The scripts
// touch two keys: a single Redis node (the platform's; Redis Cluster would
// need a hash tag).
func NewRedisCounter(rdb redis.UniversalClient) Counter { return redisCounter{rdb: rdb} }

type redisCounter struct{ rdb redis.UniversalClient }

// nowMS is Redis server time in milliseconds (TIME is allowed in scripts
// since Redis 5's effect replication).
const nowMS = `local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
`

// acquireScript: KEYS topic, user; ARGV ttl ms, maxTopic, maxUser, id.
var acquireScript = redis.NewScript(nowMS + `
local ttl = tonumber(ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
if redis.call('ZSCORE', KEYS[1], ARGV[4]) then return 1 end
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) or redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[3]) then
  return 0
end
for i = 1, 2 do
  redis.call('ZADD', KEYS[i], now + ttl, ARGV[4])
  redis.call('PEXPIRE', KEYS[i], ttl)
end
return 1
`)

// refreshScript: KEYS topic1, user1, topic2, user2, …; ARGV ttl ms, id1,
// id2, …. Only members still present are extended.
var refreshScript = redis.NewScript(nowMS + `
local ttl = tonumber(ARGV[1])
for i = 1, #KEYS do
  local id = ARGV[1 + math.floor((i + 1) / 2)]
  if redis.call('ZSCORE', KEYS[i], id) then
    redis.call('ZADD', KEYS[i], 'XX', now + ttl, id)
    redis.call('PEXPIRE', KEYS[i], ttl)
  end
end
return 0
`)

// refreshBatch bounds the slots of one refresh script.
const refreshBatch = 200

func (c redisCounter) Acquire(ctx context.Context, s Slot, maxTopic, maxUser int, ttl time.Duration) (bool, error) {
	tk, uk := slotKeys(s)
	n, err := acquireScript.Run(ctx, c.rdb, []string{tk, uk}, ttl.Milliseconds(), maxTopic, maxUser, s.ID).Int()
	if err != nil {
		return false, fmt.Errorf("realtime: counter acquire: %w", err)
	}
	return n == 1, nil
}

func (c redisCounter) Refresh(ctx context.Context, slots []Slot, ttl time.Duration) error {
	for len(slots) > 0 {
		batch := slots[:min(len(slots), refreshBatch)]
		slots = slots[len(batch):]
		keys := make([]string, 0, 2*len(batch))
		args := make([]any, 0, 1+len(batch))
		args = append(args, ttl.Milliseconds())
		for _, s := range batch {
			tk, uk := slotKeys(s)
			keys = append(keys, tk, uk)
			args = append(args, s.ID)
		}
		if err := refreshScript.Run(ctx, c.rdb, keys, args...).Err(); err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("realtime: counter refresh: %w", err)
		}
	}
	return nil
}

func (c redisCounter) Release(ctx context.Context, s Slot) error {
	tk, uk := slotKeys(s)
	_, err := c.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.ZRem(ctx, tk, s.ID)
		p.ZRem(ctx, uk, s.ID)
		return nil
	})
	if err != nil {
		return fmt.Errorf("realtime: counter release: %w", err)
	}
	return nil
}

// MemCounter is the in-process Counter of tests: several hubs sharing one
// MemCounter behave as replicas sharing Redis. Deadlines follow its clock;
// Fail simulates an unavailable Redis.
type MemCounter struct {
	clock clock.Clock
	mu    sync.Mutex
	slots map[string]memSlot
	fail  error
}

type memSlot struct {
	s        Slot
	deadline time.Time
}

// NewMemCounter returns an empty in-memory counter.
func NewMemCounter(clk clock.Clock) *MemCounter {
	return &MemCounter{clock: clk, slots: map[string]memSlot{}}
}

// Fail makes every call return err (nil restores it).
func (m *MemCounter) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

// Len returns the live records (tests).
func (m *MemCounter) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prune()
	return len(m.slots)
}

func (m *MemCounter) prune() {
	now := m.clock.Now()
	for id, r := range m.slots {
		if !r.deadline.After(now) {
			delete(m.slots, id)
		}
	}
}

// Acquire implements Counter.
func (m *MemCounter) Acquire(_ context.Context, s Slot, maxTopic, maxUser int, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return false, m.fail
	}
	m.prune()
	if _, ok := m.slots[s.ID]; ok {
		return true, nil
	}
	var nt, nu int
	for _, r := range m.slots {
		if r.s.Topic == s.Topic {
			nt++
		}
		if r.s.User == s.User {
			nu++
		}
	}
	if nt >= maxTopic || nu >= maxUser {
		return false, nil
	}
	m.slots[s.ID] = memSlot{s: s, deadline: m.clock.Now().Add(ttl)}
	return true, nil
}

// Refresh implements Counter.
func (m *MemCounter) Refresh(_ context.Context, slots []Slot, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.prune()
	for _, s := range slots {
		if r, ok := m.slots[s.ID]; ok {
			r.deadline = m.clock.Now().Add(ttl)
			m.slots[s.ID] = r
		}
	}
	return nil
}

// Release implements Counter.
func (m *MemCounter) Release(_ context.Context, s Slot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	delete(m.slots, s.ID)
	return nil
}
