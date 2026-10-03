package inbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/inbox"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// backend is one Store under test: memory always, PostgreSQL under the
// integration tag (inbox_integration_test.go).
type backend struct {
	name string
	open func(t *testing.T) (inbox.Store, db.TxManager, func(t *testing.T) []inbox.Row)
}

var backends = []backend{{"mem", func(*testing.T) (inbox.Store, db.TxManager, func(t *testing.T) []inbox.Row) {
	s := inbox.NewMemStore()
	return s, db.NopTx{}, func(*testing.T) []inbox.Row { return s.Rows() }
}}}

func addBackend(b backend) bool {
	backends = append(backends, b)
	return true
}

type env struct {
	in    *inbox.Inbox
	q     *queue.Mem
	clock *clock.Fake
	rows  func(t *testing.T) []inbox.Row
}

func forEachBackend(t *testing.T, fn func(t *testing.T, e *env)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			store, tx, rows := b.open(t)
			clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
			q := queue.NewMem()
			in := inbox.New(inbox.Deps{Store: store, Tx: tx, Queue: q, Clock: clk, Log: obs.Discard()})
			fn(t, &env{in: in, q: q, clock: clk, rows: rows})
		})
	}
}

const bounce = `{"notificationType":"Bounce","mail":{"destination":["ayse@ogrenci.edu.tr"]}}`

func TestInbox_DuplicateEvent_IsNewFalse(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		hdr := map[string]string{"X-Amz-Sns-Message-Type": "Notification", "Authorization": "Bearer secret", "cookie": "s=1"}
		isNew, err := e.in.Receive(ctx, inbox.ProviderEmail, "sns-msg-0001", true, hdr, []byte(bounce))
		if err != nil || !isNew {
			t.Fatalf("first Receive() = %v, %v; want new", isNew, err)
		}
		isNew, err = e.in.Receive(ctx, inbox.ProviderEmail, "sns-msg-0001", true, hdr, []byte(bounce))
		if err != nil || isNew {
			t.Fatalf("second Receive() = %v, %v; want isNew=false", isNew, err)
		}
		// The same key from another provider is a different event.
		if isNew, err := e.in.Receive(ctx, inbox.ProviderIYS, "sns-msg-0001", true, nil, []byte(`{}`)); err != nil || !isNew {
			t.Fatalf("other provider: %v, %v", isNew, err)
		}
		rows := e.rows(t)
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(rows))
		}
		r := rows[0]
		if r.Provider != inbox.ProviderEmail || !r.SignatureValid || len(r.PayloadSHA256) != 32 || r.Headers["X-Amz-Sns-Message-Type"] != "Notification" {
			t.Fatalf("row = %+v", r)
		}
		for k := range r.Headers {
			if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "cookie") {
				t.Fatalf("credential header %q stored", k)
			}
		}
		// One processing task per stored row: the duplicate delivery re-enqueues
		// the same task ID, which is deduplicated.
		tasks := e.q.Tasks()
		if len(tasks) != 2 || tasks[0].Type != inbox.ProcessTaskName || tasks[0].ID != r.ID.String()+":"+inbox.ProcessTaskName {
			t.Fatalf("tasks = %+v", tasks)
		}
	})
}

func TestInbox_ForgedFirstDeliveryDoesNotBlockTheRealOne(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		if isNew, err := e.in.Receive(ctx, inbox.ProviderIyzico, "evt-1", false, nil, []byte(`{"forged":true}`)); err != nil || !isNew {
			t.Fatalf("forged: %v, %v", isNew, err)
		}
		if len(e.q.Tasks()) != 0 {
			t.Fatal("an unsigned delivery was queued for processing")
		}
		if isNew, err := e.in.Receive(ctx, inbox.ProviderIyzico, "evt-1", true, nil, []byte(`{"real":true}`)); err != nil || !isNew {
			t.Fatalf("real delivery after a forged one: %v, %v; want it stored as new", isNew, err)
		}
		rows := e.rows(t)
		if len(rows) != 1 || !rows[0].SignatureValid || !strings.Contains(string(rows[0].Payload), "real") || len(e.q.Tasks()) != 1 {
			t.Fatalf("rows = %+v, tasks = %d", rows, len(e.q.Tasks()))
		}
		// A forged copy of an accepted event changes nothing.
		if isNew, _ := e.in.Receive(ctx, inbox.ProviderIyzico, "evt-1", false, nil, []byte(`{"forged":2}`)); isNew {
			t.Fatal("forged copy replaced a valid delivery")
		}
	})
}

