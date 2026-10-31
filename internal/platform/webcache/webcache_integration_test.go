//go:build integration

package webcache_test

import (
	"context"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"
	"uuid"

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
	webcache.New(webcache.Deps{Resolver: resolver(), Sender: newSender(t, web), Log: log}).Subscribe(subs)
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

// TestRevalidate_PostChanged_OutboxRelayWorker (BE-149): a post.changed row
// of a public post, written in a PostgreSQL transaction, reaches the web
// through the relay and the asynq worker as post:<id> + the author org's
// tag; one of a post off the public web sends nothing.
func TestRevalidate_PostChanged_OutboxRelayWorker(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	rdb := testutil.NewRedis(t)
	ctx := t.Context()
	log := obs.Discard()
	web := newFakeWeb(t)

	subs := outbox.NewRegistry()
	webcache.New(webcache.Deps{Resolver: resolver(), Sender: newSender(t, web), Log: log}).Subscribe(subs)
	client := queue.NewClient(rdb, log)
	t.Cleanup(func() { _ = client.Close() })
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: subs, Queue: client, Clock: clock.NewFake(t0), Log: log})
	for _, p := range []domainevent.PostChanged{
		{PostID: postID, OrgID: companyID, Public: false, Change: "published"},
		{PostID: postID, OrgID: clubID, Public: true, Change: "edited"},
	} {
		env, err := domainevent.New(p, "post", p.PostID)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Tx.WithTx(ctx, func(ctx context.Context) error { return ob.Add(ctx, env) }); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := ob.NewRelay(outbox.RelayConfig{}).Once(ctx); err != nil || n != 2 {
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
	// Both tasks done (the queue is drained, nothing retried or archived):
	// only the public post reached the web.
	insp := asynq.NewInspectorFromRedisClient(rdb)
	waitUntil(t, func() bool {
		q, err := insp.GetQueueInfo(queue.Default)
		return err == nil && len(web.got(t)) == 1 && q.Pending == 0 && q.Active == 0 && q.Retry == 0 && q.Archived == 0
	})
	if got := web.got(t); len(got) != 1 || !slices.Equal(got[0], []string{webcache.PostTag(postID), "club:odtu-robotik"}) {
		t.Fatalf("calls = %v, want one [post tag, club tag]", got)
	}
}

// TestRevalidate_EventSharesAndRejectedMedia_OutboxRelayWorker (BE-150):
// an event.updated row of a shared event leaving the public web and a
// file.rejected row of a public post's image, written in PostgreSQL
// transactions, reach the web through the relay and the asynq worker: the
// event's pages with every public post sharing it (each author org once),
// and the post's page with its author org's.
func TestRevalidate_EventSharesAndRejectedMedia_OutboxRelayWorker(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	rdb := testutil.NewRedis(t)
	ctx := t.Context()
	log := obs.Discard()
	web := newFakeWeb(t)

	subs := outbox.NewRegistry()
	webcache.New(webcache.Deps{Resolver: resolver(), Sender: newSender(t, web), Log: log}).Subscribe(subs)
	client := queue.NewClient(rdb, log)
	t.Cleanup(func() { _ = client.Close() })
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: subs, Queue: client, Clock: clock.NewFake(t0), Log: log})
	for _, p := range []domainevent.Payload{
		domainevent.EventUpdated{EventID: sharedEvID, OrgID: clubID, Fields: []string{"visibility"}},
		domainevent.FileRejected{FileID: uuid.NewV7(), Purpose: "post_media", Reason: "infected", AttachedModule: "feed", AttachedEntityID: &postID},
	} {
		env, err := domainevent.New(p, "test", uuid.NewV7())
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Tx.WithTx(ctx, func(ctx context.Context) error { return ob.Add(ctx, env) }); err != nil {
			t.Fatal(err)
		}
	}
	// file.rejected also feeds the modules' own subscriptions; here only
	// the webcache ones are registered: one task per row.
	if n, err := ob.NewRelay(outbox.RelayConfig{}).Once(ctx); err != nil || n != 2 {
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
	insp := asynq.NewInspectorFromRedisClient(rdb)
	waitUntil(t, func() bool {
		q, err := insp.GetQueueInfo(queue.Default)
		return err == nil && len(web.got(t)) == 2 && q.Pending == 0 && q.Active == 0 && q.Retry == 0 && q.Archived == 0
	})
	got := web.got(t)
	slices.SortFunc(got, func(a, b []string) int { return len(a) - len(b) })
	shares := []string{"event:bahar-senligi", "club:odtu-robotik", "sitemap",
		webcache.PostTag(share1), webcache.PostTag(share2), "company:acme", webcache.PostTag(share3)}
	if !slices.Equal(got[0], []string{webcache.PostTag(postID), "club:odtu-robotik"}) || !slices.Equal(got[1], shares) {
		t.Fatalf("calls = %v, want [post image page] and [%v]", got, shares)
	}
}

// TestRevalidate_OrganiserRenamed_OutboxRelayWorker (BE-157): an
// org.updated row of a renamed organiser and one of a changed description,
// written in PostgreSQL transactions, reach the web through the relay and
// the asynq worker: the rename also refreshes the public posts sharing its
// events (each author org once), the description the org's page alone.
func TestRevalidate_OrganiserRenamed_OutboxRelayWorker(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	rdb := testutil.NewRedis(t)
	ctx := t.Context()
	log := obs.Discard()
	web := newFakeWeb(t)

	subs := outbox.NewRegistry()
	webcache.New(webcache.Deps{Resolver: resolver(), Sender: newSender(t, web), Log: log}).Subscribe(subs)
	client := queue.NewClient(rdb, log)
	t.Cleanup(func() { _ = client.Close() })
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: subs, Queue: client, Clock: clock.NewFake(t0), Log: log})
	for _, p := range []domainevent.OrgUpdated{
		{OrgID: clubID, OrgKind: "club", Fields: []string{"name"}},
		{OrgID: companyID, OrgKind: "company", Fields: []string{"description"}},
	} {
		env, err := domainevent.New(p, "org", p.OrgID)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Tx.WithTx(ctx, func(ctx context.Context) error { return ob.Add(ctx, env) }); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := ob.NewRelay(outbox.RelayConfig{}).Once(ctx); err != nil || n != 2 {
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
	insp := asynq.NewInspectorFromRedisClient(rdb)
	waitUntil(t, func() bool {
		q, err := insp.GetQueueInfo(queue.Default)
		return err == nil && len(web.got(t)) == 2 && q.Pending == 0 && q.Active == 0 && q.Retry == 0 && q.Archived == 0
	})
	got := web.got(t)
	slices.SortFunc(got, func(a, b []string) int { return len(a) - len(b) })
	renamed := []string{"club:odtu-robotik", webcache.PostTag(share1), webcache.PostTag(share2), "company:acme", webcache.PostTag(share3)}
	if !slices.Equal(got[0], []string{"company:acme"}) || !slices.Equal(got[1], renamed) {
		t.Fatalf("calls = %v, want [company page] and [%v]", got, renamed)
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
