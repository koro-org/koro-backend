package main

import (
	"slices"
	"testing"
	"time"

	"koro/internal/payments"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// useMemStore installs the in-memory payments stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-041.
// Only tickets of the payments module edit this file (BE-001 R4).
func (w *paymentsWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.newStore = func(c *container) payments.Store { return payments.NewMemStore(c.infra.clock.Now) }
}

// TestWire_Payments pins what the payments module provides and consumes:
// its service, hooks.collectionAccount (orders' payment instructions: Koro's
// active default account), the KVKK provider, the subscriptions of 01 §6.2,
// the plain notice task and the job of 01 §7.
func TestWire_Payments(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.payments.svc == nil || c.payments.payments == nil {
		t.Fatal("payments service not built")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "payments" }) {
		t.Error("the payments KVKK provider is not registered")
	}
	for topic, name := range map[string]string{
		domainevent.TopicCollectionAccountApproved: payments.NotifyAccountApprovedTaskName,
		domainevent.TopicPaymentCaptured:           payments.NotifyPaymentCapturedTaskName,
		domainevent.TopicPaymentRefunded:           payments.NotifyPaymentRefundedTaskName,
		domainevent.TopicIncomingTransferUnmatched: payments.NotifyUnmatchedTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name }) {
			t.Errorf("%s has no %s subscription", topic, name)
		}
	}
	if !slices.ContainsFunc(c.infra.jobs.Tasks(), func(q queue.Task) bool { return q.Name == payments.NotifyAccountChangedTaskName }) {
		t.Errorf("task %s is not registered", payments.NotifyAccountChangedTaskName)
	}
	i := slices.IndexFunc(c.infra.jobs.Jobs(), func(j queue.Job) bool { return j.Name == payments.DigestJobName })
	if i < 0 {
		t.Fatalf("job %s is not scheduled", payments.DigestJobName)
	}
	if j := c.infra.jobs.Jobs()[i]; j.Cron != "30 9 * * 1-5" || j.Queue != queue.Default {
		t.Errorf("job %s = %s on %s, want 30 9 * * 1-5 on default", j.Name, j.Cron, j.Queue)
	}

	// hooks.collectionAccount answers from payments: none until an account
	// is active and the default.
	if c.hooks.collectionAccount == nil {
		t.Fatal("hooks.collectionAccount is not filled")
	}
	if _, found, err := c.hooks.collectionAccount(t.Context()); err != nil || found {
		t.Fatalf("collectionAccount (no account) = %v, %v", found, err)
	}
	mem := c.payments.built.(*payments.MemStore)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	acct, err := mem.InsertAccount(t.Context(), payments.CollectionAccount{ID: matrixCollectionAccount.ID, Label: "Ana",
		LegalPartyID: matrixCollectionAccount.ID, IBAN: matrixCollectionAccount.IBAN, HolderName: matrixCollectionAccount.HolderName,
		BankName: matrixCollectionAccount.BankName, Status: payments.AccountPending, CreatedBy: matrixCollectionAccount.ID, CreatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	by := acct.ID // any user other than the creator
	by[15]++
	acct.Status, acct.ApprovedBy, acct.ApprovedAt, acct.IsDefault = payments.AccountActive, &by, &at, true
	if _, err := mem.UpdateAccount(t.Context(), acct, payments.AccountPending); err != nil {
		t.Fatal(err)
	}
	got, found, err := c.hooks.collectionAccount(t.Context())
	if err != nil || !found || got != matrixCollectionAccount {
		t.Fatalf("collectionAccount = %+v, %v, %v", got, found, err)
	}
}