func TestInbox_ProcessRunsTheProcessorOnce(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		var got []inbox.Event
		fail := true
		if err := e.in.Register(inbox.Processor{Provider: inbox.ProviderEmail, Queue: queue.Critical, Handle: func(_ context.Context, ev inbox.Event) error {
			if fail {
				return errors.New("store busy for ayse@ogrenci.edu.tr")
			}
			got = append(got, ev)
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
		if err := e.in.Register(inbox.Processor{Provider: inbox.ProviderEmail, Handle: func(context.Context, inbox.Event) error { return nil }}); err == nil {
			t.Fatal("second processor for a provider accepted")
		}
		if _, err := e.in.Receive(ctx, inbox.ProviderEmail, "sns-msg-0002", true, nil, []byte(bounce)); err != nil {
			t.Fatal(err)
		}
		task := e.q.Tasks()[0]
		if task.Queue != queue.Critical {
			t.Fatalf("task queue = %s, want the processor's queue", task.Queue)
		}
		handle := e.in.Task().Handle

		err := handle(ctx, task.Payload)
		if err == nil || queue.IsPermanent(err) {
			t.Fatalf("failing processor: %v, want a retryable error", err)
		}
		r := e.rows(t)[0]
		if r.ProcessedAt != nil || r.Attempts != 1 || r.LastError == nil || strings.Contains(*r.LastError, "ayse@") {
			t.Fatalf("after failure: %+v", r)
		}

		fail = false
		if err := handle(ctx, task.Payload); err != nil {
			t.Fatal(err)
		}
		if err := handle(ctx, task.Payload); err != nil { // redelivered task
			t.Fatal(err)
		}
		r = e.rows(t)[0]
		if len(got) != 1 || r.ProcessedAt == nil || r.Attempts != 2 || got[0].EventKey != "sns-msg-0002" {
			t.Fatalf("processor runs = %d, row = %+v", len(got), r)
		}
		var p map[string]any
		if err := json.Unmarshal(got[0].Payload, &p); err != nil || p["notificationType"] != "Bounce" {
			t.Fatalf("event payload = %s", got[0].Payload)
		}
		// A stored, processed duplicate is not queued again.
		e.q.Reset()
		if isNew, err := e.in.Receive(ctx, inbox.ProviderEmail, "sns-msg-0002", true, nil, []byte(bounce)); err != nil || isNew || len(e.q.Tasks()) != 0 {
			t.Fatalf("processed duplicate: %v, %v, %d tasks", isNew, err, len(e.q.Tasks()))
		}
	})
}

func TestInbox_PermanentFailures(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		handle := e.in.Task().Handle
		if err := handle(ctx, []byte(`{`)); !queue.IsPermanent(err) {
			t.Fatalf("bad payload: %v", err)
		}
		missing, _ := json.Marshal(map[string]string{"id": uuid.NewV7().String()})
		if err := handle(ctx, missing); !queue.IsPermanent(err) {
			t.Fatalf("missing row: %v", err)
		}
		if _, err := e.in.Receive(ctx, inbox.ProviderParasut, "job-1", true, nil, []byte("not json")); err != nil {
			t.Fatal(err)
		}
		if err := handle(ctx, e.q.Tasks()[0].Payload); !errors.Is(err, inbox.ErrNoProcessor) || !queue.IsPermanent(err) {
			t.Fatalf("no processor: %v", err)
		}
		if r := e.rows(t)[0]; string(r.Payload) != `"not json"` || r.LastError == nil {
			t.Fatalf("row = %+v", r)
		}
	})
}

func TestInbox_ReceiveRules(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		if _, err := e.in.Receive(ctx, "stripe", "k", true, nil, nil); !errors.Is(err, inbox.ErrInvalid) {
			t.Errorf("unknown provider: %v", err)
		}
		if _, err := e.in.Receive(ctx, inbox.ProviderEmail, strings.Repeat("k", 201), true, nil, nil); !errors.Is(err, inbox.ErrInvalid) {
			t.Errorf("long event key: %v", err)
		}
		err := db.NopTx{}.WithTx(ctx, func(ctx context.Context) error {
			_, err := e.in.Receive(ctx, inbox.ProviderEmail, "k", true, nil, nil)
			return err
		})
		if !errors.Is(err, inbox.ErrInTx) {
			t.Errorf("inside a transaction: %v", err)
		}
		// Redis down: the row is stored, the error goes back to the provider
		// (5xx → it retries) and the retry re-enqueues the unprocessed row.
		e.q.Fail(errors.New("redis down"))
		if _, err := e.in.Receive(ctx, inbox.ProviderEmail, "sns-msg-0003", true, nil, []byte(`{}`)); err == nil {
			t.Fatal("enqueue failure was swallowed")
		}
		e.q.Fail(nil)
		if isNew, err := e.in.Receive(ctx, inbox.ProviderEmail, "sns-msg-0003", true, nil, []byte(`{}`)); err != nil || isNew || len(e.q.Tasks()) != 1 {
			t.Fatalf("provider retry: %v, %v, %d tasks", isNew, err, len(e.q.Tasks()))
		}
	})
}

func TestInbox_PurgeJob(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		ctx := t.Context()
		if _, err := e.in.Receive(ctx, inbox.ProviderEmail, "old", true, map[string]string{"X-A": "1"}, []byte(bounce)); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(100 * 24 * time.Hour)
		if _, err := e.in.Receive(ctx, inbox.ProviderEmail, "new", true, nil, []byte(bounce)); err != nil {
			t.Fatal(err)
		}
		days := func(n int) func(context.Context) (time.Duration, error) {
			return func(context.Context) (time.Duration, error) { return time.Duration(n) * 24 * time.Hour, nil }
		}
		job := e.in.PurgeJob(days(90), days(730))
		if err := job.Validate(); err != nil || job.Name != "webhook_inbox.purge_payloads" {
			t.Fatalf("job = %+v, %v", job, err)
		}
		if err := job.Handle(ctx); err != nil {
			t.Fatal(err)
		}
		rows := e.rows(t)
		if len(rows) != 2 || rows[0].Payload != nil || rows[0].Headers != nil || rows[0].PayloadPurgedAt == nil ||
			rows[1].Payload == nil || rows[1].PayloadPurgedAt != nil {
			t.Fatalf("after payload purge: %+v", rows)
		}
		e.clock.Advance(700 * 24 * time.Hour)
		if err := job.Handle(ctx); err != nil {
			t.Fatal(err)
		}
		if rows := e.rows(t); len(rows) != 1 || rows[0].EventKey != "new" {
			t.Fatalf("after row purge: %+v", rows)
		}
	})
}
