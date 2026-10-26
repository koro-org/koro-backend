package feed_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/feed"
)

// TestPost_EventShareCarriesOrganiser (BE-133, WEB-022): the shared event
// of an event_share post names its organiser — public org data, as on the
// event cards — also when it is another org than the post's author; the
// card needs no getEvent read for it. The reader's post and list views
// carry it, and so does the author's management view.
func TestPost_EventShareCarriesOrganiser(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	start := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	ev := events.EventSummary{ID: uuid.NewV7(), OrgID: orgID("club_b"), Title: "Komşu kulübün etkinliği", Slug: "komsu",
		Status: events.StatusPublished, StartsAt: start, EndsAt: start.Add(time.Hour), Visibility: events.VisibilityPublic}
	e.events.put(ev)
	e.trust("club_a")
	admin := persona(t, "club_admin")
	v := e.create(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindEventShare, EventID: &ev.ID})
	published, err := e.svc.PublishPost(ctx, admin, v.Post.ID)
	if err != nil {
		t.Fatal(err)
	}
	club, err := e.orgs.Get(ctx, orgID("club_b"))
	if err != nil {
		t.Fatal(err)
	}
	want := events.EventOrgDTO{OrgID: club.ID.String(), Kind: string(club.Kind), Name: club.Name, Slug: club.Slug}
	check := func(name string, pv feed.PostView) {
		t.Helper()
		d := feed.ToPostDTO(pv)
		if d.Event == nil || d.Event.Org == nil || *d.Event.Org != want {
			t.Fatalf("%s: event = %+v, want organiser %+v", name, d.Event, want)
		}
		if d.Author.OrgID != orgID("club_a").String() {
			t.Fatalf("%s: author = %+v, want club_a", name, d.Author)
		}
	}
	check("publish", published)
	got, err := e.svc.GetPost(ctx, authz.Anonymous(), v.Post.ID)
	if err != nil {
		t.Fatal(err)
	}
	check("getPost", got)
	page, err := e.svc.ListPosts(ctx, authz.Anonymous(), orgID("club_a"), feed.PageQuery{Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("listPosts = %d items, %v", len(page.Items), err)
	}
	check("listPosts", page.Items[0])
	manage, err := e.svc.ListOrgPosts(ctx, admin, orgID("club_a"), feed.ManageQuery{Limit: 10})
	if err != nil || len(manage.Items) != 1 {
		t.Fatalf("listOrgPosts = %d items, %v", len(manage.Items), err)
	}
	check("listOrgPosts", manage.Items[0])
}
