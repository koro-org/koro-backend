package payments_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// paidOrder is an order of company_a paid in full by one matched transfer:
// the booth line of club_a's payee (1,200,000: seller 1,080,000, commission
// 100,000, its VAT 20,000) and, with spec.platformLine, Koro's own line.
type paidOrder struct {
	placed
	payment payments.Payment
}

// paid places spec's order and pays it with a transfer of amount (the
// order's payable when 0) on acct, matched by finance_admin.
func (e *env) paid(t *testing.T, acct payments.CollectionAccount, spec orderSpec, amount int64) paidOrder {
	t.Helper()
	p := e.order(t, spec)
	if amount == 0 {
		amount = p.order.PayableAmount
	}
	res, err := e.match(t, e.transfer(t, acct, amount, p.order.OrderNo, true), p, "")
	if err != nil || !res.Order.Paid {
		t.Fatalf("match = %+v, %v", res, err)
	}
	pay, err := e.store.GetPayment(t.Context(), res.Payment.PaymentID, false)
	if err != nil {
		t.Fatal(err)
	}
	return paidOrder{placed: p, payment: pay}
}

// lineRefund is a goodwill refund of amount of line from payment pay.
func lineRefund(pay uuid.UUID, line uuid.UUID, amount int64) payments.RefundInput {
	return payments.RefundInput{PaymentID: pay, OrderLineID: &line, Amount: amount, Reason: payments.RefundReasonGoodwill,
		ReasonNote: "Etkinlik ertelendi", Method: payments.RefundMethodBankEFT, TargetIBAN: ibanSender}
}

// request asks for a refund as persona.
func (e *env) request(t *testing.T, persona string, in payments.RefundInput) (payments.Refund, error) {
	t.Helper()
	a := actor(t, persona)
	return e.svc.CreateRefund(ctxOf(t, a), a, in)
}

// approve approves refund id as persona with the right proof.
func (e *env) approve(t *testing.T, persona string, id uuid.UUID) (payments.Refund, error) {
	t.Helper()
	a := actor(t, persona)
	return e.svc.ApproveRefund(ctxOf(t, a), a, id, good)
}

// completeAs completes refund id as persona with a fresh refund receipt.
func (e *env) completeAs(t *testing.T, persona string, id uuid.UUID) (payments.RefundResult, error) {
	t.Helper()
	a := actor(t, persona)
	return e.svc.CompleteRefund(ctxOf(t, a), a, id, payments.RefundReceipt{BankReference: fmt.Sprintf("EFT-%d", seq.Add(1)),
		ReceiptFileID: e.files.add(files.PurposeRefundReceipt)})
}

// approved requests a refund as finance_admin_2 and approves it as finance_admin.
func (e *env) approved(t *testing.T, in payments.RefundInput) payments.Refund {
	t.Helper()
	r, err := e.request(t, "finance_admin_2", in)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if r, err = e.approve(t, "finance_admin", r.ID); err != nil || r.Status != payments.RefundStatusApproved {
		t.Fatalf("approve = %+v, %v", r, err)
	}
	return r
}

