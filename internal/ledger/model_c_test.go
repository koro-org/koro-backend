package ledger_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/ledger"
	"koro/internal/orders"
)

func TestTemplates_ModelC_FeeBalanceAndReversal(t *testing.T) {
	t.Parallel()
	payee := uuid.NewV7()
	l := ledger.FeeAllocationOf(orders.PaidLine{LineID: uuid.NewV7(), PayeeID: &payee, PayableAmount: 1_320_000, SellerNetAmount: 1_200_000, BuyerFeeAmount: 120_000, CommissionAmount: 100_000, CommissionVATAmount: 20_000})
	tx := txn(t)(ledger.OrderAllocated(ledger.ModelC, uuid.NewV7(), []ledger.AllocationLine{l}, t0))
	if sum(tx.Entries) != 0 || tx.Total() != 120_000 {
		t.Fatalf("C fee entries=%+v", tx)
	}
	got := byAccount(tx.Entries)
	for code, want := range map[ledger.Account]int64{ledger.AccountBuyerAdvances: 120_000, ledger.AccountCommissionRevenue: -100_000, ledger.AccountVATOutput: -20_000} {
		if got[code] != want {
			t.Fatalf("%s=%d", code, got[code])
		}
	}
	e := newEnv(t)
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelC, uuid.NewV7(), 120_000, t0)))
	alloc := e.post(t, tx)
	if _, err := e.svc.Reverse(t.Context(), alloc, "Synthetic reversal"); err != nil {
		t.Fatal(err)
	}
	r := split(t)(ledger.SplitRefund(l, 120_000))
	refund := txn(t)(ledger.RefundCompleted(ledger.ModelC, uuid.NewV7(), 0, []ledger.RefundLine{r}, t0))
	if sum(refund.Entries) != 0 || byAccount(refund.Entries)[ledger.AccountKoroBank] != -120_000 {
		t.Fatalf("C reversal=%+v", refund)
	}
	for _, m := range []ledger.Model{ledger.ModelA2, ledger.ModelB} {
		if _, err := ledger.PaymentReceived(m, uuid.NewV7(), 1, t0); !errors.Is(err, ledger.ErrModelNotEnabled) {
			t.Fatalf("%s capture=%v", m, err)
		}
		if _, err := ledger.RefundCompleted(m, uuid.NewV7(), 1, nil, t0); !errors.Is(err, ledger.ErrModelNotEnabled) {
			t.Fatalf("%s refund=%v", m, err)
		}
	}
	if _, err := ledger.PayoutPaid(ledger.ModelC, uuid.NewV7(), payee, 1, t0); !errors.Is(err, ledger.ErrModelNotEnabled) {
		t.Fatalf("C payout=%v", err)
	}
}
