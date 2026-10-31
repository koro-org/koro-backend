package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/orgs"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/webcache"
)

// webcacheTestSecret is a test value (not a real key).
const webcacheTestSecret = "wire-test-secret-0123456789abcdefghijklmnop"

func webcacheSubs(c *container) map[string]outbox.Subscription {
	out := map[string]outbox.Subscription{}
	for _, s := range c.infra.subs.All() {
		out[s.Name] = s
	}
	return out
}

// TestWire_WebcacheSubscriptionsRegistered: the
// webcache.revalidate_* subscriptions exist in every process; without the
// worker's configuration (api, seed, these tests) they skip (BE-106,
// BE-120, BE-149).
func TestWire_WebcacheSubscriptionsRegistered(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	subs := webcacheSubs(c)
	want := map[string]string{
		webcache.SubEventPublished:   domainevent.TopicEventPublished,
		webcache.SubEventUpdated:     domainevent.TopicEventUpdated,
		webcache.SubEventCancelled:   domainevent.TopicEventCancelled,
		webcache.SubOrgUpdated:       domainevent.TopicOrgUpdated,
		webcache.SubClubUpdated:      domainevent.TopicClubUpdated,
		webcache.SubClubSensitivity:  domainevent.TopicClubSensitivityChanged,
		webcache.SubClubClaimDecided: domainevent.TopicClubClaimDecided,
		webcache.SubFilePublished:    domainevent.TopicFilePublished,
		webcache.SubFileRejected:     domainevent.TopicFileRejected,
		webcache.SubPostChanged:      domainevent.TopicPostChanged,
	}
	for name, topic := range want {
		if s, ok := subs[name]; !ok || s.Topic != topic {
			t.Errorf("subscription %s = %+v, want topic %s", name, s, topic)
		}
	}
	env, err := domainevent.New(domainevent.OrgUpdated{OrgID: uuid.NewV7(), OrgKind: "club", Fields: []string{"name"}}, "org", uuid.NewV7())
	if err != nil {
		t.Fatal(err)
	}
	if err := subs[webcache.SubOrgUpdated].Handle(t.Context(), env); err != nil {
		t.Fatalf("unconfigured webhook = %v, want a no-op", err)
	}
}

