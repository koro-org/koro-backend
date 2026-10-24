//go:build integration

package webcache_test

import (
	"context"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
	"koro/internal/platform/webcache"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// TestRevalidate_OutboxRelayWorker_PostsSignedTags: an event.published row
// written in a PostgreSQL transaction is relayed to Redis and processed by
// the asynq worker, which POSTs the signed tags to the (fake) web; a
// cancellation the web refuses for good (422) is archived at once, not
// retried (BE-106).
func TestRevalidate_OutboxRelayWorker_PostsSignedTags(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	rdb := testutil.NewRedis(t)
	ctx := t.Context()
	log := obs.Discard()
	web := newFakeWeb(t)

	subs := outbox.NewRegistry()
	webcache.New(webcache.Deps{Resolver: resolver(), Sender: newSender(t, web.srv.URL), Log: log}).Subscribe(subs)
	client := queue.NewClient(rdb, log)
	t.Cleanup(func() { _ = client.Close() })
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: subs, Queue: client, Clock: clock.NewFake(t0), Log: log})

	add := func(p domainevent.Payload) {
		t.Helper()
		env, err := domainevent.New(p, "event", eventID)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Tx.WithTx(ctx, func(ctx context.Context) error { return ob.Add(ctx, env) }); err != nil {
			t.Fatal(err)
		}
	}
	add(domainevent.EventPublished{EventID: eventID, OrgID: clubID})
	if n, err := ob.NewRelay(outbox.RelayConfig{}).Once(ctx); err != nil || n != 1 {
		t.Fatalf("relay = %d, %v", n, err)
	}

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
	t.Cleanup(srv.Shutdown)
	waitUntil(t, func() bool { return len(web.got(t)) == 1 })
	if got := web.got(t); !slices.Equal(got[0], []string{"event:go-atolyesi", "club:odtu-robotik", "sitemap"}) {
		t.Fatalf("tags = %v", got)
	}

	// The web refuses for good: the task is archived without a retry.
	web.mu.Lock()
	web.statuses = []int{http.StatusUnprocessableEntity}
	web.mu.Unlock()
	add(domainevent.EventCancelled{EventID: eventID, OrgID: clubID})
	if n, err := ob.NewRelay(outbox.RelayConfig{}).Once(ctx); err != nil || n != 1 {
		t.Fatalf("relay = %d, %v", n, err)
	}
	insp := asynq.NewInspectorFromRedisClient(rdb)
	waitUntil(t, func() bool {
		archived, err := insp.ListArchivedTasks(queue.Default)
		return err == nil && len(archived) == 1 && archived[0].Type == webcache.SubEventCancelled && archived[0].Retried == 0
	})
	if n := len(web.got(t)); n != 2 {
		t.Fatalf("%d requests, want 2 (no retry after the 422)", n)
	}
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
