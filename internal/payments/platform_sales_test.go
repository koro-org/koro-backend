package payments_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"uuid"

	"koro/internal/ledger"
	"koro/internal/payments"
)

// legacyPoster posts as the capture did before BE-067 while on is set:
// order_allocated without Koro's own platform-native lines (their money
// stayed in buyer_advances, T-293), none at all for an order of Koro's
// lines only. It builds the "paid before the template" orders of dev and
// staging through the real chain.
type legacyPoster struct {
	next payments.Poster
	on   *atomic.Bool
}

func (p legacyPoster) Post(ctx context.Context, t ledger.Txn) (uuid.UUID, error) {
	if !p.on.Load() || t.Kind != ledger.KindOrderAllocated {
		return p.next.Post(ctx, t)
	}
	payeeLines := map[uuid.UUID]bool{}
	for _, e := range t.Entries {
		if e.Account == ledger.AccountSellerPayable {
			payeeLines[*e.OrderLineID] = true
		}
	}
	var kept []ledger.Entry
	for _, e := range t.Entries {
		if payeeLines[*e.OrderLineID] {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return uuid.Nil(), nil
	}
	t.Entries = kept
	return p.next.Post(ctx, t)
}

// withLegacy installs legacyPoster; the returned switch starts on.
func withLegacy() (envOption, *atomic.Bool) {
	on := &atomic.Bool{}
	on.Store(true)
	return withPoster(func(p payments.Poster) payments.Poster { return legacyPoster{next: p, on: on} }), on
}

// lineEntries sums the journal's entries naming line, per account.
func lineEntries(ps []ledger.Posted, line uuid.UUID) map[ledger.Account]int64 {
	out := map[ledger.Account]int64{}
	for _, p := range ps {
		for _, en := range p.Entries {
			if en.OrderLineID != nil && *en.OrderLineID == line {
				out[en.Account] += en.Amount
			}
		}
	}
	return out
}

// lateRecognitions counts the late recognitions of the journal (BE-067).
func lateRecognitions(ps []ledger.Posted) int {
	n := 0
	for _, p := range ps {
		if p.Kind == ledger.KindAdjustment && p.Memo != nil && *p.Memo == ledger.LateRecognitionMemo {
			n++
		}
	}
	return n
}

// TestMatch_PlatformOnlyOrder_AllocatesKoroSale (BE-067, T-293): an order of
// Koro's own line alone, its VAT withheld 5/10 by the buyer, is allocated
// at payment: platform_sales_revenue −250,000, vat_output −50,000 and
// +25,000 (the withholding, its own entry), buyer_advances back to zero;
// the split is Koro's alone and the integrity check is clean.
func TestMatch_PlatformOnlyOrder_AllocatesKoroSale(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{platformOnly: true, withheld: true})
	if len(p.lines) != 1 || p.order.PayableAmount != 275_000 {
		t.Fatalf("order = %+v", p.order)
	}
	res, err := e.match(t, e.transfer(t, acct, 275_000, p.order.OrderNo, false), p, "")
	if err != nil || !res.Order.Paid {
		t.Fatalf("match = %+v, %v", res, err)
	}
	ps := e.journal(t)
	b := balances(ps)
	if len(ps) != 2 || b["buyer_advances"] != 0 || b["koro_bank"] != 275_000 || b["platform_sales_revenue"] != -250_000 || b["vat_output"] != -25_000 {
		t.Fatalf("%d transactions, balances %v", len(ps), b)
	}
	var vat []int64
	for _, tx := range ps {
		if tx.Kind != ledger.KindOrderAllocated {
			continue
		}
		for _, en := range tx.Entries {
			if en.Account == ledger.AccountVATOutput {
				vat = append(vat, en.Amount)
			}
		}
	}
	if len(vat) != 2 || vat[0]+vat[1] != -25_000 || min(vat[0], vat[1]) != -50_000 {
		t.Fatalf("VAT entries %v, want −50,000 and the withholding +25,000 apart", vat)
	}
	splits, err := e.store.SplitsOf(t.Context(), res.Payment.PaymentID)
	if err != nil || len(splits) != 1 || splits[0].PlatformAmount != 275_000 || splits[0].SellerAmount != 0 {
		t.Fatalf("splits = %+v, %v", splits, err)
	}
	if rep, err := e.ledMod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("integrity = %+v, %v", rep.Findings, err)
	}
}

// TestRefund_PlatformLine_TwoParts_ExactShares (BE-067): two partial
// refunds of Koro's own line (VAT withheld) take back its sale and VAT in
// proportion to what is left of them (BE-043's RefundedShares, Koro's
// sale as a share): the second takes the exact remainder, so the line's
// revenue and VAT end at zero and buyer_advances is never touched.
func TestRefund_PlatformLine_TwoParts_ExactShares(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true, withheld: true}, 0)
	line := po.lines[1].ID
	var firstID uuid.UUID
	for i, amount := range []int64{100_001, 174_999} {
		r := e.approved(t, lineRefund(po.payment.ID, line, amount))
		res, err := e.completeAs(t, "finance_admin", r.ID)
		if err != nil || res.Unallocated != 0 {
			t.Fatalf("refund %d = %+v, %v", i, res, err)
		}
		if i == 0 {
			firstID = res.TransactionID
		}
	}
	refunds := e.refundTxns(t)
	if len(refunds) != 2 {
		t.Fatalf("refund_completed = %d", len(refunds))
	}
	// 100,001 over (250,000 sale, 25,000 VAT kept): 90,910 + 9,091.
	var first map[ledger.Account]int64
	for _, p := range refunds {
		if p.ID == firstID {
			first = lineEntries([]ledger.Posted{p}, line)
		}
	}
	if first[ledger.AccountSalesRevenue] != 90_910 || first[ledger.AccountVATOutput] != 9_091 {
		t.Fatalf("first refund = %v", first)
	}
	all := lineEntries(e.journal(t), line)
	for _, a := range []ledger.Account{ledger.AccountSalesRevenue, ledger.AccountVATOutput, ledger.AccountCommissionRevenue} {
		if all[a] != 0 {
			t.Fatalf("%s of the refunded line = %d (all %v)", a, all[a], all)
		}
	}
	b := balances(e.journal(t))
	if b["buyer_advances"] != 0 || b["koro_bank"] != 1_200_000 || b["platform_sales_revenue"] != 0 {
		t.Fatalf("balances %v", b)
	}
	if rep, err := e.ledMod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("integrity = %+v, %v", rep.Findings, err)
	}
}

