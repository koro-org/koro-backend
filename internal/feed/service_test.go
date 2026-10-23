package feed_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
)

// TestCreatePost_CompanyForcesCommercialFlag: a company post is always
// commercial, whatever the request says (03 §5.7, posts_company_commercial_chk);
// a club decides for itself (sponsored content, T-16).
func TestCreatePost_CompanyForcesCommercialFlag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := textInput("Staj ilanı yakında")
	in.CommercialFlag = false
	v := e.create(t, "company_member", "company_a", in)
	if !v.Post.CommercialFlag || v.Post.AuthorOrgKind != feed.AuthorCompany {
		t.Fatalf("company post = %+v", v.Post)
	}
	if !feed.ToManageDTO(v).CommercialFlag || !feed.ToPostDTO(v).CommercialLabel {
		t.Fatal("the DTOs lost the commercial label")
	}
	// The label cannot be switched off later either.
	off := false
	u, err := e.svc.UpdatePost(t.Context(), persona(t, "company_member"), v.Post.ID, v.ETag, feed.PostPatch{CommercialFlag: &off})
	if err != nil || !u.Post.CommercialFlag {
		t.Fatalf("UpdatePost(commercial=false) = %+v, %v", u.Post, err)
	}
	club := e.create(t, "club_admin", "club_a", textInput("Genel kurul"))
	if club.Post.CommercialFlag {
		t.Fatal("a club post became commercial")
	}
	sponsored := textInput("Sponsorumuz")
	sponsored.CommercialFlag = true
	if v := e.create(t, "club_admin", "club_a", sponsored); !v.Post.CommercialFlag {
		t.Fatal("the club's sponsored flag was dropped")
	}
	// The author is recorded for the audit, never shown to readers.
	if v.Post.AuthorUserID != persona(t, "company_member").UserID {
		t.Fatalf("author = %s", v.Post.AuthorUserID)
	}
	b, _ := json.Marshal(feed.ToPostDTO(v))
	if strings.Contains(string(b), v.Post.AuthorUserID.String()) || strings.Contains(string(b), "audience") {
		t.Fatalf("reader DTO leaks author or audience: %s", b)
	}
	if got := e.audit.of(authz.FeedPostCreate); len(got) != 3 {
		t.Fatalf("audit records = %d", len(got))
	}
}

