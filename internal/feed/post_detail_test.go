package feed_test

import (
	"errors"
	"strconv"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/platform/problem"
)

// putPost seeds a post of club_b (writer: other_club_admin) as it is: the
// segment, hidden and removed rows the write paths reach through
// targeting and moderation.
func (e *env) putPost(t *testing.T, status feed.Status, kind feed.AudienceKind, keys ...string) uuid.UUID {
	t.Helper()
	at := e.clk.Now()
	e.clk.Advance(time.Second)
	title := "Seed"
	p := feed.Post{
		ID: uuid.NewV7(), OrgID: orgID("club_b"), AuthorOrgKind: feed.AuthorClub, AuthorUserID: orgID("other_club_admin"),
		Kind: feed.KindText, Title: &title, BodyJSON: doc("seed"), AudienceKind: kind, AudienceKeys: keys,
		AudienceDimCount: 1, Status: status, PublishedAt: &at,
	}
	if kind == feed.AudienceSegment {
		p.AudienceSnapshot = []byte(`{}`)
	}
	if status == feed.StatusRemoved {
		p.RemovedAt = &at
	}
	if err := e.mem.PutPost(p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// notFoundBody is the problem a reader gets for a post: code and status
// only (the detail never says whether the post exists).
func notFoundBody(t *testing.T, err error) string {
	t.Helper()
	p, ok := errors.AsType[*problem.Error](err)
	if !ok {
		t.Fatalf("error = %v, want a problem", err)
	}
	return p.Code() + "|" + p.Title() + "|" + strconv.Itoa(p.Status())
}

// TestGetPost_VisibilityMatchesFeed (BE-126): getPost — the /posts/{post_id}
// link of notifications and e-mails — opens exactly the posts the reader's
// feed lists (03 §3.6: published, in the audience, a followers post for a
// follower or member, no commercial post for a minor) and, for an
// anonymous reader, exactly what the org pages list (public,
// non-commercial). Every other post — a draft, hidden or removed by
// moderation, out of the audience — answers the same 404 as a post that
// does not exist. Writers and moderators reading more is their own view
// (TestGetPost_WritersAndStaffRead).
func TestGetPost_VisibilityMatchesFeed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	followers := textInput("Takipçilere")
	followers.AudienceKind = feed.AudienceFollowers
	sponsored := textInput("Sponsorlu")
	sponsored.CommercialFlag = true
	posts := []uuid.UUID{
		e.published(t, "club_admin", "club_a", textInput("Herkese")).Post.ID,
		e.published(t, "club_admin", "club_a", followers).Post.ID,
		e.published(t, "club_admin", "club_a", sponsored).Post.ID,
		e.published(t, "company_admin", "company_a", textInput("Yaz stajı")).Post.ID,
		e.create(t, "club_admin", "club_a", textInput("Taslak")).Post.ID,
		e.create(t, "company_admin", "company_a", textInput("Şirket taslağı")).Post.ID,
		e.putPost(t, feed.StatusPublished, feed.AudienceSegment, uniKey),
		e.putPost(t, feed.StatusPublished, feed.AudienceSegment, "club:"+orgID("club_sensitive").String()),
		e.putPost(t, feed.StatusHidden, feed.AudiencePublic, feed.KeyAll),
		e.putPost(t, feed.StatusRemoved, feed.AudiencePublic, feed.KeyAll),
	}
	// student follows club_a and studies at uni_a; club_member is a member
	// of club_a; user_unverified has a sensitive club's member key.
	e.mem.PutFollow(persona(t, "student").UserID, orgID("club_a"))
	e.viewers.keys[persona(t, "student").UserID] = []string{uniKey}
	e.viewers.keys[persona(t, "user_unverified").UserID] = []string{"club:" + orgID("club_sensitive").String()}

	missing, err := e.svc.GetPost(ctx, authz.Anonymous(), uuid.NewV7())
	if err == nil {
		t.Fatalf("unknown post = %+v", missing)
	}
	want404 := notFoundBody(t, err)
	check := func(name string, a authz.Actor, listed []uuid.UUID) {
		t.Helper()
		seen := 0
		for _, id := range posts {
			_, err := e.svc.GetPost(ctx, a, id)
			switch in := contains(listed, id); {
			case in && err != nil:
				t.Errorf("%s: post %s is listed but getPost = %v", name, id, err)
			case !in && err == nil:
				t.Errorf("%s: post %s is not listed but getPost opens it", name, id)
			case !in && notFoundBody(t, err) != want404:
				t.Errorf("%s: invisible post %s = %q, want the unknown post's %q", name, id, notFoundBody(t, err), want404)
			case in:
				seen++
			}
		}
		if seen == 0 {
			t.Errorf("%s sees nothing: the comparison proves nothing", name)
		}
	}
	for _, name := range []string{"user_unverified", "student", "student_minor", "club_member", "university_rep",
		"other_company_member", "support"} {
		a := persona(t, name)
		check(name, a, e.feedIDs(t, a, feed.FeedQuery{}))
	}
	var pages []uuid.UUID
	for _, org := range []string{"club_a", "club_b", "company_a"} {
		p, err := e.svc.ListPosts(ctx, authz.Anonymous(), orgID(org), feed.PageQuery{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range p.Items {
			pages = append(pages, v.Post.ID)
		}
	}
	check("anonymous", authz.Anonymous(), pages)
}

// TestGetPost_MutedOrHiddenStaysReadable (BE-126): a mute or a hide is the
// reader's feed filter, not visibility: like the org page (muteOrg,
// hidePost), the post's own link still opens it.
func TestGetPost_MutedOrHiddenStaysReadable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	muted := e.published(t, "club_admin", "club_a", textInput("Sessize alınan")).Post.ID
	hidden := e.published(t, "other_club_admin", "club_b", textInput("Gizlenen")).Post.ID
	reader := persona(t, "student")
	e.mem.PutMute(reader.UserID, orgID("club_a"))
	e.mem.PutHide(reader.UserID, hidden)
	if ids := e.feedIDs(t, reader, feed.FeedQuery{}); contains(ids, muted) || contains(ids, hidden) {
		t.Fatalf("feed = %v", ids)
	}
	for _, id := range []uuid.UUID{muted, hidden} {
		if _, err := e.svc.GetPost(t.Context(), reader, id); err != nil {
			t.Errorf("getPost(%s) = %v", id, err)
		}
	}
}

// TestGetPost_WritersAndStaffRead (BE-126): the author org's writers open
// their drafts and posts in review, moderation-hidden posts and posts out
// of their own audience (the reader view; the management view is
// listOrgPostsManage); a removed post is moderators' and platform
// admins' only.
func TestGetPost_WritersAndStaffRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	draft := e.create(t, "other_club_admin", "club_b", textInput("Taslak")).Post.ID
	segment := e.putPost(t, feed.StatusPublished, feed.AudienceSegment, uniKey)
	hidden := e.putPost(t, feed.StatusHidden, feed.AudiencePublic, feed.KeyAll)
	removed := e.putPost(t, feed.StatusRemoved, feed.AudiencePublic, feed.KeyAll)
	writer := persona(t, "other_club_admin")
	for _, id := range []uuid.UUID{draft, segment, hidden} {
		v, err := e.svc.GetPost(ctx, writer, id)
		if err != nil || v.Manage {
			t.Errorf("writer getPost(%s) = manage %v, %v", id, v.Manage, err)
		}
	}
	if _, err := e.svc.GetPost(ctx, writer, removed); err == nil {
		t.Error("the writer opened a removed post")
	}
	for _, name := range []string{"moderator", "platform_admin"} {
		for _, id := range []uuid.UUID{draft, segment, hidden, removed} {
			if _, err := e.svc.GetPost(ctx, persona(t, name), id); err != nil {
				t.Errorf("%s getPost(%s) = %v", name, id, err)
			}
		}
	}
}
