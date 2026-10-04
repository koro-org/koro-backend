package feed_test

import (
	"cmp"
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
	})

	t.Run("saved list: visible posts only, keyset by save time", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		club, company, by, reader := w.org(t, feed.AuthorClub), w.org(t, feed.AuthorCompany), w.user(t), w.user(t)
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
			if _, err := s.AddSave(ctx, reader, p.ID); err != nil {
				t.Fatal(err)
			}
		}
		r := feed.Reader{UserID: &reader, Keys: []string{"all"}}
		read := func(rd feed.Reader, limit int) []feed.SavedPost {
			t.Helper()
			var out []feed.SavedPost
			var after *feed.After
			for range 20 {
				page, err := s.ListSaved(ctx, rd, after, limit)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, page...)
				if len(page) < limit {
					return out
				}
				last := page[len(page)-1]
				after = &feed.After{At: last.SavedAt, ID: last.Post.ID}
			}
			t.Fatal("saved list did not end")
			return nil
		}
		got := read(r, 2)
		gotIDs := []uuid.UUID{}
		for i, sp := range got {
			gotIDs = append(gotIDs, sp.Post.ID)
			if i > 0 && cmp.Or(sp.SavedAt.Compare(got[i-1].SavedAt), sp.Post.ID.Compare(got[i-1].Post.ID)) >= 0 {
				t.Fatalf("not newest first at %d", i)
			}
		}
		// The draft and the followers post (no follow) are skipped; the
		// commercial post is an adult's.
		want := []uuid.UUID{posts[0].ID, posts[1].ID, posts[2].ID, posts[3].ID, ad.ID}
		slices.SortFunc(want, func(a, b uuid.UUID) int { return a.Compare(b) })
		sorted := slices.SortedFunc(slices.Values(gotIDs), func(a, b uuid.UUID) int { return a.Compare(b) })
		if !slices.Equal(sorted, want) {
			t.Fatalf("saved = %v, want %v", sorted, want)
		}
		minor := r
		minor.Minor = true
		if got := read(minor, 10); len(got) != 4 {
			t.Fatalf("minor saved = %d", len(got))
		}
		w.follow(t, reader, club)
		if got := read(r, 10); len(got) != 6 {
			t.Fatalf("follower saved = %d", len(got))
		}
		if got, _ := s.ListSaved(ctx, feed.Reader{Keys: []string{"all"}}, nil, 10); len(got) != 0 {
			t.Fatalf("anonymous saved = %v", got)
		}
	})

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
}