// TestWire_WebcacheResolvesSlugsAndPosts: with the worker's configuration
// the subscription reads the org through orgs and POSTs its signed tag.
func TestWire_WebcacheResolvesSlugsAndPosts(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		got  [][]string
		bad  []string
		seen int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		ts := r.Header.Get(webcache.TimestampHeader)
		if r.URL.Path != webcache.Path || r.Header.Get(webcache.SignatureHeader) != webcache.Sign([]byte(webcacheTestSecret), ts, raw) {
			bad = append(bad, r.URL.Path)
		}
		var b struct{ Tags []string }
		_ = json.Unmarshal(raw, &b)
		got, seen = append(got, b.Tags), seen+1
	}))
	t.Cleanup(srv.Close)

	c := newTestContainer(t)
	c.infra.webcache = config.Webcache{WebInternalURL: srv.URL, RevalidateSecret: config.NewSecret(webcacheTestSecret)}
	c.web.client = srv.Client()
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var orgID uuid.UUID
	err := (db.NopTx{}).WithTx(t.Context(), func(ctx context.Context) error {
		var err error
		orgID, err = c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Robotik Kulübü", Slug: "robotik-kulubu"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := domainevent.New(domainevent.OrgUpdated{OrgID: orgID, OrgKind: "club", Fields: []string{"status"}}, "org", orgID)
	if err != nil {
		t.Fatal(err)
	}
	if err := webcacheSubs(c)[webcache.SubOrgUpdated].Handle(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	// An unknown event of an unknown org: nothing public, no request.
	ev, err := domainevent.New(domainevent.EventCancelled{EventID: uuid.NewV7(), OrgID: uuid.NewV7()}, "event", uuid.NewV7())
	if err != nil {
		t.Fatal(err)
	}
	if err := webcacheSubs(c)[webcache.SubEventCancelled].Handle(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bad) > 0 || seen != 1 || !slices.Equal(got[0], []string{"club:robotik-kulubu", "sitemap"}) {
		t.Fatalf("requests = %v (bad %v)", got, bad)
	}
}

// TestWire_WebcacheResolvesPublicPosts (BE-150): the resolver reads the
// public posts through feed.PublicPosts: a public share of an event is
// found and listed, a followers share is neither.
func TestWire_WebcacheResolvesPublicPosts(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	eventID := uuid.NewV7()
	share := func(audience feed.AudienceKind) uuid.UUID {
		t.Helper()
		p := feed.Post{
			ID: uuid.NewV7(), OrgID: authztest.ID("club_a"), AuthorOrgKind: feed.AuthorClub, AuthorUserID: authztest.ID("club_admin"),
			Kind: feed.KindEventShare, EventID: &eventID, AudienceKind: audience, AudienceKeys: []string{feed.KeyAll},
			AudienceDimCount: 1, Status: feed.StatusPublished, PublishedAt: &at, CreatedAt: at, UpdatedAt: at,
		}
		if err := c.feed.built.(*feed.MemStore).PutPost(p); err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	public, followers := share(feed.AudiencePublic), share(feed.AudienceFollowers)
	r := webResolver{c: c}
	if ref, found, err := r.Post(ctx, public); err != nil || !found || ref != (webcache.PostRef{ID: public, OrgID: authztest.ID("club_a")}) {
		t.Fatalf("Post(public) = %+v, %v, %v", ref, found, err)
	}
	if ref, found, err := r.Post(ctx, followers); err != nil || found {
		t.Fatalf("Post(followers) = %+v, %v, %v", ref, found, err)
	}
	refs, err := r.EventShares(ctx, []uuid.UUID{eventID}, 10)
	if err != nil || len(refs) != 1 || refs[0].ID != public {
		t.Fatalf("EventShares = %+v, %v", refs, err)
	}
}

// TestWire_WebcacheResolvesOrgEvents (BE-157): the resolver reads an
// organiser's events through the anonymous events catalogue — public,
// published, past ones too — newest published first; drafts, events of a
// university's audience and other orgs' events are not among them.
func TestWire_WebcacheResolvesOrgEvents(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var club, other uuid.UUID
	err := (db.NopTx{}).WithTx(ctx, func(ctx context.Context) error {
		var err error
		if club, err = c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Robotik Kulübü", Slug: "robotik-kulubu"}); err != nil {
			return err
		}
		other, err = c.orgs.svc.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Satranç Kulübü", Slug: "satranc-kulubu"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	mem := c.events.built.(*events.MemStore)
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC) // before the container clock (2026-10-03)
	put := func(org uuid.UUID, status events.Status, vis events.Visibility, starts time.Time, published int) uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		e := events.Event{
			ID: id, OrgID: org, CreatedBy: authztest.ID("club_admin"), Title: "Etkinlik", Slug: "etkinlik-" + id.String()[:8],
			Category: events.CategoryWorkshop, Format: events.FormatInPerson, City: new("İstanbul"),
			StartsAt: starts, EndsAt: starts.Add(2 * time.Hour), Visibility: vis, Status: status,
			RequiresRegistration: true, RegistrationMode: events.ModeAuto, CreatedAt: at, UpdatedAt: at,
		}
		if vis == events.VisibilityUniversity {
			u := authztest.ID("uni_a")
			e.VisibilityUniversityID = &u
		}
		if status != events.StatusDraft {
			p := at.Add(time.Duration(published) * time.Hour)
			e.PublishedAt = &p
		}
		if err := mem.PutEvent(e); err != nil {
			t.Fatal(err)
		}
		return id
	}
	past := put(club, events.StatusPublished, events.VisibilityPublic, at.Add(24*time.Hour), 1)
	upcoming := put(club, events.StatusPublished, events.VisibilityPublic, at.AddDate(0, 3, 0), 2)
	put(club, events.StatusDraft, events.VisibilityPublic, at.AddDate(0, 3, 0), 0)
	put(club, events.StatusPublished, events.VisibilityUniversity, at.AddDate(0, 3, 0), 3)
	put(other, events.StatusPublished, events.VisibilityPublic, at.AddDate(0, 3, 0), 4)

	r := webResolver{c: c}
	if got, err := r.OrgEvents(ctx, club, 10); err != nil || !slices.Equal(got, []uuid.UUID{upcoming, past}) {
		t.Fatalf("OrgEvents = %v, %v; want %v", got, err, []uuid.UUID{upcoming, past})
	}
	if got, err := r.OrgEvents(ctx, club, 1); err != nil || !slices.Equal(got, []uuid.UUID{upcoming}) {
		t.Fatalf("OrgEvents(limit 1) = %v, %v", got, err)
	}
}
