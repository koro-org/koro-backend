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
	"uuid"

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
