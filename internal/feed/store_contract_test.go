package feed_test

import (
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

	t.Run("manage list, settings, follows, viewer state", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, other, by, reader := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorClub), w.user(t), w.user(t)
		a := insert(t, s, post(org, feed.AuthorClub, by))
		b := insert(t, s, publishedAt(post(org, feed.AuthorClub, by), pub0))
		c := insert(t, s, post(org, feed.AuthorClub, by))
		insert(t, s, post(other, feed.AuthorClub, by))
		all, err := s.ListOrgManage(ctx, org, nil, nil, 10)
		if err != nil || !slices.Equal(ids(all), []uuid.UUID{c.ID, b.ID, a.ID}) {
			t.Fatalf("ListOrgManage = %v, %v", ids(all), err)
		}
		drafts, _ := s.ListOrgManage(ctx, org, []feed.Status{feed.StatusDraft}, nil, 10)
		if !slices.Equal(ids(drafts), []uuid.UUID{c.ID, a.ID}) {
			t.Fatalf("drafts = %v", ids(drafts))
		}
		after, _ := s.ListOrgManage(ctx, org, nil, &c.ID, 1)
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
		vs, err := s.ViewerState(ctx, reader, []uuid.UUID{a.ID, b.ID, c.ID}, []uuid.UUID{org, other})
		if err != nil || !vs.Liked[b.ID] || vs.Liked[a.ID] || !vs.Saved[a.ID] || vs.Saved[b.ID] || !vs.Following[org] || vs.Following[other] {
			t.Fatalf("ViewerState = %+v, %v", vs, err)
		}
		if vs, err := s.ViewerState(ctx, reader, nil, nil); err != nil || len(vs.Liked) != 0 {
			t.Fatalf("empty ViewerState = %+v, %v", vs, err)
		}
	})
}
