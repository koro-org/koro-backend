package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"uuid"

	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/money"
)

// koroLine is Koro's own platform-native line (BE-067, T-293) priced the
// way orders does (02 §19.2, 06 H-6): net, its VAT at vatBP, the
// withholding fraction (tevkifat; zero: none) and, when the buyer pays the
// commission on top, the buyer fee at commBP with 20 % VAT.
func koroLine(t *testing.T, net, vatBP int64, withheld [2]int64, commBP int64, buyerPays bool) ledger.AllocationLine {
	t.Helper()
	vat := amt(t)(money.VAT(net, vatBP))
	var held int64
	if withheld[0] > 0 {
		held = amt(t)(money.VATWithheld(vat, withheld[0], withheld[1]))
	}
	l := ledger.AllocationLine{OrderLineID: uuid.NewV7(), NetAmount: net, VATAmount: vat, VATWithheldAmount: held}
	l.PayableAmount = amt(t)(money.Sum(net, vat, -held))
	if buyerPays {
		l.CommissionAmount = amt(t)(money.Commission(net, commBP, nil, nil))
		l.CommissionVATAmount = amt(t)(money.VAT(l.CommissionAmount, 2000))
		l.PayableAmount = amt(t)(money.Sum(l.PayableAmount, l.CommissionAmount, l.CommissionVATAmount))
	}
	return l
}

// byAccount sums the entries per account; byLine per order line.
func byAccount(es []ledger.Entry) map[ledger.Account]int64 {
	out := map[ledger.Account]int64{}
	for _, e := range es {
		out[e.Account] += e.Amount
	}
	return out
}

