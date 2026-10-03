package queue_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

func noop(context.Context) error { return nil }

func TestJob_Validate(t *testing.T) {
	t.Parallel()
	ok := queue.Job{Name: "outbox.cleanup", Cron: "10 4 * * *", Queue: queue.Low, Unique: 23 * time.Hour, Handle: noop}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid job: %v", err)
	}
	cases := []struct {
		name string
		mod  func(j *queue.Job)
		want string
	}{
		{"no unique", func(j *queue.Job) { j.Unique = 0 }, "Unique is mandatory"},
		{"unique as long as the period", func(j *queue.Job) { j.Cron, j.Unique = "5 * * * *", time.Hour }, "shorter than the period 1h0m0s"},
		{"every 10s", func(j *queue.Job) { j.Cron, j.Unique = "@every 10s", 10*time.Second }, "shorter than the period 10s"},
		{"monthly uses the shortest month", func(j *queue.Job) { j.Cron, j.Unique = "30 4 1 * *", 29*24*time.Hour }, "period 672h0m0s"},
		{"bad cron", func(j *queue.Job) { j.Cron = "every day" }, "cron"},
		{"bad name", func(j *queue.Job) { j.Name = "OutboxCleanup" }, "name must be"},
		{"bad queue", func(j *queue.Job) { j.Queue = "urgent" }, "unknown queue"},
		{"nil handler", func(j *queue.Job) { j.Handle = nil }, "nil handler"},
	}
	for _, c := range cases {
		j := ok
		c.mod(&j)
		if err := j.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Validate() = %v, want %q", c.name, err, c.want)
		}
	}
	// 01 §7 examples are valid.
	for _, j := range []queue.Job{
		{Name: "audit.chain", Cron: "@every 10s", Queue: queue.Critical, Unique: 9 * time.Second, Handle: noop},
		{Name: "talent.purge", Cron: "30 4 1 * *", Queue: queue.Low, Unique: 27 * 24 * time.Hour, Handle: noop},
		{Name: "payments.unmatched_digest", Cron: "30 9 * * 1-5", Queue: queue.Default, Unique: 23 * time.Hour, Handle: noop},
	} {
		if err := j.Validate(); err != nil {
			t.Errorf("%s: %v", j.Name, err)
		}
	}
}

func TestRegistry_CollectsAndRejectsDuplicates(t *testing.T) {
	t.Parallel()
	r := queue.NewRegistry()
	r.Schedule(queue.Job{Name: "outbox.cleanup", Cron: "10 4 * * *", Queue: queue.Low, Unique: time.Hour, Handle: noop})
	r.Schedule(queue.Job{Name: "outbox.cleanup", Cron: "10 4 * * *", Queue: queue.Low, Unique: time.Hour, Handle: noop})
	r.Handle(queue.Task{Name: "outbox.cleanup", Handle: func(context.Context, []byte) error { return nil }})
	r.Handle(queue.Task{Name: "billing.poll_job", Handle: func(context.Context, []byte) error { return nil }})
	r.Schedule(queue.Job{Name: "x.y", Cron: "10 4 * * *", Queue: queue.Low, Handle: noop})
	err := r.Err()
	if err == nil || strings.Count(err.Error(), "registered twice") != 2 || !strings.Contains(err.Error(), "Unique is mandatory") {
		t.Fatalf("Err() = %v", err)
	}
	if len(r.Jobs()) != 1 || len(r.Tasks()) != 1 {
		t.Fatalf("jobs %d, tasks %d", len(r.Jobs()), len(r.Tasks()))
	}
}

