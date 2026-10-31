//go:build integration

package feed_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// pgWorld inserts the parent and reader rows as app_rw.
func pgWorld(d *db.DB) world {
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	return world{
		org: func(t *testing.T, kind feed.AuthorKind) uuid.UUID {
			id, n := uuid.NewV7(), strconv.FormatInt(seq.Add(1), 10)
			exec(t, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, $2, $3, $4)`, id, string(kind), "Org "+n, "org-"+n)
			return id
		},
		user: func(t *testing.T) uuid.UUID {
			id := uuid.NewV7()
			email := "u" + strconv.FormatInt(seq.Add(1), 10) + "@example.test"
			exec(t, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
				VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, email)
			return id
		},
		file: func(t *testing.T, org uuid.UUID) uuid.UUID {
			id := uuid.NewV7()
			exec(t, `INSERT INTO files (id, purpose, org_id, bucket, storage_key, declared_mime, size_bytes, sha256, status, scan_status)
				VALUES ($1, 'post_media', $2, 'koro-files-dev', $3, 'image/png', 10, decode(repeat('ab', 32), 'hex'), 'active', 'clean')`,
				id, org, "post_media/2026/10/"+id.String())
			return id
		},
		follow: func(t *testing.T, u, o uuid.UUID) {
			exec(t, `INSERT INTO org_follows (user_id, org_id, source) VALUES ($1, $2, 'manual')`, u, o)
		},
		mute: func(t *testing.T, u, o uuid.UUID) {
			exec(t, `INSERT INTO org_mutes (user_id, org_id) VALUES ($1, $2)`, u, o)
		},
		hide: func(t *testing.T, u, p uuid.UUID) {
			exec(t, `INSERT INTO post_hides (user_id, post_id) VALUES ($1, $2)`, u, p)
		},
		like: func(t *testing.T, u, p uuid.UUID) {
			exec(t, `INSERT INTO post_reactions (post_id, user_id) VALUES ($1, $2)`, p, u)
		},
		save: func(t *testing.T, u, p uuid.UUID) {
			exec(t, `INSERT INTO post_saves (user_id, post_id) VALUES ($1, $2)`, u, p)
		},
		trust: func(t *testing.T, o uuid.UUID, l feed.TrustLevel, n int) {
			exec(t, `INSERT INTO feed_org_settings (org_id, trust_level, approved_posts_count) VALUES ($1, $2, $3)`, o, string(l), n)
		},
		event: func(t *testing.T, org uuid.UUID) uuid.UUID {
			id, by := uuid.NewV7(), uuid.NewV7()
			email := "e" + strconv.FormatInt(seq.Add(1), 10) + "@example.test"
			exec(t, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
				VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, by, email)
			exec(t, `INSERT INTO events (id, org_id, created_by, title, slug, category, format, starts_at, ends_at, status, published_at)
				VALUES ($1, $2, $3, 'Bahar Şenliği', $4, 'social', 'in_person', now() + interval '30 days', now() + interval '31 days',
				'published', now())`, id, org, by, "e-"+strconv.FormatInt(seq.Add(1), 10))
			return id
		},
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (feed.Store, world) {
		d := testutil.NewDB(t)
		return feed.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestFeedQuery_NoNPlusOne (05 §6.4): a feed page costs the same three
// feed statements (posts, media, the reader's state) whatever its size.
func TestFeedQuery_NoNPlusOne(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	store := feed.NewGormStore(d.ORM)
	ctx := t.Context()
	org, by, reader := w.org(t, feed.AuthorClub), w.user(t), w.user(t)
	orgsDir := newFakeOrgs()
	orgsDir.orgs[org] = orgsDir.orgs[orgID("club_a")]
	o := orgsDir.orgs[org]
	o.ID = org
	orgsDir.orgs[org] = o
	for i := range 25 {
		p := post(org, feed.AuthorClub, by)
		if i%2 == 0 {
			p.Kind, p.BodyJSON, p.BodyText = feed.KindImage, nil, nil
		}
		p, err := store.InsertPost(ctx, publishedAt(p, pub0.Add(time.Duration(i)*time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		if p.Kind == feed.KindImage {
			if err := store.ReplaceMedia(ctx, org, p.ID, []feed.Media{{ID: uuid.NewV7(), FileID: w.file(t, org)}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	w.follow(t, reader, org)
	clk := clock.NewFake(t0)
	mod, err := feed.New(feed.Deps{
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(), Audit: &audittest.Recorder{}, Orgs: orgsDir,
		Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()}),
		Clock:       clk, Log: obs.Discard(), Viewers: &fakeViewers{keys: map[uuid.UUID][]string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := authz.Actor{Kind: authz.KindUser, UserID: reader, StudentStatus: authz.StudentNone}
	count := func(limit int) (int, int) {
		qc := testutil.NewQueryCounter()
		p, err := mod.Service.GetFeed(qc.Context(ctx), a, feed.FeedQuery{Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range p.Items {
			if v.Viewer == nil || !v.Viewer.FollowingAuthor {
				t.Fatalf("viewer state missing on %s", v.Post.ID)
			}
		}
		return qc.Count(), len(p.Items)
	}
	small, n5 := count(5)
	large, n20 := count(20)
	if n5 != 5 || n20 != 20 {
		t.Fatalf("page sizes = %d, %d", n5, n20)
	}
	if small != large || large > 3 {
		t.Fatalf("feed statements: %d for 5 posts, %d for 20 (want the same, ≤ 3)", small, large)
	}
}

// TestLike_Concurrent_CountsConsistent (PostgreSQL): readers liking the
// same post at once, each several times, and unliking while the hourly
// reconciliation runs leave exactly one row per reader and a like_count
// equal to the rows; every repeated request reports "no change" exactly
// once per reader, and the post's version (updated_at) never moves.
func TestLike_Concurrent_CountsConsistent(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	store := feed.NewGormStore(d.ORM)
	ctx := t.Context()
	org, by := w.org(t, feed.AuthorClub), w.user(t)
	p, err := store.InsertPost(ctx, publishedAt(post(org, feed.AuthorClub, by), pub0))
	if err != nil {
		t.Fatal(err)
	}
	const readers, repeats = 24, 3
	users := make([]uuid.UUID, readers)
	for i := range users {
		users[i] = w.user(t)
	}
	run := func(op func(u uuid.UUID) (bool, error), only func(i int) bool) (int64, error) {
		var changed atomic.Int64
		var wg sync.WaitGroup
		errs := make(chan error, readers*repeats+4)
		for i, u := range users {
			if !only(i) {
				continue
			}
			for range repeats {
				wg.Go(func() {
					ok, err := op(u)
					if err != nil {
						errs <- err
					}
					if ok {
						changed.Add(1)
					}
				})
			}
		}
		for range 2 { // the reconciliation runs meanwhile
			wg.Go(func() {
				if _, err := store.ReconcileCounters(ctx); err != nil {
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		return changed.Load(), <-errs // nil when no error was sent
	}
	check := func(want int) {
		t.Helper()
		var rows int64
		if err := d.ORM.Raw(`SELECT count(*) FROM post_reactions WHERE post_id = ?`, p.ID).Scan(&rows).Error; err != nil {
			t.Fatal(err)
		}
		got, err := store.GetPost(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rows != int64(want) || got.LikeCount != want {
			t.Fatalf("rows = %d, like_count = %d, want %d", rows, got.LikeCount, want)
		}
		if !got.UpdatedAt.Equal(p.UpdatedAt) {
			t.Fatalf("updated_at moved with the counter: %v → %v", p.UpdatedAt, got.UpdatedAt)
		}
	}
	all := func(int) bool { return true }
	if n, err := run(func(u uuid.UUID) (bool, error) { return store.AddLike(ctx, u, p.ID) }, all); err != nil || n != readers {
		t.Fatalf("new likes = %d, want %d (%v)", n, readers, err)
	}
	check(readers)
	// Half unlike (each several times), the other half like again.
	even := func(i int) bool { return i%2 == 0 }
	odd := func(i int) bool { return i%2 == 1 }
	var removed, again int64
	var errRemove, errAgain error
	var wg sync.WaitGroup
	wg.Go(func() {
		removed, errRemove = run(func(u uuid.UUID) (bool, error) { return store.RemoveLike(ctx, u, p.ID) }, even)
	})
	wg.Go(func() {
		again, errAgain = run(func(u uuid.UUID) (bool, error) { return store.AddLike(ctx, u, p.ID) }, odd)
	})
	wg.Wait()
	if errRemove != nil || errAgain != nil || removed != readers/2 || again != 0 {
		t.Fatalf("removed = %d, liked again = %d (%v, %v)", removed, again, errRemove, errAgain)
	}
	check(readers / 2)
	if n, err := store.ReconcileCounters(ctx); err != nil || n != 0 {
		t.Fatalf("ReconcileCounters after the races = %d, %v", n, err)
	}
}

// TestLike_Service_Postgres: the service on PostgreSQL — two likes are one
// row and one count, the ETag of the post stays, unlike without a like is
// a no-op; a hide is listed and undone (BE-145).
func TestLike_Service_Postgres(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	store := feed.NewGormStore(d.ORM)
	ctx := t.Context()
	org, by, reader := w.org(t, feed.AuthorClub), w.user(t), w.user(t)
	p, err := store.InsertPost(ctx, publishedAt(post(org, feed.AuthorClub, by), pub0))
	if err != nil {
		t.Fatal(err)
	}
	orgsDir := newFakeOrgs()
	o := orgsDir.orgs[orgID("club_a")]
	o.ID = org
	orgsDir.orgs[org] = o
	clk := clock.NewFake(t0)
	mod, err := feed.New(feed.Deps{
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(), Audit: &audittest.Recorder{}, Orgs: orgsDir,
		Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()}),
		Clock:       clk, Log: obs.Discard(), Viewers: &fakeViewers{keys: map[uuid.UUID][]string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := authz.Actor{Kind: authz.KindUser, UserID: reader, StudentStatus: authz.StudentNone}
	before, err := mod.Service.GetPost(ctx, a, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := mod.Service.LikePost(ctx, a, p.ID); err != nil {
			t.Fatal(err)
		}
		if err := mod.Service.SavePost(ctx, a, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := mod.Service.GetPost(ctx, a, p.ID)
	if err != nil || got.Post.LikeCount != 1 || got.Post.SaveCount != 1 || !got.Viewer.Liked || !got.Viewer.Saved {
		t.Fatalf("GetPost = %+v / %+v, %v", got.Post, got.Viewer, err)
	}
	if got.ETag != before.ETag {
		t.Fatalf("ETag moved: %s → %s", before.ETag, got.ETag)
	}
	if err := mod.Service.UnlikePost(ctx, authz.Actor{Kind: authz.KindUser, UserID: w.user(t)}, p.ID); err != nil {
		t.Fatalf("UnlikePost(no like) = %v", err)
	}
	if err := mod.Service.HidePost(ctx, a, p.ID); err != nil {
		t.Fatal(err)
	}
	if page, err := mod.Service.GetFeed(ctx, a, feed.FeedQuery{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("feed after hide = %d, %v", len(page.Items), err)
	}
	// BE-145: the hide is listed, then undone (twice is once).
	if page, err := mod.Service.ListHiddenPosts(ctx, a, feed.PageQuery{}); err != nil || len(page.Items) != 1 ||
		page.Items[0].Post.ID != p.ID {
		t.Fatalf("hidden = %+v, %v", page.Items, err)
	}
	for range 2 {
		if err := mod.Service.UnhidePost(ctx, a, p.ID); err != nil {
			t.Fatalf("UnhidePost = %v", err)
		}
	}
	if page, err := mod.Service.GetFeed(ctx, a, feed.FeedQuery{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("feed after unhide = %d, %v", len(page.Items), err)
	}
	if page, err := mod.Service.ListHiddenPosts(ctx, a, feed.PageQuery{}); err != nil || len(page.Items) != 0 {
		t.Fatalf("hidden after unhide = %d, %v", len(page.Items), err)
	}
}

// TestRemovedPostsDue_SkipsReferencedPosts_Gorm (T-152 option a; BE-057):
// a removed post a dispatch still refers to (outreach_dispatches.post_id,
// NO ACTION, kept 2 years) is not due: it goes in a later run, after the
// dispatch. The others go.
func TestRemovedPostsDue_SkipsReferencedPosts_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	s := feed.NewGormStore(d.ORM)
	ctx := t.Context()
	org, by := w.org(t, feed.AuthorClub), w.user(t)
	removedAt := pub0.AddDate(-1, -1, 0)
	var ids []uuid.UUID
	for range 2 {
		p, err := s.InsertPost(ctx, post(org, feed.AuthorClub, by))
		if err != nil {
			t.Fatal(err)
		}
		p = publishedAt(p, removedAt.AddDate(0, -1, 0))
		p.Status, p.RemovedAt = feed.StatusRemoved, &removedAt
		if _, err := s.UpdatePost(ctx, p); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO outreach_dispatches (org_id, post_id, audience_keys, audience_dim_count, channels,
		title_snapshot, deep_link, commercial, created_by) VALUES ($1, $2, ARRAY['all'], 1, ARRAY['in_app'], 'Duyuru', '/posts/x', false, $3)`,
		org, ids[0], by); err != nil {
		t.Fatal(err)
	}
	cutoff := pub0.AddDate(-1, 0, 0)
	if n, err := s.CountRemovedPostsDue(ctx, cutoff); err != nil || n != 1 {
		t.Fatalf("CountRemovedPostsDue = %d, %v; want the unreferenced post", n, err)
	}
	err := d.Tx.WithTx(ctx, func(ctx context.Context) error {
		due, err := s.RemovedPostsDue(ctx, cutoff, 10)
		if err != nil || len(due) != 1 || due[0].ID != ids[1] {
			return fmt.Errorf("RemovedPostsDue = %+v: %w", due, err)
		}
		_, err = s.PurgePosts(ctx, []uuid.UUID{due[0].ID})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPost(ctx, ids[0]); err != nil {
		t.Fatalf("the referenced post was purged: %v", err)
	}
}

func TestStatsStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	statsContract(t, func(t *testing.T) (feed.Store, world) {
		d := testutil.NewDB(t)
		return feed.NewGormStore(d.ORM), pgWorld(d)
	})
}
