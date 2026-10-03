package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// backend is one Store implementation under test: the memory store always,
// PostgreSQL under the integration build tag (backend_integration_test.go).
type backend struct {
	name string
	open func(t *testing.T) (outbox.Store, db.TxManager)
}

var backends = []backend{{"mem", func(*testing.T) (outbox.Store, db.TxManager) {
	return outbox.NewMemStore(), db.NopTx{}
}}}

func addBackend(b backend) bool {
	backends = append(backends, b)
	return true
}

// forEachBackend runs fn once per backend as a parallel subtest.
func forEachBackend(t *testing.T, fn func(t *testing.T, env *env)) {
	t.Helper()
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			store, tx := b.open(t)
			clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
			e := &env{store: store, tx: tx, clock: clk, subs: outbox.NewRegistry(), q: queue.NewMem()}
			e.ob = outbox.New(outbox.Deps{Store: store, Tx: tx, Subs: e.subs, Queue: e.q, Clock: clk, Log: obs.Discard()})
			fn(t, e)
		})
	}
}

type env struct {
	store outbox.Store
	tx    db.TxManager
	clock *clock.Fake
	subs  *outbox.Registry
	q     *queue.Mem
	ob    *outbox.Outbox
}

// rows returns every row of the store, published or not.
func (e *env) pending(t *testing.T) []outbox.Row {
	t.Helper()
	var rows []outbox.Row
	err := e.tx.WithTx(t.Context(), func(ctx context.Context) error {
		var err error
		rows, err = e.store.Due(ctx, e.clock.Now().Add(1000*time.Hour), 1000)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func confirmed(t *testing.T) domainevent.Envelope {
	t.Helper()
	reg := uuid.NewV7()
	e, err := domainevent.New(domainevent.RegistrationConfirmed{RegistrationID: reg, EventID: uuid.NewV7(), UserID: uuid.NewV7()},
		"event_registration", reg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func nop(context.Context, domainevent.Envelope) error { return nil }

func TestOutbox_AddWithoutTx_Errors(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		if err := e.ob.Add(t.Context(), confirmed(t)); !errors.Is(err, outbox.ErrNoTx) {
			t.Fatalf("Add() outside a transaction = %v, want ErrNoTx", err)
		}
		if rows := e.pending(t); len(rows) != 0 {
			t.Fatalf("a row was written outside a transaction: %+v", rows)
		}
		mem := outbox.NewMem(e.clock)
		if err := mem.Add(t.Context(), confirmed(t)); !errors.Is(err, outbox.ErrNoTx) {
			t.Fatalf("Mem.Add() outside a transaction = %v, want ErrNoTx", err)
		}
	})
}

func TestOutbox_RolledBackTx_NoEvent(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		boom := errors.New("business rule failed")
		err := e.tx.WithTx(t.Context(), func(ctx context.Context) error {
			if err := e.ob.Add(ctx, confirmed(t)); err != nil {
				return err
			}
			return boom // the business write fails after the event was added
		})
		if !errors.Is(err, boom) {
			t.Fatalf("WithTx() = %v", err)
		}
		if rows := e.pending(t); len(rows) != 0 {
			t.Fatalf("rolled back transaction left %d outbox rows", len(rows))
		}

		// The same call that commits leaves exactly one row with the
		// envelope fields filled from the clock and the request context.
		ctx := obs.WithActorID(obs.WithRequestID(t.Context(), "req-12345678"), uuid.NewV7().String())
		want := confirmed(t)
		if err := e.tx.WithTx(ctx, func(ctx context.Context) error { return e.ob.Add(ctx, want) }); err != nil {
			t.Fatal(err)
		}
		rows := e.pending(t)
		if len(rows) != 1 {
			t.Fatalf("committed transaction left %d rows, want 1", len(rows))
		}
		r := rows[0]
		if r.Topic != want.Topic || r.AggregateID != want.AggregateID || r.Version != 1 || r.ActorUserID == nil ||
			r.RequestID == nil || *r.RequestID != "req-12345678" || !r.OccurredAt.Equal(e.clock.Now()) || !r.AvailableAt.Equal(e.clock.Now()) ||
			r.ID == uuid.Nil() || r.PublishedAt != nil || r.Attempts != 0 {
			t.Fatalf("row = %+v", r)
		}
		var p map[string]any
		if err := json.Unmarshal(r.Payload, &p); err != nil || p["registration_id"] != want.AggregateID.String() {
			t.Fatalf("payload = %s (%v)", r.Payload, err)
		}
	})
}

func TestOutbox_CatalogAndMechanismEnforced(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		active, err := domainevent.New(domainevent.UserActiveDay{UserID: uuid.NewV7(), Day: civil.Date{Year: 2026, Month: 10, Day: 3}}, "user", uuid.NewV7())
		if err != nil {
			t.Fatal(err)
		}
		bad := confirmed(t)
		bad.AggregateType = ""
		unknown := confirmed(t)
		unknown.Topic = "nobody.knows"
		cases := []struct {
			name string
			e    domainevent.Envelope
			want error
		}{
			{"A topic through Add", active, outbox.ErrWrongMechanism},
			{"missing aggregate", bad, outbox.ErrInvalidEvent},
			{"unknown topic", unknown, domainevent.ErrUnknownTopic},
		}
		for _, c := range cases {
			err := e.tx.WithTx(t.Context(), func(ctx context.Context) error { return e.ob.Add(ctx, c.e) })
			if !errors.Is(err, c.want) {
				t.Errorf("%s: Add() = %v, want %v", c.name, err, c.want)
			}
		}
		if err := e.ob.AfterCommit(t.Context(), confirmed(t)); !errors.Is(err, outbox.ErrWrongMechanism) {
			t.Errorf("AfterCommit(O topic) = %v, want ErrWrongMechanism", err)
		}
	})
}

