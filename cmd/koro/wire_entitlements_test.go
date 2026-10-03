package main

import (
	"slices"
	"strings"
	"testing"

	"koro/internal/entitlements"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
)

// useMemStore installs the in-memory entitlements stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-013.
// Only tickets of the entitlements module edit this file (BE-001 R4).
// updated_at uses the real clock: these tests do not depend on it.
func (w *entitlementsWiring) useMemStore() {
	w.store = entitlements.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_EntitlementsJobs pins what the entitlements module registers
// (01 §7): entitlements.expire hourly at :45 on the default queue.
func TestWire_EntitlementsJobs(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "entitlements.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"entitlements.expire 45 * * * * default"}; !slices.Equal(jobs, want) {
		t.Fatalf("entitlements jobs %q, want %q", jobs, want)
	}
	if c.entitlements.svc == nil {
		t.Fatal("entitlements service not built")
	}
}