// TestRefund_PlatformLine_PaidBeforeTemplate_RecognisedInRefundTx (BE-067):
// a line of Koro's paid before the template (its money in buyer_advances)
// is recognised in the refund's own transaction — the same idempotent
// adjustment ledger.recognize_platform_sales posts — then reversed; the
// next refund of the line and the job find it recognised and post no
// second recognition.
func TestRefund_PlatformLine_PaidBeforeTemplate_RecognisedInRefundTx(t *testing.T) {
	t.Parallel()
	legacy, on := withLegacy()
	e := newEnv(t, legacy)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	on.Store(false)
	line := po.lines[1].ID
	if b := balances(e.journal(t)); b["buyer_advances"] != -300_000 || b["platform_sales_revenue"] != 0 {
		t.Fatalf("the legacy state = %v", b)
	}
	if rep, err := e.ledMod.CheckIntegrity(t.Context()); err != nil || len(rep.Findings) != 1 ||
		rep.Findings[0].Kind != ledger.FindingUnallocatedLine || *rep.Findings[0].OrderLineID != line {
		t.Fatalf("integrity before = %+v, %v", rep.Findings, err)
	}
	r := e.approved(t, lineRefund(po.payment.ID, line, 120_000))
	if res, err := e.completeAs(t, "finance_admin", r.ID); err != nil || res.Unallocated != 0 {
		t.Fatalf("complete = %+v, %v", res, err)
	}
	ps := e.journal(t)
	if n := lateRecognitions(ps); n != 1 {
		t.Fatalf("late recognitions = %d", n)
	}
	if got := lineEntries(ps, line); got[ledger.AccountBuyerAdvances] != 300_000 || got[ledger.AccountSalesRevenue] != -250_000+100_000 ||
		got[ledger.AccountVATOutput] != -50_000+20_000 {
		t.Fatalf("line entries %v", got)
	}
	r = e.approved(t, lineRefund(po.payment.ID, line, 30_000))
	if _, err := e.completeAs(t, "finance_admin", r.ID); err != nil {
		t.Fatal(err)
	}
	rep, err := e.ledMod.RecognizePlatformSales(t.Context())
	if err != nil || len(rep.Recognized) != 0 || len(rep.Skipped) != 0 {
		t.Fatalf("job after the refund = %+v, %v", rep, err)
	}
	if n := lateRecognitions(e.journal(t)); n != 1 {
		t.Fatalf("late recognitions after the second refund and the job = %d", n)
	}
	if b := balances(e.journal(t)); b["buyer_advances"] != 0 {
		t.Fatalf("buyer_advances = %d", b["buyer_advances"])
	}
	if rep, err := e.ledMod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("integrity after = %+v, %v", rep.Findings, err)
	}
}

// TestRefund_PlatformLine_RefundedBeforeRecognition_409 (BE-067): a line of
// Koro's refunded from buyer_advances before its sale was recognised (the
// BE-043 interim path) cannot be recognised in full: its next refund is 409
// payments.invalid_state and rolls back, the job skips it and the integrity
// check keeps reporting it for finance.
func TestRefund_PlatformLine_RefundedBeforeRecognition_409(t *testing.T) {
	t.Parallel()
	legacy, on := withLegacy()
	e := newEnv(t, legacy)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	on.Store(false)
	line := po.lines[1].ID
	// The interim refund: the order recorded it, the ledger took it from buyer_advances.
	if err := e.ordersM.Orders.ApplyRefund(t.Context(), po.order.ID, &line, 50_000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ledger.Post(t.Context(), txnOf(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 50_000, nil, t0))); err != nil {
		t.Fatal(err)
	}
	r := e.approved(t, lineRefund(po.payment.ID, line, 50_000))
	if _, err := e.completeAs(t, "finance_admin", r.ID); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("complete = %v, want payments.invalid_state", err)
	}
	if got, _ := e.store.GetRefund(t.Context(), r.ID, false); got.Status != payments.RefundStatusApproved {
		t.Fatalf("refund after the refusal = %s", got.Status)
	}
	if n := lateRecognitions(e.journal(t)); n != 0 {
		t.Fatalf("late recognitions = %d", n)
	}
	rep, err := e.ledMod.RecognizePlatformSales(t.Context())
	if err != nil || len(rep.Recognized) != 0 || len(rep.Skipped) != 1 || rep.Skipped[0] != line {
		t.Fatalf("job = %+v, %v", rep, err)
	}
	if ir, err := e.ledMod.CheckIntegrity(t.Context()); err != nil || len(ir.Findings) != 1 || *ir.Findings[0].OrderLineID != line ||
		ir.Findings[0].Detail != ledger.AllocationMissing {
		t.Fatalf("integrity = %+v, %v", ir.Findings, err)
	}
}

func txnOf(t *testing.T) func(ledger.Txn, error) ledger.Txn {
	return func(tx ledger.Txn, err error) ledger.Txn {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}
}