func TestOutbox_AfterCommit_FansOutOnCommitOnly(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		e.subs.Subscribe(domainevent.TopicUserActiveDay, "talent.ingest_active_day", nop, outbox.Queue(queue.Low))
		e.subs.Subscribe(domainevent.TopicUserActiveDay, "students.refresh_activity", nop)
		ev, err := domainevent.New(domainevent.UserActiveDay{UserID: uuid.NewV7(), Day: civil.Date{Year: 2026, Month: 10, Day: 3}}, "user", uuid.NewV7())
		if err != nil {
			t.Fatal(err)
		}
		boom := errors.New("rollback")
		_ = e.tx.WithTx(t.Context(), func(ctx context.Context) error {
			if err := e.ob.AfterCommit(ctx, ev); err != nil {
				t.Fatal(err)
			}
			return boom
		})
		if n := len(e.q.Tasks()); n != 0 {
			t.Fatalf("rolled back transaction enqueued %d tasks", n)
		}
		if err := e.tx.WithTx(t.Context(), func(ctx context.Context) error { return e.ob.AfterCommit(ctx, ev) }); err != nil {
			t.Fatal(err)
		}
		tasks := e.q.Tasks()
		if len(tasks) != 2 || tasks[0].Type != "talent.ingest_active_day" || tasks[0].Queue != queue.Low ||
			tasks[1].Type != "students.refresh_activity" || tasks[1].Queue != queue.Default ||
			!strings.HasSuffix(tasks[0].ID, ":talent.ingest_active_day") {
			t.Fatalf("tasks = %+v", tasks)
		}
	})
}