// TestPublish_NewOrgFirst3_PendingReview: an org without feed settings is
// new: its posts go to pending_review (post.submitted_for_review) until it
// has the threshold of approved posts (T-15); restricted orgs are
// reviewed, suspended ones cannot publish, the platform org never waits.
func TestPublish_NewOrgFirst3_PendingReview(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "company_admin")
	for range 3 {
		v := e.create(t, "company_admin", "company_a", textInput("İlk gönderiler"))
		p, err := e.svc.PublishPost(ctx, admin, v.Post.ID)
		if err != nil || p.Post.Status != feed.StatusPendingReview || p.Post.PublishedAt != nil {
			t.Fatalf("PublishPost(new org) = %+v, %v", p.Post, err)
		}
		// In review it is not the readers' yet.
		if _, err := e.svc.GetPost(ctx, persona(t, "student"), v.Post.ID); err == nil {
			t.Fatal("a post in review is visible to a reader")
		}
		// A second publication is a state error.
		_, err = e.svc.PublishPost(ctx, admin, v.Post.ID)
		wantCode(t, err, 409, "feed.invalid_state")
	}
	if got := e.out.Topics(); len(got) != 3 || got[0] != domainevent.TopicPostSubmittedForReview {
		t.Fatalf("events = %v", got)
	}
	// Three approved posts (BE-032's counter): the next one is published.
	e.mem.PutSettings(feed.OrgSettings{OrgID: orgID("company_a"), TrustLevel: feed.TrustNew, ApprovedPostsCount: 3})
	v := e.create(t, "company_admin", "company_a", textInput("Dördüncü"))
	if p, err := e.svc.PublishPost(ctx, admin, v.Post.ID); err != nil || p.Post.Status != feed.StatusPublished {
		t.Fatalf("PublishPost(after 3 approvals) = %+v, %v", p.Post, err)
	}
	// Restricted: reviewed again until the restriction ends.
	until := t0.Add(24 * time.Hour)
	reason := "spam"
	e.mem.PutSettings(feed.OrgSettings{OrgID: orgID("company_a"), TrustLevel: feed.TrustRestricted, ApprovedPostsCount: 9,
		RestrictedUntil: &until, Reason: &reason})
	v = e.create(t, "company_admin", "company_a", textInput("Kısıtlı"))
	if p, err := e.svc.PublishPost(ctx, admin, v.Post.ID); err != nil || p.Post.Status != feed.StatusPendingReview {
		t.Fatalf("PublishPost(restricted) = %+v, %v", p.Post, err)
	}
	// Suspended: 403 — a draft written before cannot be published, and no
	// new draft is taken (BE-032).
	v = e.create(t, "company_admin", "company_a", textInput("Askıda"))
	e.mem.PutSettings(feed.OrgSettings{OrgID: orgID("company_a"), TrustLevel: feed.TrustSuspended, Reason: &reason})
	_, err := e.svc.PublishPost(ctx, admin, v.Post.ID)
	wantCode(t, err, 403, "feed.posting_suspended")
	_, err = e.svc.CreatePost(ctx, admin, orgID("company_a"), textInput("Askıda 2"))
	wantCode(t, err, 403, "feed.posting_suspended")
	// The platform org is trusted without a settings row (k-po author).
	pa := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), PlatformRoles: []authz.PlatformRole{authz.PlatformAdmin},
		Memberships: []authz.Membership{{OrgID: orgID("platform_org"), OrgKind: authz.OrgPlatform, Role: authz.MemberStaff}},
		MFAVerified: true}
	pv, err := e.svc.CreatePost(ctx, pa, orgID("platform_org"), textInput("Koro duyurusu"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := e.svc.PublishPost(ctx, pa, pv.Post.ID); err != nil || p.Post.Status != feed.StatusPublished {
		t.Fatalf("PublishPost(platform org) = %+v, %v", p.Post, err)
	}
}

// TestPublish_EmitsPostPublished: a trusted org's post is published at
// once with post.published (A, after the commit) carrying the kind and the
// commercial flag; a stale or unclean image blocks the publication.
func TestPublish_EmitsPostPublished(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.trust("club_a")
	img := e.files.upload(orgID("club_a"), files.ScanPending)
	v := e.create(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Media: []feed.MediaInput{{FileID: img, AltText: "afiş"}}})
	if e.files.attached[img] != v.Post.ID {
		t.Fatal("the image was not attached to the post")
	}
	_, err := e.svc.PublishPost(ctx, persona(t, "club_admin"), v.Post.ID)
	wantCode(t, err, 409, "files.not_clean")
	if len(e.out.Events()) != 0 {
		t.Fatal("an event was emitted for a failed publication")
	}
	e.files.setScan(img, files.ScanClean)
	p, err := e.svc.PublishPost(ctx, persona(t, "club_admin"), v.Post.ID)
	if err != nil || p.Post.Status != feed.StatusPublished || p.Post.PublishedAt == nil || !p.Post.PublishedAt.Equal(t0) {
		t.Fatalf("PublishPost = %+v, %v", p.Post, err)
	}
	evs := e.out.Events()
	if len(evs) != 1 || evs[0].Topic != domainevent.TopicPostPublished {
		t.Fatalf("events = %v", e.out.Topics())
	}
	got, err := domainevent.Decode[domainevent.PostPublished](evs[0])
	if err != nil || got.PostID != v.Post.ID || got.OrgID != orgID("club_a") || got.Kind != "image" || got.Commercial {
		t.Fatalf("payload = %+v, %v", got, err)
	}
	if len(p.Media) != 1 || p.Media[0].URL == "" || *p.Media[0].AltText != "afiş" {
		t.Fatalf("media = %+v", p.Media)
	}
	if len(e.audit.of(authz.FeedPostPublish)) != 1 {
		t.Fatal("publication not audited")
	}
	// The follower hook (T-84: a no-op in P1) accepts the event.
	if err := feed.HandlePostPublished(e.mod, ctx, evs[0]); err != nil {
		t.Fatalf("follower hook: %v", err)
	}
}

