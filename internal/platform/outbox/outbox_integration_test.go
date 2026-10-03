//go:build integration

package outbox_test

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/hibiken/asynq"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// Every backend test also runs against PostgreSQL (app_rw, Exec mode).
var _ = addBackend(backend{"postgres", func(t *testing.T) (outbox.Store, db.TxManager) {
	d := testutil.NewDB(t)
	return outbox.NewGormStore(d.ORM), d.Tx
}})

// TestRelay_DuplicateDelivery_SecondEnqueueRejected: a relay that enqueued
// a row's tasks but crashed before marking it published relays the row
// again; asynq rejects the second enqueue by task ID, before and after the
// task ran (Retention), so the handler runs exactly once (05 §8.3, R-002 §3e).
func TestRelay_DuplicateDelivery_SecondEnqueueRejected(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	rdb := testutil.NewRedis(t)
	ctx := t.Context()
	log := obs.Discard()
	client := queue.NewClient(rdb, log)
	t.Cleanup(func() { _ = client.Close() })
	insp := asynq.NewInspectorFromRedisClient(rdb)

	var runs atomic.Int32
	done := make(chan struct{}, 4)
	subs := outbox.NewRegistry()
	subs.Subscribe(domainevent.TopicRegistrationConfirmed, "events.send_ticket_email", func(context.Context, domainevent.Envelope) error {
		runs.Add(1)
		done <- struct{}{}
		return nil
	})
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: subs, Queue: client, Clock: clk, Log: log})
	relay := ob.NewRelay(outbox.RelayConfig{})

	if err := d.Tx.WithTx(ctx, func(ctx context.Context) error { return ob.Add(ctx, confirmed(t)) }); err != nil {
		t.Fatal(err)
	}
	var idText string
	if err := d.Pool.QueryRow(ctx, `SELECT id::text FROM outbox`).Scan(&idText); err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(idText)
	unpublish := func() {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, `UPDATE outbox SET published_at = NULL`); err != nil {
			t.Fatal(err)
		}
	}
	relayOnce := func() {
		t.Helper()
		if n, err := relay.Once(ctx); err != nil || n != 1 {
			t.Fatalf("Once() = %d, %v; want the row published", n, err)
		}
	}

	relayOnce()
	unpublish() // the UPDATE was lost: the next relay sees the row again
	relayOnce()
	pending, err := insp.ListPendingTasks(queue.Default)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != outbox.TaskID(id, "events.send_ticket_email") {
		t.Fatalf("pending tasks = %+v, want exactly one with the stable task ID", pending)
	}

	// Run the task, then relay the row a third time: the completed task is
	// retained for 24 h and still blocks its ID.
	mux := queue.NewMux(log)
	for _, s := range subs.All() {
		if err := mux.Handle(s.Name, s.HandleTask); err != nil {
			t.Fatal(err)
		}
	}
	srv := queue.NewServer(ctx, rdb, mux, queue.ServerConfig{Concurrency: 1, ShutdownTimeout: time.Second, Log: log})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the task was not processed")
	}
	waitFor(t, func() bool {
		info, err := insp.GetTaskInfo(queue.Default, outbox.TaskID(id, "events.send_ticket_email"))
		return err == nil && info.State == asynq.TaskStateCompleted
	})
	unpublish()
	relayOnce()
	time.Sleep(1500 * time.Millisecond) // a duplicate would be picked up by the running server now
	srv.Shutdown()
	if n := runs.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want exactly 1", n)
	}
	if pending, _ := insp.ListPendingTasks(queue.Default); len(pending) != 0 {
		t.Fatalf("a duplicate task was queued: %+v", pending)
	}
}

// TestRelay_ConcurrentRelays_SkipLocked: two relays on the same table never
// take the same row (FOR UPDATE SKIP LOCKED).
func TestRelay_ConcurrentRelays_SkipLocked(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	const rows = 60
	subs := outbox.NewRegistry()
	subs.Subscribe(domainevent.TopicRegistrationConfirmed, "events.send_ticket_email", nop)
	q := &countingQueue{seen: map[string]int{}}
	clk := clock.Real()
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: subs, Queue: q, Clock: clk, Log: obs.Discard()})
	for range rows {
		if err := d.Tx.WithTx(ctx, func(ctx context.Context) error { return ob.Add(ctx, confirmed(t)) }); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var published atomic.Int32
	for range 3 {
		wg.Go(func() {
			relay := ob.NewRelay(outbox.RelayConfig{Batch: 7})
			for {
				n, err := relay.Once(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
				published.Add(int32(n))
			}
		})
	}
	wg.Wait()
	if published.Load() != rows {
		t.Fatalf("published %d rows, want %d", published.Load(), rows)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.seen) != rows {
		t.Fatalf("%d distinct tasks, want %d", len(q.seen), rows)
	}
	for id, n := range q.seen {
		if n != 1 {
			t.Fatalf("task %s enqueued %d times: two relays took the same row", id, n)
		}
	}
}

// countingQueue counts enqueues per task ID without deduplicating, and is
// slow enough for the relays to overlap.
type countingQueue struct {
	mu   sync.Mutex
	seen map[string]int
}

func (q *countingQueue) Enqueue(_ context.Context, _ string, _ any, id string, _ ...queue.Option) error {
	time.Sleep(2 * time.Millisecond)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seen[id]++
	return nil
}

func (q *countingQueue) AfterCommit(ctx context.Context, typ string, payload any, id string, opts ...queue.Option) {
	_ = q.Enqueue(ctx, typ, payload, id, opts...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
