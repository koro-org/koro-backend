package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/billing"
	"koro/internal/catalog"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// scriptedProvider is FakeProvider with scripted failures.
type scriptedProvider struct {
	*billing.FakeProvider
	draftErr, issueErr error
	poll               *billing.JobStatus
	issues             int
}

func (p *scriptedProvider) CreateDraft(ctx context.Context, in billing.DraftInvoice) (billing.ProviderRef, error) {
	if p.draftErr != nil {
		return billing.ProviderRef{}, p.draftErr
	}
	return p.FakeProvider.CreateDraft(ctx, in)
}

func (p *scriptedProvider) Issue(ctx context.Context, ref billing.ProviderRef, doc billing.DocumentType) (string, error) {
	p.issues++
	if p.issueErr != nil {
		err := p.issueErr
		p.issueErr = nil // fails once
		return "", err
	}
	return p.FakeProvider.Issue(ctx, ref, doc)
}

func (p *scriptedProvider) PollJob(ctx context.Context, job string) (billing.JobStatus, error) {
	if p.poll != nil {
		return *p.poll, nil
	}
	return p.FakeProvider.PollJob(ctx, job)
}

// newProviderEnv is an env whose policies say invoice_automation
// 'provider' with p wired.
func newProviderEnv(t *testing.T, p billing.InvoiceProvider) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	return newEnvWith(t, clk, billing.NewMemStore(clk.Now), db.NopTx{}, func(e *env) {
		e.provider, e.terms.automation = p, catalog.InvoiceProvider
	})
}

// koroInvoice plans Koro's own service invoice of a paid platform-native
// line (on payment) and returns it.
func (e *env) koroInvoice(t *testing.T) billing.Invoice {
	t.Helper()
	o, _ := e.order(t, orders.ModelA, lineSpec{trigger: catalog.InvoiceOnPayment, payer: "seller"})
	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 1_200_000, OrderPaid: true})
	invs := e.invoices(t, billing.Filter{OrderID: &o.ID})
	if len(invs) != 1 || invs[0].IssuerPartyID != e.koro {
		t.Fatalf("planned = %+v", invs)
	}
	return invs[0]
}

// eventOf returns the last committed event of the topic about the invoice.
func (e *env) eventOf(t *testing.T, topic string, invoice uuid.UUID) (domainevent.Envelope, bool) {
	t.Helper()
	evs := e.outbox.Events()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Topic == topic && evs[i].AggregateID == invoice {
			return evs[i], true
		}
	}
	return domainevent.Envelope{}, false
}

// request delivers the invoice.requested event of an invoice.
func (e *env) request(t *testing.T, inv billing.Invoice) error {
	t.Helper()
	ev, ok := e.eventOf(t, domainevent.TopicInvoiceRequested, inv.ID)
	if !ok {
		t.Fatalf("no invoice.requested for %s (topics %v)", inv.ID, e.outbox.Topics())
	}
	return e.deliverEnv(t, ev)
}

// lastPoll returns the last enqueued billing.poll_jobs task.
func (e *env) lastPoll(t *testing.T) queue.Enqueued {
	t.Helper()
	var last *queue.Enqueued
	for _, task := range e.queue.Tasks() {
		if task.Type == billing.PollJobsTaskName {
			task := task
			last = &task
		}
	}
	if last == nil {
		t.Fatal("no poll enqueued")
	}
	return *last
}

// runPoll runs a poll task.
func (e *env) runPoll(t *testing.T, task queue.Enqueued) error {
	t.Helper()
	for _, h := range e.mod.Tasks() {
		if h.Name == billing.PollJobsTaskName {
			return h.Handle(t.Context(), task.Payload)
		}
	}
	t.Fatal("billing.poll_jobs is not registered")
	return nil
}

