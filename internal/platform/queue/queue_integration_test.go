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

// TestScheduler_UniquePreventsDoubleFire observes every attempt in two complete
// replica rounds. Cron timing is covered separately: a pending count sampled
// between ticks cannot prove that the other replica was rejected.
func TestScheduler_UniquePreventsDoubleFire(t *testing.T) {
	t.Parallel()
	raw, rdb := testutil.NewRedis(t), testutil.NewRedis(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	job := queue.Job{Name: "outbox.cleanup", Cron: "@every 3s", Queue: queue.Low, Unique: 2 * time.Second, Handle: noop}
	type registration struct {
		task *asynq.Task
		opts []asynq.Option
	}
	var registered [2]registration
	var clients, controls [2]*asynq.Client
	for i := range registered {
		s := queue.NewScheduler(ctx, rdb, obs.Discard())
		t.Cleanup(s.Shutdown)
		task, opts, err := queue.RegisterForTest(s, job)
		if err != nil {
			t.Fatal(err)
		}
		registered[i] = registration{task, opts}
		clients[i] = asynq.NewClientFromRedisClient(rdb)
		controls[i] = asynq.NewClientFromRedisClient(raw)
		t.Cleanup(func() { _ = clients[i].Close(); _ = controls[i].Close() })
	}
	invalid := queue.NewScheduler(ctx, rdb, obs.Discard())
	t.Cleanup(invalid.Shutdown)
	if err := invalid.Register(queue.Job{Name: job.Name, Cron: job.Cron, Queue: job.Queue, Handle: noop}); err == nil {
		t.Fatal("a job without Unique was registered")
	}

	// No scheduler is running: each barrier releases exactly two attempts,
	// and the round ends only after both results have arrived.
	runRound := func(round int, replicas [2]*asynq.Client, unique bool) {
		t.Helper()
		start := make(chan struct{})
		ready := make(chan struct{}, len(replicas))
		results := make(chan error, len(replicas))
		for i, client := range replicas {
			go func() {
				ready <- struct{}{}
				select {
				case <-start:
				case <-ctx.Done():
					results <- ctx.Err()
					return
				}
				opts := registered[i].opts
				if !unique {
					// The control differs only in Unique, using the same
					// production registration for queue/retry/timeout.
					opts = nil
					for _, opt := range registered[i].opts {
						if opt.Type() != asynq.UniqueOpt {
							opts = append(opts, opt)
						}
					}
				}
				_, err := client.EnqueueContext(ctx, registered[i].task, opts...)
				results <- err
			}()
		}
		for range replicas {
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatalf("round %d unique=%t: replicas not ready: %v", round, unique, ctx.Err())
			}
		}
		close(start)
		successes, duplicates := 0, 0
		for range replicas {
			select {
			case err := <-results:
				switch {
				case err == nil:
					successes++
				case errors.Is(err, asynq.ErrDuplicateTask):
					duplicates++
				default:
					t.Errorf("round %d unique=%t: enqueue: %v", round, unique, err)
				}
			case <-ctx.Done():
				t.Fatalf("round %d unique=%t: waiting for all results: %v", round, unique, ctx.Err())
			}
		}
		wantSuccess, wantDuplicate := 2, 0
		if unique {
			wantSuccess, wantDuplicate = 1, 1
		}
		if successes != wantSuccess || duplicates != wantDuplicate {
			t.Fatalf("round %d unique=%t: successes=%d duplicates=%d; want %d, %d", round, unique,
				successes, duplicates, wantSuccess, wantDuplicate)
		}
	}

	for round := 1; round <= 2; round++ {
		runRound(round, controls, false)
		runRound(round, clients, true)
		if round == 1 {
			// Observe this task's real lock and its natural expiration.
			// Use whole seconds: asynq truncates fractional Unique TTLs.
			keys, err := rdb.Keys(ctx, "asynq:{"+job.Queue+"}:unique:*").Result()
			if err != nil || len(keys) != 1 {
				t.Fatalf("unique lock keys = %v (%v), want one", keys, err)
			}
			if ttl, err := rdb.PTTL(ctx, keys[0]).Result(); err != nil || ttl <= 0 || ttl > job.Unique {
				t.Fatalf("unique lock TTL = %v (%v), want (0, %v]", ttl, err, job.Unique)
			}
			tick := time.NewTicker(20 * time.Millisecond)
			for {
				n, err := rdb.Exists(ctx, keys[0]).Result()
				if err != nil {
					tick.Stop()
					t.Fatal(err)
				}
				if n == 0 {
					break
				}
				select {
				case <-tick.C:
				case <-ctx.Done():
					tick.Stop()
					t.Fatalf("unique lock did not expire: %v", ctx.Err())
				}
			}
			tick.Stop()
		}
	}
	for _, check := range []struct {
		name      string
		inspector *asynq.Inspector
		want      int
	}{
		{"control", asynq.NewInspectorFromRedisClient(raw), 4},
		{"unique", asynq.NewInspectorFromRedisClient(rdb), 2},
	} {
		t.Cleanup(func() { _ = check.inspector.Close() })
		tasks, err := check.inspector.ListPendingTasks(job.Queue)
		if err != nil || len(tasks) != check.want {
			t.Fatalf("%s: pending = %d (%v), want %d", check.name, len(tasks), err, check.want)
		}
		for _, task := range tasks {
			if task.Type != job.Name || task.MaxRetry != queue.JobMaxRetry || task.Timeout != queue.DefaultTimeout {
				t.Fatalf("%s: registered task options not preserved: %+v", check.name, task)
			}
		}
	}
}

// Keep a real cron smoke test alongside the controlled replica rounds.
func TestScheduler_EnqueuesRegisteredJob(t *testing.T) {
	t.Parallel()
	rdb := testutil.NewRedis(t)
	s := queue.NewScheduler(t.Context(), rdb, obs.Discard())
	t.Cleanup(s.Shutdown)
	job := queue.Job{Name: "outbox.cleanup", Cron: "@every 2s", Queue: queue.Low, Unique: time.Second, Handle: noop}
	if err := s.Register(job); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	insp := asynq.NewInspectorFromRedisClient(rdb)
	t.Cleanup(func() { _ = insp.Close() })
	waitUntil(t, func() bool {
		tasks, err := insp.ListPendingTasks(job.Queue)
		return err == nil && len(tasks) > 0 && tasks[0].Type == job.Name
	})
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