func TestRelay_OneTaskPerSubscriber_TaskIDStable(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		e.subs.Subscribe(domainevent.TopicRegistrationConfirmed, "events.send_ticket_email", nop,
			outbox.Queue(queue.Critical), outbox.MaxRetry(queue.MoneyMaxRetry))
		e.subs.Subscribe(domainevent.TopicRegistrationConfirmed, "talent.ingest_registration", nop, outbox.Queue(queue.Low))
		ev := confirmed(t)
		if err := e.tx.WithTx(t.Context(), func(ctx context.Context) error { return e.ob.Add(ctx, ev) }); err != nil {
			t.Fatal(err)
		}
		id := e.pending(t)[0].ID
		relay := e.ob.NewRelay(outbox.RelayConfig{})
		if n, err := relay.Once(t.Context()); err != nil || n != 1 {
			t.Fatalf("Once() = %d, %v; want 1 published", n, err)
		}
		tasks := e.q.Tasks()
		if len(tasks) != 2 {
			t.Fatalf("tasks = %+v, want one per subscriber", tasks)
		}
		want := map[string]queue.Enqueued{
			"events.send_ticket_email":   {ID: id.String() + ":events.send_ticket_email", Queue: queue.Critical, MaxRetry: queue.MoneyMaxRetry},
			"talent.ingest_registration": {ID: id.String() + ":talent.ingest_registration", Queue: queue.Low, MaxRetry: queue.DefaultMaxRetry},
		}
		for _, task := range tasks {
			w := want[task.Type]
			if task.ID != w.ID || task.Queue != w.Queue || task.MaxRetry != w.MaxRetry {
				t.Errorf("task %s = %+v, want ID %s queue %s max_retry %d", task.Type, task, w.ID, w.Queue, w.MaxRetry)
			}
			var got domainevent.Envelope
			if err := json.Unmarshal(task.Payload, &got); err != nil || got.ID != id || got.Topic != ev.Topic ||
				got.AggregateID != ev.AggregateID || string(got.Payload) == "" {
				t.Errorf("task payload = %s (%v)", task.Payload, err)
			}
		}
		if rows := e.pending(t); len(rows) != 0 {
			t.Fatalf("published row still pending: %+v", rows)
		}
		if n, err := relay.Once(t.Context()); err != nil || n != 0 {
			t.Fatalf("second Once() = %d, %v; want nothing to do", n, err)
		}

		// The task ID depends only on the row and the handler: a relay that
		// crashed before marking the row would enqueue the same IDs again.
		if outbox.TaskID(id, "events.send_ticket_email") != tasks[0].ID && outbox.TaskID(id, "events.send_ticket_email") != tasks[1].ID {
			t.Fatal("TaskID is not stable")
		}
	})
}

func TestRelay_BackoffOnFailure(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		e.subs.Subscribe(domainevent.TopicRegistrationConfirmed, "events.send_ticket_email", nop)
		if err := e.tx.WithTx(t.Context(), func(ctx context.Context) error { return e.ob.Add(ctx, confirmed(t)) }); err != nil {
			t.Fatal(err)
		}
		relay := e.ob.NewRelay(outbox.RelayConfig{BackoffMin: time.Second, BackoffMax: 5 * time.Second})
		e.q.Fail(errors.New("redis: connection refused to user ayse@ogrenci.edu.tr"))

		start := e.clock.Now()
		for attempt, wantDelay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second} {
			if n, err := relay.Once(t.Context()); err != nil || n != 0 {
				t.Fatalf("attempt %d: Once() = %d, %v; want 0 published, no error", attempt+1, n, err)
			}
			rows := e.pending(t)
			if len(rows) != 1 {
				t.Fatalf("rows = %+v", rows)
			}
			r := rows[0]
			if r.Attempts != attempt+1 || !r.AvailableAt.Equal(e.clock.Now().Add(wantDelay)) || r.LastError == nil ||
				r.PublishedAt != nil {
				t.Fatalf("attempt %d: row = %+v (available_at %s, want now+%s)", attempt+1, r, r.AvailableAt, wantDelay)
			}
			if strings.Contains(*r.LastError, "ayse@") {
				t.Fatalf("last_error leaks personal data: %q", *r.LastError)
			}
			// Not due before the back-off elapses.
			if n, _ := relay.Once(t.Context()); n != 0 || len(e.q.Tasks()) != 0 {
				t.Fatal("row relayed before its back-off elapsed")
			}
			e.clock.Advance(wantDelay)
		}
		if e.clock.Now().Sub(start) != 12*time.Second {
			t.Fatalf("total back-off %s", e.clock.Now().Sub(start))
		}

		e.q.Fail(nil) // Redis is back
		if n, err := relay.Once(t.Context()); err != nil || n != 1 || len(e.q.Tasks()) != 1 {
			t.Fatalf("after recovery Once() = %d, %v with %d tasks", n, err, len(e.q.Tasks()))
		}
	})
}

