package queue

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Enqueued is one task recorded by Mem.
type Enqueued struct {
	Type      string
	Payload   []byte
	ID        string // dedup ID ("" when none)
	Queue     string
	MaxRetry  int
	Timeout   time.Duration
	ProcessIn time.Duration
}

// Mem is the recording Enqueuer of service and wiring tests (05 §13.2). It
// deduplicates by task ID like asynq within the retention window. Fail makes
// the next calls return an error (Redis down).
type Mem struct {
	mu    sync.Mutex
	tasks []Enqueued
	ids   map[string]bool
	fail  error
}

// NewMem returns an empty recording Enqueuer.
func NewMem() *Mem { return &Mem{ids: map[string]bool{}} }

// Fail makes every following Enqueue return err (nil restores success).
func (m *Mem) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

// Tasks returns a copy of the recorded tasks in enqueue order.
func (m *Mem) Tasks() []Enqueued {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.tasks)
}

// Reset forgets the recorded tasks (task IDs stay taken, as in asynq).
func (m *Mem) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks = nil
}

// Enqueue implements Enqueuer.
func (m *Mem) Enqueue(_ context.Context, typ string, payload any, dedupID string, opts ...Option) error {
	task, _, err := build(typ, payload, dedupID, opts)
	if err != nil {
		return err
	}
	o, _ := resolve(opts)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	if dedupID != "" {
		if m.ids[dedupID] {
			return nil
		}
		m.ids[dedupID] = true
	}
	m.tasks = append(m.tasks, Enqueued{
		Type: typ, Payload: task.Payload(), ID: dedupID,
		Queue: o.queue, MaxRetry: o.maxRetry, Timeout: o.timeout, ProcessIn: o.processIn,
	})
	return nil
}

// AfterCommit implements Enqueuer.
func (m *Mem) AfterCommit(ctx context.Context, typ string, payload any, dedupID string, opts ...Option) {
	afterCommit(ctx, m, nil, typ, payload, dedupID, opts)
}
