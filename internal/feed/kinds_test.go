package feed_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/feed"
)

// TestKindsOf_CanonicalSet (BE-191): the kind filter is a set: the same
// kinds in any order, repeated or not, are the same filter and the same
// cursor fingerprint part; one kind is the kind itself (the cursors of
// the single kind filter before BE-191 stay valid), none is "".
func TestKindsOf_CanonicalSet(t *testing.T) {
	t.Parallel()
	a := feed.KindsOf([]string{"text", "image", "text"})
	b := feed.KindsOf([]string{"image", "text"})
	if !slices.Equal(a, b) || a.String() != "image,text" || b.String() != a.String() {
		t.Fatalf("KindsOf = %v (%q), %v (%q)", a, a.String(), b, b.String())
	}
	if one := feed.KindsOf([]string{"opportunity_call"}); one.String() != "opportunity_call" {
		t.Fatalf("one kind = %q", one.String())
	}
	if none := feed.KindsOf(nil); none != nil || none.String() != "" || !none.Valid() {
		t.Fatalf("no kind = %v", none)
	}
	if feed.KindsOf([]string{"text", "poll"}).Valid() {
		t.Fatal("an unknown kind is valid")
	}
}

// TestFeed_KindFilter (BE-191): getFeed and listPosts keep the posts of
// the kinds asked (several: any of them), every kind without a filter; an
// unknown kind is 422 query.kind.
func TestFeed_KindFilter(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	reader := persona(t, "student")
	text := e.published(t, "club_admin", "club_a", textInput("Duyuru"))
	call := e.putCall(t, "club_a", feed.AuthorClub, clubCall, nil)
	ad := e.putCall(t, "company_a", feed.AuthorCompany, companyCall, nil)
	feedOf := func(kinds ...string) []uuid.UUID {
		t.Helper()
		return e.feedIDs(t, reader, feed.FeedQuery{Kinds: feed.KindsOf(kinds)})
	}
	if got := feedOf("opportunity_call"); !slices.Equal(got, []uuid.UUID{ad.ID, call.ID}) {
		t.Fatalf("opportunity calls = %v", got)
	}
	if got := feedOf("text", "opportunity_call"); !slices.Equal(got, []uuid.UUID{ad.ID, call.ID, text.Post.ID}) {
		t.Fatalf("text and calls = %v", got)
	}
	if got := feedOf("image", "event_share"); len(got) != 0 {
		t.Fatalf("images and shares = %v", got)
	}
	if got := feedOf(); len(got) != 3 {
		t.Fatalf("every kind = %v", got)
	}
	page, err := e.svc.ListPosts(ctx, reader, orgID("club_a"), feed.OrgPageQuery{Kinds: feed.Kinds{feed.KindOpportunityCall}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Post.ID != call.ID {
		t.Fatalf("club page calls = %+v, %v", page.Items, err)
	}
	page, err = e.svc.ListPosts(ctx, reader, orgID("club_a"), feed.OrgPageQuery{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("club page = %+v, %v", page.Items, err)
	}
	_, err = e.svc.GetFeed(ctx, reader, feed.FeedQuery{Kinds: feed.Kinds{"poll"}})
	wantField(t, err, "query.kind")
	_, err = e.svc.ListPosts(ctx, reader, orgID("club_a"), feed.OrgPageQuery{Kinds: feed.Kinds{"poll"}})
	wantField(t, err, "query.kind")
}
