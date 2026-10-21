package realtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

// ChannelPrefix prefixes every pub/sub channel of the hub: topic
// "event:<id>:checkins" travels on "rt:event:<id>:checkins" (04 §W).
const ChannelPrefix = "rt:"

// Bus carries the hub's messages between API replicas (Redis pub/sub in
// production). A message reaches every replica, each replica delivers it to
// its own connections of the topic. Delivery is at most once: a message
// published while a replica is not subscribed (start, Redis failover) is
// lost; the boards resend absolute counters with every change and a new
// connection starts from a snapshot, so a lost message heals itself.
type Bus interface {
	// Publish sends payload on channel (ChannelPrefix + topic).
	Publish(ctx context.Context, channel string, payload []byte) error
	// Subscribe delivers every message of the ChannelPrefix channels to
	// deliver until ctx ends. It returns once the subscription is active;
	// deliver must not block.
	Subscribe(ctx context.Context, deliver func(channel string, payload []byte)) error
}

// NewRedisBus returns the Redis pub/sub bus over rdb: one PSUBSCRIBE of
// "rt:*" per replica, opened on the first connection (go-redis reconnects
// and resubscribes by itself).
func NewRedisBus(rdb redis.UniversalClient) Bus { return redisBus{rdb: rdb} }

type redisBus struct{ rdb redis.UniversalClient }

func (b redisBus) Publish(ctx context.Context, channel string, payload []byte) error {
	if err := b.rdb.Publish(ctx, channel, payload).Err(); err != nil {
		return fmt.Errorf("realtime: publish: %w", err)
	}
	return nil
}

func (b redisBus) Subscribe(ctx context.Context, deliver func(channel string, payload []byte)) error {
	ps := b.rdb.PSubscribe(ctx, ChannelPrefix+"*")
	if _, err := ps.Receive(ctx); err != nil { // the subscription confirmation
		_ = ps.Close()
		return fmt.Errorf("realtime: subscribe: %w", err)
	}
	ch := ps.Channel()
	go func() {
		defer func() { _ = ps.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-ch:
				if !ok {
					return
				}
				deliver(m.Channel, []byte(m.Payload))
			}
		}
	}()
	return nil
}

// NewMemBus returns an in-process bus: a single replica (tests, and a
// process built without Redis). Publish delivers synchronously.
func NewMemBus() Bus { return &memBus{subs: map[int]func(string, []byte){}} }

type memBus struct {
	mu   sync.RWMutex
	next int
	subs map[int]func(string, []byte)
}

func (b *memBus) Publish(_ context.Context, channel string, payload []byte) error {
	if !strings.HasPrefix(channel, ChannelPrefix) {
		return errors.New("realtime: channel without the rt prefix")
	}
	b.mu.RLock()
	subs := make([]func(string, []byte), 0, len(b.subs))
	for _, d := range b.subs {
		subs = append(subs, d)
	}
	b.mu.RUnlock()
	for _, d := range subs { // outside the lock: deliver takes the hub's
		d(channel, payload)
	}
	return nil
}

func (b *memBus) Subscribe(ctx context.Context, deliver func(channel string, payload []byte)) error {
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = deliver
	b.mu.Unlock()
	context.AfterFunc(ctx, func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	})
	return nil
}