// refundTxns returns the refund_completed transactions of the journal.
func (e *env) refundTxns(t *testing.T) []ledger.Posted {
	t.Helper()
	ps, err := e.ledStore.List(t.Context(), ledger.JournalFilter{Kind: ledger.KindRefundCompleted}, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func payeeBalance(t *testing.T, e *env) int64 {
	t.Helper()
	b, err := e.ledger.PayeeBalance(t.Context(), id("payee_club_a"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCreateRefund_IdempotencyReplay (04 §1.8: adminCreateRefund, money
// out): the same Idempotency-Key with the same body answers the first
// refund again — one row, one audit record — and with another body 422
// idempotency.key_reused; another key is another request.
func TestCreateRefund_IdempotencyReplay(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	fin := actor(t, "finance_admin")
	key := idempotency.Key{Scope: fin.UserID.String() + ":POST /admin/refunds", Value: "refund-key-1", Status: 201}
	ctx := idempotency.WithKey(ctxOf(t, fin), key)
	in := lineRefund(po.payment.ID, po.lines[0].ID, 200_000)
	first, err := e.svc.CreateRefund(ctx, fin, in)
	if err != nil || first.Status != payments.RefundStatusRequested || first.RequestedBy != fin.UserID || first.Amount != 200_000 ||
		first.TargetIBAN == nil || *first.TargetIBAN != ibanSender || first.ReasonNote == nil {
		t.Fatalf("create = %+v, %v", first, err)
	}
	again, err := e.svc.CreateRefund(ctx, fin, in)
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay = %+v, %v; want refund %s", again, err, first.ID)
	}
	if n := len(e.mem.Refunds()); n != 1 {
		t.Fatalf("%d refunds after the replay", n)
	}
	if n := len(e.audit.of(authz.PaymentsRefundCreate)); n != 1 {
		t.Fatalf("%d audit records after the replay", n)
	}
	other := in
	other.Amount = 300_000
	if _, err := e.svc.CreateRefund(ctx, fin, other); !errors.Is(err, idempotency.ErrKeyReused) {
		t.Fatalf("same key, other body = %v", err)
	}
	ctx2 := idempotency.WithKey(ctxOf(t, fin), idempotency.Key{Scope: key.Scope, Value: "refund-key-2", Status: 201})
	second, err := e.svc.CreateRefund(ctx2, fin, in)
	if err != nil || second.ID == first.ID {
		t.Fatalf("another key = %+v, %v", second, err)
	}
	// The audit record carries the masked IBAN only.
	ev := e.audit.of(authz.PaymentsRefundCreate)[0]
	if ev.Metadata["target_iban_masked"] == ibanSender || ev.Metadata["target_iban_masked"] == nil || ev.Reason != "Etkinlik ertelendi" {
		t.Fatalf("audit = %+v", ev)
	}
}

// TestApproveRefund_Requester_403FourEyes (03 §6.1 "İade"): the requester
// never approves (403 authz.four_eyes_violation, nothing changes), a
// platform admin never approves (fin only), another finance admin does —
// once; the store refuses an approval by the requester too
// (refunds_four_eyes_chk).
func TestApproveRefund_Requester_403FourEyes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	r, err := e.request(t, "finance_admin", lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.approve(t, "finance_admin", r.ID); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("requester approves = %v", err)
	}
	if _, err := e.approve(t, "platform_admin", r.ID); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform admin approves = %v", err)
	}
	if _, err := e.approve(t, "student", r.ID); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("student approves = %v", err)
	}
	got, _ := e.store.GetRefund(t.Context(), r.ID, false)
	if got.Status != payments.RefundStatusRequested || got.ApprovedBy != nil {
		t.Fatalf("after the refusals: %+v", got)
	}
	self := got
	at := t0
	self.Status, self.ApprovedBy, self.ApprovedAt = payments.RefundStatusApproved, &got.RequestedBy, &at
	if _, err := e.store.UpdateRefund(t.Context(), self, payments.RefundStatusRequested); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("store approval by the requester = %v", err)
	}
	ok, err := e.approve(t, "finance_admin_2", r.ID)
	if err != nil || ok.Status != payments.RefundStatusApproved || ok.ApprovedBy == nil || *ok.ApprovedBy != id("finance_admin_2") {
		t.Fatalf("second eye = %+v, %v", ok, err)
	}
	if _, err := e.approve(t, "finance_admin_2", r.ID); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("second approval = %v", err)
	}
	// A platform admin's request is approved by a finance admin.
	pr, err := e.request(t, "platform_admin", lineRefund(po.payment.ID, po.lines[0].ID, 1))
	if err == nil {
		t.Fatalf("a request beyond the line (the approved refund holds it all) = %+v", pr)
	}
	if !errors.Is(err, payments.ErrRefundExceedsPaid) {
		t.Fatalf("request beyond the line = %v", err)
	}
}

// TestApproveRefund_RequiresReauth (03 §6.2): the approval carries the
// re-authentication proof — none 403 identity.reauth_required, a wrong one
// 403 identity.reauth_failed, and the refund stays requested; the audit
// record of the approval names the proof's kind.
func TestApproveRefund_RequiresReauth(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	r, err := e.request(t, "finance_admin_2", lineRefund(po.payment.ID, po.lines[0].ID, 600_000))
	if err != nil {
		t.Fatal(err)
	}
	fin := actor(t, "finance_admin")
	if _, err := e.svc.ApproveRefund(ctxOf(t, fin), fin, r.ID, identity.ReauthProof{}); !errors.Is(err, identity.ErrReauthRequired) {
		t.Fatalf("no proof = %v", err)
	}
	if _, err := e.svc.ApproveRefund(ctxOf(t, fin), fin, r.ID, identity.ReauthProof{Password: "wrong-password-123"}); !errors.Is(err, identity.ErrReauthFailed) {
		t.Fatalf("wrong proof = %v", err)
	}
	if got, _ := e.store.GetRefund(t.Context(), r.ID, false); got.Status != payments.RefundStatusRequested {
		t.Fatalf("after the failed proofs: %s", got.Status)
	}
	if n := len(e.audit.of(authz.PaymentsRefundApprove)); n != 0 {
		t.Fatalf("%d approval audit records before the right proof", n)
	}
	if _, err := e.svc.ApproveRefund(ctxOf(t, fin), fin, r.ID, good); err != nil {
		t.Fatal(err)
	}
	evs := e.audit.of(authz.PaymentsRefundApprove)
	if len(evs) != 1 || evs[0].Metadata["reauth"] != "password" || evs[0].Metadata["requested_by"] != id("finance_admin_2").String() {
		t.Fatalf("approval audit = %+v", evs)
	}
}