// TestTemplates_PlatformLine_AllocationSumsToZero (BE-067, T-293): Koro's
// own line is Koro's sale — buyer_advances +payable, platform_sales_revenue
// −net, vat_output −VAT and, with tevkifat, vat_output +withheld as an
// entry of its own (the VAT Koro owes is VAT − withheld) — in the same
// order_allocated as the payee lines of a mixed order; every transaction
// sums to zero, names its lines and posts and replays on the ledger.
// Inconsistent input is refused before anything is posted.
func TestTemplates_PlatformLine_AllocationSumsToZero(t *testing.T) {
	t.Parallel()
	payee := uuid.NewV7()
	positive := []struct {
		name    string
		lines   func(t *testing.T) []ledger.AllocationLine
		entries int
		want    map[ledger.Account]int64
	}{
		{"Koro's own line alone (250,000 + 20 % VAT)", func(t *testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{koroLine(t, 250_000, 2000, [2]int64{}, 0, false)}
		}, 3, map[ledger.Account]int64{
			ledger.AccountBuyerAdvances: 300_000, ledger.AccountSalesRevenue: -250_000, ledger.AccountVATOutput: -50_000,
		}},
		{"VAT withholding 5/10: its own entry, Koro owes the rest", func(t *testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{koroLine(t, 1_000_000, 2000, [2]int64{5, 10}, 0, false)}
		}, 4, map[ledger.Account]int64{
			ledger.AccountBuyerAdvances: 1_100_000, ledger.AccountSalesRevenue: -1_000_000, ledger.AccountVATOutput: -100_000,
		}},
		{"buyer fee on top of Koro's sale", func(t *testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{koroLine(t, 250_000, 2000, [2]int64{}, 1000, true)}
		}, 5, map[ledger.Account]int64{
			ledger.AccountBuyerAdvances: 330_000, ledger.AccountSalesRevenue: -250_000, ledger.AccountCommissionRevenue: -25_000,
			ledger.AccountVATOutput: -55_000,
		}},
		{"VAT-exempt sale: no VAT entry", func(t *testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{koroLine(t, 99_999, 0, [2]int64{}, 0, false)}
		}, 2, map[ledger.Account]int64{ledger.AccountBuyerAdvances: 99_999, ledger.AccountSalesRevenue: -99_999}},
		{"mixed order: a payee line and two of Koro's lines, odd amounts", func(t *testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{line(payee), koroLine(t, 333_333, 2000, [2]int64{2, 10}, 0, false),
				koroLine(t, 1, 1000, [2]int64{}, 0, false)}
		}, 4 + 4 + 2, map[ledger.Account]int64{
			// 1,200,000 + (333,333 + 66,667 − 13,333) + 1 (VAT of 1 kuruş at 10 % rounds to 0)
			ledger.AccountBuyerAdvances: 1_200_000 + 386_667 + 1, ledger.AccountSellerPayable: -1_080_000,
			ledger.AccountCommissionRevenue: -100_000, ledger.AccountSalesRevenue: -333_334,
			ledger.AccountVATOutput: -20_000 - 66_667 + 13_333,
		}},
	}
	for _, c := range positive {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lines := c.lines(t)
			order := uuid.NewV7()
			tx := txn(t)(ledger.OrderAllocated(ledger.ModelA, order, lines, t0))
			if tx.Kind != ledger.KindOrderAllocated || tx.RefType != ledger.RefOrder || tx.RefID != order {
				t.Fatalf("header %+v", tx)
			}
			if s := sum(tx.Entries); s != 0 || len(tx.Entries) != c.entries {
				t.Fatalf("sum %d over %d entries, want 0 over %d: %+v", s, len(tx.Entries), c.entries, tx.Entries)
			}
			if got := byAccount(tx.Entries); !mapsEqual(got, c.want) {
				t.Fatalf("accounts %v, want %v", got, c.want)
			}
			perLine := map[uuid.UUID]int64{}
			for _, e := range tx.Entries {
				if e.OrderLineID == nil || e.Amount == 0 {
					t.Fatalf("entry %+v: every allocation entry names its line and moves money", e)
				}
				if (e.Account == ledger.AccountSellerPayable) != (e.PayeeID != nil) {
					t.Fatalf("entry %+v breaks the payee rule", e)
				}
				perLine[*e.OrderLineID] += e.Amount
			}
			for id, s := range perLine {
				if s != 0 {
					t.Fatalf("line %s sums to %d: each line is balanced on its own", id, s)
				}
			}
			for _, l := range lines {
				if l.Koro() && l.VATWithheldAmount > 0 && !slices.ContainsFunc(tx.Entries, func(e ledger.Entry) bool {
					return entryKey(e) == entryKey(ledger.Entry{Account: ledger.AccountVATOutput, OrderLineID: ptr(l.OrderLineID), Amount: l.VATWithheldAmount})
				}) {
					t.Fatalf("no withholding entry of its own for %+v: %+v", l, tx.Entries)
				}
				if sh := l.Shares(); sh.Total() != l.PayableAmount {
					t.Fatalf("shares %+v of %+v do not add up to the payable amount", sh, l)
				}
			}
			e := newEnv(t)
			var total int64
			for _, l := range lines {
				total += l.PayableAmount
			}
			e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), total, t0)))
			id := e.post(t, tx)
			if again := e.post(t, tx); again != id {
				t.Fatalf("replay = %s, want %s", again, id)
			}
		})
	}

	base := koroLine(t, 250_000, 2000, [2]int64{5, 10}, 0, false)
	bad := func(mut func(*ledger.AllocationLine)) []ledger.AllocationLine {
		l := base
		mut(&l)
		return []ledger.AllocationLine{l}
	}
	for _, c := range []struct {
		name  string
		model ledger.Model
		lines []ledger.AllocationLine
		want  error
	}{
		{"payable one kuruş more", ledger.ModelA, bad(func(l *ledger.AllocationLine) { l.PayableAmount++ }), ledger.ErrInvalidTxn},
		{"payable without the withholding", ledger.ModelA, bad(func(l *ledger.AllocationLine) { l.PayableAmount += l.VATWithheldAmount }), ledger.ErrInvalidTxn},
		{"a seller share on Koro's own line", ledger.ModelA, bad(func(l *ledger.AllocationLine) {
			l.SellerNetAmount, l.PayableAmount = 1, l.PayableAmount+1
		}), ledger.ErrInvalidTxn},
		{"withholding above the VAT", ledger.ModelA, bad(func(l *ledger.AllocationLine) {
			l.VATWithheldAmount, l.PayableAmount = l.VATAmount+1, l.NetAmount-1
		}), ledger.ErrInvalidTxn},
		{"negative net", ledger.ModelA, bad(func(l *ledger.AllocationLine) { l.NetAmount = -l.NetAmount }), ledger.ErrInvalidTxn},
		{"a sale on a payee line", ledger.ModelA, func() []ledger.AllocationLine {
			l := line(payee)
			l.NetAmount = 1
			return []ledger.AllocationLine{l}
		}(), ledger.ErrInvalidTxn},
		{"only a free Koro line", ledger.ModelA, []ledger.AllocationLine{{OrderLineID: uuid.NewV7()}}, ledger.ErrInvalidTxn},
		{"model B (T-11)", ledger.ModelB, []ledger.AllocationLine{base}, ledger.ErrModelNotEnabled},
		{"model C (BE-066)", ledger.ModelC, []ledger.AllocationLine{base}, ledger.ErrModelNotEnabled},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if tx, err := ledger.OrderAllocated(c.model, uuid.NewV7(), c.lines, t0); !errors.Is(err, c.want) {
				t.Fatalf("OrderAllocated = %+v, %v; want %v", tx, err, c.want)
			}
		})
	}
}

