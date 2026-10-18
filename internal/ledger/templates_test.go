package ledger_test

import (
	"errors"
	"math"
	"testing"
	"uuid"

	"koro/internal/ledger"
	"koro/internal/platform/money"
)

// modelALine computes a line's frozen money with platform/money the way
// orders does (02 §19.2, 06 H-6): VAT and commission per line, rounded half
// away from zero, optional withholding, commission from the seller or the
// buyer.
func modelALine(t *testing.T, payee uuid.UUID, net, vatBP, commBP int64, withheld [2]int64, buyerPays bool) ledger.AllocationLine {
	t.Helper()
	vat := amt(t)(money.VAT(net, vatBP))
	gross := amt(t)(money.Add(net, vat))
	var held int64
	if withheld[0] > 0 {
		held = amt(t)(money.VATWithheld(vat, withheld[0], withheld[1]))
	}
	comm := amt(t)(money.Commission(net, commBP, nil, nil))
	commVAT := amt(t)(money.VAT(comm, 2000))
	fee := amt(t)(money.Add(comm, commVAT))
	l := ledger.AllocationLine{OrderLineID: uuid.NewV7(), PayeeID: &payee, CommissionAmount: comm, CommissionVATAmount: commVAT}
	base := amt(t)(money.Sub(gross, held))
	if buyerPays {
		l.SellerNetAmount, l.PayableAmount = base, amt(t)(money.Add(base, fee))
	} else {
		l.SellerNetAmount, l.PayableAmount = amt(t)(money.Sub(base, fee)), base
	}
	return l
}