// TestCompleteRefund_WithoutReceipt_422: the EFT receipt is required — no
// file, an unknown one or a file of another purpose is 422, a missing bank
// reference too; the refund stays approved and nothing is posted.
func TestCompleteRefund_WithoutReceipt_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
	fin := actor(t, "finance_admin")
	for name, in := range map[string]payments.RefundReceipt{
		"no receipt":       {BankReference: "EFT-1"},
		"unknown receipt":  {BankReference: "EFT-1", ReceiptFileID: uuid.NewV7()},
		"transfer receipt": {BankReference: "EFT-1", ReceiptFileID: e.files.add(files.PurposeTransferReceipt)},
		"no bank ref":      {ReceiptFileID: e.files.add(files.PurposeRefundReceipt)},
	} {
		if _, err := e.svc.CompleteRefund(ctxOf(t, fin), fin, r.ID, in); !errors.Is(err, problem.ErrValidation) {
			t.Errorf("%s: %v, want 422 validation_failed", name, err)
		}
	}
	got, _ := e.store.GetRefund(t.Context(), r.ID, false)
	if got.Status != payments.RefundStatusApproved || got.ReceiptFileID != nil || len(e.refundTxns(t)) != 0 {
		t.Fatalf("after the refusals: %+v, %d refund transactions", got, len(e.refundTxns(t)))
	}
	res, err := e.completeAs(t, "finance_admin", r.ID)
	if err != nil || res.Refund.Status != payments.RefundStatusCompleted || res.Refund.ReceiptFileID == nil || res.Refund.BankReference == nil {
		t.Fatalf("complete = %+v, %v", res, err)
	}
	if mod, owner := e.files.owner(*res.Refund.ReceiptFileID); mod != "payments" || owner != r.ID {
		t.Fatalf("receipt attached to %s/%s", mod, owner)
	}
}

// TestCompleteRefund_ExceedsPaid_422: the completion is the authoritative
// limit. A refund that slipped past the request's limit (two requests that
// raced, seeded here) is refused at completion with 422
// orders.refund_exceeds_paid once the line is refunded in full: the refund
// stays approved, the payment, the order and the ledger are untouched. A
// request above what Koro holds of the line is 422 at once.
func TestCompleteRefund_ExceedsPaid_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 1_250_000) // 50,000 overpayment
	line := po.lines[0].ID
	if _, err := e.request(t, "finance_admin", lineRefund(po.payment.ID, line, 1_200_001)); !errors.Is(err, payments.ErrRefundExceedsPaid) {
		t.Fatalf("request above the line = %v", err)
	}
	first := e.approved(t, lineRefund(po.payment.ID, line, 1_200_000))
	at := t0
	raced := payments.Refund{ID: uuid.NewV7(), PaymentID: po.payment.ID, OrderID: po.order.ID, OrderLineID: &line, Amount: 40_000,
		Reason: payments.RefundReasonGoodwill, Status: payments.RefundStatusApproved, RequestedBy: id("finance_admin_2"),
		ApprovedBy: &[]uuid.UUID{id("finance_admin")}[0], ApprovedAt: &at, Method: payments.RefundMethodBankEFT, TargetIBAN: ptr(ibanSender)}
	if err := e.mem.SeedRefund(raced); err != nil {
		t.Fatal(err)
	}
	if _, err := e.completeAs(t, "finance_admin", first.ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.completeAs(t, "finance_admin", raced.ID)
	if !errors.Is(err, payments.ErrRefundExceedsPaid) {
		t.Fatalf("complete beyond the line = %v", err)
	}
	var pe *problem.Error
	if !errors.As(err, &pe) || pe.Status() != 422 || pe.Code() != "orders.refund_exceeds_paid" {
		t.Fatalf("problem = %+v", pe)
	}
	got, _ := e.store.GetRefund(t.Context(), raced.ID, false)
	pay, _ := e.store.GetPayment(t.Context(), po.payment.ID, false)
	o, _ := e.ordersM.Orders.ForPayment(t.Context(), po.order.ID)
	if got.Status != payments.RefundStatusApproved || pay.RefundedAmount != 1_200_000 || o.RefundedAmount != 1_200_000 ||
		len(e.refundTxns(t)) != 1 {
		t.Fatalf("after the refusal: refund %s, payment refunded %d, order refunded %d, %d refund transactions",
			got.Status, pay.RefundedAmount, o.RefundedAmount, len(e.refundTxns(t)))
	}
}