// entryKey compares entries by value (their IDs are pointers).
func entryKey(e ledger.Entry) string {
	id := func(p *uuid.UUID) string {
		if p == nil {
			return "-"
		}
		return p.String()
	}
	return fmt.Sprintf("%s|%s|%s|%d", e.Account, id(e.PayeeID), id(e.OrderLineID), e.Amount)
}

func mapsEqual(a, b map[ledger.Account]int64) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// TestAllocationOf_FromPaidLine (BE-067): the allocation of a paid line is
// built from orders' frozen amounts: a payee line's shares; Koro's own
// line's sale, VAT and withholding, with the commission only when the buyer
// paid it on top (a commission "the seller" pays on Koro's own line is
// Koro's to itself and moves nothing).
func TestAllocationOf_FromPaidLine(t *testing.T) {
	t.Parallel()
	payee := uuid.NewV7()
	pl := orders.PaidLine{LineID: uuid.NewV7(), PayeeID: &payee, PayableAmount: 1_200_000, SellerNetAmount: 1_080_000,
		CommissionAmount: 100_000, CommissionVATAmount: 20_000, NetAmount: 1_000_000, VATAmount: 200_000}
	a := ledger.AllocationOf(pl)
	if a.PayeeID == nil || *a.PayeeID != payee || a.SellerNetAmount != 1_080_000 || a.NetAmount != 0 || a.VATAmount != 0 {
		t.Fatalf("payee line = %+v", a)
	}
	if _, err := ledger.OrderAllocated(ledger.ModelA, uuid.NewV7(), []ledger.AllocationLine{a}, t0); err != nil {
		t.Fatal(err)
	}
	// Koro's own line, commission from the seller (Koro): left out.
	own := orders.PaidLine{LineID: uuid.NewV7(), PayableAmount: 275_000, SellerNetAmount: 245_000, CommissionAmount: 25_000,
		CommissionVATAmount: 5_000, NetAmount: 250_000, VATAmount: 50_000, VATWithheldAmount: 25_000}
	a = ledger.AllocationOf(own)
	if a.PayeeID != nil || a.SellerNetAmount != 0 || a.CommissionAmount != 0 || a.NetAmount != 250_000 || a.VATWithheldAmount != 25_000 {
		t.Fatalf("Koro line (seller pays) = %+v", a)
	}
	if _, err := ledger.OrderAllocated(ledger.ModelA, uuid.NewV7(), []ledger.AllocationLine{a}, t0); err != nil {
		t.Fatal(err)
	}
	// The buyer paid the commission on top: Koro's fee revenue.
	own.BuyerFeeAmount, own.PayableAmount, own.SellerNetAmount = 30_000, 305_000, 275_000
	a = ledger.AllocationOf(own)
	if a.CommissionAmount != 25_000 || a.CommissionVATAmount != 5_000 {
		t.Fatalf("Koro line (buyer fee) = %+v", a)
	}
	tx := txn(t)(ledger.OrderAllocated(ledger.ModelA, uuid.NewV7(), []ledger.AllocationLine{a}, t0))
	if got := byAccount(tx.Entries); got[ledger.AccountCommissionRevenue] != -25_000 || got[ledger.AccountVATOutput] != -50_000+25_000-5_000 {
		t.Fatalf("buyer fee entries %v", got)
	}
}