// TestTemplates_ModelA_AllocationSumsToZero: every model A allocation built
// from platform/money amounts sums to zero, moves each share to its account
// and names its line; inconsistent or out-of-model input is rejected before
// anything is posted (positive/negative table, 06 H-6).
func TestTemplates_ModelA_AllocationSumsToZero(t *testing.T) {
	t.Parallel()
	p1, p2 := uuid.NewV7(), uuid.NewV7()

	positive := []struct {
		name  string
		lines func(t *testing.T) []ledger.AllocationLine
		// entries is the expected entry count (zero shares are left out).
		entries int
	}{
		{"02 §19.2 example: 10 % from the seller", func(*testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{line(p1)}
		}, 4},
		{"commission paid by the buyer (fee on top)", func(t *testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{modelALine(t, p1, 1_000_000, 2000, 1000, [2]int64{}, true)}
		}, 4},
		{"0 % commission: no revenue or VAT entry", func(t *testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{modelALine(t, p1, 500_000, 2000, 0, [2]int64{}, false)}
		}, 2},
		{"odd amounts round half away from zero", func(t *testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{
				modelALine(t, p1, 333_333, 2000, 750, [2]int64{}, false),
				modelALine(t, p1, 1, 1000, 1250, [2]int64{}, false),
				modelALine(t, p2, 99_999, 100, 333, [2]int64{}, true),
			}
		}, 10},
		{"VAT withholding 5/10", func(t *testing.T) []ledger.AllocationLine {
			return []ledger.AllocationLine{modelALine(t, p1, 1_000_000, 2000, 1000, [2]int64{5, 10}, false)}
		}, 4},
		{"two payees, a free line in between", func(t *testing.T) []ledger.AllocationLine {
			free := modelALine(t, p2, 0, 2000, 1000, [2]int64{}, false)
			return []ledger.AllocationLine{line(p1), free, modelALine(t, p2, 2_500_000, 1000, 1500, [2]int64{}, true)}
		}, 8},
	}
	for _, c := range positive {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lines := c.lines(t)
			order := uuid.NewV7()
			tx, err := ledger.OrderAllocated(ledger.ModelA, order, lines, t0)
			if err != nil {
				t.Fatalf("OrderAllocated: %v", err)
			}
			if tx.Kind != ledger.KindOrderAllocated || tx.RefType != ledger.RefOrder || tx.RefID != order || !tx.OccurredAt.Equal(t0) {
				t.Fatalf("header %+v", tx)
			}
			if s := sum(tx.Entries); s != 0 || len(tx.Entries) != c.entries {
				t.Fatalf("sum %d over %d entries, want 0 over %d", s, len(tx.Entries), c.entries)
			}
			want := map[ledger.Account]int64{}
			payees := map[uuid.UUID]int64{}
			for _, l := range lines {
				want[ledger.AccountBuyerAdvances] += l.PayableAmount
				want[ledger.AccountSellerPayable] -= l.SellerNetAmount
				want[ledger.AccountCommissionRevenue] -= l.CommissionAmount
				want[ledger.AccountVATOutput] -= l.CommissionVATAmount
				payees[*l.PayeeID] -= l.SellerNetAmount
			}
			got := map[ledger.Account]int64{}
			gotPayees := map[uuid.UUID]int64{}
			for _, e := range tx.Entries {
				got[e.Account] += e.Amount
				if e.OrderLineID == nil || e.Amount == 0 {
					t.Fatalf("entry %+v: every allocation entry names its line and moves money", e)
				}
				if (e.Account == ledger.AccountSellerPayable) != (e.PayeeID != nil) {
					t.Fatalf("entry %+v breaks the payee rule", e)
				}
				if e.PayeeID != nil {
					gotPayees[*e.PayeeID] += e.Amount
				}
			}
			for a, w := range want {
				if got[a] != w {
					t.Errorf("%s moved %d, want %d", a, got[a], w)
				}
			}
			for p, w := range payees {
				if gotPayees[p] != w {
					t.Errorf("payee %s credited %d, want %d", p, gotPayees[p], w)
				}
			}
			// The same order posts and replays on the ledger.
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

	bad := func(mut func(*ledger.AllocationLine)) []ledger.AllocationLine {
		l := line(p1)
		mut(&l)
		return []ledger.AllocationLine{l}
	}
	negative := []struct {
		name  string
		model ledger.Model
		lines []ledger.AllocationLine
		want  error
	}{
		{"payable is one kuruş more than the shares", ledger.ModelA, bad(func(l *ledger.AllocationLine) { l.PayableAmount++ }), ledger.ErrInvalidTxn},
		{"payable is one kuruş less than the shares", ledger.ModelA, bad(func(l *ledger.AllocationLine) { l.PayableAmount-- }), ledger.ErrInvalidTxn},
		{"negative commission", ledger.ModelA, bad(func(l *ledger.AllocationLine) {
			l.CommissionAmount, l.SellerNetAmount = -100_000, 1_280_000
		}), ledger.ErrInvalidTxn},
		{"negative seller net", ledger.ModelA, bad(func(l *ledger.AllocationLine) {
			l.SellerNetAmount, l.CommissionAmount = -1, 1_180_001
		}), ledger.ErrInvalidTxn},
		{"negative payable (a refund is not an allocation)", ledger.ModelA, bad(func(l *ledger.AllocationLine) {
			l.PayableAmount, l.SellerNetAmount, l.CommissionAmount, l.CommissionVATAmount = -1_200_000, -1_080_000, -100_000, -20_000
		}), ledger.ErrInvalidTxn},
		{"line without a payee (TODO(T-11): model B)", ledger.ModelA, bad(func(l *ledger.AllocationLine) { l.PayeeID = nil }), ledger.ErrInvalidTxn},
		{"nil payee", ledger.ModelA, bad(func(l *ledger.AllocationLine) { l.PayeeID = ptr(uuid.Nil()) }), ledger.ErrInvalidTxn},
		{"nil line", ledger.ModelA, bad(func(l *ledger.AllocationLine) { l.OrderLineID = uuid.Nil() }), ledger.ErrInvalidTxn},
		{"shares overflow", ledger.ModelA, bad(func(l *ledger.AllocationLine) {
			l.SellerNetAmount, l.CommissionAmount = math.MaxInt64, 1
		}), ledger.ErrInvalidTxn},
		{"lines overflow together", ledger.ModelA, []ledger.AllocationLine{
			{OrderLineID: uuid.NewV7(), PayeeID: &p1, PayableAmount: math.MaxInt64, SellerNetAmount: math.MaxInt64},
			{OrderLineID: uuid.NewV7(), PayeeID: &p1, PayableAmount: 1, SellerNetAmount: 1},
		}, ledger.ErrInvalidTxn},
		{"no lines", ledger.ModelA, nil, ledger.ErrInvalidTxn},
		{"only free lines", ledger.ModelA, []ledger.AllocationLine{{OrderLineID: uuid.NewV7(), PayeeID: &p1}}, ledger.ErrInvalidTxn},
		{"model B (T-11)", ledger.ModelB, []ledger.AllocationLine{line(p1)}, ledger.ErrModelNotEnabled},
		{"model C excludes direct money", ledger.ModelC, []ledger.AllocationLine{line(p1)}, ledger.ErrInvalidTxn},
		{"unknown model", ledger.Model("Z"), []ledger.AllocationLine{line(p1)}, ledger.ErrInvalidTxn},
	}
	for _, c := range negative {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tx, err := ledger.OrderAllocated(c.model, uuid.NewV7(), c.lines, t0)
			if !errors.Is(err, c.want) {
				t.Fatalf("OrderAllocated = %+v, %v; want %v", tx, err, c.want)
			}
		})
	}

	if _, err := ledger.OrderAllocated(ledger.ModelA2, uuid.NewV7(), []ledger.AllocationLine{line(p1)}, t0); !errors.Is(err, ledger.ErrModelNotEnabled) {
		t.Fatalf("A2 allocation = %v", err)
	}
}