// TestRefund_EmitsPaymentRefunded (01 §6.2): the completion writes
// payment.refunded (O) with the refund, payment, order, line and amount;
// the worker tells the buyer's admins and, for a payee line, the seller's.
func TestRefund_EmitsPaymentRefunded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
	if n := len(e.events(domainevent.TopicPaymentRefunded)); n != 0 {
		t.Fatalf("%d payment.refunded before the completion", n)
	}
	if _, err := e.completeAs(t, "finance_admin", r.ID); err != nil {
		t.Fatal(err)
	}
	evs := e.events(domainevent.TopicPaymentRefunded)
	if len(evs) != 1 {
		t.Fatalf("%d payment.refunded", len(evs))
	}
	p, err := domainevent.Decode[domainevent.PaymentRefunded](evs[0])
	if err != nil || p != (domainevent.PaymentRefunded{RefundID: r.ID, PaymentID: po.payment.ID, OrderID: po.order.ID,
		OrderLineID: po.lines[0].ID, Amount: 1_200_000}) {
		t.Fatalf("payload = %+v, %v", p, err)
	}
	e.deliver(t, domainevent.TopicPaymentRefunded)
	if got := e.sender.to("payments.refund_completed", "in_app"); !containsAll(got, id("company_admin")) {
		t.Fatalf("buyer notices to %v", got)
	}
	if got := e.sender.to("payments.order_line_refunded", "in_app"); !containsAll(got, id("club_admin")) || slices.Contains(got, id("company_admin")) {
		t.Fatalf("seller notices to %v", got)
	}
	// A replayed task sends nothing new (dedupe key).
	n := len(e.sender.sent)
	e.deliver(t, domainevent.TopicPaymentRefunded)
	if len(e.sender.sent) != n {
		t.Fatal("the replayed notice task sent again")
	}
}

// TestCompleteRefund_LineInTwoParts_ExactShares: a line refunded in two
// parts (from two payments of the order) gives back exactly its shares —
// the first split in proportion, the second the remainder against the
// ledger's refunded shares — so after the full refund the line's accounts
// are back to zero, the payee is owed nothing for it, the order is
// refunded and the payments too.
func TestCompleteRefund_LineInTwoParts_ExactShares(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	r1, err := e.match(t, e.transfer(t, acct, 700_001, p.order.OrderNo, true), p, "")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.match(t, e.transfer(t, acct, 499_999, p.order.OrderNo, true), p, "")
	if err != nil || !r2.Order.Paid {
		t.Fatalf("second = %+v, %v", r2, err)
	}
	line := p.lines[0].ID
	if b := payeeBalance(t, e); b != 1_080_000 {
		t.Fatalf("payee balance before %d", b)
	}
	a := e.approved(t, lineRefund(r1.Payment.PaymentID, line, 700_001))
	if _, err := e.completeAs(t, "finance_admin", a.ID); err != nil {
		t.Fatal(err)
	}
	o, _ := e.ordersM.Orders.ForPayment(t.Context(), p.order.ID)
	if o.Status != orders.StatusPartiallyRefunded || o.RefundedAmount != 700_001 {
		t.Fatalf("order after the first part = %+v", o)
	}
	b := e.approved(t, lineRefund(r2.Payment.PaymentID, line, 499_999))
	res, err := e.completeAs(t, "finance_admin", b.ID)
	if err != nil || res.Unallocated != 0 || res.Clawback != 0 {
		t.Fatalf("second part = %+v, %v", res, err)
	}
	shares, err := e.ledMod.RefundedShares(t.Context(), line)
	if err != nil || shares != (ledger.LineShares{Seller: 1_080_000, Commission: 100_000, CommissionVAT: 20_000}) {
		t.Fatalf("refunded shares = %+v, %v", shares, err)
	}
	bal := balances(e.journal(t))
	payee := "seller_payable:" + id("payee_club_a").String()
	if bal[payee] != 0 || bal["platform_commission_revenue"] != 0 || bal["vat_output"] != 0 || bal["koro_bank"] != 0 ||
		bal["buyer_advances"] != 0 {
		t.Fatalf("balances after the full refund = %v", bal)
	}
	o, _ = e.ordersM.Orders.ForPayment(t.Context(), p.order.ID)
	if o.Status != orders.StatusRefunded || o.RefundedAmount != 1_200_000 {
		t.Fatalf("order = %+v", o)
	}
	for _, pid := range []uuid.UUID{r1.Payment.PaymentID, r2.Payment.PaymentID} {
		if pay, _ := e.store.GetPayment(t.Context(), pid, false); pay.Status != payments.PaymentRefunded || pay.RefundedAmount != pay.Amount {
			t.Fatalf("payment %s = %+v", pid, pay)
		}
	}
}

