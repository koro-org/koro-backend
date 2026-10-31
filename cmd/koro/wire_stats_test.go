package main

import (
	"fmt"
	"testing"

	"koro/internal/authz/authztest"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/stats"
)

// useMemStore: stats owns no store and no seam (BE-163); its ports are the
// other modules' services, which their own useMemStore installs.
func (w *statsWiring) useMemStore() {}

// TestWire_StatsReadsEveryModuleAndRoutes (BE-163): stats builds over the
// Statistics reads of the modules that own the rows (each asserted on its
// service), answers the platform panel over the in-memory stores and
// registers its four read operations on the admin and org surfaces.
func TestWire_StatsReadsEveryModuleAndRoutes(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.stats.svc == nil {
		t.Fatal("stats did not set its service")
	}
	pad, _ := authztest.Get("platform_admin")
	st, err := c.stats.svc.Platform(t.Context(), pad.Actor, stats.RangeQuery{})
	if err != nil {
		t.Fatalf("Platform: %v", err)
	}
	if st.Range.From == "" || st.Users.ByStatus == nil || st.Orgs.ByKind == nil {
		t.Fatalf("platform stats = %+v", st)
	}

	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: c.actors})
	if err != nil {
		t.Fatal(err)
	}
	c.registerRoutes(app.V1)
	paths := app.API.OpenAPI().Paths
	for _, tc := range []struct{ path, id, surface string }{
		{"/api/v1/admin/stats", "getAdminStats", "admin"},
		{"/api/v1/orgs/{org_id}/stats/club", "getClubStats", "org"},
		{"/api/v1/orgs/{org_id}/stats/company", "getCompanyStats", "org"},
		{"/api/v1/orgs/{org_id}/stats/university", "getUniversityStats", "org"},
	} {
		p := paths[tc.path]
		if p == nil || p.Get == nil || p.Get.OperationID != tc.id {
			t.Fatalf("%s: operation %s not registered", tc.path, tc.id)
		}
		if s := fmt.Sprint(p.Get.Extensions["x-koro-surface"]); s != tc.surface {
			t.Fatalf("%s: surface %s, want %s", tc.id, s, tc.surface)
		}
		if len(p.Get.Tags) != 1 || p.Get.Tags[0] != "stats" {
			t.Fatalf("%s: tags %v", tc.id, p.Get.Tags)
		}
	}
}

// TestWire_StatsNeedsEveryRead: a module that stops offering the read stats
// needs stops the build instead of answering a panel without it.
func TestWire_StatsNeedsEveryRead(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	c.orders.svc = nil
	if err := c.stats.build(c); err == nil {
		t.Fatal("stats built without the orders read")
	}
}
