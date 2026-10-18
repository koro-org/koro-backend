package payments_test

import (
	"errors"
	"testing"

	"koro/internal/files"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
)

// exerciseModelC follows real capture/refund services with a nonzero fee.
// The direct transfer is evidence only, never Koro bank or seller debt.
func exerciseModelC(t *testing.T, e *env, directFirst bool) {
	t.Helper()
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{modelC: true})
	fin := actor(t, "finance_admin")
	ctx := idempotency.WithKey(ctxOf(t, fin), idempotency.Key{Scope: fin.UserID.String() + ":direct", Value: "model-c-confirm", Status: 200})
	in := payments.DirectInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo, EvidenceFileID: e.files.add(files.PurposeTransferReceipt)}
	var direct payments.DirectResult
	confirm := func() {
		var err error
		direct, err = e.svc.ConfirmDirectPayment(ctx, fin, in)
		if err != nil {
			t.Fatal(err)
		}
		again, err := e.svc.ConfirmDirectPayment(ctx, fin, in)
		if err != nil || again.Payment.PaymentID != direct.Payment.PaymentID {
			t.Fatalf("direct replay = %+v, %v", again, err)
		}
	}
	var fee payments.MatchResult
	capture := func() {
		var err error
		fee, err = e.match(t, e.transfer(t, acct, 120_000, p.order.OrderNo, true), p, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	if directFirst {
		confirm()
	} else {
		capture()
	}
	before, err := e.ordersM.Orders.ForPayment(t.Context(), p.order.ID)
	if err != nil || before.Status != orders.StatusAwaitingPayment {
		t.Fatalf("first condition = %+v, %v", before, err)
	}
	if directFirst {
		if len(e.journal(t)) != 0 || direct.Order.Paid {
			t.Fatal("direct evidence moved Koro money or paid early")
		}
		capture()
	} else {
		bs := balances(e.journal(t))
		if bs["buyer_advances"] != -120_000 || bs["platform_commission_revenue"] != 0 {
			t.Fatalf("fee awaiting confirmation = %v", bs)
		}
		confirm()
	}
	after, err := e.ordersM.Orders.ForPayment(t.Context(), p.order.ID)
	if err != nil || after.Status != orders.StatusPaid || after.PaidAmount != 120_000 {
		t.Fatalf("paid = %+v, %v", after, err)
	}
	if _, err := e.svc.ConfirmDirectPayment(ctxOf(t, fin), fin, in); !errors.Is(err, orders.ErrInvalidState) {
		t.Fatalf("duplicate evidence = %v", err)
	}
	if got := len(e.journal(t)); got != 2 {
		t.Fatalf("ledger txns = %d", got)
	}
	bs := balances(e.journal(t))
	for code, want := range map[string]int64{"koro_bank": 120_000, "buyer_advances": 0, "platform_commission_revenue": -100_000, "vat_output": -20_000} {
		if bs[code] != want {
			t.Fatalf("%s = %d want %d", code, bs[code], want)
		}
	}
	for _, tx := range e.journal(t) {
		for _, en := range tx.Entries {
			if en.PayeeID != nil || en.Account == ledger.AccountSellerPayable || en.Account == ledger.AccountSalesRevenue {
				t.Fatalf("C carries direct money: %+v", en)
			}
		}
	}
	evs := e.events(domainevent.TopicPaymentCaptured)
	paid := 0
	for _, ev := range evs {
		x, err := domainevent.Decode[domainevent.PaymentCaptured](ev)
		if err != nil {
			t.Fatal(err)
		}
		if x.OrderPaid {
			paid++
		}
	}
	if len(evs) != 2 || paid != 1 {
		t.Fatalf("capture events = %d, paid transitions = %d", len(evs), paid)
	}
	if _, err := e.request(t, "finance_admin_2", lineRefund(direct.Payment.PaymentID, p.lines[0].ID, 1)); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("direct refund = %v", err)
	}
	if _, err := e.request(t, "finance_admin_2", lineRefund(fee.Payment.PaymentID, p.lines[0].ID, 120_001)); !errors.Is(err, orders.ErrRefundExceedsPaid) {
		t.Fatalf("refund exceeds held fee = %v", err)
	}
	for _, amount := range []int64{1, 1, 119_998} {
		r := e.approved(t, lineRefund(fee.Payment.PaymentID, p.lines[0].ID, amount))
		result, err := e.completeAs(t, "finance_admin", r.ID)
		if err != nil || result.Unallocated != 0 || result.Clawback != 0 {
			t.Fatalf("fee reversal = %+v, %v", result, err)
		}
	}
	for code, amount := range balances(e.journal(t)) {
		if amount != 0 {
			t.Fatalf("after complete reversal %s = %d", code, amount)
		}
	}
	if got, _ := e.ordersM.Orders.ForPayment(t.Context(), p.order.ID); got.Status != orders.StatusRefunded || got.RefundedAmount != 120_000 {
		t.Fatalf("refunded order = %+v", got)
	}
	if ls, err := e.ordersM.Orders.PayableLines(t.Context(), *p.lines[0].PayeeID, e.clk.Now()); err != nil || len(ls) != 0 {
		t.Fatalf("C payout candidates = %v, %v", ls, err)
	}
}

func TestModelC_CaptureConfirmRefundBothOrders(t *testing.T) {
	t.Parallel()
	for _, first := range []bool{false, true} {
		name := "fee_first"
		if first {
			name = "direct_first"
		}
		t.Run(name, func(t *testing.T) { t.Parallel(); exerciseModelC(t, newEnv(t), first) })
	}
}

func TestModelC_CancelBeforeCompletionRefundsOnlyAdvance(t *testing.T) {
	t.Parallel()
	for _, direct := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed", true: "confirmed"}[direct], func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			p := e.order(t, orderSpec{modelC: true})
			fin := actor(t, "finance_admin")
			if direct {
				if _, err := e.svc.ConfirmDirectPayment(ctxOf(t, fin), fin, payments.DirectInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo, EvidenceFileID: e.files.add(files.PurposeTransferReceipt)}); err != nil {
					t.Fatal(err)
				}
			}
			amount := int64(120_000)
			if direct {
				amount = 60_000
			}
			fee, err := e.match(t, e.transfer(t, e.activeAccount(t, ibanKoro, true), amount, p.order.OrderNo, true), p, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.ordersM.Service.CancelOrder(ctxOf(t, fin), fin, p.order.ID, "Synthetic cancellation"); err != nil {
				t.Fatal(err)
			}
			r := e.approved(t, payments.RefundInput{PaymentID: fee.Payment.PaymentID, Amount: amount, Reason: payments.RefundReasonOther, TargetIBAN: ibanSender})
			result, err := e.completeAs(t, "finance_admin", r.ID)
			if err != nil || result.Unallocated != amount {
				t.Fatalf("advance refund=%+v,%v", result, err)
			}
			for code, amount := range balances(e.journal(t)) {
				if amount != 0 {
					t.Fatalf("cancel refund %s=%d", code, amount)
				}
			}
			got, err := e.ordStore.GetOrder(t.Context(), p.order.ID, false)
			if err != nil || (got.DirectConfirmedAt != nil) != direct || got.RefundedAmount != amount {
				t.Fatalf("cancelled order=%+v,%v", got, err)
			}
			if _, err := e.svc.ConfirmDirectPayment(ctxOf(t, fin), fin, payments.DirectInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo, EvidenceFileID: e.files.add(files.PurposeTransferReceipt)}); !errors.Is(err, orders.ErrInvalidState) {
				t.Fatalf("cancelled direct=%v", err)
			}
		})
	}
}
