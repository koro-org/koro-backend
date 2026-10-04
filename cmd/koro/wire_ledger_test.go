package main

import (
	"slices"
	"strings"
	"testing"

	"koro/internal/ledger"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
)

// useMemStore installs the in-memory ledger stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-039.
// Only tickets of the ledger module edit this file (BE-001 R4).
func (w *ledgerWiring) useMemStore() {
	w.store = ledger.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_LedgerJobs pins what the ledger module registers (01 §7):
// ledger.integrity_check daily at 05:15 on the low queue.
func TestWire_LedgerJobs(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "ledger.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"ledger.integrity_check 15 5 * * * low"}; !slices.Equal(jobs, want) {
		t.Fatalf("ledger jobs %q, want %q", jobs, want)
	}
	if c.ledger.svc == nil {
		t.Fatal("ledger service not built")
	}
}