// TestRefund_PaidOutLine_ClawsBackFromPayee (06 H-59, T-231): a line whose
// payout was paid is refunded under the same four eyes; its seller part is
// claimed back from the payee (payee_receivable naming the line, no
// payee) instead of turning the payee balance negative; commission and VAT
// come back as usual.
func TestRefund_PaidOutLine_ClawsBackFromPayee(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	ctx := t.Context()
	line := po.lines[0].ID
	l, err := e.ordStore.GetLine(ctx, line, false)
	if err != nil {
		t.Fatal(err)
	}
	now, via := e.clk.Now(), orders.ViaAdmin
	done := l
	done.FulfillmentStatus, done.FulfilledAt, done.FulfilledVia = orders.FulfillFulfilled, &now, &via
	if _, err := e.ordStore.UpdateLine(ctx, done, l.FulfillmentStatus); err != nil {
		t.Fatal(err)
	}
	payout := uuid.NewV7()
	if n, err := e.ordStore.MarkLinesPaidOut(ctx, []uuid.UUID{line}, payout, now); err != nil || n != 1 {
		t.Fatalf("mark paid out = %d, %v", n, err)
	}
	tx, err := ledger.PayoutPaid(ledger.ModelA, payout, id("payee_club_a"), 1_080_000, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ledger.Post(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if b := payeeBalance(t, e); b != 0 {
		t.Fatalf("payee balance after the payout %d", b)
	}
	r := e.approved(t, lineRefund(po.payment.ID, line, 1_200_000))
	res, err := e.completeAs(t, "finance_admin", r.ID)
	if err != nil || res.Clawback != 1_080_000 {
		t.Fatalf("complete = %+v, %v", res, err)
	}
	if b := payeeBalance(t, e); b != 0 {
		t.Fatalf("payee balance after the clawback %d", b)
	}
	txs := e.refundTxns(t)
	if len(txs) != 1 {
		t.Fatalf("%d refund transactions", len(txs))
	}
	var receivable int64
	for _, en := range txs[0].Entries {
		switch en.Account {
		case ledger.AccountSellerPayable:
			t.Fatalf("a paid-out line's refund debits seller_payable: %+v", en)
		case ledger.AccountPayeeReceivable:
			if en.PayeeID != nil || en.OrderLineID == nil || *en.OrderLineID != line {
				t.Fatalf("receivable entry %+v", en)
			}
			receivable += en.Amount
		}
	}
	if receivable != 1_080_000 {
		t.Fatalf("payee_receivable %d", receivable)
	}
	if evs := e.audit.of(authz.PaymentsRefundComplete); len(evs) != 1 || evs[0].Metadata["clawback_amount"] != int64(1_080_000) {
		t.Fatalf("completion audit = %+v", evs)
	}
}

// openPayout reports the given line in a pending payout (ledger's
// LinesInPayouts) once set.
type openPayout struct {
	payments.LedgerLines
	line *uuid.UUID
}

func (o openPayout) LinesInPayouts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out, err := o.LedgerLines.LinesInPayouts(ctx, ids)
	if err != nil || o.line == nil {
		return out, err
	}
	if slices.Contains(ids, *o.line) {
		out[*o.line] = uuid.NewV7()
	}
	return out, nil
}

// TestRefund_LineInOpenPayout_409 (06 H-59): a payee line in a payout that
// waits for its approval or its transfer is not refunded — neither
// requested nor completed (409 payments.line_in_payout); the refund waits
// until the payout is paid (claw back) or cancelled.
func TestRefund_LineInOpenPayout_409(t *testing.T) {
	t.Parallel()
	held := &openPayout{}
	e := newEnv(t, withLedgerLines(func(l payments.LedgerLines) payments.LedgerLines { held.LedgerLines = l; return held }))
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	line := po.lines[0].ID
	r := e.approved(t, lineRefund(po.payment.ID, line, 600_000))
	held.line = &line
	if _, err := e.request(t, "finance_admin", lineRefund(po.payment.ID, line, 1)); !errors.Is(err, payments.ErrLineInPayout) {
		t.Fatalf("request = %v", err)
	}
	if _, err := e.completeAs(t, "finance_admin", r.ID); !errors.Is(err, payments.ErrLineInPayout) {
		t.Fatalf("complete = %v", err)
	}
	if got, _ := e.store.GetRefund(t.Context(), r.ID, false); got.Status != payments.RefundStatusApproved || len(e.refundTxns(t)) != 0 {
		t.Fatalf("after the refusal: %s", got.Status)
	}
	held.line = nil
	if _, err := e.completeAs(t, "finance_admin", r.ID); err != nil {
		t.Fatalf("complete once the payout is gone: %v", err)
	}
}

// TestCreateRefund_Pools: each reason draws from its pool. An overpayment
// is the transfer's unapplied part (the match already opened its refund,
// so a second one finds nothing left); an order still awaiting payment is
// 409; a paid order needs a line; an unpaid order's money is refunded as a
// whole (cancelled after a partial payment: buyer_advances back, the order
// refunded); a direct_confirmed payment never gives money back.
func TestCreateRefund_Pools(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	po := e.paid(t, acct, orderSpec{}, 1_500_000) // 300,000 overpayment, its refund requested by the match
	over := payments.RefundInput{PaymentID: po.payment.ID, Amount: 1, Reason: payments.RefundReasonOverpayment, TargetIBAN: ibanSender}
	if _, err := e.request(t, "finance_admin_2", over); !errors.Is(err, payments.ErrRefundExceedsPaid) {
		t.Fatalf("a second overpayment refund = %v", err)
	}
	noLine := lineRefund(po.payment.ID, po.lines[0].ID, 10)
	noLine.OrderLineID = nil
	if _, err := e.request(t, "finance_admin_2", noLine); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("a paid order without a line = %v", err)
	}
	withLine := over
	withLine.OrderLineID = &po.lines[0].ID
	if _, err := e.request(t, "finance_admin_2", withLine); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("an overpayment of a line = %v", err)
	}
	bad := lineRefund(po.payment.ID, po.lines[0].ID, 10)
	bad.TargetIBAN = "TR00 0000"
	if _, err := e.request(t, "finance_admin_2", bad); !errors.Is(err, payments.ErrIBANInvalid) {
		t.Fatalf("a malformed IBAN = %v", err)
	}
	bad = lineRefund(po.payment.ID, po.lines[0].ID, 10)
	bad.Method = payments.RefundMethodIyzico
	if _, err := e.request(t, "finance_admin_2", bad); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("an iyzico refund = %v", err)
	}
	if _, err := e.request(t, "finance_admin_2", lineRefund(uuid.NewV7(), po.lines[0].ID, 10)); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("an unknown payment = %v", err)
	}
	other := e.order(t, orderSpec{})
	if _, err := e.request(t, "finance_admin_2", lineRefund(po.payment.ID, other.lines[0].ID, 10)); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("a line of another order = %v", err)
	}
	// The match's overpayment refund completes from buyer_advances; the
	// order is untouched.
	matchRefund := e.mem.Refunds()[0]
	if _, err := e.approve(t, "finance_admin_2", matchRefund.ID); err != nil { // the match's requester is finance_admin
		t.Fatal(err)
	}
	res, err := e.completeAs(t, "finance_admin", matchRefund.ID)
	if err != nil || res.Unallocated != 300_000 {
		t.Fatalf("overpayment completion = %+v, %v", res, err)
	}
	if o, _ := e.ordersM.Orders.ForPayment(t.Context(), po.order.ID); o.RefundedAmount != 0 || o.Status != orders.StatusPaid {
		t.Fatalf("the order after an overpayment refund = %+v", o)
	}
	if pay, _ := e.store.GetPayment(t.Context(), po.payment.ID, false); pay.Status != payments.PaymentPartiallyRefunded || pay.RefundedAmount != 300_000 {
		t.Fatalf("payment = %+v", pay)
	}

	// A partial payment of an order still waiting: 409; once the order is
	// cancelled its money goes back as a whole.
	part := e.order(t, orderSpec{})
	pr, err := e.match(t, e.transfer(t, acct, 400_000, part.order.OrderNo, true), part, "")
	if err != nil || pr.Order.Paid {
		t.Fatalf("partial = %+v, %v", pr, err)
	}
	whole := payments.RefundInput{PaymentID: pr.Payment.PaymentID, Amount: 400_000, Reason: payments.RefundReasonOther, TargetIBAN: ibanSender}
	if _, err := e.request(t, "finance_admin_2", whole); !errors.Is(err, orders.ErrInvalidState) {
		t.Fatalf("an order awaiting payment = %v", err)
	}
	// Cancelled (orders' CancelOrder releases the catalog units this env
	// has no catalog for): the row as orders writes it.
	cur, err := e.ordStore.GetOrder(t.Context(), part.order.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	cancelledAt, why := e.clk.Now(), "buyer_cancelled"
	cur.Status, cur.CancelledAt, cur.CancelReason = orders.StatusCancelled, &cancelledAt, &why
	if _, err := e.ordStore.UpdateOrder(t.Context(), cur, orders.StatusAwaitingPayment); err != nil {
		t.Fatal(err)
	}
	lined := lineRefund(pr.Payment.PaymentID, part.lines[0].ID, 400_000)
	if _, err := e.request(t, "finance_admin_2", lined); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("a line of an unpaid order = %v", err)
	}
	whole.Amount = 400_001
	if _, err := e.request(t, "finance_admin_2", whole); !errors.Is(err, payments.ErrRefundExceedsPaid) {
		t.Fatalf("more than was paid = %v", err)
	}
	whole.Amount = 400_000
	cr := e.approved(t, whole)
	if res, err := e.completeAs(t, "finance_admin", cr.ID); err != nil || res.Unallocated != 400_000 {
		t.Fatalf("cancelled order's refund = %+v, %v", res, err)
	}
	if o, _ := e.ordersM.Orders.ForPayment(t.Context(), part.order.ID); o.Status != orders.StatusCancelled || o.RefundedAmount != 400_000 {
		t.Fatalf("cancelled order = %+v", o)
	}
	bal := balances(e.journal(t))
	if bal["buyer_advances"] != -1_200_000+1_200_000 || bal["koro_bank"] != 1_200_000 {
		t.Fatalf("balances = %v", bal)
	}
}