// TestTemplates_ModelA_Shapes pins the entries of the other model A
// templates (02 §21.3) and their input checks.
func TestTemplates_ModelA_Shapes(t *testing.T) {
	t.Parallel()
	payee, payment, payout, refund := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()

	pr := txn(t)(ledger.PaymentReceived(ledger.ModelA, payment, 1_200_000, t0))
	if pr.Kind != ledger.KindPaymentReceived || pr.RefType != ledger.RefPayment || pr.RefID != payment || len(pr.Entries) != 2 ||
		pr.Entries[0] != (ledger.Entry{Account: ledger.AccountKoroBank, Amount: 1_200_000}) ||
		pr.Entries[1] != (ledger.Entry{Account: ledger.AccountBuyerAdvances, Amount: -1_200_000}) {
		t.Fatalf("PaymentReceived = %+v", pr)
	}
	if pr.Total() != 1_200_000 {
		t.Fatalf("Total = %d", pr.Total())
	}

	po := txn(t)(ledger.PayoutPaid(ledger.ModelA, payout, payee, 1_080_000, t0))
	if po.Kind != ledger.KindPayoutPaid || po.RefType != ledger.RefPayout || po.RefID != payout || len(po.Entries) != 2 ||
		po.Entries[0].Account != ledger.AccountSellerPayable || *po.Entries[0].PayeeID != payee || po.Entries[0].Amount != 1_080_000 ||
		po.Entries[1] != (ledger.Entry{Account: ledger.AccountKoroBank, Amount: -1_080_000}) {
		t.Fatalf("PayoutPaid = %+v", po)
	}

	// Refund of an overpayment only: buyer_advances back to the bank.
	rf := txn(t)(ledger.RefundCompleted(ledger.ModelA, refund, 50_000, nil, t0))
	if len(rf.Entries) != 2 || rf.Entries[0] != (ledger.Entry{Account: ledger.AccountBuyerAdvances, Amount: 50_000}) ||
		rf.Entries[1] != (ledger.Entry{Account: ledger.AccountKoroBank, Amount: -50_000}) || rf.RefType != ledger.RefRefund {
		t.Fatalf("RefundCompleted(unallocated) = %+v", rf)
	}

	// Refund after allocation: the line's shares come back.
	l := line(payee)
	part := split(t)(ledger.SplitRefund(l, 1_200_000))
	if part.SellerAmount != 1_080_000 || part.CommissionAmount != 100_000 || part.CommissionVATAmount != 20_000 {
		t.Fatalf("SplitRefund(full) = %+v", part)
	}
	rf = txn(t)(ledger.RefundCompleted(ledger.ModelA, refund, 0, []ledger.RefundLine{part}, t0))
	if sum(rf.Entries) != 0 || len(rf.Entries) != 4 || rf.Entries[3] != (ledger.Entry{Account: ledger.AccountKoroBank, Amount: -1_200_000}) {
		t.Fatalf("RefundCompleted(line) = %+v", rf)
	}

	// 06 H-59: a paid-out line's seller part is claimed back from the payee
	// (payee_receivable, naming the line, no payee) instead of
	// seller_payable; commission and VAT come back as usual.
	part.PaidOut = true
	cb := txn(t)(ledger.RefundCompleted(ledger.ModelA, refund, 0, []ledger.RefundLine{part}, t0))
	if sum(cb.Entries) != 0 || len(cb.Entries) != 4 ||
		cb.Entries[0].Account != ledger.AccountPayeeReceivable || cb.Entries[0].Amount != 1_080_000 ||
		cb.Entries[0].OrderLineID == nil || *cb.Entries[0].OrderLineID != part.OrderLineID ||
		cb.Entries[1].Account != ledger.AccountCommissionRevenue || cb.Entries[2].Account != ledger.AccountVATOutput ||
		cb.Entries[3] != (ledger.Entry{Account: ledger.AccountKoroBank, Amount: -1_200_000}) {
		t.Fatalf("RefundCompleted(paid-out line) = %+v", cb)
	}
	for _, e := range cb.Entries {
		if e.PayeeID != nil {
			t.Fatalf("a clawback entry names a payee: %+v", e)
		}
	}

	for _, c := range []struct {
		name string
		err  error
		want error
	}{
		{"payment of zero", errOf(ledger.PaymentReceived(ledger.ModelA, payment, 0, t0)), ledger.ErrInvalidTxn},
		{"payment model A2", errOf(ledger.PaymentReceived(ledger.ModelA2, payment, 1, t0)), ledger.ErrModelNotEnabled},
		{"payout of zero", errOf(ledger.PayoutPaid(ledger.ModelA, payout, payee, 0, t0)), ledger.ErrInvalidTxn},
		{"payout without payee", errOf(ledger.PayoutPaid(ledger.ModelA, payout, uuid.Nil(), 1, t0)), ledger.ErrInvalidTxn},
		{"payout model B", errOf(ledger.PayoutPaid(ledger.ModelB, payout, payee, 1, t0)), ledger.ErrModelNotEnabled},
		{"refund of nothing", errOf(ledger.RefundCompleted(ledger.ModelA, refund, 0, nil, t0)), ledger.ErrInvalidTxn},
		{"refund negative unallocated", errOf(ledger.RefundCompleted(ledger.ModelA, refund, -1, nil, t0)), ledger.ErrInvalidTxn},
		{"refund negative share", errOf(ledger.RefundCompleted(ledger.ModelA, refund, 0,
			[]ledger.RefundLine{{OrderLineID: uuid.NewV7(), PayeeID: payee, SellerAmount: -1}}, t0)), ledger.ErrInvalidTxn},
		{"refund line without payee", errOf(ledger.RefundCompleted(ledger.ModelA, refund, 0,
			[]ledger.RefundLine{{OrderLineID: uuid.NewV7(), SellerAmount: 1}}, t0)), ledger.ErrInvalidTxn},
		{"refund overflow", errOf(ledger.RefundCompleted(ledger.ModelA, refund, math.MaxInt64,
			[]ledger.RefundLine{{OrderLineID: uuid.NewV7(), PayeeID: payee, SellerAmount: 1}}, t0)), ledger.ErrInvalidTxn},
		{"split above payable", errOf(ledger.SplitRefund(l, 1_200_001)), ledger.ErrInvalidTxn},
		{"split of zero", errOf(ledger.SplitRefund(l, 0)), ledger.ErrInvalidTxn},
	} {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, c.err, c.want)
		}
	}
}

