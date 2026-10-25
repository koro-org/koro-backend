package payments_test

import (
	"errors"
	"slices"
	"testing"

	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/files"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// TestMatch_FullPayment_OrderPaidAndEntitlementsActive (01 §6.3): a
// transfer of exactly what the order waits for captures one payment, pays
// the order (its platform-native line fulfilled at once: in_fulfilment),
// activates the pending right of Koro's line, writes a released split per
// line, posts payment_received and order_allocated (the payee line) and
// emits payment.captured; the transfer is matched to the order.
func TestMatch_FullPayment_OrderPaidAndEntitlementsActive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{platformLine: true})
	if p.order.KoroCollectAmount != 1_500_000 {
		t.Fatalf("order = %+v", p.order)
	}
	tr := e.transfer(t, acct, 1_500_000, "Ödeme "+p.order.OrderNo, true)
	res, err := e.match(t, tr, p, "")
	if err != nil {
		t.Fatalf("MatchTransfer: %v", err)
	}
	if res.Status != payments.TransferMatched || res.AppliedAmount != 1_500_000 || res.OverpaymentRefundID != nil ||
		res.Payment.Amount != 1_500_000 || res.Payment.Status != payments.PaymentCaptured || res.Payment.Provider != payments.ProviderBankTransfer ||
		!res.Order.Paid || res.Order.Status != orders.StatusInFulfilment || res.Order.DueAmount != 0 || res.Order.PaidAmount != 1_500_000 {
		t.Fatalf("result = %+v", res)
	}
	got, _ := e.store.GetTransfer(t.Context(), tr.ID, false)
	if got.MatchedOrderID == nil || *got.MatchedOrderID != p.order.ID || got.MatchedBy == nil || *got.MatchedBy != id("finance_admin") {
		t.Fatalf("transfer = %+v", got)
	}
	pay, err := e.mod.Payments.Get(t.Context(), res.Payment.PaymentID)
	if err != nil || pay.IncomingTransferID == nil || *pay.IncomingTransferID != tr.ID || pay.OrgID != p.order.OrgID ||
		pay.CapturedBy == nil || *pay.CapturedBy != id("finance_admin") {
		t.Fatalf("payment = %+v, %v", pay, err)
	}
	rights, err := e.entStore.BySource(t.Context(), entitlements.SourceOrderLine, p.lines[1].ID)
	if err != nil || len(rights) != 1 || rights[0].Status != entitlements.StatusActive {
		t.Fatalf("right of the platform line = %+v, %v", rights, err)
	}
	splits, _ := e.store.SplitsOf(t.Context(), pay.ID)
	if len(splits) != 2 {
		t.Fatalf("splits = %+v", splits)
	}
	for _, s := range splits {
		switch s.OrderLineID {
		case p.lines[0].ID:
			if s.SellerAmount != 1_080_000 || s.PlatformAmount != 120_000 || s.Status != payments.SplitReleased {
				t.Errorf("booth split = %+v", s)
			}
		case p.lines[1].ID:
			if s.SellerAmount != 0 || s.PlatformAmount != 300_000 {
				t.Errorf("platform split = %+v", s)
			}
		}
	}
	kinds := map[ledger.Kind]int{}
	for _, tx := range e.journal(t) {
		kinds[tx.Kind]++
	}
	if kinds[ledger.KindPaymentReceived] != 1 || kinds[ledger.KindOrderAllocated] != 1 || len(kinds) != 2 {
		t.Fatalf("journal kinds = %v", kinds)
	}
	evs := e.events(domainevent.TopicPaymentCaptured)
	if len(evs) != 1 {
		t.Fatalf("payment.captured = %d", len(evs))
	}
	pc, _ := domainevent.Decode[domainevent.PaymentCaptured](evs[0])
	if pc.PaymentID != pay.ID || pc.OrderID != p.order.ID || pc.Amount != 1_500_000 || !pc.OrderPaid {
		t.Fatalf("payload = %+v", pc)
	}
	if ev := e.audit.Of(authz.PaymentsTransferMatch); len(ev) != 1 || ev[0].Metadata["order_paid"] != true {
		t.Fatalf("audit = %+v", ev)
	}

	// The notices: the buyer's admins (payment received), the seller's admins (order paid).
	e.deliver(t, domainevent.TopicPaymentCaptured)
	if got := e.sender.to("payments.payment_received", notify.ChannelInApp); !containsAll(got, id("company_admin")) || slices.Contains(got, id("company_member")) {
		t.Errorf("payment_received to %v", got)
	}
	if got := e.sender.to("payments.order_paid", notify.ChannelInApp); !containsAll(got, id("club_admin")) || len(got) != 1 {
		t.Errorf("order_paid to %v", got)
	}
}

