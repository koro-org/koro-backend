package main

import (
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/ledger"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
)

// useMemStore installs the in-memory ledger stores used by service,
// wiring and authz matrix tests (05 §13.2): the MemStore is the ledger
// store and the payout store (BE-044). First owner: BE-039.
// Only tickets of the ledger module edit this file (BE-001 R4).
func (w *ledgerWiring) useMemStore() {
	w.store = ledger.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_LedgerPayouts pins what the payout flow registers (BE-044): the
// payout store is the in-memory store, the notices of payout.created,
// .approved and .paid (01 §6.2), the KVKK provider of the payouts and the
// "open payouts" closeOrg check after orders' "open orders" (both kept).
func TestWire_LedgerPayouts(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	for topic, name := range map[string]string{
		domainevent.TopicPayoutCreated:  ledger.NotifyPayoutCreatedTaskName,
		domainevent.TopicPayoutApproved: ledger.NotifyPayoutApprovedTaskName,
		domainevent.TopicPayoutPaid:     ledger.NotifyPayoutPaidTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name }) {
			t.Errorf("%s has no %s subscription", topic, name)
		}
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "ledger" }) {
		t.Error("the ledger KVKK provider is not registered")
	}
	if n := len(c.hooks.openCommerce); n < 2 {
		t.Fatalf("hooks.openCommerce has %d checks, want orders' and ledger's", n)
	}
	for i, h := range c.hooks.openCommerce {
		if open, err := h(t.Context(), uuid.NewV7()); err != nil || open {
			t.Errorf("openCommerce[%d](unknown org) = %v, %v", i, open, err)
		}
	}
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
