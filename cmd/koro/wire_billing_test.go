package main

import (
	"slices"
	"testing"

	"koro/internal/billing"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
)

// useMemStore installs the in-memory billing store used by service,
// wiring and authz matrix tests (05 §13.2) and the test envelope (the
// integration tokens). First owner: BE-042.
// Only tickets of the billing module edit this file (BE-001 R4).
func (w *billingWiring) useMemStore() {
	w.store = billing.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
	w.sealer = testEnvelope()
}

// TestWire_Billing pins what billing registers (BE-042): the plan
// consumers of payment.captured, order_line.fulfilled, payout.paid and
// payment.refunded and the invoice e-mail of invoice.issued (01 §6.2) on
// the critical queue with the money retries, the notice task, the KVKK
// provider and the two token columns for crypto.rewrap.
func TestWire_Billing(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	for topic, name := range map[string]string{
		domainevent.TopicPaymentCaptured:    billing.PlanOnPaymentTaskName,
		domainevent.TopicOrderLineFulfilled: billing.PlanOnFulfillmentTaskName,
		domainevent.TopicPayoutPaid:         billing.PlanOnPayoutTaskName,
		domainevent.TopicPaymentRefunded:    billing.PlanOnRefundTaskName,
		domainevent.TopicInvoiceIssued:      billing.SendInvoiceTaskName,
	} {
		i := slices.IndexFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name })
		if i < 0 {
			t.Errorf("%s has no %s subscription", topic, name)
			continue
		}
		if s := c.infra.subs.For(topic)[i]; s.Queue != "critical" || s.MaxRetry != 25 {
			t.Errorf("%s: queue %q, retries %d; want critical, 25", name, s.Queue, s.MaxRetry)
		}
	}
	found := false
	for _, task := range c.infra.jobs.Tasks() {
		found = found || task.Name == billing.NotifyInvoiceTaskName
	}
	if !found {
		t.Error("billing.notify_invoice is not registered")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "billing" }) {
		t.Error("the billing KVKK provider is not registered")
	}
	var cols []string
	for _, col := range c.keys.columns {
		if col.Table == "integration_credentials" {
			cols = append(cols, col.Column)
		}
	}
	if want := []string{"access_token_enc", "refresh_token_enc"}; !slices.Equal(cols, want) {
		t.Errorf("rewrap columns %v, want %v", cols, want)
	}
	if c.billing.svc == nil {
		t.Fatal("billing service not built")
	}
}