// TestMatch_Partial_Accumulates (T-14): a smaller transfer is captured whole
// and applied as a partial payment (order awaiting payment, no splits, no
// allocation); the next one completes the order.
func TestMatch_Partial_Accumulates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	first, err := e.match(t, e.transfer(t, acct, 400_000, p.order.OrderNo, false), p, "")
	if err != nil || first.Order.Paid || first.Order.Status != orders.StatusAwaitingPayment || first.Order.DueAmount != 800_000 {
		t.Fatalf("partial = %+v, %v", first, err)
	}
	if s, _ := e.store.SplitsOf(t.Context(), first.Payment.PaymentID); len(s) != 0 {
		t.Fatalf("splits of a partial payment = %+v", s)
	}
	second, err := e.match(t, e.transfer(t, acct, 800_000, p.order.OrderNo, false), p, "")
	if err != nil || !second.Order.Paid || second.Order.Status != orders.StatusPaid || second.Order.PaidAmount != 1_200_000 {
		t.Fatalf("completing = %+v, %v", second, err)
	}
	if s, _ := e.store.SplitsOf(t.Context(), second.Payment.PaymentID); len(s) != 1 {
		t.Fatalf("splits = %+v", s)
	}
	pc := e.events(domainevent.TopicPaymentCaptured)
	if len(pc) != 2 {
		t.Fatalf("payment.captured = %d", len(pc))
	}
	e.deliver(t, domainevent.TopicPaymentCaptured)
	if got := e.sender.to("payments.order_paid", notify.ChannelInApp); len(got) != 1 {
		t.Errorf("order_paid notices = %v (only the completing payment)", got)
	}
}

// TestMatch_Overpayment_CreatesRefundRecord (T-14, 03 k27): money beyond
// what the order waits for opens a refund request (overpayment, requested,
// bank EFT) to the sender's IBAN, requested by the matcher; without a
// sender IBAN the refund IBAN is required.
func TestMatch_Overpayment_CreatesRefundRecord(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	res, err := e.match(t, e.transfer(t, acct, 1_500_000, p.order.OrderNo, true), p, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.AppliedAmount != 1_200_000 || res.Payment.Amount != 1_500_000 || !res.Order.Paid || res.OverpaymentRefundID == nil {
		t.Fatalf("result = %+v", res)
	}
	rs := e.mem.Refunds()
	if len(rs) != 1 {
		t.Fatalf("refunds = %+v", rs)
	}
	r := rs[0]
	if r.ID != *res.OverpaymentRefundID || r.Amount != 300_000 || r.Reason != payments.RefundReasonOverpayment || r.Status != payments.RefundStatusRequested ||
		r.Method != payments.RefundMethodBankEFT || r.TargetIBAN == nil || *r.TargetIBAN != ibanSender || r.RequestedBy != id("finance_admin") ||
		r.PaymentID != res.Payment.PaymentID || r.OrderID != p.order.ID || r.OrderLineID != nil {
		t.Fatalf("refund = %+v", r)
	}
	if ev := e.audit.Of(authz.PaymentsRefundCreate); len(ev) != 1 || ev[0].Metadata["reason"] != "overpayment" {
		t.Fatalf("refund audit = %+v", ev)
	}

	// No sender IBAN: the refund IBAN is required, nothing is captured without it.
	p2 := e.order(t, orderSpec{})
	tr := e.transfer(t, acct, 1_300_000, p2.order.OrderNo, false)
	if _, err := e.match(t, tr, p2, ""); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("overpayment without an IBAN = %v", err)
	}
	if got, _ := e.store.GetTransfer(t.Context(), tr.ID, false); got.Status != payments.TransferUnmatched {
		t.Fatalf("refused match left %s", got.Status)
	}
	res2, err := e.match(t, tr, p2, "TR68 0006 4000 0000 0009 8765 43")
	if err != nil || res2.OverpaymentRefundID == nil {
		t.Fatalf("with refund_iban: %+v, %v", res2, err)
	}
	if rs := e.mem.Refunds(); len(rs) != 2 || !slices.ContainsFunc(rs, func(r payments.Refund) bool { return *r.TargetIBAN == ibanOther && r.Amount == 100_000 }) {
		t.Fatalf("refunds = %+v", rs)
	}
}

