package feed_test

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/feed"
	"koro/internal/platform/problem"
)

// world creates the parent and reader rows a store needs (FKs and the
// BE-029 tables in PostgreSQL; MemStore seeds in memory).
type world struct {
	org    func(t *testing.T, kind feed.AuthorKind) uuid.UUID
	user   func(t *testing.T) uuid.UUID
	file   func(t *testing.T, org uuid.UUID) uuid.UUID
	follow func(t *testing.T, user, org uuid.UUID)
	mute   func(t *testing.T, user, org uuid.UUID)
	hide   func(t *testing.T, user, post uuid.UUID)
	like   func(t *testing.T, user, post uuid.UUID)
	save   func(t *testing.T, user, post uuid.UUID)
	trust  func(t *testing.T, org uuid.UUID, level feed.TrustLevel, approved int)
	event  func(t *testing.T, org uuid.UUID) uuid.UUID // a published event of org (event_share)
}

func memWorld(s *feed.MemStore) world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	return world{
		org:    func(*testing.T, feed.AuthorKind) uuid.UUID { return uuid.NewV7() },
		user:   id,
		file:   func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() },
		follow: func(_ *testing.T, u, o uuid.UUID) { s.PutFollow(u, o) },
		mute:   func(_ *testing.T, u, o uuid.UUID) { s.PutMute(u, o) },
		hide:   func(_ *testing.T, u, p uuid.UUID) { s.PutHide(u, p) },
		like:   func(_ *testing.T, u, p uuid.UUID) { s.PutLike(u, p) },
		save:   func(_ *testing.T, u, p uuid.UUID) { s.PutSave(u, p) },
		trust: func(_ *testing.T, o uuid.UUID, l feed.TrustLevel, n int) {
			s.PutSettings(feed.OrgSettings{OrgID: o, TrustLevel: l, ApprovedPostsCount: n})
		},
		event: func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() },
	}
}

var seq atomic.Int64