// TestFeed_KeysetStableUnderInserts: posts published between two pages
// never shift the next page (no duplicate, no gap); they appear on the
// next first page.
func TestFeed_KeysetStableUnderInserts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	var want []uuid.UUID
	for i := range 7 {
		v := e.published(t, "club_admin", "club_a", textInput("gönderi "+string(rune('a'+i))))
		want = append([]uuid.UUID{v.Post.ID}, want...)
	}
	// Two posts share a publication instant: the id breaks the tie.
	e.clk.Advance(-time.Second)
	v := e.published(t, "club_admin", "club_a", textInput("aynı an"))
	want = append([]uuid.UUID{v.Post.ID}, want...)
	slices.SortStableFunc(want[:2], func(a, b uuid.UUID) int { return -a.Compare(b) })

	reader := persona(t, "student")
	q := feed.FeedQuery{Limit: 3}
	first, err := e.svc.GetFeed(ctx, reader, q)
	if err != nil || len(first.Items) != 3 || first.Next == nil {
		t.Fatalf("first page = %d items, next %v, %v", len(first.Items), first.Next, err)
	}
	// New posts land on top while the reader pages on.
	fresh := e.published(t, "club_admin", "club_a", textInput("yeni"))
	var got []uuid.UUID
	for _, it := range first.Items {
		got = append(got, it.Post.ID)
	}
	q.After = first.Next
	got = append(got, e.feedIDs(t, reader, q)...)
	if !slices.Equal(got, want) {
		t.Fatalf("pages = %v\nwant    %v", got, want)
	}
	again := e.feedIDs(t, reader, feed.FeedQuery{Limit: 3})
	if again[0] != fresh.Post.ID || len(again) != len(want)+1 {
		t.Fatalf("fresh first page = %v", again)
	}
	// Default and maximum sizes (04 §2.7: ≤ 50, LIMIT 21 = 20 + 1).
	for range 60 {
		e.published(t, "club_admin", "club_a", textInput("dolgu"))
	}
	if p, _ := e.svc.GetFeed(ctx, reader, feed.FeedQuery{}); len(p.Items) != 20 || p.Next == nil {
		t.Fatalf("default page = %d", len(p.Items))
	}
	if p, _ := e.svc.GetFeed(ctx, reader, feed.FeedQuery{Limit: 500}); len(p.Items) != 50 {
		t.Fatalf("clamped page = %d", len(p.Items))
	}
}

// TestFeed_MinorSeesNoCommercial: a minor's feed, org page and post page
// carry no commercial post (03 k15); an adult sees it.
func TestFeed_MinorSeesNoCommercial(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ad := e.published(t, "company_admin", "company_a", textInput("Yaz stajı"))
	sponsored := textInput("Sponsorlu")
	sponsored.CommercialFlag = true
	clubAd := e.published(t, "club_admin", "club_a", sponsored)
	plain := e.published(t, "club_admin", "club_a", textInput("Bilgilendirme"))
	minor := persona(t, "student_minor")
	ids := e.feedIDs(t, minor, feed.FeedQuery{})
	if contains(ids, ad.Post.ID) || contains(ids, clubAd.Post.ID) || !contains(ids, plain.Post.ID) {
		t.Fatalf("minor feed = %v", ids)
	}
	if _, err := e.svc.GetPost(ctx, minor, ad.Post.ID); err == nil {
		t.Fatal("a minor opened a commercial post")
	}
	page, err := e.svc.ListPosts(ctx, minor, orgID("company_a"), feed.PageQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("minor company page = %d, %v", len(page.Items), err)
	}
	adult := e.feedIDs(t, persona(t, "student"), feed.FeedQuery{})
	if !contains(adult, ad.Post.ID) || !contains(adult, clubAd.Post.ID) {
		t.Fatalf("adult feed = %v", adult)
	}
	if v, err := e.svc.GetPost(ctx, persona(t, "student"), ad.Post.ID); err != nil || !feed.ToPostDTO(v).CommercialLabel {
		t.Fatalf("adult GetPost = %v", err)
	}
}

// TestFeed_MutedOrgExcluded: an org the reader muted (org_mutes, BE-029)
// leaves their feed, not other readers'; its page still shows it.
func TestFeed_MutedOrgExcluded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.published(t, "club_admin", "club_a", textInput("A"))
	b := e.published(t, "other_club_admin", "club_b", textInput("B"))
	reader := persona(t, "student")
	e.mem.PutMute(reader.UserID, orgID("club_a"))
	ids := e.feedIDs(t, reader, feed.FeedQuery{})
	if contains(ids, a.Post.ID) || !contains(ids, b.Post.ID) {
		t.Fatalf("muted feed = %v", ids)
	}
	if other := e.feedIDs(t, persona(t, "user_unverified"), feed.FeedQuery{}); !contains(other, a.Post.ID) {
		t.Fatal("the mute leaked to another reader")
	}
	page, err := e.svc.ListPosts(t.Context(), reader, orgID("club_a"), feed.PageQuery{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("org page of a muted org = %d, %v", len(page.Items), err)
	}
}

// TestFeed_HiddenPostExcluded: a post the reader hid (post_hides, BE-029)
// leaves their feed only.
func TestFeed_HiddenPostExcluded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.published(t, "club_admin", "club_a", textInput("gizlenecek"))
	b := e.published(t, "club_admin", "club_a", textInput("kalacak"))
	reader := persona(t, "student")
	e.mem.PutHide(reader.UserID, a.Post.ID)
	ids := e.feedIDs(t, reader, feed.FeedQuery{})
	if contains(ids, a.Post.ID) || !contains(ids, b.Post.ID) {
		t.Fatalf("feed = %v", ids)
	}
	if other := e.feedIDs(t, persona(t, "student_minor"), feed.FeedQuery{}); !contains(other, a.Post.ID) {
		t.Fatal("the hide leaked to another reader")
	}
}