// TestProviderFlow_IssuesKoroInvoiceThroughProvider: Koro's invoice under
// invoice_automation 'provider' is queued with the next reserved number
// (no upload notice), drafted, made provider_pending, issued, polled with
// a growing delay and issued with the provider's number, ETTN, document
// type, e-document and PDF (stored and attached) — invoice.issued sends it
// on like any invoice; the recipient's contact is mapped once.
func TestProviderFlow_IssuesKoroInvoiceThroughProvider(t *testing.T) {
	t.Parallel()
	fake := billing.NewFakeProvider()
	fake.PendingPolls = 1
	e := newProviderEnv(t, fake)
	inv := e.koroInvoice(t)
	if inv.Status != billing.StatusQueued || inv.IssuingMode != billing.ModeProvider || inv.Provider != billing.ProviderParasut ||
		inv.ReservedSeries == nil || *inv.ReservedSeries != "KRO" || inv.ReservedNo == nil || *inv.ReservedNo != 1 {
		t.Fatalf("planned = %+v", inv)
	}
	for _, task := range e.queue.Tasks() {
		if task.Type == billing.NotifyInvoiceTaskName {
			t.Fatalf("a provider invoice was announced for upload: %+v", task)
		}
	}
	if err := e.request(t, inv); err != nil {
		t.Fatalf("issue: %v", err)
	}
	cur := e.invoice(t, inv.ID)
	if cur.Status != billing.StatusProviderPending || cur.ProviderContactID == nil || cur.ProviderInvoiceID == nil ||
		cur.ProviderJobID == nil || cur.IssueDate == nil || cur.IssueDate.String() != "2026-10-14" {
		t.Fatalf("after the request = %+v", cur)
	}
	first := e.lastPoll(t)
	if first.ProcessIn != 30*time.Second || first.Queue != queue.Default {
		t.Fatalf("first poll = %+v", first)
	}
	if err := e.runPoll(t, first); err != nil { // pending
		t.Fatal(err)
	}
	second := e.lastPoll(t)
	if second.ProcessIn != time.Minute || e.invoice(t, inv.ID).Attempts != 1 {
		t.Fatalf("second poll = %+v, attempts %d", second, e.invoice(t, inv.ID).Attempts)
	}
	if err := e.runPoll(t, second); err != nil { // done
		t.Fatal(err)
	}
	got := e.invoice(t, inv.ID)
	if got.Status != billing.StatusIssued || got.InvoiceNo == nil || !strings.HasPrefix(*got.InvoiceNo, "KRO2026") ||
		got.ETTN == nil || got.DocumentType != billing.DocEArsiv || got.PDFFileID == nil || len(got.PDFSHA256) != 32 ||
		got.IssuedAt == nil || got.ProviderEDocID == nil || got.LastError != nil {
		t.Fatalf("issued = %+v", got)
	}
	if owner := e.files.attached[*got.PDFFileID]; owner.Module != "billing" || owner.EntityID != got.ID {
		t.Fatalf("PDF owner = %+v", owner)
	}
	if _, ok := e.eventOf(t, domainevent.TopicInvoiceIssued, got.ID); !ok {
		t.Fatal("no invoice.issued")
	}
	if n := len(e.audit.Of("billing.invoice.provider_issue")); n != 1 {
		t.Fatalf("provider_issue audit = %d", n)
	}
	// A repeated poll or request changes nothing.
	if err := e.runPoll(t, second); err != nil {
		t.Fatal(err)
	}
	if err := e.request(t, inv); err != nil || e.invoice(t, inv.ID).Status != billing.StatusIssued {
		t.Fatalf("repeated request: %v", err)
	}
	// The next invoice takes the next number and the mapped contact.
	next := e.koroInvoice(t)
	if next.ReservedNo == nil || *next.ReservedNo != 2 {
		t.Fatalf("next reserved number = %v", next.ReservedNo)
	}
	if err := e.request(t, next); err != nil {
		t.Fatal(err)
	}
	if c := e.invoice(t, next.ID).ProviderContactID; c == nil || *c != *cur.ProviderContactID {
		t.Fatalf("contact of the next invoice = %v, want the mapped %s", c, *cur.ProviderContactID)
	}
}