func TestRelay_TopicWithoutSubscribers_MarkedPublished(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		for range 3 {
			if err := e.tx.WithTx(t.Context(), func(ctx context.Context) error { return e.ob.Add(ctx, confirmed(t)) }); err != nil {
				t.Fatal(err)
			}
		}
		relay := e.ob.NewRelay(outbox.RelayConfig{Batch: 2})
		if n, err := relay.Once(t.Context()); err != nil || n != 2 {
			t.Fatalf("Once() = %d, %v; want a batch of 2", n, err)
		}
		if n, err := relay.Once(t.Context()); err != nil || n != 1 {
			t.Fatalf("Once() = %d, %v; want the last row", n, err)
		}
		if len(e.q.Tasks()) != 0 || len(e.pending(t)) != 0 {
			t.Fatalf("tasks %d, pending %d; want none", len(e.q.Tasks()), len(e.pending(t)))
		}
	})
}

func TestOutbox_CleanupJob_DeletesOldPublishedRows(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		for range 3 {
			if err := e.tx.WithTx(t.Context(), func(ctx context.Context) error { return e.ob.Add(ctx, confirmed(t)) }); err != nil {
				t.Fatal(err)
			}
		}
		relay := e.ob.NewRelay(outbox.RelayConfig{Batch: 2})
		if _, err := relay.Once(t.Context()); err != nil { // publishes two rows now
			t.Fatal(err)
		}
		e.clock.Advance(8 * 24 * time.Hour)
		job := e.ob.CleanupJob(func(context.Context) (time.Duration, error) { return 7 * 24 * time.Hour, nil })
		if err := job.Validate(); err != nil || job.Name != "outbox.cleanup" || job.Queue != queue.Low {
			t.Fatalf("job = %+v, %v", job, err)
		}
		if err := job.Handle(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The unpublished row stays, whatever its age.
		if rows := e.pending(t); len(rows) != 1 {
			t.Fatalf("pending after cleanup = %d, want the unpublished row", len(rows))
		}
		if _, err := relay.Once(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := job.Handle(t.Context()); err != nil { // published just now: kept
			t.Fatal(err)
		}
		if ms, ok := e.store.(*outbox.MemStore); ok && len(ms.Rows()) != 1 {
			t.Fatalf("mem rows after cleanup = %d, want 1", len(ms.Rows()))
		}
	})
}

func TestRegistry_RejectsBadSubscriptions(t *testing.T) {
	t.Parallel()
	r := outbox.NewRegistry()
	r.Subscribe(domainevent.TopicFileUploaded, "files.scan_file", nop)
	r.Subscribe("nobody.knows", "files.other", nop)
	r.Subscribe(domainevent.TopicFileUploaded, "Files Scan", nop)
	r.Subscribe(domainevent.TopicFileUploaded, "files.scan_file", nop)
	r.Subscribe(domainevent.TopicFileUploaded, "files.third", nil)
	r.Subscribe(domainevent.TopicFileUploaded, "files.fourth", nop, outbox.Queue("urgent"))
	err := r.Err()
	for _, want := range []string{"not in the catalog", "Files Scan", "registered twice", "nil handler", "unknown queue"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Err() = %v, want it to mention %q", err, want)
		}
	}
	if got := r.All(); len(got) != 1 || got[0].Name != "files.scan_file" || len(r.For(domainevent.TopicFileUploaded)) != 1 {
		t.Fatalf("All() = %+v", got)
	}
}