// TestFeed_FollowersAudience_RequiresFollow: a followers post reaches the
// author org's followers and members only; the following tab keeps the
// followed and member orgs; anonymous readers never see it.
func TestFeed_FollowersAudience_RequiresFollow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	in := textInput("Yalnız takipçilere")
	in.AudienceKind = feed.AudienceFollowers
	fp := e.published(t, "club_admin", "club_a", in)
	pub := e.published(t, "other_club_admin", "club_b", textInput("Herkese"))
	follower, stranger, member := persona(t, "student"), persona(t, "user_unverified"), persona(t, "club_member")
	e.mem.PutFollow(follower.UserID, orgID("club_a"))

	if ids := e.feedIDs(t, stranger, feed.FeedQuery{}); contains(ids, fp.Post.ID) || !contains(ids, pub.Post.ID) {
		t.Fatalf("stranger feed = %v", ids)
	}
	if _, err := e.svc.GetPost(ctx, stranger, fp.Post.ID); err == nil {
		t.Fatal("a stranger opened a followers post")
	}
	if _, err := e.svc.GetPost(ctx, authz.Anonymous(), fp.Post.ID); err == nil {
		t.Fatal("an anonymous reader opened a followers post")
	}
	for _, r := range []authz.Actor{follower, member} {
		if ids := e.feedIDs(t, r, feed.FeedQuery{}); !contains(ids, fp.Post.ID) {
			t.Fatalf("follower/member feed = %v", ids)
		}
		if _, err := e.svc.GetPost(ctx, r, fp.Post.ID); err != nil {
			t.Fatalf("follower/member GetPost: %v", err)
		}
		following := e.feedIDs(t, r, feed.FeedQuery{Tab: feed.TabFollowing})
		if !contains(following, fp.Post.ID) || contains(following, pub.Post.ID) {
			t.Fatalf("following tab = %v", following)
		}
	}
	// The viewer flags say whom the reader follows (a member is not a follow row).
	p, err := e.svc.GetFeed(ctx, follower, feed.FeedQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range p.Items {
		if want := v.Post.OrgID == orgID("club_a"); v.Viewer == nil || v.Viewer.FollowingAuthor != want {
			t.Fatalf("viewer flags of %s = %+v", v.Post.ID, v.Viewer)
		}
	}
	// Moderators see it for moderation.
	if _, err := e.svc.GetPost(ctx, persona(t, "moderator"), fp.Post.ID); err != nil {
		t.Fatalf("moderator GetPost: %v", err)
	}
}

// TestListPosts_AnonymousOnlyPublicNonCommercial: on an org page an
// anonymous reader sees public, non-commercial posts only (T-57); a
// signed-in reader sees what the feed would show; the org's writers see
// everything published.
func TestListPosts_AnonymousOnlyPublicNonCommercial(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	pub := e.published(t, "club_admin", "club_a", textInput("Açık"))
	followers := textInput("Takipçi")
	followers.AudienceKind = feed.AudienceFollowers
	fol := e.published(t, "club_admin", "club_a", followers)
	sponsored := textInput("Sponsorlu")
	sponsored.CommercialFlag = true
	ad := e.published(t, "club_admin", "club_a", sponsored)
	draft := e.create(t, "club_admin", "club_a", textInput("Taslak"))

	ids := func(a authz.Actor, org string) []uuid.UUID {
		t.Helper()
		p, err := e.svc.ListPosts(ctx, a, orgID(org), feed.PageQuery{})
		if err != nil {
			t.Fatalf("ListPosts: %v", err)
		}
		var out []uuid.UUID
		for _, v := range p.Items {
			out = append(out, v.Post.ID)
			if !a.IsUser() && v.Viewer != nil {
				t.Fatal("anonymous view carries viewer flags")
			}
		}
		return out
	}
	if got := ids(authz.Anonymous(), "club_a"); !slices.Equal(got, []uuid.UUID{pub.Post.ID}) {
		t.Fatalf("anonymous page = %v", got)
	}
	if got := ids(persona(t, "student"), "club_a"); !slices.Equal(got, []uuid.UUID{ad.Post.ID, pub.Post.ID}) {
		t.Fatalf("student page = %v", got)
	}
	if got := ids(persona(t, "club_admin"), "club_a"); len(got) != 3 || contains(got, draft.Post.ID) {
		t.Fatalf("writer page = %v (followers %s)", got, fol.Post.ID)
	}
	// A company page anonymous: nothing (every company post is commercial).
	e.published(t, "company_admin", "company_a", textInput("Şirket"))
	if got := ids(authz.Anonymous(), "company_a"); len(got) != 0 {
		t.Fatalf("anonymous company page = %v", got)
	}
	if _, err := e.svc.GetPost(ctx, authz.Anonymous(), ad.Post.ID); err == nil {
		t.Fatal("anonymous reader opened a commercial post")
	}
	if _, err := e.svc.GetPost(ctx, authz.Anonymous(), pub.Post.ID); err != nil {
		t.Fatalf("anonymous GetPost(public): %v", err)
	}
	// An unknown org's page is 404; the anonymous feed does not exist.
	_, err := e.svc.ListPosts(ctx, authz.Anonymous(), uuid.NewV7(), feed.PageQuery{})
	wantNotFound(t, err)
	_, err = e.svc.GetFeed(ctx, authz.Anonymous(), feed.FeedQuery{})
	wantForbidden(t, err)
}

