package main

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/feed"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
)

// useMemStore installs the in-memory feed stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-027.
// Only tickets of the feed module edit this file (BE-001 R4).
func (w *feedWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.newStore = func(c *container) feed.Store { return feed.NewMemStore(c.infra.clock.Now) }
}

// TestWire_FeedProvidesServiceAndPrivacy pins what feed binds in build:
// its service (feed.Posts for messaging, answering from the built store),
// its KVKK provider and the post.published subscription.
func TestWire_FeedProvidesServiceAndPrivacy(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.feed.svc == nil || c.feed.built == nil {
		t.Fatal("feed service not built")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "feed" }) {
		t.Fatal("feed privacy provider not registered")
	}
	ctx := t.Context()
	if _, err := c.feed.svc.Get(ctx, uuid.NewV7()); err == nil {
		t.Fatal("Get(unknown) found a post")
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	title := "Kanca"
	p := feed.Post{
		ID: uuid.NewV7(), OrgID: authztest.ID("club_a"), AuthorOrgKind: feed.AuthorClub, AuthorUserID: authztest.ID("club_admin"),
		Kind: feed.KindText, Title: &title, BodyJSON: feed.JSON(`{"type":"doc"}`), AudienceKind: feed.AudiencePublic,
		AudienceKeys: []string{feed.KeyAll}, AudienceDimCount: 1, Status: feed.StatusDraft, CreatedAt: at, UpdatedAt: at,
	}
	if err := c.feed.built.(*feed.MemStore).PutPost(p); err != nil {
		t.Fatal(err)
	}
	got, err := c.feed.svc.Get(ctx, p.ID)
	if err != nil || got.ID != p.ID || got.OrgID != p.OrgID || got.Kind != feed.KindText || got.Title != title {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if subs := c.infra.subs.For(domainevent.TopicPostPublished); !slices.ContainsFunc(subs, func(s outbox.Subscription) bool {
		return s.Name == feed.NotifyFollowersTaskName
	}) {
		t.Fatalf("post.published subscribers = %v", subs)
	}
}