func errOf[T any](_ T, err error) error { return err }

// TestTemplates_SplitRefund_LosesNoKurus: a partial refund's shares add up
// to the refund exactly, for every amount, and the refund posts balanced
// (platform/money Allocate; negative mirror via Reverse elsewhere).
func TestTemplates_SplitRefund_LosesNoKurus(t *testing.T) {
	t.Parallel()
	payee := uuid.NewV7()
	l := modelALine(t, payee, 333_333, 2000, 750, [2]int64{}, false)
	for _, amount := range []int64{1, 2, 3, 7, 99, 100_001, l.PayableAmount - 1, l.PayableAmount} {
		r := split(t)(ledger.SplitRefund(l, amount))
		if got := r.SellerAmount + r.CommissionAmount + r.CommissionVATAmount; got != amount ||
			r.SellerAmount > l.SellerNetAmount || r.CommissionAmount > l.CommissionAmount || r.CommissionVATAmount > l.CommissionVATAmount {
			t.Fatalf("SplitRefund(%d) = %+v (sum %d)", amount, r, got)
		}
		tx := txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{r}, t0))
		if sum(tx.Entries) != 0 || tx.Total() != amount {
			t.Fatalf("RefundCompleted(%d): sum %d, total %d", amount, sum(tx.Entries), tx.Total())
		}
	}
}

// TestChart_PerPayeeAccountsAreLiabilities: the payee balance is minus the
// sum of per-payee entries, which is right only for liability accounts.
func TestChart_PerPayeeAccountsAreLiabilities(t *testing.T) {
	t.Parallel()
	chart := ledger.Chart()
	if len(chart) != 13 {
		t.Fatalf("chart has %d accounts, want the 13 of 000110", len(chart))
	}
	perPayee := 0
	for _, a := range chart {
		if a.PerPayee {
			perPayee++
			if a.Kind != ledger.Liability {
				t.Errorf("%s is per payee but %s", a.Code, a.Kind)
			}
		}
	}
	if perPayee != 1 {
		t.Fatalf("%d per-payee accounts, want 1 (seller_payable)", perPayee)
	}
}