// TestUpdatePost_PublishedAudienceImmutable: a published post keeps its
// kind, audience and commercial label (409); its text may change, with
// edited_at and a new ETag; If-Match is enforced (428/412).
func TestUpdatePost_PublishedAudienceImmutable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	v := e.published(t, "club_admin", "club_a", textInput("ilk metin"))
	cur, err := e.svc.GetPost(ctx, admin, v.Post.ID)
	if err != nil {
		t.Fatal(err)
	}
	fol, img, on := feed.AudienceFollowers, feed.KindImage, true
	for _, p := range []feed.PostPatch{{AudienceKind: &fol}, {Kind: &img}, {CommercialFlag: &on}} {
		_, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, cur.ETag, p)
		wantCode(t, err, 409, "feed.published_immutable")
	}
	_, err = e.svc.UpdatePost(ctx, admin, v.Post.ID, "", feed.PostPatch{Body: doc("x")})
	wantCode(t, err, 428, "precondition_required")
	_, err = e.svc.UpdatePost(ctx, admin, v.Post.ID, `"0"`, feed.PostPatch{Body: doc("x")})
	wantCode(t, err, 412, "precondition_failed")
	e.clk.Advance(time.Minute)
	u, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, cur.ETag, feed.PostPatch{Body: doc("düzeltilmiş metin")})
	if err != nil || u.Post.EditedAt == nil || *u.Post.BodyText != "düzeltilmiş metin" || u.ETag == cur.ETag ||
		u.Post.AudienceKind != feed.AudiencePublic || u.Post.Status != feed.StatusPublished {
		t.Fatalf("UpdatePost(text) = %+v, %v", u.Post, err)
	}
	_, err = e.svc.UpdatePost(ctx, admin, v.Post.ID, cur.ETag, feed.PostPatch{Body: doc("y")})
	wantCode(t, err, 412, "precondition_failed")
	_, err = e.svc.UpdatePost(ctx, admin, v.Post.ID, u.ETag, feed.PostPatch{})
	wantCode(t, err, 422, "feed.empty_patch")
	// A draft may still change its audience and kind.
	d := e.create(t, "club_admin", "club_a", textInput("taslak"))
	if u, err := e.svc.UpdatePost(ctx, admin, d.Post.ID, d.ETag, feed.PostPatch{AudienceKind: &fol}); err != nil ||
		u.Post.AudienceKind != feed.AudienceFollowers {
		t.Fatalf("draft audience change = %+v, %v", u.Post, err)
	}
	// Another org's writer: the published post is visible (403), a draft is not (404).
	_, err = e.svc.UpdatePost(ctx, persona(t, "other_club_admin"), v.Post.ID, u.ETag, feed.PostPatch{Body: doc("z")})
	wantForbidden(t, err)
	_, err = e.svc.UpdatePost(ctx, persona(t, "other_club_admin"), d.Post.ID, d.ETag, feed.PostPatch{Body: doc("z")})
	wantNotFound(t, err)
}

