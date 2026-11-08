package main

import (
	"fmt"
	"testing"

	"koro/internal/authz/authztest"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/recommend"
)

// useMemStore: recommend owns no store and no seam (BE-174); its ports are
// the other modules' services, which their own useMemStore installs.
func (w *recommendWiring) useMemStore() {}

// TestWire_RecommendReadsItsModulesAndRoutes (BE-174): recommend builds
// over the system reads of students, events, directory, feed and messaging
// (each asserted on its service; the opportunity read of feed: BE-191; the
// contacted calls of messaging: BE-198), answers
// a verified student over the in-memory stores (on by default for an
// adult, off for a minor) and registers its read on the self surface.
func TestWire_RecommendReadsItsModulesAndRoutes(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.recommend.svc == nil {
		t.Fatal("recommend did not set its service")
	}
	for name, enabled := range map[string]bool{"student": true, "student_minor": false} {
		p, _ := authztest.Get(name)
		r, err := c.recommend.svc.List(t.Context(), p.Actor, recommend.Query{})
		if err != nil {
			t.Fatalf("%s: List: %v", name, err)
		}
		if r.Enabled != enabled || r.Events == nil || r.Clubs == nil || r.Opportunities == nil {
			t.Fatalf("%s: recommendations = %+v", name, r)
		}
	}

	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors})
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	p := app.API.OpenAPI().Paths["/api/v1/me/recommendations"]
	if p == nil || p.Get == nil || p.Get.OperationID != "listMyRecommendations" {
		t.Fatal("listMyRecommendations not registered")
	}
	if s := fmt.Sprint(p.Get.Extensions["x-koro-surface"]); s != "self" {
		t.Fatalf("surface %s, want self", s)
	}
	if len(p.Get.Tags) != 1 || p.Get.Tags[0] != "recommend" {
		t.Fatalf("tags %v", p.Get.Tags)
	}
}

// TestWire_RecommendNeedsEveryRead: a module that stops offering the read
// recommend needs (feed's; messaging's contacted calls, BE-198) stops the
// build instead of answering without it.
func TestWire_RecommendNeedsEveryRead(t *testing.T) {
	t.Parallel()
	for name, drop := range map[string]func(c *container){
		"feed":      func(c *container) { c.feed.svc = nil },
		"messaging": func(c *container) { c.messaging.svc = nil },
	} {
		c := newTestContainer(t)
		if err := c.build(); err != nil {
			t.Fatal(err)
		}
		drop(c)
		if err := c.recommend.build(c); err == nil {
			t.Fatalf("recommend built without the %s read", name)
		}
	}
}
