//go:build integration

package feed_test

import (
	"os"
	"strconv"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
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
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Audit: &auditLog{}, Orgs: orgsDir,
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