// TestDeletePost_DraftHardDelete_PublishedRemoved: a post never published
// (draft, pending_review) is deleted for good; a published one becomes
// removed: gone for readers and the author, kept for moderators, listed
// with its status in the management list.
func TestDeletePost_DraftHardDelete_PublishedRemoved(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	d := e.create(t, "club_admin", "club_a", textInput("taslak"))
	if err := e.svc.DeletePost(ctx, admin, d.Post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mem.GetPost(ctx, d.Post.ID); err == nil {
		t.Fatal("the draft still exists")
	}
	wantNotFound(t, e.svc.DeletePost(ctx, admin, d.Post.ID))
	// pending_review (a new org's post) is deleted too.
	r := e.create(t, "company_admin", "company_a", textInput("incelemede"))
	if p, err := e.svc.PublishPost(ctx, persona(t, "company_admin"), r.Post.ID); err != nil || p.Post.Status != feed.StatusPendingReview {
		t.Fatal(p.Post.Status, err)
	}
	if err := e.svc.DeletePost(ctx, persona(t, "company_admin"), r.Post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mem.GetPost(ctx, r.Post.ID); err == nil {
		t.Fatal("the post in review still exists")
	}
	// Published → removed.
	p := e.published(t, "club_admin", "club_a", textInput("yayında"))
	wantForbidden(t, e.svc.DeletePost(ctx, persona(t, "club_member"), p.Post.ID))
	if err := e.svc.DeletePost(ctx, admin, p.Post.ID); err != nil {
		t.Fatal(err)
	}
	row, err := e.mem.GetPost(ctx, p.Post.ID)
	if err != nil || row.Status != feed.StatusRemoved || row.RemovedAt == nil {
		t.Fatalf("removed row = %+v, %v", row, err)
	}
	if ids := e.feedIDs(t, persona(t, "student"), feed.FeedQuery{}); contains(ids, p.Post.ID) {
		t.Fatal("a removed post is in the feed")
	}
	_, err = e.svc.GetPost(ctx, admin, p.Post.ID)
	wantNotFound(t, err)
	wantNotFound(t, e.svc.DeletePost(ctx, admin, p.Post.ID))
	if _, err := e.svc.GetPost(ctx, persona(t, "moderator"), p.Post.ID); err != nil {
		t.Fatalf("moderator GetPost(removed): %v", err)
	}
	list, err := e.svc.ListOrgPosts(ctx, admin, orgID("club_a"), feed.ManageQuery{Statuses: []feed.Status{feed.StatusRemoved}})
	if err != nil || len(list.Items) != 1 || list.Items[0].Post.Status != feed.StatusRemoved || list.Items[0].Post.BodyJSON != nil {
		t.Fatalf("management list = %+v, %v", list.Items, err)
	}
	if n := len(e.audit.of(authz.FeedPostDelete)); n != 3 {
		t.Fatalf("delete audits = %d", n)
	}
}

// TestPost_OrgIsolation (06 H-38): another org's writer never reaches a
// draft (404) nor writes into another org (404 on its surface); members
// without a writer role get 403; a suspended org's members cannot write.
func TestPost_OrgIsolation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	d := e.create(t, "club_admin", "club_a", textInput("taslak"))
	other := persona(t, "other_club_admin")
	for _, err := range []error{
		func() error { _, err := e.svc.GetPost(ctx, other, d.Post.ID); return err }(),
		func() error { _, err := e.svc.PublishPost(ctx, other, d.Post.ID); return err }(),
		e.svc.DeletePost(ctx, other, d.Post.ID),
		func() error { _, err := e.svc.PostStats(ctx, other, d.Post.ID); return err }(),
		func() error { _, err := e.svc.CreatePost(ctx, other, orgID("club_a"), textInput("x")); return err }(),
		func() error {
			_, err := e.svc.ListOrgPosts(ctx, other, orgID("club_a"), feed.ManageQuery{})
			return err
		}(),
	} {
		wantNotFound(t, err)
	}
	_, err := e.svc.CreatePost(ctx, persona(t, "club_member"), orgID("club_a"), textInput("x"))
	wantForbidden(t, err)
	_, err = e.svc.GetPost(ctx, persona(t, "club_member"), d.Post.ID)
	wantNotFound(t, err)
	e.orgs.setStatus(orgID("club_a"), orgs.StatusSuspended)
	_, err = e.svc.CreatePost(ctx, persona(t, "club_admin"), orgID("club_a"), textInput("x"))
	wantCode(t, err, 403, "orgs.org_suspended")
	e.orgs.setStatus(orgID("company_b"), orgs.StatusPendingReview)
	_, err = e.svc.CreatePost(ctx, persona(t, "other_company_member"), orgID("company_b"), textInput("x"))
	wantCode(t, err, 403, "orgs.org_not_verified")
	// Stats: the writers' counters, nobody else's.
	p := e.published(t, "company_admin", "company_a", textInput("istatistik"))
	if st, err := e.svc.PostStats(ctx, persona(t, "company_member"), p.Post.ID); err != nil || st.LikeCount != 0 {
		t.Fatalf("PostStats = %+v, %v", st, err)
	}
	_, err = e.svc.PostStats(ctx, persona(t, "student"), p.Post.ID)
	wantForbidden(t, err)
}

// TestPost_KindsAndEventShare: the shape of each kind, the shared event
// (a published public event; a sensitive event by its own org only) and
// the opportunity of an opportunity call.
func TestPost_KindsAndEventShare(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	create := func(in feed.PostInput) error {
		_, err := e.svc.CreatePost(ctx, admin, orgID("club_a"), in)
		return err
	}
	wantField(t, create(feed.PostInput{Kind: feed.KindText}), "body.body_json")
	wantField(t, create(feed.PostInput{Kind: feed.KindImage}), "body.media")
	wantField(t, create(feed.PostInput{Kind: feed.KindEventShare}), "body.event_id")
	wantField(t, create(feed.PostInput{Kind: feed.KindOpportunityCall, Body: doc("x")}), "body.opportunity")
	img := e.files.upload(orgID("club_a"), files.ScanClean)
	wantField(t, create(feed.PostInput{Kind: feed.KindText, Body: doc("x"), Media: []feed.MediaInput{{FileID: img}}}), "body.media")
	foreign := e.files.upload(orgID("club_b"), files.ScanClean)
	wantField(t, create(feed.PostInput{Kind: feed.KindImage, Media: []feed.MediaInput{{FileID: foreign}}}), "body.media[0].file_id")
	wantField(t, create(feed.PostInput{Kind: feed.KindText, Title: strings.Repeat("ş", 141), Body: doc("x")}), "body.title")
	segment := feed.AudienceSegment
	wantField(t, create(feed.PostInput{Kind: feed.KindText, Body: doc("x"), AudienceKind: segment}), "body.audience_kind")

	start := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	pub := events.EventSummary{ID: uuid.NewV7(), OrgID: orgID("club_b"), Title: "Açık etkinlik", Status: events.StatusPublished,
		StartsAt: start, EndsAt: start.Add(time.Hour), Visibility: events.VisibilityPublic}
	members := pub
	members.ID, members.Visibility = uuid.NewV7(), events.VisibilityMembers
	sensitive := pub
	sensitive.ID, sensitive.OrgID, sensitive.Sensitive = uuid.NewV7(), orgID("club_sensitive"), true
	for _, ev := range []events.EventSummary{pub, members, sensitive} {
		e.events.put(ev)
	}
	for _, id := range []uuid.UUID{members.ID, sensitive.ID, uuid.NewV7()} {
		wantField(t, create(feed.PostInput{Kind: feed.KindEventShare, EventID: &id}), "body.event_id")
	}
	e.trust("club_a")
	v := e.create(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindEventShare, EventID: &pub.ID})
	// The event is cancelled before publication: 422.
	cancelled := pub
	cancelled.Status = events.StatusCancelled
	e.events.put(cancelled)
	_, err := e.svc.PublishPost(ctx, admin, v.Post.ID)
	wantField(t, err, "body.event_id")
	e.events.put(pub)
	p, err := e.svc.PublishPost(ctx, admin, v.Post.ID)
	if err != nil || p.Event == nil || p.Event.Title != "Açık etkinlik" {
		t.Fatalf("event share = %+v, %v", p.Event, err)
	}
	// The sensitive club shares its own sensitive event.
	sv := e.create(t, "club_admin_sensitive", "club_sensitive", feed.PostInput{Kind: feed.KindEventShare, EventID: &sensitive.ID})
	if sv.Post.EventID == nil || *sv.Post.EventID != sensitive.ID {
		t.Fatal("own sensitive event not shared")
	}

	// Opportunity calls: a club applies through an external link only.
	op := &feed.Opportunity{Type: "internship", ApplyVia: "contact_request", Deadline: "2026-12-01"}
	wantField(t, create(feed.PostInput{Kind: feed.KindOpportunityCall, Body: doc("x"), Opportunity: op}), "body.opportunity.apply_via")
	op = &feed.Opportunity{Type: "talent_call", ApplyVia: "external_url", ExternalURL: "javascript:alert(1)"}
	wantField(t, create(feed.PostInput{Kind: feed.KindOpportunityCall, Body: doc("x"), Opportunity: op}), "body.opportunity.external_url")
	op = &feed.Opportunity{Type: "talent_call", ApplyVia: "external_url", ExternalURL: "https://forms.koro.test/a",
		EligibleYears: []int{4, 3, 3}, WorkMode: "remote"}
	if err := create(feed.PostInput{Kind: feed.KindOpportunityCall, Body: doc("Başvurular açık"), Opportunity: op}); err != nil {
		t.Fatal(err)
	}
	cv := e.create(t, "company_member", "company_a", feed.PostInput{Kind: feed.KindOpportunityCall, Title: "Yaz stajı",
		Body: doc("Veri mühendisliği"), Opportunity: &feed.Opportunity{Type: "internship", ApplyVia: "contact_request", Deadline: "2026-12-01"}})
	sum, err := e.svc.Get(ctx, cv.Post.ID)
	if err != nil || sum.Opportunity == nil || sum.Opportunity.ApplyVia != "contact_request" || sum.Title != "Yaz stajı" {
		t.Fatalf("Posts.Get = %+v, %v", sum, err)
	}
}

