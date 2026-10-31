package feed_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/feed"
)

// TestPublicPosts_AnonymousReadRule (BE-150): the public web's system read
// (webcache) finds exactly the posts an anonymous reader opens — published,
// public, not commercial — and the shares of an event among them, newest
// first, up to the limit.
func TestPublicPosts_AnonymousReadRule(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	start := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	ev := events.EventSummary{ID: uuid.NewV7(), OrgID: orgID("club_a"), Title: "Bahar Şenliği", Slug: "bahar-senligi",
		Status: events.StatusPublished, StartsAt: start, EndsAt: start.Add(time.Hour), Visibility: events.VisibilityPublic}
	e.events.put(ev)
	shareOf := func(audience feed.AudienceKind) feed.PostInput {
		return feed.PostInput{Kind: feed.KindEventShare, EventID: &ev.ID, AudienceKind: audience}
	}

	public := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	followersIn := textInput("Takipçilere")
	followersIn.AudienceKind = feed.AudienceFollowers
	followers := e.published(t, "club_admin", "club_a", followersIn).Post.ID
	commercial := e.published(t, "company_admin", "company_a", textInput("İlan")).Post.ID
	draft := e.create(t, "club_admin", "club_a", textInput("Taslak")).Post.ID
	hidden := e.putPost(t, feed.StatusHidden, feed.AudiencePublic, feed.KeyAll)

	if s, found, err := e.svc.PublicPost(ctx, public); err != nil || !found || s.ID != public || s.OrgID != orgID("club_a") {
		t.Fatalf("PublicPost(public) = %+v, %v, %v", s, found, err)
	}
	for name, id := range map[string]uuid.UUID{
		"followers": followers, "commercial": commercial, "draft": draft, "hidden": hidden, "missing": uuid.NewV7(),
	} {
		if s, found, err := e.svc.PublicPost(ctx, id); err != nil || found {
			t.Errorf("PublicPost(%s) = %+v, %v, %v; want not found", name, s, found, err)
		}
	}

	older := e.published(t, "club_admin", "club_a", shareOf(feed.AudiencePublic)).Post.ID
	newer := e.published(t, "club_admin", "club_a", shareOf(feed.AudiencePublic)).Post.ID
	e.published(t, "club_admin", "club_a", shareOf(feed.AudienceFollowers))
	e.create(t, "club_admin", "club_a", shareOf(feed.AudiencePublic))
	shares := func(limit int) []uuid.UUID {
		t.Helper()
		ps, err := e.svc.PublicEventShares(ctx, []uuid.UUID{ev.ID}, limit)
		if err != nil {
			t.Fatal(err)
		}
		out := []uuid.UUID{}
		for _, p := range ps {
			if p.OrgID != orgID("club_a") {
				t.Fatalf("share %s of org %s", p.ID, p.OrgID)
			}
			out = append(out, p.ID)
		}
		return out
	}
	if got := shares(10); !slices.Equal(got, []uuid.UUID{newer, older}) {
		t.Fatalf("PublicEventShares = %v, want %v", got, []uuid.UUID{newer, older})
	}
	if got := shares(1); !slices.Equal(got, []uuid.UUID{newer}) {
		t.Fatalf("PublicEventShares(limit 1) = %v", got)
	}
	if got := shares(0); len(got) != 0 {
		t.Fatalf("PublicEventShares(limit 0) = %v", got)
	}
	// An organiser's events at once (BE-157): one newest-first list; no
	// event, no post.
	ev2 := events.EventSummary{ID: uuid.NewV7(), OrgID: orgID("club_a"), Title: "Yaz Kampı", Slug: "yaz-kampi",
		Status: events.StatusPublished, StartsAt: start, EndsAt: start.Add(time.Hour), Visibility: events.VisibilityPublic}
	e.events.put(ev2)
	newest := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindEventShare, EventID: &ev2.ID, AudienceKind: feed.AudiencePublic}).Post.ID
	ps, err := e.svc.PublicEventShares(ctx, []uuid.UUID{ev.ID, ev2.ID}, 10)
	if err != nil || len(ps) != 3 || ps[0].ID != newest || ps[1].ID != newer || ps[2].ID != older {
		t.Fatalf("PublicEventShares(two events) = %+v, %v; want %v", ps, err, []uuid.UUID{newest, newer, older})
	}
	if ps, err := e.svc.PublicEventShares(ctx, nil, 10); err != nil || len(ps) != 0 {
		t.Fatalf("PublicEventShares(no event) = %+v, %v", ps, err)
	}
}
