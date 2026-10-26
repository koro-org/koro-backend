package ledger_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/ledger"
	"koro/internal/platform/problem/problemtest"
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
	problemtest.WantErr(t, err, ledger.ErrLineNotPayable)
	e.approve(t, "finance_admin_2", p.ID)
	e.markPaid(t, "finance_admin", p.ID)
	if b := e.balance(t, payee); b != 0 {
		t.Fatalf("remaining seller balance after payout: %d", b)
	}
	_, err = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee})
	problemtest.WantErr(t, err, ledger.ErrNothingPayable)
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
	problemtest.WantErr(t, err, ledger.ErrNothingPayable)
}

// TestAdminListPayeeReceivables (BE-129, 06 H-59): a refund after the payout
// claims the seller share back from the payee; the line is listed with its
// order reference and the open amount until a collection posts it back;
// the payee balance's receivable is their sum. Staff only (403), an
// unknown payee 404.
func TestAdminListPayeeReceivables(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	id := e.payable(t, payee)
	free := e.payable(t, payee) // paid out too, never refunded
	p := e.create(t, "finance_admin", payee)
	e.approve(t, "finance_admin_2", p.ID)
	e.markPaid(t, "finance_admin", p.ID)
	a := actor(t, "finance_admin")
	if ls, err := e.svc.AdminListPayeeReceivables(ctxOf(t, a), a, payee); err != nil || len(ls) != 0 {
		t.Fatalf("receivables before a refund = %+v, %v", ls, err)
	}
	original := line(payee)
	original.OrderLineID = id
	refund := split(t)(ledger.SplitRefund(original, 600_000))
	refund.PaidOut = true
	e.post(t, txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{refund}, t0)))
	ls, err := e.svc.AdminListPayeeReceivables(ctxOf(t, a), a, payee)
	if err != nil || len(ls) != 1 || ls[0].OrderLineID != id || ls[0].PayoutID != p.ID || ls[0].Amount != refund.SellerAmount ||
		ls[0].OrderNo == "" || ls[0].OrderID == uuid.Nil() {
		t.Fatalf("receivables = %+v, %v", ls, err)
	}
	// A collection of part of it leaves the rest open.
	e.post(t, ledger.Txn{Kind: ledger.KindAdjustment, RefType: ledger.RefManual, RefID: uuid.NewV7(), OccurredAt: t0,
		Entries: []ledger.Entry{{Account: ledger.AccountPayeeReceivable, OrderLineID: &id, Amount: -100_000},
			{Account: ledger.AccountKoroBank, Amount: 100_000}}})
	if ls, _ = e.svc.AdminListPayeeReceivables(ctxOf(t, a), a, payee); len(ls) != 1 || ls[0].Amount != refund.SellerAmount-100_000 {
		t.Fatalf("receivables after a partial collection = %+v", ls)
	}
	for _, l := range ls {
		if l.OrderLineID == free {
			t.Fatal("a line without a claim is listed")
		}
	}
	pad := actor(t, "platform_admin")
	if ls, err := e.svc.AdminListPayeeReceivables(ctxOf(t, pad), pad, payee); err != nil || len(ls) != 1 {
		t.Fatalf("platform admin = %+v, %v", ls, err)
	}
	cla := actor(t, "club_admin")
	_, err = e.svc.AdminListPayeeReceivables(ctxOf(t, cla), cla, payee)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.AdminListPayeeReceivables(ctxOf(t, a), a, uuid.NewV7())
	problemtest.WantErr(t, err, ledger.ErrNotFound)

	app := newHTTP(t, e.env)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/admin/payees/"+payee.String()+"/receivables", nil)
	pp, _ := authztest.Get("finance_admin")
	req.Header.Set("Authorization", "Bearer "+pp.Token)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var out struct {
		Items []struct {
			LineID   string `json:"line_id"`
			PayoutID string `json:"payout_id"`
			Currency string `json:"currency"`
			Amount   int64  `json:"amount"`
		}
		TotalAmount int64 `json:"total_amount"`
		Currency    string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK || len(out.Items) != 1 ||
		out.Items[0].LineID != id.String() || out.Items[0].PayoutID != p.ID.String() || out.Items[0].Currency != "TRY" ||
		out.TotalAmount != ls[0].Amount || out.Currency != "TRY" {
		t.Fatalf("adminListPayeeReceivables = %d %s", rec.Code, rec.Body)
	}
}