// TestPrivacy_AuthorPosts: the KVKK provider exports a user's posts and
// keeps them on erasure (org content; the author reference is the
// anonymised account).
func TestPrivacy_AuthorPosts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.create(t, "club_admin", "club_a", textInput("kişisel değil"))
	sec, err := e.mod.Privacy.Export(ctx, persona(t, "club_admin").UserID)
	if err != nil || len(sec.Items) != 1 || sec.Items[0]["post_id"] != v.Post.ID.String() || e.mod.Privacy.Name() != "feed" {
		t.Fatalf("Export = %+v, %v", sec, err)
	}
	for range 2 {
		res, err := e.mod.Privacy.Erase(ctx, persona(t, "club_admin").UserID)
		if err != nil || res.Retained != 1 || res.RetainedReason == "" || res.Deleted != 0 {
			t.Fatalf("Erase = %+v, %v", res, err)
		}
	}
	if _, err := e.mem.GetPost(ctx, v.Post.ID); err != nil {
		t.Fatal("erasure deleted the org's post")
	}
}

// TestAudienceMatch pins 02 §9.1: OR within a dimension, AND across.
func TestAudienceMatch(t *testing.T) {
	t.Parallel()
	reader := []string{"all", "uni:u1", "year:3", "club:c1"}
	for _, c := range []struct {
		keys []string
		dims int16
		want bool
	}{
		{[]string{"all"}, 1, true},
		{[]string{"uni:u1", "uni:u2"}, 1, true},
		{[]string{"uni:u2"}, 1, false},
		{[]string{"uni:u1", "year:3"}, 2, true},
		{[]string{"uni:u1", "year:4"}, 2, false},
		{[]string{"uni:u1", "club:c2", "club:c1"}, 2, true},
	} {
		if got := feed.AudienceMatch(c.keys, c.dims, reader); got != c.want {
			t.Errorf("match(%v, %d) = %v", c.keys, c.dims, got)
		}
	}
}

