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
// over the system reads of students, events, directory and feed (each
// asserted on its service), answers a verified student over the in-memory
// stores (on by default for an adult, off for a minor) and registers its
// read on the self surface.
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
		if r.Enabled != enabled || r.Events == nil || r.Clubs == nil {
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
// recommend needs stops the build instead of answering without it.
func TestWire_RecommendNeedsEveryRead(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	c.feed.svc = nil
	if err := c.recommend.build(c); err == nil {
		t.Fatal("recommend built without the feed read")
	}
}