// TestProviderFlow_PlanStaysUpload: without a provider, for the payee's
// own invoice and for a line with VAT withholding, the invoice is an
// upload as before.
func TestProviderFlow_PlanStaysUpload(t *testing.T) {
	t.Parallel()
	t.Run("no provider wired", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.terms.automation = catalog.InvoiceProvider
		if inv := e.koroInvoice(t); inv.IssuingMode != billing.ModeManualUpload || inv.Status != billing.StatusPlanned {
			t.Fatalf("planned = %+v", inv)
		}
	})
	t.Run("the payee's invoice", func(t *testing.T) {
		t.Parallel()
		e := newProviderEnv(t, billing.NewFakeProvider())
		if inv := e.planned(t); inv.IssuingMode != billing.ModeSellerUploaded || inv.Status != billing.StatusPlanned {
			t.Fatalf("planned = %+v", inv)
		}
	})
	t.Run("a withheld line", func(t *testing.T) {
		t.Parallel()
		e := newProviderEnv(t, billing.NewFakeProvider())
		o, _ := e.order(t, orders.ModelA, lineSpec{trigger: catalog.InvoiceOnPayment, payer: "seller", withheld: 60_000})
		e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 1_140_000, OrderPaid: true})
		invs := e.invoices(t, billing.Filter{OrderID: &o.ID})
		if len(invs) != 1 || invs[0].IssuingMode != billing.ModeManualUpload {
			t.Fatalf("planned = %+v", invs)
		}
	})
	t.Run("Koro's platform fee", func(t *testing.T) {
		t.Parallel()
		e := newProviderEnv(t, billing.NewFakeProvider())
		o, _ := e.order(t, orders.ModelA, lineSpec{payee: true, trigger: catalog.InvoiceOnFulfillment, payer: "buyer"})
		e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 1_320_000, OrderPaid: true})
		fee := e.invoices(t, billing.Filter{OrderID: &o.ID, Kind: billing.KindPlatformFee})
		if len(fee) != 1 || fee[0].IssuingMode != billing.ModeProvider || fee[0].Status != billing.StatusQueued {
			t.Fatalf("platform fee = %+v", fee)
		}
	})
}

// TestProviderFlow_RefusalFailsAndTellsFinance: a refusal of the provider
// ends the invoice failed with its message, audited, and finance is told
// in-app (invoice.failed, A); the task itself succeeds (no retry).
func TestProviderFlow_RefusalFailsAndTellsFinance(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{FakeProvider: billing.NewFakeProvider(),
		draftErr: errors.New("billing parasut: sales_invoices.create: HTTP 422: Invalid (/data/attributes/tax_number)")}
	p.draftErr = errors.Join(billing.ErrProviderInvalid, p.draftErr)
	e := newProviderEnv(t, p)
	inv := e.koroInvoice(t)
	if err := e.request(t, inv); err != nil {
		t.Fatalf("request = %v, want nil (failed, not retried)", err)
	}
	got := e.invoice(t, inv.ID)
	if got.Status != billing.StatusFailed || got.LastError == nil || !strings.Contains(*got.LastError, "tax_number") {
		t.Fatalf("failed = %+v", got)
	}
	ev, ok := e.eventOf(t, domainevent.TopicInvoiceFailed, inv.ID)
	if !ok {
		t.Fatal("no invoice.failed")
	}
	if len(e.audit.Of("billing.invoice.provider_fail")) != 1 {
		t.Fatal("no provider_fail audit")
	}
	if err := e.deliverEnv(t, ev); err != nil {
		t.Fatal(err)
	}
	var to []uuid.UUID
	for _, m := range e.sender.all() {
		if m.Template == "billing.invoice_failed" && m.Channel == notify.ChannelInApp {
			to = append(to, *m.UserID)
		}
	}
	if len(to) != 2 {
		t.Fatalf("finance notices = %v, want both finance admins", to)
	}
	// A failed invoice may be cancelled.
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, inv.ID, "Sağlayıcı reddetti"); err != nil {
		t.Fatalf("cancel failed invoice: %v", err)
	}
}

// TestProviderFlow_TransientErrorRetries: a transient error of the issue is
// returned (asynq retries); the retry issues once, from provider_pending.
func TestProviderFlow_TransientErrorRetries(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{FakeProvider: billing.NewFakeProvider(), issueErr: errors.New("billing parasut: e_archives.create: HTTP 503")}
	e := newProviderEnv(t, p)
	inv := e.koroInvoice(t)
	if err := e.request(t, inv); err == nil {
		t.Fatal("transient error swallowed")
	}
	if got := e.invoice(t, inv.ID); got.Status != billing.StatusProviderPending || got.ProviderJobID != nil {
		t.Fatalf("after a transient error = %+v", got)
	}
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, inv.ID, "Vazgeçildi"); err == nil {
		t.Fatal("a provider_pending invoice was cancelled (the issue may have started)")
	}
	if err := e.request(t, inv); err != nil {
		t.Fatal(err)
	}
	if got := e.invoice(t, inv.ID); got.ProviderJobID == nil || p.issues != 2 {
		t.Fatalf("after the retry = %+v, issues %d", got, p.issues)
	}
}