// TestRejectRefund: a finance admin — the requester included — rejects a
// request with a note; an approved or rejected refund is 409; a rejected
// refund frees its money for a new request.
func TestRejectRefund(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	r, err := e.request(t, "finance_admin", lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
	if err != nil {
		t.Fatal(err)
	}
	fin := actor(t, "finance_admin")
	if _, err := e.svc.RejectRefund(ctxOf(t, fin), fin, r.ID, ""); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("no note = %v", err)
	}
	pad := actor(t, "platform_admin")
	if _, err := e.svc.RejectRefund(ctxOf(t, pad), pad, r.ID, "Hayır"); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform admin rejects = %v", err)
	}
	got, err := e.svc.RejectRefund(ctxOf(t, fin), fin, r.ID, "Tutar hatalı")
	if err != nil || got.Status != payments.RefundStatusRejected || got.RejectedBy == nil || *got.RejectedBy != fin.UserID ||
		got.RejectionReason == nil {
		t.Fatalf("reject = %+v, %v", got, err)
	}
	if _, err := e.approve(t, "finance_admin_2", r.ID); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("approve a rejected refund = %v", err)
	}
	if _, err := e.svc.RejectRefund(ctxOf(t, fin), fin, r.ID, "Tekrar"); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("reject again = %v", err)
	}
	a := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
	if _, err := e.svc.RejectRefund(ctxOf(t, fin), fin, a.ID, "Geç kaldı"); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("reject an approved refund = %v", err)
	}
}

