//go:build integration

package queue_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// TestScheduler_UniquePreventsDoubleFire: two worker replicas run a
// scheduler each (05 §18.2-5, R-002 §3e). Without Unique both enqueue the
// job; with the mandatory Job.Unique only one task exists per period.
func TestScheduler_UniquePreventsDoubleFire(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	log := obs.Discard()

	// Control: the trap is real (raw asynq, no Unique).
	raw := testutil.NewRedis(t)
	for range 2 {
		s := asynq.NewSchedulerFromRedisClient(raw, &asynq.SchedulerOpts{})
		if _, err := s.Register("@every 2s", asynq.NewTask("outbox.cleanup", nil), asynq.Queue(queue.Low)); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Shutdown)
	}

	// Koro: the same two replicas through queue.Scheduler.
	rdb := testutil.NewRedis(t)
	job := queue.Job{Name: "outbox.cleanup", Cron: "@every 2s", Queue: queue.Low, Unique: 1500 * time.Millisecond, Handle: noop}
	for range 2 {
		s := queue.NewScheduler(ctx, rdb, log)
		if err := s.Register(job); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Shutdown)
	}
	if err := queue.NewScheduler(ctx, rdb, log).Register(queue.Job{Name: "outbox.cleanup", Cron: "@every 1s",
		Queue: queue.Low, Handle: noop}); err == nil {
		t.Fatal("a job without Unique was registered")
	}

	time.Sleep(5 * time.Second) // two ticks (≈2 s, ≈4 s) of every scheduler

	pending := func(insp *asynq.Inspector) int {
		tasks, err := insp.ListPendingTasks(queue.Low)
		if err != nil {
			t.Fatal(err)
		}
		return len(tasks)
	}
	if n := pending(asynq.NewInspectorFromRedisClient(raw)); n != 4 {
		t.Fatalf("control without Unique: %d pending tasks, want 4 (two replicas × two ticks)", n)
	}
	if n := pending(asynq.NewInspectorFromRedisClient(rdb)); n != 2 {
		t.Fatalf("with Unique: %d pending tasks, want 2 (one per tick)", n)
	}
}

// TestServer_DeadLetterReported: a permanent failure archives the task and
// reaches OnDeadLetter with a scrubbed error; a transient failure is retried.
func TestServer_DeadLetterReported(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rdb := testutil.NewRedis(t)
	log := obs.Discard()
	client := queue.NewClient(rdb, log)
	t.Cleanup(func() { _ = client.Close() })

	var mu sync.Mutex
	var dead []queue.DeadLetter
	gotDead := make(chan struct{}, 1)
	mux := queue.NewMux(log)
	_ = mux.Handle("payments.capture_payment", func(context.Context, []byte) error {
		return queue.Permanent(errors.New("payer ayse@ogrenci.edu.tr unknown"))
	})
	srv := queue.NewServer(ctx, rdb, mux, queue.ServerConfig{Concurrency: 2, ShutdownTimeout: time.Second, Log: log,
		OnDeadLetter: func(_ context.Context, d queue.DeadLetter) {
			mu.Lock()
			dead = append(dead, d)
			mu.Unlock()
			gotDead <- struct{}{}
		}})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	if err := srv.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := client.Enqueue(ctx, "payments.capture_payment", map[string]string{"payment_id": "p1"}, "p1:payments.capture_payment",
		queue.Queue(queue.Critical), queue.MaxRetry(queue.MoneyMaxRetry)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gotDead:
	case <-time.After(15 * time.Second):
		t.Fatal("dead letter not reported")
	}
	mu.Lock()
	defer mu.Unlock()
	d := dead[0]
	if d.Queue != queue.Critical || d.Type != "payments.capture_payment" || d.TaskID != "p1:payments.capture_payment" ||
		d.MaxRetry != queue.MoneyMaxRetry || d.Err == "" || strings.Contains(d.Err, "ayse@ogrenci") {
		t.Fatalf("dead letter = %+v", d)
	}
	insp := asynq.NewInspectorFromRedisClient(rdb)
	waitUntil(t, func() bool {
		archived, err := insp.ListArchivedTasks(queue.Critical)
		return err == nil && len(archived) == 1 && archived[0].LastErr != "" && !strings.Contains(archived[0].LastErr, "ayse@ogrenci")
	})
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCheckPersistence: the test Redis runs like production (AOF,
// noeviction); a volatile policy is reported.
func TestCheckPersistence(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rdb := testutil.NewRedis(t)
	if err := queue.CheckPersistence(ctx, rdb); err != nil {
		t.Fatalf("CheckPersistence() = %v", err)
	}
	// CONFIG SET is server-wide: change and restore within this test only
	// on a policy no other test depends on reading.
	if err := rdb.ConfigSet(ctx, "maxmemory-policy", "allkeys-lru").Err(); err != nil {
		t.Fatal(err)
	}
	err := queue.CheckPersistence(ctx, rdb)
	_ = rdb.ConfigSet(ctx, "maxmemory-policy", "noeviction").Err()
	if err == nil || !strings.Contains(err.Error(), "noeviction") {
		t.Fatalf("CheckPersistence() with allkeys-lru = %v", err)
	}
}