// TestTemplates_PlatformSaleRecognized (BE-067): the late recognition of a
// line paid before the template is an adjustment keyed by the order line
// (ref_type manual), with the line's order_allocated entries and a memo; a
// payee line, a free line and models without a template are refused.
func TestTemplates_PlatformSaleRecognized(t *testing.T) {
	t.Parallel()
	l := koroLine(t, 250_000, 2000, [2]int64{5, 10}, 0, false)
	tx := txn(t)(ledger.PlatformSaleRecognized(ledger.ModelA, l, t0))
	if tx.Kind != ledger.KindAdjustment || tx.RefType != ledger.RefManual || tx.RefID != l.OrderLineID || tx.Memo != ledger.LateRecognitionMemo ||
		!tx.OccurredAt.Equal(t0) {
		t.Fatalf("header %+v", tx)
	}
	alloc := txn(t)(ledger.OrderAllocated(ledger.ModelA2, uuid.NewV7(), []ledger.AllocationLine{l}, t0))
	if !slices.EqualFunc(tx.Entries, alloc.Entries, func(a, b ledger.Entry) bool { return entryKey(a) == entryKey(b) }) || sum(tx.Entries) != 0 {
		t.Fatalf("entries %+v, want the allocation's %+v", tx.Entries, alloc.Entries)
	}
	e := newEnv(t)
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), l.PayableAmount, t0)))
	id := e.post(t, tx)
	if again := e.post(t, txn(t)(ledger.PlatformSaleRecognized(ledger.ModelA, l, t0.Add(1)))); again != id {
		t.Fatalf("a second recognition of the line = %s, want the replay of %s", again, id)
	}
	for _, c := range []struct {
		name string
		err  error
		want error
	}{
		{"payee line", errOf(ledger.PlatformSaleRecognized(ledger.ModelA, line(uuid.NewV7()), t0)), ledger.ErrInvalidTxn},
		{"free line", errOf(ledger.PlatformSaleRecognized(ledger.ModelA, ledger.AllocationLine{OrderLineID: uuid.NewV7()}, t0)), ledger.ErrInvalidTxn},
		{"inconsistent line", errOf(ledger.PlatformSaleRecognized(ledger.ModelA, func() ledger.AllocationLine {
			x := l
			x.PayableAmount--
			return x
		}(), t0)), ledger.ErrInvalidTxn},
		{"model B", errOf(ledger.PlatformSaleRecognized(ledger.ModelB, l, t0)), ledger.ErrModelNotEnabled},
		{"model C", errOf(ledger.PlatformSaleRecognized(ledger.ModelC, l, t0)), ledger.ErrModelNotEnabled},
	} {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, c.err, c.want)
		}
	}
}

