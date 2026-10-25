package billing_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/billing"
	"koro/internal/catalog"
	"koro/internal/orders"
	"koro/internal/platform/domainevent"
)

// TestPlan_PaymentCaptured_ModelA_CreatesExpectedInvoices pins the model A
// plan (T-11 default; 02 §18.3, 04 §C.6): a partial payment plans nothing;
// the payment that completes the order plans the on_payment service
// invoices — the payee's to the buyer (seller_uploaded, the club's) and
// Koro's for a platform-native line (manual_upload) — but not the
// on_fulfillment line's, which its order_line.fulfilled plans; payout.paid
// plans Koro's commission invoice to the payee for the paid-out lines.
func TestPlan_PaymentCaptured_ModelA_CreatesExpectedInvoices(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o, ls := e.order(t, orders.ModelA,
		lineSpec{payee: true, trigger: catalog.InvoiceOnPayment, payer: "seller", withheld: 60_000},
		lineSpec{payee: true, trigger: catalog.InvoiceOnFulfillment, payer: "seller"},
		lineSpec{payee: false, trigger: catalog.InvoiceOnPayment},
	)
	seller, later, platform := ls[0], ls[1], ls[2]

	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 600_000, OrderPaid: false})
	if invs := e.invoices(t, billing.Filter{}); len(invs) != 0 {
		t.Fatalf("a partial payment planned %d invoices", len(invs))
	}

	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 600_000, OrderPaid: true})
	invs := e.invoices(t, billing.Filter{OrderID: &o.ID})
	if len(invs) != 2 {
		t.Fatalf("planned %d invoices on payment, want 2: %+v", len(invs), invs)
	}
	byLine := map[uuid.UUID]billing.Invoice{}
	for _, inv := range invs {
		ls := e.lines(t, inv.ID)
		if len(ls) != 1 || ls[0].OrderLineID == nil {
			t.Fatalf("invoice %s lines %+v", inv.ID, ls)
		}
		byLine[*ls[0].OrderLineID] = inv
	}
	club, company := authztest.ID("club_a"), authztest.ID("company_a")

	s := byLine[seller.ID]
	if s.Kind != billing.KindServiceSale || s.Status != billing.StatusPlanned || s.IssuingMode != billing.ModeSellerUploaded ||
		s.Trigger != billing.TriggerOnPayment || s.IssuerPartyID != e.payeeParty || s.RecipientPartyID != e.buyer ||
		s.IssuerOrgID == nil || *s.IssuerOrgID != club || s.RecipientOrgID == nil || *s.RecipientOrgID != company ||
		s.OrderID == nil || *s.OrderID != o.ID || s.PayoutID != nil {
		t.Fatalf("payee's service invoice = %+v", s)
	}
	if s.NetAmount != 1_000_000 || s.VATAmount != 200_000 || s.TotalAmount != 1_200_000 || s.VATWithheldAmount != 60_000 ||
		s.PayableAmount != 1_140_000 || s.Currency != "TRY" {
		t.Fatalf("amounts = net %d vat %d total %d withheld %d payable %d", s.NetAmount, s.VATAmount, s.TotalAmount,
			s.VATWithheldAmount, s.PayableAmount)
	}
	if s.IssuerSnapshot.Title != "Test Vakfı" || s.RecipientSnapshot.Title != "Alıcı Test A.Ş." ||
		s.RecipientSnapshot.Email != "eski@alici.example.test" {
		t.Fatalf("snapshots = %+v / %+v (the buyer's is the order's frozen one)", s.IssuerSnapshot, s.RecipientSnapshot)
	}
	if want := "service_sale:" + o.ID.String() + ":" + seller.ID.String() + ":1"; s.IdempotencyKey != want {
		t.Fatalf("key %q, want %q", s.IdempotencyKey, want)
	}
	k := byLine[platform.ID]
	if k.Kind != billing.KindServiceSale || k.IssuingMode != billing.ModeManualUpload || k.IssuerPartyID != e.koro ||
		k.IssuerOrgID != nil || k.RecipientPartyID != e.buyer {
		t.Fatalf("Koro's service invoice of the platform line = %+v", k)
	}
	if _, ok := byLine[later.ID]; ok {
		t.Fatal("the on_fulfillment line was planned on payment")
	}
	if n := len(e.audit.Of(authz.BillingInvoicePlan)); n != 2 {
		t.Fatalf("plan audit events = %d, want 2", n)
	}
	if ev := e.audit.Of(authz.BillingInvoicePlan)[0]; ev.SystemJob != billing.PlanOnPaymentTaskName || ev.ActorUserID != nil {
		t.Fatalf("plan audit actor = %+v", ev)
	}

	// The on_fulfillment line is planned when it is fulfilled; the payee's
	// club admins hear about it (it uploads), finance about Koro's.
	e.runNotices(t)
	e.orders.update(later.ID, func(l *orders.Line) { l.FulfillmentStatus = orders.FulfillFulfilled })
	e.mustDeliver(t, domainevent.OrderLineFulfilled{OrderLineID: later.ID, OrderID: o.ID, PayeeID: e.payee})
	invs = e.invoices(t, billing.Filter{OrderID: &o.ID})
	if len(invs) != 3 || invs[2].Trigger != billing.TriggerOnFulfillment || invs[2].IssuingMode != billing.ModeSellerUploaded {
		t.Fatalf("after fulfilment: %+v", invs)
	}
	// The on_payment line's fulfilment plans nothing more.
	e.orders.update(seller.ID, func(l *orders.Line) { l.FulfillmentStatus = orders.FulfillFulfilled })
	e.mustDeliver(t, domainevent.OrderLineFulfilled{OrderLineID: seller.ID, OrderID: o.ID, PayeeID: e.payee})
	if n := len(e.invoices(t, billing.Filter{OrderID: &o.ID})); n != 3 {
		t.Fatalf("an on_payment line's fulfilment planned again: %d", n)
	}

	// payout.paid: Koro's commission invoice to the payee for both seller lines.
	payout := uuid.NewV7()
	for _, id := range []uuid.UUID{seller.ID, later.ID} {
		e.orders.update(id, func(l *orders.Line) { l.PayoutID = &payout })
	}
	e.mustDeliver(t, domainevent.PayoutPaid{PayoutID: payout, PayeeID: e.payee, Amount: 2_160_000})
	cs := e.invoices(t, billing.Filter{PayoutID: &payout})
	if len(cs) != 1 {
		t.Fatalf("commission invoices = %d", len(cs))
	}
	c := cs[0]
	if c.Kind != billing.KindCommission || c.IssuingMode != billing.ModeManualUpload || c.Trigger != billing.TriggerOnPayout ||
		c.IssuerPartyID != e.koro || c.RecipientPartyID != e.payeeParty || c.RecipientOrgID == nil || *c.RecipientOrgID != club ||
		c.OrderID != nil || c.NetAmount != 200_000 || c.VATAmount != 40_000 || c.TotalAmount != 240_000 ||
		c.IdempotencyKey != "commission:"+payout.String()+":all:1" {
		t.Fatalf("commission invoice = %+v", c)
	}
	if ls := e.lines(t, c.ID); len(ls) != 2 || ls[0].Position != 1 || ls[1].Position != 2 || ls[0].VATRateBP != 2000 {
		t.Fatalf("commission lines = %+v", ls)
	}

	e.runNotices(t)
	var clubAdmin, fin int
	for _, m := range e.sender.all() {
		if m.Template != "billing.invoice_planned" || m.UserID == nil {
			continue
		}
		switch *m.UserID {
		case authztest.ID("club_admin"):
			clubAdmin++
		case authztest.ID("finance_admin"):
			fin++
		}
	}
	// club_admin: the two payee invoices; finance_admin: Koro's platform-line and commission invoices.
	if clubAdmin != 2 || fin != 2 {
		t.Fatalf("planned notices: club admin %d, finance %d; want 2 and 2", clubAdmin, fin)
	}
}