func TestMem_DeduplicatesAndFails(t *testing.T) {
	t.Parallel()
	m := queue.NewMem()
	ctx := t.Context()
	if err := m.Enqueue(ctx, "files.scan_file", map[string]string{"file_id": "f1"}, "f1:files.scan_file",
		queue.Queue(queue.Low), queue.MaxRetry(3), queue.ProcessIn(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = m.Enqueue(ctx, "files.scan_file", nil, "f1:files.scan_file")
	tasks := m.Tasks()
	if len(tasks) != 1 || tasks[0].Queue != queue.Low || tasks[0].MaxRetry != 3 || tasks[0].ProcessIn != time.Minute ||
		tasks[0].Timeout != queue.DefaultTimeout || string(tasks[0].Payload) != `{"file_id":"f1"}` {
		t.Fatalf("tasks = %+v", tasks)
	}
	if err := m.Enqueue(ctx, "files.scan_file", nil, "", queue.Queue("urgent")); err == nil {
		t.Fatal("unknown queue accepted")
	}
	if err := m.Enqueue(ctx, "files.scan_file", strings.Repeat("x", 70<<10), ""); err == nil {
		t.Fatal("oversized payload accepted")
	}
	boom := errors.New("redis down")
	m.Fail(boom)
	if err := m.Enqueue(ctx, "files.scan_file", nil, "f2"); !errors.Is(err, boom) {
		t.Fatalf("Enqueue() with failure = %v", err)
	}

	// AfterCommit: never on rollback, after commit otherwise.
	m.Fail(nil)
	m.Reset()
	_ = db.NopTx{}.WithTx(ctx, func(ctx context.Context) error {
		m.AfterCommit(ctx, "talent.ingest_like", nil, "")
		return boom
	})
	if len(m.Tasks()) != 0 {
		t.Fatal("AfterCommit enqueued after a rollback")
	}
	_ = db.NopTx{}.WithTx(ctx, func(ctx context.Context) error {
		m.AfterCommit(ctx, "talent.ingest_like", nil, "")
		if len(m.Tasks()) != 0 {
			t.Fatal("AfterCommit enqueued before the commit")
		}
		return nil
	})
	if len(m.Tasks()) != 1 {
		t.Fatal("AfterCommit did not enqueue after the commit")
	}
}

func TestPermanent(t *testing.T) {
	t.Parallel()
	base := errors.New("record gone")
	p := queue.Permanent(base)
	if !queue.IsPermanent(p) || !errors.Is(p, base) || queue.IsPermanent(base) || queue.Permanent(nil) != nil {
		t.Fatalf("Permanent(%v) = %v", base, p)
	}
	if again := queue.Permanent(p); again.Error() != p.Error() {
		t.Fatal("Permanent is not idempotent")
	}
}

func TestRetryDelay_CappedAtSixHours(t *testing.T) {
	t.Parallel()
	task := asynq.NewTask("x.y", nil)
	// asynq's default: n^4 + 15 s + jitter of up to 30·(n+1) s.
	if d := queue.RetryDelay(1, errors.New("x"), task); d < 16*time.Second || d > 76*time.Second {
		t.Fatalf("first retry delay %s", d)
	}
	if d := queue.RetryDelay(25, errors.New("x"), task); d != queue.MaxRetryDelay {
		t.Fatalf("25th retry delay %s, want %s", d, queue.MaxRetryDelay)
	}
}

func TestMux_RecoversScrubsAndRoutes(t *testing.T) {
	t.Parallel()
	mux := queue.NewMux(obs.Discard())
	var gotTaskID string
	_ = mux.Handle("events.send_ticket_email", func(ctx context.Context, payload []byte) error {
		gotTaskID = obs.TaskID(ctx)
		if string(payload) == "panic" {
			panic("boom for ayse@ogrenci.edu.tr")
		}
		if string(payload) == "fail" {
			return queue.Permanent(errors.New("smtp rejected ayse@ogrenci.edu.tr"))
		}
		return nil
	})
	if err := mux.Handle("events.send_ticket_email", func(context.Context, []byte) error { return nil }); err == nil {
		t.Fatal("duplicate handler accepted")
	}
	ctx := t.Context()
	if err := mux.ProcessTask(ctx, asynq.NewTask("events.send_ticket_email", []byte("ok"))); err != nil {
		t.Fatal(err)
	}
	err := mux.ProcessTask(ctx, asynq.NewTask("events.send_ticket_email", []byte("fail")))
	if !queue.IsPermanent(err) || strings.Contains(err.Error(), "ayse@") {
		t.Fatalf("failing task error = %v: must stay permanent and be scrubbed", err)
	}
	if err := mux.ProcessTask(ctx, asynq.NewTask("events.send_ticket_email", []byte("panic"))); err == nil || strings.Contains(err.Error(), "ayse") {
		t.Fatalf("panicking task error = %v", err)
	}
	if err := mux.ProcessTask(ctx, asynq.NewTask("nobody.handles", nil)); !queue.IsPermanent(err) {
		t.Fatalf("unknown task type error = %v, want permanent", err)
	}
	if gotTaskID != "" {
		t.Fatalf("task id %q without an asynq context", gotTaskID)
	}
	if got := mux.Types(); len(got) != 1 || got[0] != "events.send_ticket_email" {
		t.Fatalf("Types() = %v", got)
	}
}

func TestOpenRedis_FailsFastWithoutLeakingTheURL(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if _, err := queue.OpenRedis(ctx, "http://localhost:6379", 1); err == nil {
		t.Fatal("http URL accepted")
	}
	_, err := queue.OpenRedis(ctx, "redis://:s3cr3t-pw@127.0.0.1:1/0", 1)
	if err == nil || strings.Contains(err.Error(), "s3cr3t-pw") {
		t.Fatalf("unreachable redis: %v", err)
	}
}

func TestClient_EnqueueFailsWhenRedisIsDown(t *testing.T) {
	t.Parallel()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	c := queue.NewClient(rdb, obs.Discard())
	if err := c.Enqueue(t.Context(), "files.scan_file", nil, "id-1"); err == nil {
		t.Fatal("Enqueue() succeeded without Redis")
	}
	// AfterCommit never returns the error (best effort, logged).
	c.AfterCommit(t.Context(), "files.scan_file", nil, "id-1")
}