// TestMatch_LedgerBalanced (02 §21.3): every posted transaction sums to
// zero; Koro's bank holds the transfer, the payee is owed its net, Koro
// earned its commission and its VAT and, on its own platform-native line,
// its sale and the sale's VAT (BE-067, T-293: allocated with the booth in
// the same order_allocated), and only the overpayment waits in
// buyer_advances until the refund (BE-043).
func TestMatch_LedgerBalanced(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{platformLine: true})
	if _, err := e.match(t, e.transfer(t, acct, 1_600_000, p.order.OrderNo, true), p, ""); err != nil {
		t.Fatal(err)
	}
	ps := e.journal(t)
	for _, tx := range ps {
		var sum int64
		for _, en := range tx.Entries {
			sum += en.Amount
		}
		if sum != 0 {
			t.Fatalf("transaction %s %s sums to %d", tx.Kind, tx.ID, sum)
		}
	}
	b := balances(ps)
	payee := "seller_payable:" + id("payee_club_a").String()
	want := map[string]int64{
		"koro_bank":                   1_600_000,
		"buyer_advances":              -1_600_000 + 1_200_000 + 300_000, // the booth and Koro's line are allocated; the overpayment waits
		payee:                         -1_080_000,
		"platform_commission_revenue": -100_000,
		"platform_sales_revenue":      -250_000,
		"vat_output":                  -20_000 - 50_000,
	}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, b[k], v, b)
		}
	}
	bal, err := e.ledger.PayeeBalance(t.Context(), id("payee_club_a"))
	if err != nil || bal != 1_080_000 {
		t.Fatalf("payee balance = %d, %v", bal, err)
	}
	if n := len(ps); n != 2 {
		t.Fatalf("%d transactions, want payment_received and one order_allocated for both lines", n)
	}
	if rep, err := e.ledMod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("integrity = %+v, %v", rep.Findings, err)
	}
}

// TestMatch_SameIdempotencyKey_NoDoubleCapture (06 H-5): a retry with the
// same key replays the first capture (one payment, one ledger posting, one
// event); another key on the matched transfer is 409.
func TestMatch_SameIdempotencyKey_NoDoubleCapture(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	tr := e.transfer(t, acct, 1_200_000, p.order.OrderNo, false)
	fin := actor(t, "finance_admin")
	key := func(v string) idempotency.Key {
		return idempotency.Key{Scope: fin.UserID.String() + ":POST /admin/incoming-transfers/{transfer_id}/match", Value: v, Status: 200}
	}
	ctx := idempotency.WithKey(ctxOf(t, fin), key("match-tr-1"))
	first, err := e.matchCtx(ctx, tr, p, "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.matchCtx(ctx, tr, p, "")
	if err != nil || again.Payment.PaymentID != first.Payment.PaymentID || !again.Order.Paid {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if n := len(e.mem.Payments()); n != 1 {
		t.Fatalf("payments = %d", n)
	}
	if n := len(e.journal(t)); n != 2 || len(e.events(domainevent.TopicPaymentCaptured)) != 1 {
		t.Fatalf("journal = %d transactions, events = %d", n, len(e.events(domainevent.TopicPaymentCaptured)))
	}
	if _, err := e.matchCtx(idempotency.WithKey(ctxOf(t, fin), key("match-tr-2")), tr, p, ""); !errors.Is(err, payments.ErrTransferAlreadyMatched) {
		t.Fatalf("other key = %v", err)
	}
	if _, err := e.matchCtx(ctx, tr, p, ibanOther); !errors.Is(err, idempotency.ErrKeyReused) {
		t.Fatalf("same key, other body = %v", err)
	}
}

// TestMatch_Refusals: the reference code must be the order's (422), an
// order that does not wait for payment is 409 orders.invalid_state, an
// unknown order is 422, an ignored transfer 409, the role is fin only
// (403) — and nothing is written in any of these cases.
func TestMatch_Refusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	tr := e.transfer(t, acct, 1_200_000, p.order.OrderNo, false)
	fin := actor(t, "finance_admin")
	ctx := ctxOf(t, fin)
	if _, err := e.svc.MatchTransfer(ctx, fin, tr.ID, payments.MatchInput{OrderID: p.order.ID, ExpectedOrderNo: "KR00000000"}); !errors.Is(err, payments.ErrOrderReferenceMismatch) {
		t.Fatalf("reference mismatch = %v", err)
	}
	if _, err := e.svc.MatchTransfer(ctx, fin, tr.ID, payments.MatchInput{OrderID: id("nope"), ExpectedOrderNo: p.order.OrderNo}); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("unknown order = %v", err)
	}
	if _, err := e.svc.MatchTransfer(ctx, fin, id("nope"), payments.MatchInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo}); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("unknown transfer = %v", err)
	}
	pad := actor(t, "platform_admin")
	if _, err := e.svc.MatchTransfer(ctxOf(t, pad), pad, tr.ID, payments.MatchInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform admin matches = %v", err)
	}
	if _, err := e.match(t, tr, p, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.match(t, e.transfer(t, acct, 1, p.order.OrderNo, false), p, ""); !errors.Is(err, orders.ErrInvalidState) {
		t.Fatalf("paid order = %v", err)
	}
	ig := e.transfer(t, acct, 5, "x", false)
	if _, err := e.svc.IgnoreTransfer(ctx, fin, ig.ID, "faiz"); err != nil {
		t.Fatal(err)
	}
	p2 := e.order(t, orderSpec{})
	if _, err := e.match(t, ig, p2, ""); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("ignored transfer = %v", err)
	}
	if n := len(e.mem.Payments()); n != 1 {
		t.Fatalf("payments = %d, want only the one match", n)
	}
}