// TestProviderFlow_CancelledWhileQueuedStops: an invoice cancelled while
// queued is left alone by the flow.
func TestProviderFlow_CancelledWhileQueuedStops(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{FakeProvider: billing.NewFakeProvider()}
	e := newProviderEnv(t, p)
	inv := e.koroInvoice(t)
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminCancelInvoice(ctxOf(t, fin), fin, inv.ID, "Vazgeçildi"); err != nil {
		t.Fatal(err)
	}
	if err := e.request(t, inv); err != nil || p.issues != 0 || e.invoice(t, inv.ID).Status != billing.StatusCancelled {
		t.Fatalf("request after cancel = %v, issues %d", err, p.issues)
	}
}

// TestPollJobs_BackoffMax24h: a job that stays pending is polled with a
// delay doubling from 30 s to at most 2 h; the delays never add up to more
// than 24 hours, after which the invoice fails (invoice.failed) instead of
// another poll.
func TestPollJobs_BackoffMax24h(t *testing.T) {
	t.Parallel()
	fake := billing.NewFakeProvider()
	fake.PendingPolls = 1 << 20
	e := newProviderEnv(t, fake)
	inv := e.koroInvoice(t)
	if err := e.request(t, inv); err != nil {
		t.Fatal(err)
	}
	var (
		delays []time.Duration
		total  time.Duration
		seen   = map[string]bool{}
	)
	for range 100 {
		task := e.lastPoll(t)
		if seen[task.ID] {
			break // no new poll: the flow stopped
		}
		seen[task.ID] = true
		delays = append(delays, task.ProcessIn)
		total += task.ProcessIn
		if task.MaxRetry != queue.MoneyMaxRetry {
			t.Fatalf("poll retries = %d", task.MaxRetry)
		}
		if err := e.runPoll(t, task); err != nil {
			t.Fatalf("poll %d: %v", len(delays), err)
		}
	}
	if delays[0] != 30*time.Second {
		t.Fatalf("first delay = %s", delays[0])
	}
	for i := 1; i < len(delays); i++ {
		want := min(2*delays[i-1], 2*time.Hour)
		if delays[i] != want {
			t.Fatalf("delay %d = %s after %s, want %s (doubling, capped at 2 h)", i, delays[i], delays[i-1], want)
		}
	}
	if total > 24*time.Hour || total+2*time.Hour <= 24*time.Hour {
		t.Fatalf("polls waited %s in %d polls; want at most 24 h and no room for another 2 h poll", total, len(delays))
	}
	got := e.invoice(t, inv.ID)
	if got.Status != billing.StatusFailed || got.LastError == nil || !strings.Contains(*got.LastError, "24 hours") {
		t.Fatalf("after the polls = %+v", got)
	}
	ev, ok := e.eventOf(t, domainevent.TopicInvoiceFailed, inv.ID)
	if !ok {
		t.Fatal("no invoice.failed")
	}
	var p domainevent.InvoiceFailed
	if err := json.Unmarshal(ev.Payload, &p); err != nil || p.Attempts != len(delays)-1 {
		t.Fatalf("invoice.failed = %+v, %v; want %d attempts", p, err, len(delays)-1)
	}
}

// TestProviderFlow_FailedJobFailsInvoice: a job the provider reports
// failed ends the invoice failed with the provider's message.
func TestProviderFlow_FailedJobFailsInvoice(t *testing.T) {
	t.Parallel()
	p := &scriptedProvider{FakeProvider: billing.NewFakeProvider(),
		poll: &billing.JobStatus{State: billing.JobFailed, Error: "GİB reddetti"}}
	e := newProviderEnv(t, p)
	inv := e.koroInvoice(t)
	if err := e.request(t, inv); err != nil {
		t.Fatal(err)
	}
	if err := e.runPoll(t, e.lastPoll(t)); err != nil {
		t.Fatal(err)
	}
	if got := e.invoice(t, inv.ID); got.Status != billing.StatusFailed || got.LastError == nil || *got.LastError != "GİB reddetti" {
		t.Fatalf("after a failed job = %+v", got)
	}
	// A poll payload without an invoice is permanent.
	if err := e.runPoll(t, queue.Enqueued{Payload: []byte(`{}`)}); !queue.IsPermanent(err) {
		t.Fatalf("empty poll payload = %v", err)
	}
}