// TestPlan_ModelCFeeOnly_UndecidedModelsNothing: model C plans only Koro's
// fee invoice to the buyer (orchestrator note, 2026-10-14); model A with
// the buyer paying the commission plans the fee invoice next to the
// service invoice; A2 and B plan nothing until T-11 is decided.
func TestPlan_ModelCFeeOnly_UndecidedModelsNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c, _ := e.order(t, orders.ModelC, lineSpec{payee: true, trigger: catalog.InvoiceOnPayment, payer: "buyer"},
		lineSpec{payee: true, trigger: catalog.InvoiceOnFulfillment, payer: "buyer"})
	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: c.ID, Amount: 240_000, OrderPaid: false})
	if got := e.invoices(t, billing.Filter{OrderID: &c.ID}); len(got) != 0 {
		t.Fatalf("partial C invoice plan = %+v", got)
	}
	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: c.ID, Amount: 240_000, OrderPaid: true})
	invs := e.invoices(t, billing.Filter{OrderID: &c.ID})
	if len(invs) != 1 || invs[0].Kind != billing.KindPlatformFee || invs[0].IssuerPartyID != e.koro ||
		invs[0].RecipientPartyID != e.buyer || invs[0].NetAmount != 200_000 || invs[0].TotalAmount != 240_000 {
		t.Fatalf("model C plan = %+v", invs)
	}
	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: c.ID, Amount: 2_400_000, OrderPaid: true})
	if got := e.invoices(t, billing.Filter{OrderID: &c.ID}); len(got) != 1 {
		t.Fatalf("replayed C invoice plan = %+v", got)
	}
	if ls := e.lines(t, invs[0].ID); len(ls) != 2 {
		t.Fatalf("fee lines = %d", len(ls))
	}

	a, _ := e.order(t, orders.ModelA, lineSpec{payee: true, trigger: catalog.InvoiceOnPayment, payer: "buyer"})
	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: a.ID, Amount: 1_320_000, OrderPaid: true})
	kinds := map[billing.Kind]int{}
	for _, inv := range e.invoices(t, billing.Filter{OrderID: &a.ID}) {
		kinds[inv.Kind]++
	}
	if kinds[billing.KindServiceSale] != 1 || kinds[billing.KindPlatformFee] != 1 || len(kinds) != 2 {
		t.Fatalf("model A, buyer pays the commission: %v", kinds)
	}

	for _, m := range []string{orders.ModelA2, orders.ModelB} {
		o, ls := e.order(t, m, lineSpec{payee: true, trigger: catalog.InvoiceOnPayment, payer: "seller", fulfil: true})
		e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 1_200_000, OrderPaid: true})
		e.mustDeliver(t, domainevent.OrderLineFulfilled{OrderLineID: ls[0].ID, OrderID: o.ID, PayeeID: e.payee})
		if invs := e.invoices(t, billing.Filter{OrderID: &o.ID}); len(invs) != 0 {
			t.Fatalf("model %s planned %d invoices before T-11", m, len(invs))
		}
	}
}