// TestTemplates_PlatformRefund_LosesNoKurus (BE-067): a refund of Koro's
// own line takes back its sale, buyer fee and VAT (platform_sales_revenue
// +, platform_commission_revenue +, vat_output +, koro_bank −) in
// proportion to the line's shares, adding up to the refund exactly; a full
// refund brings every account of the line back to zero. Shapes mixing a
// payee line's and Koro's line's shares are refused.
func TestTemplates_PlatformRefund_LosesNoKurus(t *testing.T) {
	t.Parallel()
	l := koroLine(t, 333_333, 2000, [2]int64{2, 10}, 750, true)
	for _, amount := range []int64{1, 2, 3, 99, 100_001, l.PayableAmount - 1, l.PayableAmount} {
		r := split(t)(ledger.SplitRefund(l, amount))
		sh := l.Shares()
		if got := r.SalesAmount + r.CommissionAmount + r.CommissionVATAmount; got != amount || r.SellerAmount != 0 || r.PayeeID != uuid.Nil() ||
			r.SalesAmount > sh.Sales || r.CommissionAmount > sh.Commission || r.CommissionVATAmount > sh.CommissionVAT {
			t.Fatalf("SplitRefund(%d) = %+v (sum %d)", amount, r, got)
		}
		tx := txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{r}, t0))
		if sum(tx.Entries) != 0 || tx.Total() != amount {
			t.Fatalf("RefundCompleted(%d): sum %d, total %d", amount, sum(tx.Entries), tx.Total())
		}
		for _, e := range tx.Entries {
			if e.Account == ledger.AccountSellerPayable || e.Account == ledger.AccountPayeeReceivable || e.Account == ledger.AccountBuyerAdvances {
				t.Fatalf("a refund of Koro's own line touches %s: %+v", e.Account, tx.Entries)
			}
		}
	}
	// Allocation + full refund: the line's revenue, fee and VAT are zero.
	full := split(t)(ledger.SplitRefund(l, l.PayableAmount))
	alloc := txn(t)(ledger.OrderAllocated(ledger.ModelA, uuid.NewV7(), []ledger.AllocationLine{l}, t0))
	back := txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{full}, t0))
	acc := byAccount(append(slices.Clone(alloc.Entries), back.Entries...))
	if acc[ledger.AccountSalesRevenue] != 0 || acc[ledger.AccountCommissionRevenue] != 0 || acc[ledger.AccountVATOutput] != 0 ||
		acc[ledger.AccountBuyerAdvances] != l.PayableAmount || acc[ledger.AccountKoroBank] != -l.PayableAmount {
		t.Fatalf("allocation + full refund = %v", acc)
	}
	payee, line0 := uuid.NewV7(), uuid.NewV7()
	for _, c := range []struct {
		name string
		r    ledger.RefundLine
	}{
		{"a seller part on Koro's own line", ledger.RefundLine{OrderLineID: line0, SalesAmount: 1, SellerAmount: 1}},
		{"Koro's own line paid out", ledger.RefundLine{OrderLineID: line0, SalesAmount: 1, PaidOut: true}},
		{"a sale on a payee line", ledger.RefundLine{OrderLineID: line0, PayeeID: payee, SellerAmount: 1, SalesAmount: 1}},
		{"a negative sale", ledger.RefundLine{OrderLineID: line0, SalesAmount: -1, CommissionVATAmount: 2}},
	} {
		if _, err := ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{c.r}, t0); !errors.Is(err, ledger.ErrInvalidTxn) {
			t.Errorf("%s: %v, want %v", c.name, err, ledger.ErrInvalidTxn)
		}
	}
}

// collectedOf is the settled form of an allocation line (orders' view) of
// model A with refunded amount; a commission on Koro's own line is a buyer
// fee.
func collectedOf(l ledger.AllocationLine, refunded int64) orders.CollectedLine {
	var fee int64
	if l.Koro() {
		fee = l.CommissionAmount + l.CommissionVATAmount
	}
	return orders.CollectedLine{
		PaidLine: orders.PaidLine{LineID: l.OrderLineID, PayeeID: l.PayeeID, PayableAmount: l.PayableAmount, SellerNetAmount: l.SellerNetAmount,
			CommissionAmount: l.CommissionAmount, CommissionVATAmount: l.CommissionVATAmount, NetAmount: l.NetAmount, VATAmount: l.VATAmount,
			VATWithheldAmount: l.VATWithheldAmount, BuyerFeeAmount: fee},
		OrderID: uuid.NewV7(), ModelCode: orders.ModelA, RefundedAmount: refunded,
	}
}