// TestFeed_AuthorLogoAndVerified (BE-094, T-251): the author carries the
// org's verification badge and a signed logo URL; a page reads the author
// orgs with one GetMany and signs each logo once, whatever the number of
// posts; a logo the scan has not cleared is left out.
func TestFeed_AuthorLogoAndVerified(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	logo := e.files.upload(orgID("club_a"), files.ScanClean)
	a := e.orgs.orgs[orgID("club_a")]
	a.LogoFileID, a.Verified = &logo, true
	e.orgs.orgs[a.ID] = a
	pending := e.files.upload(orgID("club_b"), files.ScanPending)
	b := e.orgs.orgs[orgID("club_b")]
	b.LogoFileID = &pending
	e.orgs.orgs[b.ID] = b
	for range 3 {
		e.published(t, "club_admin", "club_a", textInput("A"))
	}
	e.published(t, "other_club_admin", "club_b", textInput("B"))

	before, signed := e.orgs.getMany, e.files.presigns(logo)
	p, err := e.svc.GetFeed(ctx, persona(t, "student"), feed.FeedQuery{Limit: 10})
	if err != nil || len(p.Items) != 4 {
		t.Fatalf("GetFeed = %d items, %v", len(p.Items), err)
	}
	if n := e.orgs.getMany - before; n != 1 {
		t.Fatalf("GetMany calls per page = %d, want 1", n)
	}
	if n := e.files.presigns(logo) - signed; n != 1 {
		t.Fatalf("logo signed %d times on one page, want 1", n)
	}
	for _, v := range p.Items {
		d := feed.ToPostDTO(v)
		switch v.Post.OrgID {
		case a.ID:
			if !d.Author.Verified || d.Author.LogoURL != "https://files.example.test/"+logo.String() {
				t.Fatalf("club_a author = %+v", d.Author)
			}
		case b.ID:
			if d.Author.Verified || d.Author.LogoURL != "" {
				t.Fatalf("club_b author = %+v (unscanned logo, not verified)", d.Author)
			}
		}
	}
	// The JSON names the mobile client reads.
	raw, err := json.Marshal(feed.ToPostDTO(p.Items[0]).Author)
	if err != nil || !strings.Contains(string(raw), `"verified":`) {
		t.Fatalf("author JSON = %s, %v", raw, err)
	}
}