// TestListRefundsDue: a cancelled line of a paid order waits for a refund;
// the queue shows it with the payment that can fund it, minus what open
// refunds already cover, and drops it once a request covers it all.
func TestListRefundsDue(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	ctx := t.Context()
	l, _ := e.ordStore.GetLine(ctx, po.lines[0].ID, false)
	cancelled := l
	cancelled.FulfillmentStatus = orders.FulfillCancelled
	if _, err := e.ordStore.UpdateLine(ctx, cancelled, l.FulfillmentStatus); err != nil {
		t.Fatal(err)
	}
	fin := actor(t, "finance_admin")
	due, err := e.svc.ListRefundsDue(ctxOf(t, fin), fin, 0)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %+v, %v", due, err)
	}
	d := due[0]
	if d.OrderID != po.order.ID || d.OrderNo != po.order.OrderNo || d.LineID == nil || *d.LineID != l.ID || d.Amount != 1_200_000 ||
		len(d.Payments) != 1 || d.Payments[0] != (payments.FundingPayment{PaymentID: po.payment.ID, Available: 1_500_000}) {
		t.Fatalf("due row = %+v", d)
	}
	if _, err := e.request(t, "finance_admin", lineRefund(po.payment.ID, l.ID, 500_000)); err != nil {
		t.Fatal(err)
	}
	due, _ = e.svc.ListRefundsDue(ctxOf(t, fin), fin, 0)
	if len(due) != 1 || due[0].Amount != 700_000 || due[0].Payments[0].Available != 1_000_000 {
		t.Fatalf("due after a partial request = %+v", due)
	}
	if _, err := e.request(t, "finance_admin", lineRefund(po.payment.ID, l.ID, 700_000)); err != nil {
		t.Fatal(err)
	}
	if due, _ = e.svc.ListRefundsDue(ctxOf(t, fin), fin, 0); len(due) != 0 {
		t.Fatalf("due once covered = %+v", due)
	}
	st := actor(t, "student")
	if _, err := e.svc.ListRefundsDue(ctxOf(t, st), st, 0); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("student = %v", err)
	}
}