// TestRecognizePlatformSales_IdempotentAndChecked (BE-067, T-293): the
// lines of Koro's own sales paid before the template (money in
// buyer_advances, no allocation) are found by the integrity check
// (unallocated_line, missing_allocation) and recognised once by
// ledger.recognize_platform_sales; a second run recognises nothing and
// posts nothing. A line allocated at payment is left alone; a line refunded
// from buyer_advances before its recognition is skipped and stays a
// finding (finance corrects it); a payee line short of its allocation is
// an allocation_mismatch.
func TestRecognizePlatformSales_IdempotentAndChecked(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	old := koroLine(t, 250_000, 2000, [2]int64{5, 10}, 0, false)
	oldMixed, oldPayee := koroLine(t, 100_000, 2000, [2]int64{}, 1000, true), line(payee)
	fresh := koroLine(t, 400_000, 2000, [2]int64{}, 0, false)
	refunded := koroLine(t, 50_000, 2000, [2]int64{}, 0, false)
	short := line(payee)
	short.PayableAmount, short.SellerNetAmount = 600_000, 480_000 // allocated as a 600,000 line, settled as 1,200,000 below
	// Before BE-067: the payment reached buyer_advances; only payee lines were allocated.
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), old.PayableAmount, t0)))
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), oldMixed.PayableAmount+oldPayee.PayableAmount, t0)))
	e.post(t, txn(t)(ledger.OrderAllocated(ledger.ModelA, uuid.NewV7(), []ledger.AllocationLine{oldPayee}, t0)))
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), refunded.PayableAmount, t0)))
	e.post(t, txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 10_000, nil, t0))) // the interim path (BE-043)
	// After BE-067: allocated at payment.
	e.paidOrder(t, fresh)
	e.paidOrder(t, short)
	shortCollected := collectedOf(short, 0)
	shortCollected.PayableAmount, shortCollected.SellerNetAmount = 1_200_000, 1_080_000
	e.orders.collect(collectedOf(old, 0), collectedOf(oldMixed, 0), collectedOf(oldPayee, 0), collectedOf(fresh, 0), collectedOf(refunded, 10_000),
		shortCollected)

	findings := func() map[uuid.UUID]string {
		t.Helper()
		rep, err := e.mod.CheckIntegrity(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]string{}
		for _, f := range rep.Findings {
			if f.Kind != ledger.FindingUnallocatedLine || f.OrderLineID == nil || f.Amount == nil {
				t.Fatalf("unexpected finding %+v", f)
			}
			out[*f.OrderLineID] = f.Detail
		}
		return out
	}
	want := map[uuid.UUID]string{old.OrderLineID: ledger.AllocationMissing, oldMixed.OrderLineID: ledger.AllocationMissing,
		refunded.OrderLineID: ledger.AllocationMissing, short.OrderLineID: ledger.AllocationMismatch}
	if got := findings(); !maps2Equal(got, want) {
		t.Fatalf("findings before = %v, want %v", got, want)
	}

	rep, err := e.mod.RecognizePlatformSales(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantRec := []uuid.UUID{old.OrderLineID, oldMixed.OrderLineID}
	slices.SortFunc(wantRec, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if !slices.Equal(rep.Recognized, wantRec) || !slices.Equal(rep.Skipped, []uuid.UUID{refunded.OrderLineID}) {
		t.Fatalf("first run = %+v, want recognised %v, skipped %v", rep, wantRec, refunded.OrderLineID)
	}
	countAdj := func() int {
		t.Helper()
		page, err := e.svc.ListTransactions(t.Context(), actor(t, "finance_admin"), ledger.JournalQuery{
			Filter: ledger.JournalFilter{Kind: ledger.KindAdjustment}, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, p := range page.Items {
			if p.Memo != nil && *p.Memo == ledger.LateRecognitionMemo {
				n++
			}
		}
		return n
	}
	if n := countAdj(); n != 2 {
		t.Fatalf("%d late recognitions after the first run, want 2", n)
	}
	delete(want, old.OrderLineID)
	delete(want, oldMixed.OrderLineID)
	if got := findings(); !maps2Equal(got, want) {
		t.Fatalf("findings after = %v, want %v", got, want)
	}
	// The second run finds nothing to do and posts nothing.
	rep, err = e.mod.RecognizePlatformSales(t.Context())
	if err != nil || len(rep.Recognized) != 0 || !slices.Equal(rep.Skipped, []uuid.UUID{refunded.OrderLineID}) {
		t.Fatalf("second run = %+v, %v", rep, err)
	}
	if n := countAdj(); n != 2 {
		t.Fatalf("%d late recognitions after the second run, want 2", n)
	}
	// The scheduled job is the same run.
	for _, j := range e.mod.Jobs() {
		if j.Name == ledger.RecognizeJobName {
			if err := j.Handle(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := countAdj(); n != 2 {
		t.Fatalf("%d late recognitions after the job, want 2", n)
	}
	// buyer_advances holds only what is not Koro's sale or a payee's: the
	// skipped line's money less its interim refund, and the short line's
	// unallocated part.
	page, err := e.svc.ListTransactions(t.Context(), actor(t, "finance_admin"), ledger.JournalQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var es []ledger.Entry
	for _, p := range page.Items {
		for _, r := range p.Entries {
			es = append(es, r.Entry())
		}
	}
	acc := byAccount(es)
	if acc[ledger.AccountBuyerAdvances] != -(refunded.PayableAmount-10_000) || sum(es) != 0 ||
		acc[ledger.AccountSalesRevenue] != -(old.NetAmount+oldMixed.NetAmount+fresh.NetAmount) {
		t.Fatalf("accounts %v", acc)
	}
}

func maps2Equal(a, b map[uuid.UUID]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A full first page of free lines must not hide paid sales on later pages
// from either the integrity check or the recognition job.
func TestRecognizePlatformSales_LaterPage(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	for range 500 {
		e.orders.collect(collectedOf(ledger.AllocationLine{OrderLineID: uuid.NewV7()}, 0))
	}
	l := koroLine(t, 1, 0, [2]int64{}, 0, false)
	e.orders.collect(collectedOf(l, 0))
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 1, t0)))
	before, err := e.mod.CheckIntegrity(t.Context())
	if err != nil || len(before.Findings) != 1 || before.Findings[0].OrderLineID == nil ||
		*before.Findings[0].OrderLineID != l.OrderLineID {
		t.Fatalf("integrity before = %+v, %v", before, err)
	}
	rep, err := e.mod.RecognizePlatformSales(t.Context())
	if err != nil || !slices.Equal(rep.Recognized, []uuid.UUID{l.OrderLineID}) || len(rep.Skipped) != 0 {
		t.Fatalf("recognition = %+v, %v", rep, err)
	}
	if after, err := e.mod.CheckIntegrity(t.Context()); err != nil || !after.OK() {
		t.Fatalf("integrity after = %+v, %v", after, err)
	}
}

// refundedBeforeAllocation simulates an interim refund committed after
// the job read its candidate page but before it obtained the order lock.
type refundedBeforeAllocation struct{ *fakeOrders }

func (f refundedBeforeAllocation) ForAllocation(ctx context.Context, lineID uuid.UUID) (orders.CollectedLine, error) {
	f.refund(lineID, 1)
	return f.fakeOrders.ForAllocation(ctx, lineID)
}

func TestRecognizePlatformSales_RereadsRefundUnderLock(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0)
	mem := ledger.NewMemStore(clk.Now)
	ord := refundedBeforeAllocation{newFakeOrders()}
	e := newPayoutEnvWith(t, clk, mem, mem, db.NopTx{}, ord)
	l := koroLine(t, 100, 0, [2]int64{}, 0, false)
	ord.collect(collectedOf(l, 0))
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 100, t0)))
	rep, err := e.mod.RecognizePlatformSales(t.Context())
	if err != nil || len(rep.Recognized) != 0 || !slices.Equal(rep.Skipped, []uuid.UUID{l.OrderLineID}) {
		t.Fatalf("recognition with a stale page = %+v, %v", rep, err)
	}
	if got, err := e.mod.LineAllocation(t.Context(), l.OrderLineID); err != nil || got != 0 {
		t.Fatalf("allocation = %d, %v; refunded line must remain for finance", got, err)
	}
}