func TestSubscription_HandleTaskDecodesTheEnvelope(t *testing.T) {
	t.Parallel()
	var got domainevent.Envelope
	var gotRequestID string
	s := outbox.Subscription{Topic: domainevent.TopicRegistrationConfirmed, Name: "events.send_ticket_email",
		Handle: func(ctx context.Context, e domainevent.Envelope) error {
			got, gotRequestID = e, obs.RequestID(ctx)
			return nil
		}}
	ev := confirmed(t)
	ev.ID, ev.RequestID = uuid.NewV7(), "req-abcdefgh"
	b, _ := json.Marshal(ev)
	if err := s.HandleTask(t.Context(), b); err != nil || got.ID != ev.ID || gotRequestID != "req-abcdefgh" {
		t.Fatalf("HandleTask() = %v, envelope %+v, request id %q", err, got, gotRequestID)
	}
	if err := s.HandleTask(t.Context(), []byte("{")); !queue.IsPermanent(err) {
		t.Fatalf("undecodable payload: %v, want a permanent error", err)
	}
	ev.Topic = domainevent.TopicEventCancelled
	b, _ = json.Marshal(ev)
	if err := s.HandleTask(t.Context(), b); !queue.IsPermanent(err) {
		t.Fatalf("foreign topic: %v, want a permanent error", err)
	}
}

func TestMem_RecordsCommittedEventsOnly(t *testing.T) {
	t.Parallel()
	m := outbox.NewMem(clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)))
	tx := db.NopTx{}
	_ = tx.WithTx(t.Context(), func(ctx context.Context) error {
		_ = m.Add(ctx, confirmed(t))
		return errors.New("rollback")
	})
	ev, _ := domainevent.New(domainevent.FileUploaded{FileID: uuid.NewV7()}, "file", uuid.NewV7())
	_ = tx.WithTx(t.Context(), func(ctx context.Context) error {
		if err := m.Add(ctx, confirmed(t)); err != nil {
			return err
		}
		return m.AfterCommit(ctx, ev)
	})
	if got := m.Topics(); !slices.Equal(got, []string{"registration.confirmed", "file.uploaded"}) {
		t.Fatalf("Topics() = %v", got)
	}
	if e := m.Events()[0]; e.ID == uuid.Nil() || e.OccurredAt.IsZero() {
		t.Fatalf("event not filled: %+v", e)
	}
}

func TestRelay_RunStopsOnCancelAndBeats(t *testing.T) {
	t.Parallel()
	forEachBackend(t, func(t *testing.T, e *env) {
		relay := e.ob.NewRelay(outbox.RelayConfig{Idle: 5 * time.Millisecond})
		if err := relay.Alive(e.clock.Now()); err == nil {
			t.Fatal("Alive() before Run must fail")
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { relay.Run(ctx); close(done) }()
		e.subs.Subscribe(domainevent.TopicRegistrationConfirmed, "events.send_ticket_email", nop)
		if err := e.tx.WithTx(t.Context(), func(ctx context.Context) error { return e.ob.Add(ctx, confirmed(t)) }); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(5 * time.Second)
		for len(e.q.Tasks()) == 0 {
			select {
			case <-deadline:
				t.Fatal("Run did not relay the row")
			case <-time.After(5 * time.Millisecond):
			}
		}
		if err := relay.Alive(e.clock.Now()); err != nil {
			t.Fatalf("Alive() = %v", err)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop on cancel")
		}
		e.clock.Advance(relay.MaxBeatAge() + time.Second)
		if err := relay.Alive(e.clock.Now()); err == nil {
			t.Fatal("Alive() with a stale beat must fail")
		}
	})
}
