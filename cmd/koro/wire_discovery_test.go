package main

import (
	"fmt"
	"testing"
	"uuid"

	"koro/internal/discovery"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// useMemStore installs the in-memory discovery stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-051.
// Only tickets of the discovery module edit this file (BE-001 R4).
// Discovery owns no store: the seams are the transaction manager of its
// audit records and the shared test token deriver (identity's).
func (w *discoveryWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.deriver, w.keyIDs = testDeriver(), []string{"test1"}
}

// TestWire_DiscoveryFillsAnonResolverAndRoutes (BE-051): discovery builds
// over the students level 2 index, talent's bands, entitlements, orgs and
// audit, fills hooks.anonResolver (messaging) and registers its two
// operations on the org surface; a pseudonym nobody holds resolves to
// nothing, and the pseudonym of the hook is the one of the service.
func TestWire_DiscoveryFillsAnonResolverAndRoutes(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.discovery.svc == nil || c.hooks.anonResolver == nil {
		t.Fatal("discovery did not set its service or hooks.anonResolver")
	}
	company, user := uuid.NewV7(), uuid.NewV7()
	anon, err := c.discovery.svc.AnonID(company, user)
	if err != nil || !discovery.ValidAnonID(anon) {
		t.Fatalf("AnonID = %q, %v", anon, err)
	}
	if id, found, err := c.hooks.anonResolver(t.Context(), company, anon); err != nil || found || id != uuid.Nil() {
		t.Fatalf("resolver for a student without a profile = %v, %v, %v; want not found", id, found, err)
	}
	if _, found, _ := c.hooks.anonResolver(t.Context(), company, "not-a-pseudonym"); found {
		t.Fatal("a malformed pseudonym resolved")
	}

	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors})
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	paths := app.API.OpenAPI().Paths
	for _, tc := range []struct{ path, id string }{
		{"/api/v1/orgs/{org_id}/discovery/profiles", "searchDiscovery"},
		{"/api/v1/orgs/{org_id}/discovery/profiles/{anon_id}", "getDiscoveryProfile"},
	} {
		p := paths[tc.path]
		if p == nil || p.Get == nil || p.Get.OperationID != tc.id {
			t.Fatalf("%s: operation %s not registered", tc.path, tc.id)
		}
		if fmt.Sprint(p.Get.Extensions["x-koro-surface"]) != "org" {
			t.Fatalf("%s: surface %v", tc.id, p.Get.Extensions["x-koro-surface"])
		}
	}
	if rl := fmt.Sprint(paths["/api/v1/orgs/{org_id}/discovery/profiles"].Get.Extensions["x-koro-rate-limit"]); rl != "search" {
		t.Fatalf("searchDiscovery rate class = %v, want search (04 §1.9)", rl)
	}
}
