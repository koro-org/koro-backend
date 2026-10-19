package ledger_test

import (
	"testing"
	"uuid"

	"koro/internal/ledger"
)

// T-305: the amount is the frozen seller share less completed refund ledger
// shares, including rounding. The remaining share can enter one payout only.
func TestRefundFollowups_PayoutSubtractsCompletedSellerShares(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	id := e.payable(t, payee)
	original := line(payee)
	original.OrderLineID = id
	refund := split(t)(ledger.SplitRefund(original, 600_001))
	e.post(t, txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{refund}, t0)))
	want := original.SellerNetAmount - refund.SellerAmount
	a := actor(t, "finance_admin")
	groups, err := e.svc.AdminListPayables(ctxOf(t, a), a, ledger.PayablesQuery{PayeeID: &payee})
	if err != nil || len(groups) != 1 || groups[0].Total != want || len(groups[0].Lines) != 1 || groups[0].Lines[0].Amount != want {
		t.Fatalf("payables after rounded refund: %+v %v; want %d", groups, err, want)
	}
	p := e.create(t, "finance_admin", payee)
	if p.Amount != want || len(p.Items) != 1 || p.Items[0].Amount != want {
		t.Fatalf("payout: %+v, want %d", p, want)
	}
	_, err = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee, LineIDs: []uuid.UUID{id}})
	wantProblem(t, err, ledger.ErrLineNotPayable)
	e.approve(t, "finance_admin_2", p.ID)
	e.markPaid(t, "finance_admin", p.ID)
	if b := e.balance(t, payee); b != 0 {
		t.Fatalf("remaining seller balance after payout: %d", b)
	}
	_, err = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee})
	wantProblem(t, err, ledger.ErrNothingPayable)
}

func TestRefundFollowups_FullyReturnedSellerShareHasNoPayout(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	id := e.payable(t, payee)
	original := line(payee)
	original.OrderLineID = id
	refund := split(t)(ledger.SplitRefund(original, original.PayableAmount))
	e.post(t, txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{refund}, t0)))
	a := actor(t, "finance_admin")
	_, err := e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee})
	wantProblem(t, err, ledger.ErrNothingPayable)
}