// TestPayments_ListAndGet: the payment list (newest first, filters, pages)
// and a payment with its order's reference, splits and refunds.
func TestPayments_ListAndGet(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	first := e.paid(t, acct, orderSpec{platformLine: true}, 0)
	e.clk.Advance(time.Minute)
	second := e.paid(t, acct, orderSpec{}, 0)
	pad := actor(t, "platform_admin")
	pg, err := e.svc.ListPayments(ctxOf(t, pad), pad, payments.PaymentQuery{Limit: 1})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].ID != second.payment.ID || pg.Next == nil {
		t.Fatalf("page 1 = %+v, %v", pg, err)
	}
	pg, err = e.svc.ListPayments(ctxOf(t, pad), pad, payments.PaymentQuery{Limit: 1, After: pg.Next})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].ID != first.payment.ID || pg.Next != nil {
		t.Fatalf("page 2 = %+v, %v", pg, err)
	}
	pg, _ = e.svc.ListPayments(ctxOf(t, pad), pad, payments.PaymentQuery{Filter: payments.PaymentFilter{OrderID: &first.order.ID}})
	if len(pg.Items) != 1 || pg.Items[0].ID != first.payment.ID {
		t.Fatalf("by order = %+v", pg)
	}
	if _, err := e.svc.ListPayments(ctxOf(t, pad), pad, payments.PaymentQuery{Filter: payments.PaymentFilter{Status: "lost"}}); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("bad status = %v", err)
	}
	if _, err := e.request(t, "finance_admin", lineRefund(first.payment.ID, first.lines[0].ID, 1000)); err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.GetPaymentView(ctxOf(t, pad), pad, first.payment.ID)
	if err != nil || v.OrderNo != first.order.OrderNo || len(v.Splits) != 2 || len(v.Refunds) != 1 {
		t.Fatalf("view = %+v, %v", v, err)
	}
	if _, err := e.svc.GetPaymentView(ctxOf(t, pad), pad, uuid.NewV7()); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	rp, err := e.svc.ListRefunds(ctxOf(t, pad), pad, payments.RefundQuery{Filter: payments.RefundFilter{Status: payments.RefundStatusRequested,
		PaymentID: &first.payment.ID}})
	if err != nil || len(rp.Items) != 1 {
		t.Fatalf("refunds = %+v, %v", rp, err)
	}
	st := actor(t, "student")
	if _, err := e.svc.ListPayments(ctxOf(t, st), st, payments.PaymentQuery{}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("student = %v", err)
	}
}

// TestRefund_PlatformLine_RevokesRights: Koro's own platform-native line
// was allocated as Koro's sale (BE-067, T-293): its refund takes the sale
// and its VAT back (nothing from buyer_advances) and, refunded in full,
// revokes its rights.
func TestRefund_PlatformLine_RevokesRights(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	r := e.approved(t, lineRefund(po.payment.ID, po.lines[1].ID, 300_000))
	res, err := e.completeAs(t, "finance_admin", r.ID)
	if err != nil || res.Unallocated != 0 || res.Clawback != 0 {
		t.Fatalf("complete = %+v, %v", res, err)
	}
	rights, _ := e.entStore.BySource(t.Context(), entitlements.SourceOrderLine, po.lines[1].ID)
	if len(rights) != 1 || rights[0].Status == entitlements.StatusActive {
		t.Fatalf("rights = %+v", rights)
	}
	b := balances(e.journal(t))
	if b["platform_sales_revenue"] != 0 || b["vat_output"] != -20_000 || b["buyer_advances"] != 0 || b["koro_bank"] != 1_200_000 {
		t.Fatalf("balances after the refund of Koro's line = %v", b)
	}
}

// TestRefunds_KVKKExport: the staff references of refunds are exported and
// retained on erasure (financial records).
func TestRefunds_KVKKExport(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 1000))
	sec, err := e.mod.Privacy.Export(t.Context(), id("finance_admin_2"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range sec.Items {
		if it["record"] == "refund" && it["refund_id"] == r.ID.String() {
			found = slices.Equal(it["roles"].([]string), []string{"requested"})
		}
	}
	if !found {
		t.Fatalf("export = %+v", sec.Items)
	}
	res, err := e.mod.Privacy.Erase(t.Context(), id("finance_admin_2"))
	if err != nil || res.Retained < 1 || res.RetainedReason == "" {
		t.Fatalf("erase = %+v, %v", res, err)
	}
}