// post is a valid public text draft of org by author.
func post(org uuid.UUID, kind feed.AuthorKind, author uuid.UUID) feed.Post {
	title := "Gönderi " + strconv.FormatInt(seq.Add(1), 10)
	text := "metin"
	return feed.Post{
		ID: uuid.NewV7(), OrgID: org, AuthorOrgKind: kind, AuthorUserID: author, Kind: feed.KindText, Title: &title,
		BodyJSON: feed.JSON(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"metin"}]}]}`),
		BodyText: &text, AudienceKind: feed.AudiencePublic, AudienceKeys: []string{feed.KeyAll}, AudienceDimCount: 1,
		CommercialFlag: kind == feed.AuthorCompany, Status: feed.StatusDraft,
	}
}

// publishedAt makes p a published post of at.
func publishedAt(p feed.Post, at time.Time) feed.Post {
	p.Status, p.PublishedAt = feed.StatusPublished, &at
	return p
}

var pub0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) (feed.Store, world) {
		s := feed.NewMemStore(func() time.Time { return t0 })
		return s, memWorld(s)
	})
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (feed.Store, world)) {
	t.Helper()
	insert := func(t *testing.T, s feed.Store, p feed.Post) feed.Post {
		t.Helper()
		got, err := s.InsertPost(t.Context(), p)
		if err != nil {
			t.Fatalf("InsertPost: %v", err)
		}
		return got
	}
	ids := func(ps []feed.Post) []uuid.UUID {
		out := []uuid.UUID{}
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return out
	}

	t.Run("posts: insert, read, scoped update and delete", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorClub), w.user(t)
		p := insert(t, s, post(org, feed.AuthorClub, by))
		if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() || p.SchemaVersion != 1 || p.LikeCount != 0 ||
			!slices.Equal(p.AudienceKeys, []string{"all"}) {
			t.Fatalf("stored = %+v", p)
		}
		got, err := s.GetPost(ctx, p.ID)
		if err != nil || *got.Title != *p.Title || string(got.BodyJSON) == "" || got.AuthorUserID != by {
			t.Fatalf("GetPost = %+v, %v", got, err)
		}
		if _, err := s.GetPost(ctx, uuid.NewV7()); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("GetPost(unknown) = %v", err)
		}
		if _, err := s.LockPost(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		// Another org's id never updates the row (06 H-38).
		foreign := p
		foreign.OrgID = other
		if _, err := s.UpdatePost(ctx, foreign); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("UpdatePost(other org) = %v", err)
		}
		next := publishedAt(p, pub0)
		upd, err := s.UpdatePost(ctx, next)
		if err != nil || upd.Status != feed.StatusPublished || !upd.UpdatedAt.After(p.UpdatedAt) && !upd.UpdatedAt.Equal(p.UpdatedAt) {
			t.Fatalf("UpdatePost = %+v, %v", upd, err)
		}
		// The CHECKs hold in both stores: a company post must be commercial,
		// a published post needs published_at.
		bad := post(w.org(t, feed.AuthorCompany), feed.AuthorCompany, by)
		bad.CommercialFlag = false
		if _, err := s.InsertPost(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("company non-commercial post = %v", err)
		}
		bad = post(org, feed.AuthorClub, by)
		bad.Status = feed.StatusPublished
		if _, err := s.InsertPost(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("published without published_at = %v", err)
		}
		// Only a post never published is deleted, and only by its org.
		if err := s.DeletePost(ctx, org, p.ID); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("DeletePost(published) = %v", err)
		}
		d := insert(t, s, post(org, feed.AuthorClub, by))
		if err := s.DeletePost(ctx, other, d.ID); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("DeletePost(other org) = %v", err)
		}
		if err := s.DeletePost(ctx, org, d.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetPost(ctx, d.ID); !errors.Is(err, feed.ErrNotFound) {
			t.Fatal("deleted post still readable")
		}
		mine, err := s.PostsOfAuthor(ctx, by)
		if err != nil || !slices.Equal(ids(mine), []uuid.UUID{p.ID}) {
			t.Fatalf("PostsOfAuthor = %v, %v", ids(mine), err)
		}
	})

	t.Run("media: scoped replace, page read, cascade", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorClub), w.user(t)
		p := insert(t, s, post(org, feed.AuthorClub, by))
		p.Kind, p.BodyJSON, p.BodyText = feed.KindImage, nil, nil
		if _, err := s.UpdatePost(ctx, p); err != nil {
			t.Fatal(err)
		}
		f1, f2 := w.file(t, org), w.file(t, org)
		alt := "afiş"
		media := []feed.Media{{ID: uuid.NewV7(), FileID: f1, AltText: &alt}, {ID: uuid.NewV7(), FileID: f2}}
		if err := s.ReplaceMedia(ctx, other, p.ID, media); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("ReplaceMedia(other org) = %v", err)
		}
		if err := s.ReplaceMedia(ctx, org, p.ID, media); err != nil {
			t.Fatal(err)
		}
		got, err := s.MediaOf(ctx, []uuid.UUID{p.ID, uuid.NewV7()})
		if err != nil || len(got) != 1 || len(got[p.ID]) != 2 || got[p.ID][0].FileID != f1 || got[p.ID][1].Position != 1 ||
			*got[p.ID][0].AltText != "afiş" {
			t.Fatalf("MediaOf = %+v, %v", got, err)
		}
		// Replacing keeps the new list only.
		if err := s.ReplaceMedia(ctx, org, p.ID, media[1:]); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.MediaOf(ctx, []uuid.UUID{p.ID}); len(got[p.ID]) != 1 || got[p.ID][0].FileID != f2 || got[p.ID][0].Position != 0 {
			t.Fatalf("after replace = %+v", got)
		}
		// A file belongs to one post (post_media_file_id_uq).
		q := insert(t, s, post(org, feed.AuthorClub, by))
		q.Kind, q.BodyJSON, q.BodyText = feed.KindImage, nil, nil
		if _, err := s.UpdatePost(ctx, q); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceMedia(ctx, org, q.ID, []feed.Media{{ID: uuid.NewV7(), FileID: f2}}); err == nil {
			t.Fatal("a file was attached to two posts")
		}
		if err := s.DeletePost(ctx, org, p.ID); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.MediaOf(ctx, []uuid.UUID{p.ID}); len(got) != 0 {
			t.Fatal("media outlived their post")
		}
	})

	t.Run("dropped media: scoped drop, read, due unless reported, forget, cascade (BE-158)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by, reader := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorClub), w.user(t), w.user(t)
		p := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		q := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		f1, f2, f3 := w.file(t, org), w.file(t, org), w.file(t, org)
		alt, width := "afiş", 1080
		old, young := pub0.AddDate(0, -13, 0), pub0.AddDate(0, -1, 0)
		drop := []feed.DroppedMedia{{FileID: f1, AltText: &alt, Width: &width, Height: &width, DroppedAt: old}, {FileID: f2, DroppedAt: young}}
		if err := s.DropMedia(ctx, other, p.ID, drop); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("DropMedia(other org) = %v", err)
		}
		if err := s.DropMedia(ctx, org, p.ID, drop); err != nil {
			t.Fatal(err)
		}
		if err := s.DropMedia(ctx, org, q.ID, []feed.DroppedMedia{{FileID: f3, DroppedAt: old}}); err != nil {
			t.Fatal(err)
		}
		// A file is dropped once (post_dropped_media_pkey).
		if err := s.DropMedia(ctx, org, q.ID, []feed.DroppedMedia{{FileID: f1, DroppedAt: old}}); err == nil {
			t.Fatal("a file was dropped twice")
		}
		got, err := s.DroppedMediaOf(ctx, []uuid.UUID{p.ID, uuid.NewV7()})
		if err != nil || len(got) != 1 || len(got[p.ID]) != 2 {
			t.Fatalf("DroppedMediaOf = %+v, %v", got, err)
		}
		if d := got[p.ID][0]; d.FileID != f1 || d.PostID != p.ID || d.AltText == nil || *d.AltText != alt || d.Width == nil ||
			*d.Width != width || !d.DroppedAt.Equal(old) || got[p.ID][1].FileID != f2 {
			t.Fatalf("DroppedMediaOf(p) = %+v", got[p.ID])
		}

		// Due after 12 months, unless a report on the post is unresolved.
		cutoff := pub0.AddDate(0, -12, 0)
		r, _, err := s.InsertReport(ctx, feed.Report{ID: uuid.NewV7(), ReporterUserID: reader, TargetType: feed.TargetPost,
			TargetID: p.ID, Reason: feed.ReasonSpam})
		if err != nil {
			t.Fatal(err)
		}
		if n, err := s.CountDroppedMediaDue(ctx, cutoff); err != nil || n != 1 {
			t.Fatalf("CountDroppedMediaDue with an open report = %d, %v; want 1", n, err)
		}
		if due, err := s.DroppedMediaDue(ctx, cutoff, 10); err != nil || len(due) != 1 || due[0].FileID != f3 || due[0].PostID != q.ID {
			t.Fatalf("DroppedMediaDue with an open report = %+v, %v", due, err)
		}
		resolved := pub0
		r.Status, r.ResolvedAt = feed.ReportDismissed, &resolved
		if _, err := s.UpdateReport(ctx, r); err != nil {
			t.Fatal(err)
		}
		if n, err := s.CountDroppedMediaDue(ctx, cutoff); err != nil || n != 2 {
			t.Fatalf("CountDroppedMediaDue = %d, %v; want 2", n, err)
		}
		if due, err := s.DroppedMediaDue(ctx, cutoff, 1); err != nil || len(due) != 1 {
			t.Fatalf("DroppedMediaDue(limit 1) = %+v, %v", due, err)
		}
		if n, err := s.ForgetDroppedMedia(ctx, []uuid.UUID{f1, f3, uuid.NewV7()}); err != nil || n != 2 {
			t.Fatalf("ForgetDroppedMedia = %d, %v; want 2", n, err)
		}
		if got, _ := s.DroppedMediaOf(ctx, []uuid.UUID{p.ID, q.ID}); len(got) != 1 || len(got[p.ID]) != 1 || got[p.ID][0].FileID != f2 {
			t.Fatalf("after ForgetDroppedMedia = %+v", got)
		}
		// The rows go with their post.
		if n, err := s.PurgePosts(ctx, []uuid.UUID{p.ID}); err != nil || n != 1 {
			t.Fatalf("PurgePosts = %d, %v", n, err)
		}
		if got, _ := s.DroppedMediaOf(ctx, []uuid.UUID{p.ID}); len(got) != 0 {
			t.Fatalf("dropped media outlived their post: %+v", got)
		}
	})

	t.Run("feed: audience, minor, followers, mutes, hides, tabs, keyset", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		club, company, other, by := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorCompany), w.org(t, feed.AuthorClub), w.user(t)
		reader, follower := w.user(t), w.user(t)
		at := func(i int) time.Time { return pub0.Add(time.Duration(i) * time.Minute) }
		pub := insert(t, s, publishedAt(post(club, feed.AuthorClub, by), at(1)))
		ad := insert(t, s, publishedAt(post(company, feed.AuthorCompany, by), at(2)))
		fp := post(club, feed.AuthorClub, by)
		fp.AudienceKind = feed.AudienceFollowers
		fol := insert(t, s, publishedAt(fp, at(3)))
		img := post(other, feed.AuthorClub, by)
		img.Kind = feed.KindImage
		otherPost := insert(t, s, publishedAt(img, at(4)))
		insert(t, s, post(club, feed.AuthorClub, by)) // a draft is never listed
		w.follow(t, follower, club)
		list := func(f feed.FeedFilter, after *feed.After, limit int) []uuid.UUID {
			t.Helper()
			ps, err := s.ListFeed(ctx, f, after, limit)
			if err != nil {
				t.Fatalf("ListFeed: %v", err)
			}
			return ids(ps)
		}
		r := feed.Reader{UserID: &reader, Keys: []string{"all"}}
		if got := list(feed.FeedFilter{Reader: r}, nil, 10); !slices.Equal(got, []uuid.UUID{otherPost.ID, ad.ID, pub.ID}) {
			t.Fatalf("reader feed = %v", got)
		}
		minor := r
		minor.Minor = true
		if got := list(feed.FeedFilter{Reader: minor}, nil, 10); !slices.Equal(got, []uuid.UUID{otherPost.ID, pub.ID}) {
			t.Fatalf("minor feed = %v", got)
		}
		fr := feed.Reader{UserID: &follower, Keys: []string{"all"}}
		if got := list(feed.FeedFilter{Reader: fr}, nil, 10); !slices.Equal(got, []uuid.UUID{otherPost.ID, fol.ID, ad.ID, pub.ID}) {
			t.Fatalf("follower feed = %v", got)
		}
		member := feed.Reader{UserID: &reader, Keys: []string{"all"}, MemberOrgs: []uuid.UUID{club}}
		if got := list(feed.FeedFilter{Reader: member, Following: true}, nil, 10); !slices.Equal(got, []uuid.UUID{fol.ID, pub.ID}) {
			t.Fatalf("member following tab = %v", got)
		}
		if got := list(feed.FeedFilter{Reader: fr, Following: true}, nil, 10); !slices.Equal(got, []uuid.UUID{fol.ID, pub.ID}) {
			t.Fatalf("follower following tab = %v", got)
		}
		if got := list(feed.FeedFilter{Reader: r, Kind: feed.KindImage}, nil, 10); !slices.Equal(got, []uuid.UUID{otherPost.ID}) {
			t.Fatalf("image feed = %v", got)
		}
		// Keyset: strictly after (published_at, id).
		if got := list(feed.FeedFilter{Reader: fr}, &feed.After{At: at(3), ID: fol.ID}, 10); !slices.Equal(got, []uuid.UUID{ad.ID, pub.ID}) {
			t.Fatalf("after fol = %v", got)
		}
		if got := list(feed.FeedFilter{Reader: fr}, nil, 2); !slices.Equal(got, []uuid.UUID{otherPost.ID, fol.ID}) {
			t.Fatalf("limit 2 = %v", got)
		}
		// Mutes and hides are the reader's own.
		w.mute(t, reader, company)
		w.hide(t, reader, otherPost.ID)
		if got := list(feed.FeedFilter{Reader: r}, nil, 10); !slices.Equal(got, []uuid.UUID{pub.ID}) {
			t.Fatalf("muted/hidden feed = %v", got)
		}
		if got := list(feed.FeedFilter{Reader: fr}, nil, 10); len(got) != 4 {
			t.Fatalf("another reader's feed = %v", got)
		}
		// Segment keys (BE-032 writes them): every dimension must match.
		seg := post(club, feed.AuthorClub, by)
		seg.AudienceKind, seg.AudienceKeys, seg.AudienceDimCount = feed.AudienceSegment, []string{"uni:u1", "year:3", "year:4"}, 2
		seg.AudienceSnapshot = feed.JSON(`{"uni":["u1"],"year":[3,4]}`)
		segPost := insert(t, s, publishedAt(seg, at(5)))
		third := feed.Reader{UserID: &follower, Keys: []string{"all", "uni:u1", "year:3"}}
		second := feed.Reader{UserID: &follower, Keys: []string{"all", "uni:u1", "year:2"}}
		if got := list(feed.FeedFilter{Reader: third}, nil, 1); !slices.Equal(got, []uuid.UUID{segPost.ID}) {
			t.Fatalf("segment match = %v", got)
		}
		if got := list(feed.FeedFilter{Reader: second}, nil, 1); slices.Contains(got, segPost.ID) {
			t.Fatalf("segment non-match = %v", got)
		}
		// Org page: anonymous public non-commercial; All sees everything.
		page := func(org uuid.UUID, rd feed.Reader) []uuid.UUID {
			t.Helper()
			ps, err := s.ListOrgPosts(ctx, org, rd, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			return ids(ps)
		}
		if got := page(club, feed.Reader{Keys: []string{"all"}}); !slices.Equal(got, []uuid.UUID{pub.ID}) {
			t.Fatalf("anonymous club page = %v", got)
		}
		if got := page(company, feed.Reader{Keys: []string{"all"}}); len(got) != 0 {
			t.Fatalf("anonymous company page = %v", got)
		}
		if got := page(club, feed.Reader{All: true}); !slices.Equal(got, []uuid.UUID{segPost.ID, fol.ID, pub.ID}) {
			t.Fatalf("staff club page = %v", got)
		}
		if got := page(company, r); !slices.Equal(got, []uuid.UUID{ad.ID}) {
			t.Fatalf("muted org page = %v (mutes are feed-only)", got)
		}
		// Anonymous following tab is empty.
		if got := list(feed.FeedFilter{Reader: feed.Reader{Keys: []string{"all"}}, Following: true}, nil, 10); len(got) != 0 {
			t.Fatalf("anonymous following = %v", got)
		}
	})

	t.Run("event shares: published, the reader's visibility, every org, newest first, several events (BE-150, BE-157)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		club, company, by := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorCompany), w.user(t)
		ev, other := w.event(t, club), w.event(t, club)
		at := func(i int) time.Time { return pub0.Add(time.Duration(i) * time.Minute) }
		share := func(org uuid.UUID, kind feed.AuthorKind, event uuid.UUID, edit func(*feed.Post)) feed.Post {
			p := post(org, kind, by)
			p.Kind, p.EventID, p.BodyJSON, p.BodyText = feed.KindEventShare, &event, nil, nil
			if edit != nil {
				edit(&p)
			}
			return insert(t, s, p)
		}
		published := func(i int) func(*feed.Post) {
			return func(p *feed.Post) { *p = publishedAt(*p, at(i)) }
		}
		older := share(club, feed.AuthorClub, ev, published(1))
		newer := share(club, feed.AuthorClub, ev, published(3))
		ad := share(company, feed.AuthorCompany, ev, published(2)) // commercial: not anonymous
		followers := share(club, feed.AuthorClub, ev, func(p *feed.Post) {
			*p = publishedAt(*p, at(4))
			p.AudienceKind = feed.AudienceFollowers
		})
		share(club, feed.AuthorClub, ev, nil)                             // a draft
		otherShare := share(club, feed.AuthorClub, other, published(5))   // another event
		insert(t, s, publishedAt(post(club, feed.AuthorClub, by), at(6))) // not a share
		list := func(r feed.Reader, limit int, evs ...uuid.UUID) []uuid.UUID {
			t.Helper()
			if evs == nil {
				evs = []uuid.UUID{ev}
			}
			ps, err := s.ListEventShares(ctx, evs, r, limit)
			if err != nil {
				t.Fatalf("ListEventShares: %v", err)
			}
			return ids(ps)
		}
		anon := feed.Reader{Keys: []string{feed.KeyAll}}
		if got := list(anon, 10); !slices.Equal(got, []uuid.UUID{newer.ID, older.ID}) {
			t.Fatalf("anonymous shares = %v, want %v", got, []uuid.UUID{newer.ID, older.ID})
		}
		if got := list(anon, 1); !slices.Equal(got, []uuid.UUID{newer.ID}) {
			t.Fatalf("limit 1 = %v", got)
		}
		if got := list(feed.Reader{All: true}, 10); !slices.Equal(got, []uuid.UUID{followers.ID, newer.ID, ad.ID, older.ID}) {
			t.Fatalf("all shares = %v", got)
		}
		// Several events (an organiser's, BE-157): one newest-first list.
		if got := list(anon, 10, ev, other); !slices.Equal(got, []uuid.UUID{otherShare.ID, newer.ID, older.ID}) {
			t.Fatalf("shares of two events = %v", got)
		}
		if got := list(anon, 2, other, ev); !slices.Equal(got, []uuid.UUID{otherShare.ID, newer.ID}) {
			t.Fatalf("shares of two events, limit 2 = %v", got)
		}
		for name, evs := range map[string][]uuid.UUID{"unknown event": {uuid.NewV7()}, "no event": {}} {
			if ps, err := s.ListEventShares(ctx, evs, anon, 10); err != nil || len(ps) != 0 {
				t.Fatalf("%s = %v, %v", name, ps, err)
			}
		}
	})

	t.Run("manage list, settings, follows, viewer state", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by, reader := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorClub), w.user(t), w.user(t)
		a := insert(t, s, post(org, feed.AuthorClub, by))
		b := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		c := insert(t, s, post(org, feed.AuthorClub, by))
		insert(t, s, post(other, feed.AuthorClub, by))
		all, err := s.ListOrgManage(ctx, org, feed.ManageFilter{}, nil, 10)
		if err != nil || !slices.Equal(ids(all), []uuid.UUID{c.ID, b.ID, a.ID}) {
			t.Fatalf("ListOrgManage = %v, %v", ids(all), err)
		}
		drafts, _ := s.ListOrgManage(ctx, org, feed.ManageFilter{Statuses: []feed.Status{feed.StatusDraft}}, nil, 10)
		if !slices.Equal(ids(drafts), []uuid.UUID{c.ID, a.ID}) {
			t.Fatalf("drafts = %v", ids(drafts))
		}
		// BE-127: a kind narrows the list (with the statuses).
		if got, _ := s.ListOrgManage(ctx, org, feed.ManageFilter{Kind: a.Kind}, nil, 10); !slices.Equal(ids(got), []uuid.UUID{c.ID, b.ID, a.ID}) {
			t.Fatalf("kind %s = %v", a.Kind, ids(got))
		}
		if got, _ := s.ListOrgManage(ctx, org, feed.ManageFilter{Statuses: []feed.Status{feed.StatusPublished}, Kind: feed.KindOpportunityCall}, nil, 10); len(got) != 0 {
			t.Fatalf("published opportunity calls = %v", ids(got))
		}
		after, _ := s.ListOrgManage(ctx, org, feed.ManageFilter{}, &c.ID, 1)
		if !slices.Equal(ids(after), []uuid.UUID{b.ID}) {
			t.Fatalf("after = %v", ids(after))
		}
		if _, ok, err := s.OrgSettings(ctx, org); ok || err != nil {
			t.Fatalf("OrgSettings(new) = %v, %v", ok, err)
		}
		w.trust(t, org, feed.TrustTrusted, 4)
		st, ok, err := s.OrgSettings(ctx, org)
		if !ok || err != nil || st.TrustLevel != feed.TrustTrusted || st.ApprovedPostsCount != 4 {
			t.Fatalf("OrgSettings = %+v, %v, %v", st, ok, err)
		}
		if f, _ := s.Follows(ctx, reader, org); f {
			t.Fatal("follows before the row")
		}
		w.follow(t, reader, org)
		w.like(t, reader, b.ID)
		w.save(t, reader, a.ID)
		if f, _ := s.Follows(ctx, reader, org); !f {
			t.Fatal("follow row not read")
		}
		if _, err := s.AddMute(ctx, reader, other); err != nil {
			t.Fatal(err)
		}
		vs, err := s.ViewerState(ctx, reader, []uuid.UUID{a.ID, b.ID, c.ID}, []uuid.UUID{org, other})
		if err != nil || !vs.Liked[b.ID] || vs.Liked[a.ID] || !vs.Saved[a.ID] || vs.Saved[b.ID] || !vs.Following[org] || vs.Following[other] ||
			!vs.Muted[other] || vs.Muted[org] {
			t.Fatalf("ViewerState = %+v, %v", vs, err)
		}
		if vs, err := s.ViewerState(ctx, reader, nil, nil); err != nil || len(vs.Liked) != 0 {
			t.Fatalf("empty ViewerState = %+v, %v", vs, err)
		}
	})
	t.Run("likes and saves: idempotent rows, counters move with them", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by, u1, u2 := w.org(t, feed.AuthorClub), w.user(t), w.user(t), w.user(t)
		p := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		counts := func() (int, int, time.Time) {
			t.Helper()
			got, err := s.GetPost(ctx, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			return got.LikeCount, got.SaveCount, got.UpdatedAt
		}
		_, _, v0 := counts()
		for i, want := range []bool{true, false} {
			if ok, err := s.AddLike(ctx, u1, p.ID); err != nil || ok != want {
				t.Fatalf("AddLike #%d = %v, %v", i, ok, err)
			}
		}
		if ok, _ := s.AddLike(ctx, u2, p.ID); !ok {
			t.Fatal("second user's like")
		}
		if ok, _ := s.AddSave(ctx, u1, p.ID); !ok {
			t.Fatal("save")
		}
		if ok, _ := s.AddSave(ctx, u1, p.ID); ok {
			t.Fatal("save twice")
		}
		if l, sv, v := counts(); l != 2 || sv != 1 || !v.Equal(v0) {
			t.Fatalf("counts = %d, %d (updated_at %v → %v: counters must not move the version)", l, sv, v0, v)
		}
		vs, _ := s.ViewerState(ctx, u1, []uuid.UUID{p.ID}, nil)
		if !vs.Liked[p.ID] || !vs.Saved[p.ID] {
			t.Fatalf("ViewerState = %+v", vs)
		}
		for i, want := range []bool{true, false} {
			if ok, err := s.RemoveLike(ctx, u1, p.ID); err != nil || ok != want {
				t.Fatalf("RemoveLike #%d = %v, %v", i, ok, err)
			}
			if ok, err := s.RemoveSave(ctx, u1, p.ID); err != nil || ok != want {
				t.Fatalf("RemoveSave #%d = %v, %v", i, ok, err)
			}
		}
		if l, sv, _ := counts(); l != 1 || sv != 0 {
			t.Fatalf("counts after removal = %d, %d", l, sv)
		}
		// A row without a count (drift) goes without the counter below 0.
		q := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0.Add(time.Minute)))
		w.like(t, u1, q.ID)
		if ok, err := s.RemoveLike(ctx, u1, q.ID); err != nil || !ok {
			t.Fatalf("RemoveLike(drift) = %v, %v", ok, err)
		}
		if got, _ := s.GetPost(ctx, q.ID); got.LikeCount != 0 {
			t.Fatalf("like_count = %d", got.LikeCount)
		}
		// A missing post is 404, not a row.
		if _, err := s.AddLike(ctx, u1, uuid.NewV7()); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("AddLike(missing) = %v", err)
		}
		if _, err := s.AddSave(ctx, u1, uuid.NewV7()); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("AddSave(missing) = %v", err)
		}
		if ok, err := s.AddHide(ctx, u1, p.ID); err != nil || !ok {
			t.Fatalf("AddHide = %v, %v", ok, err)
		}
		if ok, err := s.AddHide(ctx, u1, p.ID); err != nil || ok {
			t.Fatalf("AddHide twice = %v, %v", ok, err)
		}
		if _, err := s.AddHide(ctx, u1, uuid.NewV7()); !errors.Is(err, problem.ErrNotFound) {
			t.Fatalf("AddHide(missing) = %v", err)
		}
		// BE-145: a hide is undone once; another reader's hide stays.
		if _, err := s.AddHide(ctx, u2, p.ID); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.RemoveHide(ctx, u1, p.ID); err != nil || !ok {
			t.Fatalf("RemoveHide = %v, %v", ok, err)
		}
		if ok, err := s.RemoveHide(ctx, u1, p.ID); err != nil || ok {
			t.Fatalf("RemoveHide twice = %v, %v", ok, err)
		}
		if ok, err := s.RemoveHide(ctx, u1, uuid.NewV7()); err != nil || ok {
			t.Fatalf("RemoveHide(missing) = %v, %v", ok, err)
		}
		if in, _ := s.InteractionsOf(ctx, u2); len(in.Hides) != 1 {
			t.Fatalf("another reader's hides = %d", len(in.Hides))
		}
	})

	t.Run("withdrawn likes: the post exists and holds no like of the user (BE-090)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by, liker, unliker := w.org(t, feed.AuthorClub), w.user(t), w.user(t), w.user(t)
		p := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		removed := insert(t, s, post(org, feed.AuthorClub, by))
		if _, err := s.AddLike(ctx, liker, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddLike(ctx, unliker, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RemoveLike(ctx, unliker, p.ID); err != nil {
			t.Fatal(err)
		}
		w.like(t, unliker, removed.ID)
		if err := s.DeletePost(ctx, org, removed.ID); err != nil {
			t.Fatal(err)
		}
		still := feed.LikeKey{UserID: liker, PostID: p.ID}
		gone := feed.LikeKey{UserID: unliker, PostID: p.ID}
		deleted := feed.LikeKey{UserID: unliker, PostID: removed.ID}
		missing := feed.LikeKey{UserID: liker, PostID: uuid.NewV7()}
		got, err := s.WithdrawnLikes(ctx, []feed.LikeKey{still, gone, deleted, missing, gone})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !got[gone] {
			t.Fatalf("WithdrawnLikes = %v, want only %v", got, gone)
		}
		if got, err := s.WithdrawnLikes(ctx, nil); err != nil || len(got) != 0 {
			t.Fatalf("WithdrawnLikes(nil) = %v, %v", got, err)
		}
	})

	// The reader's own post lists (saved; hidden, BE-145) read the same way.
	for _, l := range []struct {
		name string
		add  func(feed.Store, context.Context, uuid.UUID, uuid.UUID) (bool, error)
		list func(feed.Store, context.Context, feed.Reader, *feed.After, int) ([]feed.MarkedPost, error)
	}{
		{"saved", feed.Store.AddSave, feed.Store.ListSaved},
		{"hidden", feed.Store.AddHide, feed.Store.ListHidden},
	} {
		t.Run(l.name+" list: visible posts only, keyset by row time", func(t *testing.T) {
			t.Parallel()
			s, w := newStore(t)
			ctx := t.Context()
			add := func(ctx context.Context, u, p uuid.UUID) (bool, error) { return l.add(s, ctx, u, p) }
			list := func(ctx context.Context, r feed.Reader, after *feed.After, n int) ([]feed.MarkedPost, error) {
				return l.list(s, ctx, r, after, n)
			}
			club, company, by, reader, other := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorCompany), w.user(t), w.user(t), w.user(t)
			var posts []feed.Post
			for i := range 4 {
				posts = append(posts, insert(t, s, publishedAt(post(club, feed.AuthorClub, by), pub0.Add(time.Duration(i)*time.Minute))))
			}
			draft := insert(t, s, post(club, feed.AuthorClub, by))
			ad := insert(t, s, publishedAt(post(company, feed.AuthorCompany, by), pub0))
			fol := post(club, feed.AuthorClub, by)
			fol.AudienceKind = feed.AudienceFollowers
			fol = insert(t, s, publishedAt(fol, pub0))
			for _, p := range append(posts, draft, ad, fol) {
				if _, err := add(ctx, reader, p.ID); err != nil {
					t.Fatal(err)
				}
			}
			// Another reader's row is not the reader's.
			if _, err := add(ctx, other, posts[0].ID); err != nil {
				t.Fatal(err)
			}
			r := feed.Reader{UserID: &reader, Keys: []string{"all"}}
			read := func(rd feed.Reader, limit int) []feed.MarkedPost {
				t.Helper()
				var out []feed.MarkedPost
				var after *feed.After
				for range 20 {
					page, err := list(ctx, rd, after, limit)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, page...)
					if len(page) < limit {
						return out
					}
					last := page[len(page)-1]
					after = &feed.After{At: last.MarkedAt, ID: last.Post.ID}
				}
				t.Fatal("list did not end")
				return nil
			}
			got := read(r, 2)
			gotIDs := []uuid.UUID{}
			for i, sp := range got {
				gotIDs = append(gotIDs, sp.Post.ID)
				if i > 0 && cmp.Or(sp.MarkedAt.Compare(got[i-1].MarkedAt), sp.Post.ID.Compare(got[i-1].Post.ID)) >= 0 {
					t.Fatalf("not newest first at %d", i)
				}
			}
			// The draft and the followers post (no follow) are skipped; the
			// commercial post is an adult's.
			want := []uuid.UUID{posts[0].ID, posts[1].ID, posts[2].ID, posts[3].ID, ad.ID}
			slices.SortFunc(want, func(a, b uuid.UUID) int { return a.Compare(b) })
			sorted := slices.SortedFunc(slices.Values(gotIDs), func(a, b uuid.UUID) int { return a.Compare(b) })
			if !slices.Equal(sorted, want) {
				t.Fatalf("%s = %v, want %v", l.name, sorted, want)
			}
			minor := r
			minor.Minor = true
			if got := read(minor, 10); len(got) != 4 {
				t.Fatalf("minor %s = %d", l.name, len(got))
			}
			w.follow(t, reader, club)
			if got := read(r, 10); len(got) != 6 {
				t.Fatalf("follower %s = %d", l.name, len(got))
			}
			// A removed post leaves the list (its row stays until the post goes).
			gone := posts[1]
			at := pub0.Add(time.Hour)
			gone.Status, gone.RemovedAt = feed.StatusRemoved, &at
			if _, err := s.UpdatePost(ctx, gone); err != nil {
				t.Fatal(err)
			}
			if got := read(r, 10); len(got) != 5 || slices.ContainsFunc(got, func(m feed.MarkedPost) bool { return m.Post.ID == gone.ID }) {
				t.Fatalf("%s after removal = %d", l.name, len(got))
			}
			if got, _ := list(ctx, feed.Reader{Keys: []string{"all"}}, nil, 10); len(got) != 0 {
				t.Fatalf("anonymous %s = %v", l.name, got)
			}
		})
	}

	t.Run("follows and mutes", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		a, b, c, u, other := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorClub), w.org(t, feed.AuthorCompany), w.user(t), w.user(t)
		yes := true
		if created, err := s.FollowManual(ctx, u, a, &yes); err != nil || !created {
			t.Fatalf("FollowManual(new) = %v, %v", created, err)
		}
		if created, err := s.FollowManual(ctx, u, a, nil); err != nil || created {
			t.Fatalf("FollowManual(again) = %v, %v", created, err)
		}
		if ok, err := s.AddAutoFollow(ctx, u, a); err != nil || ok {
			t.Fatalf("AddAutoFollow over manual = %v, %v", ok, err)
		}
		if ok, err := s.RemoveAutoFollow(ctx, u, a); err != nil || ok {
			t.Fatalf("RemoveAutoFollow(manual) = %v, %v", ok, err)
		}
		for _, x := range []struct{ user, org uuid.UUID }{{u, b}, {other, b}, {u, c}} {
			if ok, err := s.AddAutoFollow(ctx, x.user, x.org); err != nil || !ok {
				t.Fatalf("AddAutoFollow = %v, %v", ok, err)
			}
		}
		list := func(user uuid.UUID) []feed.Follow {
			t.Helper()
			var out []feed.Follow
			var after *feed.After
			for range 10 {
				page, err := s.ListFollows(ctx, user, after, 1)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, page...)
				if len(page) < 1 {
					return out
				}
				after = &feed.After{At: page[0].CreatedAt, ID: page[0].OrgID}
			}
			return out
		}
		fs := list(u)
		if len(fs) != 3 {
			t.Fatalf("follows = %+v", fs)
		}
		byOrg := map[uuid.UUID]feed.Follow{}
		for _, f := range fs {
			byOrg[f.OrgID] = f
		}
		if f := byOrg[a]; f.Source != feed.FollowManual || !f.Notify {
			t.Fatalf("manual follow = %+v", f)
		}
		if f := byOrg[b]; f.Source != feed.FollowAutoMembership || f.Notify {
			t.Fatalf("auto follow = %+v", f)
		}
		// notify: nil keeps it, false clears it; an auto follow confirmed by
		// hand becomes manual.
		no := false
		if _, err := s.FollowManual(ctx, u, a, &no); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FollowManual(ctx, u, c, nil); err != nil {
			t.Fatal(err)
		}
		fs = list(u)
		for _, f := range fs {
			switch f.OrgID {
			case a:
				if f.Notify {
					t.Fatal("notify not cleared")
				}
			case c:
				if f.Source != feed.FollowManual {
					t.Fatalf("confirmed follow = %+v", f)
				}
			}
		}
		if ok, err := s.RemoveAutoFollow(ctx, u, b); err != nil || !ok {
			t.Fatalf("RemoveAutoFollow = %v, %v", ok, err)
		}
		if n, err := s.RemoveAutoFollowsOf(ctx, b); err != nil || n != 1 {
			t.Fatalf("RemoveAutoFollowsOf = %d, %v", n, err)
		}
		if f, _ := s.Follows(ctx, other, b); f {
			t.Fatal("RemoveAutoFollowsOf left a row")
		}
		if ok, _ := s.RemoveFollow(ctx, u, c); !ok {
			t.Fatal("RemoveFollow")
		}
		if ok, _ := s.RemoveFollow(ctx, u, c); ok {
			t.Fatal("RemoveFollow twice")
		}
		if fs := list(u); len(fs) != 1 || fs[0].OrgID != a {
			t.Fatalf("follows after removals = %+v", fs)
		}
		// Mutes.
		for i, want := range []bool{true, false} {
			if ok, err := s.AddMute(ctx, u, a); err != nil || ok != want {
				t.Fatalf("AddMute #%d = %v, %v", i, ok, err)
			}
		}
		if _, err := s.AddMute(ctx, u, c); err != nil {
			t.Fatal(err)
		}
		ms, err := s.ListMutes(ctx, u, nil, 10)
		if err != nil || len(ms) != 2 {
			t.Fatalf("ListMutes = %+v, %v", ms, err)
		}
		next, err := s.ListMutes(ctx, u, &feed.After{At: ms[0].CreatedAt, ID: ms[0].OrgID}, 10)
		if err != nil || len(next) != 1 || next[0].OrgID != ms[1].OrgID {
			t.Fatalf("ListMutes after = %+v, %v", next, err)
		}
		if ok, _ := s.RemoveMute(ctx, u, a); !ok {
			t.Fatal("RemoveMute")
		}
		if ok, _ := s.RemoveMute(ctx, u, a); ok {
			t.Fatal("RemoveMute twice")
		}
		if ms, _ := s.ListMutes(ctx, other, nil, 10); len(ms) != 0 {
			t.Fatalf("another user's mutes = %+v", ms)
		}
	})

	t.Run("reconcile counters, export and erase", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by, u1, u2 := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorClub), w.user(t), w.user(t), w.user(t)
		p := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		q := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0.Add(time.Minute)))
		before, _ := s.GetPost(ctx, p.ID)
		// p: two seeded likes and one seeded save without counts (drift);
		// q: counted through the store (no drift).
		w.like(t, u1, p.ID)
		w.like(t, u2, p.ID)
		w.save(t, u1, p.ID)
		if _, err := s.AddLike(ctx, u1, q.ID); err != nil {
			t.Fatal(err)
		}
		n, err := s.ReconcileCounters(ctx)
		if err != nil || n != 1 {
			t.Fatalf("ReconcileCounters = %d, %v", n, err)
		}
		got, _ := s.GetPost(ctx, p.ID)
		if got.LikeCount != 2 || got.SaveCount != 1 || !got.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("reconciled = %d, %d (updated_at moved: %v)", got.LikeCount, got.SaveCount, !got.UpdatedAt.Equal(before.UpdatedAt))
		}
		if n, err := s.ReconcileCounters(ctx); err != nil || n != 0 {
			t.Fatalf("second ReconcileCounters = %d, %v", n, err)
		}
		// Export / erase of u1: 2 likes, 1 save, 1 hide, 1 follow, 1 mute.
		if _, err := s.AddHide(ctx, u1, q.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FollowManual(ctx, u1, org, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddMute(ctx, u1, other); err != nil {
			t.Fatal(err)
		}
		in, err := s.InteractionsOf(ctx, u1)
		if err != nil || len(in.Likes) != 2 || len(in.Saves) != 1 || len(in.Hides) != 1 || len(in.Follows) != 1 || len(in.Mutes) != 1 {
			t.Fatalf("InteractionsOf = %+v, %v", in, err)
		}
		if n, err := s.EraseInteractions(ctx, u1); err != nil || n != 6 {
			t.Fatalf("EraseInteractions = %d, %v", n, err)
		}
		if got, _ := s.GetPost(ctx, p.ID); got.LikeCount != 1 || got.SaveCount != 0 {
			t.Fatalf("p after erase = %d, %d", got.LikeCount, got.SaveCount)
		}
		if got, _ := s.GetPost(ctx, q.ID); got.LikeCount != 0 {
			t.Fatalf("q after erase = %d", got.LikeCount)
		}
		if in, _ := s.InteractionsOf(ctx, u1); len(in.Likes)+len(in.Saves)+len(in.Hides)+len(in.Follows)+len(in.Mutes) != 0 {
			t.Fatalf("left after erase = %+v", in)
		}
		if in, _ := s.InteractionsOf(ctx, u2); len(in.Likes) != 1 {
			t.Fatalf("u2 lost its like: %+v", in)
		}
		if n, err := s.EraseInteractions(ctx, u1); err != nil || n != 0 {
			t.Fatalf("second EraseInteractions = %d, %v", n, err)
		}
	})

	// ---- BE-032: reports, moderation actions, trust, review queue

	t.Run("reports: one open per reporter and target, queue order, erase", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t, feed.AuthorClub), w.user(t)
		p := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		u1, u2, mod := w.user(t), w.user(t), w.user(t)
		details := "Ücret istiyorlar"
		rep := func(u uuid.UUID, tt feed.TargetType, target uuid.UUID) (feed.Report, bool) {
			t.Helper()
			r, created, err := s.InsertReport(ctx, feed.Report{ID: uuid.NewV7(), ReporterUserID: u, TargetType: tt,
				TargetID: target, Reason: feed.ReasonScamOrMisleading, Details: &details})
			if err != nil {
				t.Fatalf("InsertReport: %v", err)
			}
			return r, created
		}
		r1, created := rep(u1, feed.TargetPost, p.ID)
		if !created || r1.Status != feed.ReportOpen || r1.CreatedAt.IsZero() || r1.Details == nil || *r1.Details != details {
			t.Fatalf("first report = %+v, %v", r1, created)
		}
		if again, created := rep(u1, feed.TargetPost, p.ID); created || again.ID != r1.ID {
			t.Fatalf("duplicate = %+v, %v", again, created)
		}
		r2, _ := rep(u2, feed.TargetPost, p.ID)
		ro, created := rep(u1, feed.TargetOrg, org)
		if !created || ro.ID == r1.ID {
			t.Fatal("an org report of the same user is another report")
		}
		if n, err := s.OpenReporters(ctx, feed.TargetPost, p.ID); err != nil || n != 2 {
			t.Fatalf("OpenReporters = %d, %v", n, err)
		}
		if _, err := s.GetReport(ctx, uuid.NewV7()); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("GetReport(unknown) = %v", err)
		}
		if got, err := s.LockReport(ctx, r1.ID); err != nil || got.ReporterUserID != u1 {
			t.Fatalf("LockReport = %+v, %v", got, err)
		}
		next := r1
		next.Status, next.AssignedTo = feed.ReportInReview, &mod
		upd, err := s.UpdateReport(ctx, next)
		if err != nil || upd.Status != feed.ReportInReview || upd.AssignedTo == nil || *upd.AssignedTo != mod {
			t.Fatalf("UpdateReport(in_review) = %+v, %v", upd, err)
		}
		// A resolved report needs resolved_at (content_reports_resolved_chk).
		bad := upd
		bad.Status = feed.ReportDismissed
		if _, err := s.UpdateReport(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("dismissed without resolved_at = %v", err)
		}
		at := pub0.Add(time.Hour)
		note := "Kural dışı değil"
		bad.ResolvedAt, bad.ResolutionNote = &at, &note
		if res, err := s.UpdateReport(ctx, bad); err != nil || res.ResolvedAt == nil || *res.ResolutionNote != note {
			t.Fatalf("UpdateReport(dismissed) = %+v, %v", res, err)
		}
		if n, _ := s.OpenReporters(ctx, feed.TargetPost, p.ID); n != 1 {
			t.Fatalf("OpenReporters after dismissal = %d", n)
		}
		r3, created := rep(u1, feed.TargetPost, p.ID)
		if !created || r3.ID == r1.ID {
			t.Fatal("after dismissal the same user reports anew")
		}
		// The queue: open reports, oldest first; newest first on request; keyset.
		open := feed.ReportFilter{Statuses: []feed.ReportStatus{feed.ReportOpen, feed.ReportInReview}}
		got, err := s.ListReports(ctx, open, nil, 10)
		if err != nil || !slices.Equal(reportIDs(got), []uuid.UUID{r2.ID, ro.ID, r3.ID}) {
			t.Fatalf("ListReports(open) = %v, %v", reportIDs(got), err)
		}
		open.Newest = true
		if got, _ := s.ListReports(ctx, open, nil, 2); !slices.Equal(reportIDs(got), []uuid.UUID{r3.ID, ro.ID}) {
			t.Fatalf("ListReports(newest) = %v", reportIDs(got))
		}
		if got, _ := s.ListReports(ctx, open, &feed.After{At: ro.CreatedAt, ID: ro.ID}, 10); !slices.Equal(reportIDs(got), []uuid.UUID{r2.ID}) {
			t.Fatalf("ListReports(newest, after) = %v", reportIDs(got))
		}
		if got, _ := s.ListReports(ctx, feed.ReportFilter{TargetType: feed.TargetOrg}, nil, 10); len(got) != 1 || got[0].ID != ro.ID {
			t.Fatalf("ListReports(org) = %v", reportIDs(got))
		}
		if got, _ := s.ListReports(ctx, feed.ReportFilter{Reason: feed.ReasonSpam}, nil, 10); len(got) != 0 {
			t.Fatalf("ListReports(spam) = %v", reportIDs(got))
		}
		if mine, err := s.ReportsOf(ctx, u1); err != nil || len(mine) != 3 {
			t.Fatalf("ReportsOf = %d, %v", len(mine), err)
		}
		if n, err := s.DeleteReportsOf(ctx, u1); err != nil || n != 3 {
			t.Fatalf("DeleteReportsOf = %d, %v", n, err)
		}
		if mine, _ := s.ReportsOf(ctx, u1); len(mine) != 0 {
			t.Fatal("reports survived the erasure")
		}
		if _, err := s.GetReport(ctx, r2.ID); err != nil {
			t.Fatal("another reporter's report was erased")
		}
	})

	t.Run("moderation actions: append, history, latest, by report and actor", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t, feed.AuthorClub), w.user(t)
		p := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		q := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		reporter, mod := w.user(t), w.user(t)
		r, _, err := s.InsertReport(ctx, feed.Report{ID: uuid.NewV7(), ReporterUserID: reporter, TargetType: feed.TargetPost,
			TargetID: p.ID, Reason: feed.ReasonSpam})
		if err != nil {
			t.Fatal(err)
		}
		act := func(target uuid.UUID, a feed.ModAction, report *uuid.UUID) feed.ModerationAction {
			t.Helper()
			note := "not"
			m, err := s.InsertModerationAction(ctx, feed.ModerationAction{ID: uuid.NewV7(), ReportID: report,
				TargetType: feed.TargetPost, TargetID: target, Action: a, ReasonCode: "spam", ActorUserID: mod, Note: &note})
			if err != nil {
				t.Fatalf("InsertModerationAction: %v", err)
			}
			return m
		}
		a1 := act(p.ID, feed.ModHide, &r.ID)
		a2 := act(p.ID, feed.ModRestore, nil)
		aq := act(q.ID, feed.ModRemove, nil)
		if a1.CreatedAt.IsZero() || a1.ReportID == nil || *a1.ReportID != r.ID {
			t.Fatalf("a1 = %+v", a1)
		}
		if _, err := s.InsertModerationAction(ctx, feed.ModerationAction{ID: uuid.NewV7(), TargetType: feed.TargetPost,
			TargetID: p.ID, Action: feed.ModHide, ReasonCode: "Spam!", ActorUserID: mod}); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("bad reason code = %v", err)
		}
		if got, err := s.GetModerationAction(ctx, a1.ID); err != nil || got.Action != feed.ModHide || got.ActorUserID != mod {
			t.Fatalf("GetModerationAction = %+v, %v", got, err)
		}
		if _, err := s.GetModerationAction(ctx, uuid.NewV7()); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("GetModerationAction(unknown) = %v", err)
		}
		target := p.ID
		hist, err := s.ListModerationActions(ctx, feed.ActionFilter{TargetType: feed.TargetPost, TargetID: &target}, nil, 10)
		if err != nil || !slices.Equal(actionIDs(hist), []uuid.UUID{a2.ID, a1.ID}) {
			t.Fatalf("history = %v, %v", actionIDs(hist), err)
		}
		if all, _ := s.ListModerationActions(ctx, feed.ActionFilter{}, &a2.ID, 10); !slices.Contains(actionIDs(all), a1.ID) ||
			slices.Contains(actionIDs(all), aq.ID) {
			t.Fatalf("history after a2 = %v", actionIDs(all))
		}
		last, err := s.LatestActions(ctx, feed.TargetPost, []uuid.UUID{p.ID, q.ID, uuid.NewV7()})
		if err != nil || len(last) != 2 || last[p.ID].ID != a2.ID || last[q.ID].ID != aq.ID {
			t.Fatalf("LatestActions = %+v, %v", last, err)
		}
		if n, err := s.ReportActions(ctx, r.ID); err != nil || n != 1 {
			t.Fatalf("ReportActions = %d, %v", n, err)
		}
		if mine, err := s.ActionsBy(ctx, mod); err != nil || len(mine) != 3 {
			t.Fatalf("ActionsBy = %d, %v", len(mine), err)
		}
		// Erasing the report keeps the decision; its report_id is cleared.
		if _, err := s.DeleteReportsOf(ctx, reporter); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetModerationAction(ctx, a1.ID); got.ReportID != nil || got.Action != feed.ModHide {
			t.Fatalf("a1 after the report's erasure = %+v", got)
		}
	})

	t.Run("trust: approvals, auto trust, set trust", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, admin := w.org(t, feed.AuthorCompany), w.user(t)
		for i := range 2 {
			st, err := s.AddApprovedPost(ctx, org)
			if err != nil || st.ApprovedPostsCount != i+1 || st.TrustLevel != feed.TrustNew {
				t.Fatalf("AddApprovedPost %d = %+v, %v", i, st, err)
			}
		}
		if ok, err := s.PromoteTrusted(ctx, org, 3); err != nil || ok {
			t.Fatalf("PromoteTrusted(2 of 3) = %v, %v", ok, err)
		}
		if _, err := s.AddApprovedPost(ctx, org); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.PromoteTrusted(ctx, org, 3); err != nil || !ok {
			t.Fatalf("PromoteTrusted(3 of 3) = %v, %v", ok, err)
		}
		if ok, _ := s.PromoteTrusted(ctx, org, 3); ok {
			t.Fatal("a trusted org promoted twice")
		}
		until, reason := pub0.Add(48*time.Hour), "spam"
		st, err := s.SetTrust(ctx, feed.OrgSettings{OrgID: org, TrustLevel: feed.TrustRestricted, RestrictedUntil: &until,
			Reason: &reason, UpdatedBy: &admin})
		if err != nil || st.TrustLevel != feed.TrustRestricted || st.ApprovedPostsCount != 3 || st.RestrictedUntil == nil ||
			!st.RestrictedUntil.Equal(until) || st.UpdatedBy == nil || *st.UpdatedBy != admin {
			t.Fatalf("SetTrust(restricted) = %+v, %v", st, err)
		}
		if _, err := s.SetTrust(ctx, feed.OrgSettings{OrgID: org, TrustLevel: feed.TrustSuspended}); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("suspended without a reason = %v", err)
		}
		// A restricted org is not promoted.
		if ok, _ := s.PromoteTrusted(ctx, org, 1); ok {
			t.Fatal("a restricted org was promoted")
		}
		fresh := w.org(t, feed.AuthorClub)
		st, err = s.SetTrust(ctx, feed.OrgSettings{OrgID: fresh, TrustLevel: feed.TrustSuspended, Reason: &reason, UpdatedBy: &admin})
		if err != nil || st.TrustLevel != feed.TrustSuspended || st.ApprovedPostsCount != 0 {
			t.Fatalf("SetTrust(new row) = %+v, %v", st, err)
		}
		if got, ok, _ := s.OrgSettings(ctx, fresh); !ok || got.TrustLevel != feed.TrustSuspended {
			t.Fatalf("OrgSettings = %+v, %v", got, ok)
		}
	})

	t.Run("retention: removed posts, resolved reports, moderation actions (BE-057)", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by, reader, mod := w.org(t, feed.AuthorClub), w.user(t), w.user(t), w.user(t)
		removed := func(at time.Time, image bool) feed.Post {
			t.Helper()
			p := insert(t, s, post(org, feed.AuthorClub, by))
			if image {
				p.Kind, p.BodyJSON, p.BodyText = feed.KindImage, nil, nil
			}
			p = publishedAt(p, at.AddDate(0, -1, 0))
			p.Status, p.RemovedAt = feed.StatusRemoved, &at
			got, err := s.UpdatePost(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		old := removed(pub0.AddDate(0, -13, 0), true)
		if err := s.ReplaceMedia(ctx, org, old.ID, []feed.Media{{ID: uuid.NewV7(), FileID: w.file(t, org)}}); err != nil {
			t.Fatal(err)
		}
		w.like(t, reader, old.ID)
		young := removed(pub0.AddDate(0, -1, 0), false)
		cutoff := pub0.AddDate(0, -12, 0)
		if n, err := s.CountRemovedPostsDue(ctx, cutoff); err != nil || n != 1 {
			t.Fatalf("CountRemovedPostsDue = %d, %v; want 1", n, err)
		}
		due, err := s.RemovedPostsDue(ctx, cutoff, 10)
		if err != nil || len(due) != 1 || due[0].ID != old.ID {
			t.Fatalf("RemovedPostsDue = %+v, %v", due, err)
		}
		if n, err := s.PurgePosts(ctx, []uuid.UUID{old.ID}); err != nil || n != 1 {
			t.Fatalf("PurgePosts = %d, %v", n, err)
		}
		if _, err := s.GetPost(ctx, old.ID); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("purged post found: %v", err)
		}
		if media, _ := s.MediaOf(ctx, []uuid.UUID{old.ID}); len(media) != 0 {
			t.Fatalf("media outlived the purged post: %+v", media)
		}
		if in, _ := s.InteractionsOf(ctx, reader); len(in.Likes) != 0 {
			t.Fatalf("likes outlived the purged post: %+v", in.Likes)
		}
		if _, err := s.GetPost(ctx, young.ID); err != nil {
			t.Fatalf("the young removed post was purged: %v", err)
		}

		// A resolved report goes after 12 months; its action keeps the
		// decision (report_id → NULL).
		target := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		r, _, err := s.InsertReport(ctx, feed.Report{ID: uuid.NewV7(), ReporterUserID: reader, TargetType: feed.TargetPost,
			TargetID: target.ID, Reason: feed.ReasonSpam})
		if err != nil {
			t.Fatal(err)
		}
		resolved := pub0.AddDate(0, -13, 0)
		r.Status, r.ResolvedAt = feed.ReportDismissed, &resolved
		if _, err := s.UpdateReport(ctx, r); err != nil {
			t.Fatal(err)
		}
		open, _, err := s.InsertReport(ctx, feed.Report{ID: uuid.NewV7(), ReporterUserID: by, TargetType: feed.TargetPost,
			TargetID: target.ID, Reason: feed.ReasonSpam})
		if err != nil {
			t.Fatal(err)
		}
		rid := r.ID
		act, err := s.InsertModerationAction(ctx, feed.ModerationAction{ID: uuid.NewV7(), ReportID: &rid, TargetType: feed.TargetPost,
			TargetID: target.ID, Action: feed.ModHide, ReasonCode: "spam", ActorUserID: mod})
		if err != nil {
			t.Fatal(err)
		}
		if n, err := s.PurgeResolvedReports(ctx, cutoff, 10, true); err != nil || n != 1 {
			t.Fatalf("PurgeResolvedReports dry run = %d, %v; want 1", n, err)
		}
		if n, err := s.PurgeResolvedReports(ctx, cutoff, 10, false); err != nil || n != 1 {
			t.Fatalf("PurgeResolvedReports = %d, %v", n, err)
		}
		if _, err := s.GetReport(ctx, r.ID); !errors.Is(err, feed.ErrNotFound) {
			t.Fatalf("purged report found: %v", err)
		}
		if _, err := s.GetReport(ctx, open.ID); err != nil {
			t.Fatalf("the open report was purged: %v", err)
		}
		if got, err := s.GetModerationAction(ctx, act.ID); err != nil || got.ReportID != nil {
			t.Fatalf("action after its report's purge = %+v, %v; want report_id NULL", got, err)
		}
		// The actions are 2-year records: nothing of today is due.
		if n, err := s.PurgeModerationActions(ctx, time.Now().AddDate(-2, -1, 0), 10, true); err != nil || n != 0 {
			t.Fatalf("PurgeModerationActions dry run = %d, %v", n, err)
		}
	})

	t.Run("review queue: pending posts of every org, oldest first", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		a, b, by := w.org(t, feed.AuthorCompany), w.org(t, feed.AuthorClub), w.user(t)
		review := func(org uuid.UUID, kind feed.AuthorKind) feed.Post {
			p := post(org, kind, by)
			p.Status = feed.StatusPendingReview
			return insert(t, s, p)
		}
		p1 := review(a, feed.AuthorCompany)
		insert(t, s, post(a, feed.AuthorCompany, by)) // a draft is not in the queue
		p2 := review(b, feed.AuthorClub)
		got, err := s.ListReview(ctx, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for _, p := range got {
			if p.ID == p1.ID || p.ID == p2.ID {
				ids = append(ids, p.ID)
			}
		}
		if !slices.Equal(ids, []uuid.UUID{p1.ID, p2.ID}) {
			t.Fatalf("ListReview = %v", ids)
		}
		if next, _ := s.ListReview(ctx, &feed.After{At: p1.CreatedAt, ID: p1.ID}, 10); slices.ContainsFunc(next,
			func(p feed.Post) bool { return p.ID == p1.ID }) || !slices.ContainsFunc(next, func(p feed.Post) bool { return p.ID == p2.ID }) {
			t.Fatal("ListReview keyset")
		}
	})
}

func reportIDs(rs []feed.Report) []uuid.UUID {
	out := []uuid.UUID{}
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func actionIDs(ms []feed.ModerationAction) []uuid.UUID {
	out := []uuid.UUID{}
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
