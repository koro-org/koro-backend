package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"koro/internal/billing"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
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

// TestWire_Billing pins what billing registers (BE-042, BE-053): the plan
// consumers of payment.captured, order_line.fulfilled, payout.paid and
// payment.refunded, the invoice e-mail of invoice.issued and the provider
// flow of invoice.requested and invoice.failed (01 §6.2) on the critical
// queue with the money retries, the notice and poll tasks, the KVKK
// provider and the two token columns for crypto.rewrap; without a provider
// (BILLING_INVOICE_PROVIDER=none) no token rotation is scheduled.
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
		domainevent.TopicInvoiceRequested:   billing.IssueInvoiceTaskName,
		domainevent.TopicInvoiceFailed:      billing.NotifyInvoiceFailedTaskName,
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
	for _, name := range []string{billing.NotifyInvoiceTaskName, billing.PollJobsTaskName} {
		if !slices.ContainsFunc(c.infra.jobs.Tasks(), func(task queue.Task) bool { return task.Name == name }) {
			t.Errorf("%s is not registered", name)
		}
	}
	if slices.ContainsFunc(c.infra.jobs.Jobs(), func(j queue.Job) bool { return j.Name == billing.RefreshTokenTaskName }) {
		t.Error("billing.refresh_token is scheduled without a provider")
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

// testParasutConfig is a complete Paraşüt section (test values, no real
// account; nothing is called while building).
func testParasutConfig() config.Billing {
	return config.Billing{InvoiceProvider: config.BillingProviderParasut, Parasut: config.BillingParasut{
		BaseURL: "https://api.parasut.example.test", TokenURL: "https://id.parasut.example.test/token", ClientID: "koro-test-client",
		ClientSecret: config.NewSecret("test-client-secret-not-real"), Series: "KRT", Scenario: "basic", RequestsPer10s: 5,
		Timeout: 10 * time.Second,
	}}
}

// TestWire_BillingInvoiceProviders: the selection map of
// BILLING_INVOICE_PROVIDER has one constructor per accepted name (BE-053,
// the BE-113 rule); none wires nothing, mem the in-memory adapter, parasut
// the Paraşüt adapter with its token rotation (refused without the shared
// envelope); an unknown name names the key. A container configured with
// parasut builds and schedules billing.refresh_token.
func TestWire_BillingInvoiceProviders(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if got, want := invoiceProviders(c, config.Billing{}, billing.Deps{}).names(),
		slices.Sorted(slices.Values(config.BillingInvoiceProviders())); !slices.Equal(got, want) {
		t.Fatalf("BILLING_INVOICE_PROVIDER: wired adapters %v, configuration accepts %v", got, want)
	}
	d := billing.Deps{Store: billing.NewMemStore(clock.Real().Now), Tx: db.NopTx{}, Clock: c.infra.clock, Log: c.infra.log,
		Audit: c.audit.svc, Sealer: testEnvelope()}
	pick := func(cfg config.Billing, d billing.Deps, name string) (invoiceAdapter, error) {
		return invoiceProviders(c, cfg, d).pick("BILLING_INVOICE_PROVIDER", name)
	}
	if a, err := pick(config.Billing{}, d, config.BillingProviderNone); err != nil || a.provider != nil || a.tokens != nil {
		t.Fatalf("none = %+v, %v", a, err)
	}
	if a, err := pick(config.Billing{}, d, config.BillingProviderMem); err != nil || a.tokens != nil {
		t.Fatalf("mem = %+v, %v", a, err)
	} else if _, ok := a.provider.(*billing.FakeProvider); !ok {
		t.Fatalf("mem = %T", a.provider)
	}
	a, err := pick(testParasutConfig(), d, config.BillingProviderParasut)
	if _, ok := a.provider.(*billing.ParasutProvider); err != nil || !ok || a.tokens == nil {
		t.Fatalf("parasut = %T, %v, %v", a.provider, a.tokens, err)
	}
	noSealer := d
	noSealer.Sealer = nil
	if _, err := pick(testParasutConfig(), noSealer, config.BillingProviderParasut); err == nil {
		t.Fatal("parasut without the envelope: no error")
	}
	if _, err := pick(config.Billing{}, d, "nilvera"); err == nil || !strings.Contains(err.Error(), "BILLING_INVOICE_PROVIDER") {
		t.Fatalf("unknown adapter = %v", err)
	}

	pc := newTestContainer(t)
	pc.infra.cfg.Billing = testParasutConfig()
	if err := pc.build(); err != nil {
		t.Fatalf("build with parasut: %v", err)
	}
	if !slices.ContainsFunc(pc.infra.jobs.Jobs(), func(j queue.Job) bool { return j.Name == billing.RefreshTokenTaskName }) {
		t.Fatal("billing.refresh_token is not scheduled with parasut")
	}
}