// TestPlan_DuplicateEvent_NoDuplicateInvoice: the same event delivered
// again (asynq at least once) and another event of the same business fact
// plan nothing twice: the idempotency key is unique.
func TestPlan_DuplicateEvent_NoDuplicateInvoice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o, ls := e.order(t, orders.ModelA, lineSpec{payee: true, trigger: catalog.InvoiceOnPayment, payer: "buyer", fulfil: true})
	ev, err := domainevent.New(domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 1_320_000, OrderPaid: true},
		"payment", uuid.NewV7())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := e.deliverEnv(t, ev); err != nil {
			t.Fatal(err)
		}
	}
	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 1, OrderPaid: true})
	invs := e.invoices(t, billing.Filter{OrderID: &o.ID})
	if len(invs) != 2 {
		t.Fatalf("invoices after duplicates = %d, want 2 (service + fee)", len(invs))
	}
	if n := len(e.audit.Of(authz.BillingInvoicePlan)); n != 2 {
		t.Fatalf("plan audit events = %d, want 2", n)
	}
	for _, inv := range invs {
		if n := len(e.lines(t, inv.ID)); n != 1 {
			t.Fatalf("invoice %s has %d lines", inv.Kind, n)
		}
	}
	// A late order_line.fulfilled of the on_payment line plans nothing.
	e.mustDeliver(t, domainevent.OrderLineFulfilled{OrderLineID: ls[0].ID, OrderID: o.ID, PayeeID: e.payee})
	payout := uuid.NewV7()
	e.orders.update(ls[0].ID, func(l *orders.Line) { l.PayoutID = &payout })
	for range 2 {
		e.mustDeliver(t, domainevent.PayoutPaid{PayoutID: payout, PayeeID: e.payee, Amount: 1_000_000})
	}
	if n := len(e.invoices(t, billing.Filter{})); n != 2 {
		t.Fatalf("invoices = %d (the buyer pays the commission: no commission invoice)", n)
	}
	// A stale event (unknown order) is skipped, not retried.
	e.mustDeliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: uuid.NewV7(), Amount: 1, OrderPaid: true})
}

// TestPlan_NoPlatformParty_Retries: Koro's invoices need Koro's legal party
// (bootstrap-commerce); without it the plan fails (retried) and nothing of
// the event is stored half.
func TestPlan_NoPlatformParty_Retries(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.parties.mu.Lock()
	e.parties.platform = nil
	e.parties.mu.Unlock()
	o, _ := e.order(t, orders.ModelA, lineSpec{payee: false, trigger: catalog.InvoiceOnPayment})
	if err := e.deliver(t, domainevent.PaymentCaptured{PaymentID: uuid.NewV7(), OrderID: o.ID, Amount: 1, OrderPaid: true}); err == nil {
		t.Fatal("the plan without Koro's party succeeded")
	}
}