// TestConfirmDirectPayment_ModelC (06 H-56, T-18, T-227): with Koro's fee
// in, the confirmation of the buyer's direct payment records a
// direct_confirmed payment of the direct amount, pays the order, attaches
// the evidence, allocates the collected fee and emits payment.captured.
// A model A order is 409.
func TestConfirmDirectPayment_ModelC(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin := actor(t, "finance_admin")
	ctx := ctxOf(t, fin)
	evidence := e.files.add(files.PurposeTransferReceipt)
	a := e.order(t, orderSpec{})
	if _, err := e.svc.ConfirmDirectPayment(ctx, fin, payments.DirectInput{OrderID: a.order.ID, ExpectedOrderNo: a.order.OrderNo,
		EvidenceFileID: evidence}); !errors.Is(err, orders.ErrInvalidState) {
		t.Fatalf("model A = %v", err)
	}
	c := e.order(t, orderSpec{modelC: true, paidFee: true})
	in := payments.DirectInput{OrderID: c.order.ID, ExpectedOrderNo: c.order.OrderNo, EvidenceFileID: evidence}
	res, err := e.svc.ConfirmDirectPayment(ctx, fin, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Payment.Provider != payments.ProviderDirectConfirmed || res.Payment.Amount != 1_200_000 || !res.Order.Paid ||
		res.Order.Status != orders.StatusPaid || res.Order.PaidAmount != 120_000 {
		t.Fatalf("result = %+v", res)
	}
	pay, _ := e.mod.Payments.Get(t.Context(), res.Payment.PaymentID)
	if pay.IncomingTransferID != nil || pay.EvidenceFileID == nil || *pay.EvidenceFileID != evidence || pay.CapturedBy == nil {
		t.Fatalf("payment = %+v", pay)
	}
	if mod, owner := e.files.owner(evidence); mod != "payments" || owner != pay.ID {
		t.Fatalf("evidence attached to %s/%s", mod, owner)
	}
	if s, _ := e.store.SplitsOf(t.Context(), pay.ID); len(s) != 1 || s[0].SellerAmount != 0 || s[0].PlatformAmount != 120_000 {
		t.Fatalf("splits = %+v", s)
	}
	if len(e.journal(t)) != 1 {
		t.Fatal("the completed C order did not allocate its fee")
	}
	if evs := e.events(domainevent.TopicPaymentCaptured); len(evs) != 1 {
		t.Fatalf("payment.captured = %d", len(evs))
	}
	if _, err := e.svc.ConfirmDirectPayment(ctx, fin, in); !errors.Is(err, orders.ErrInvalidState) {
		t.Fatalf("confirm twice = %v", err)
	}
	other := e.files.add(files.PurposeRefundReceipt)
	in.EvidenceFileID = other
	if _, err := e.svc.ConfirmDirectPayment(ctx, fin, in); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("evidence of another purpose = %v", err)
	}
}
