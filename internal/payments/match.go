package payments

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/money"
)

// MatchTransfer implements Service (fin·k27; Idempotency-Key): the payment
// capture of 01 §6.3 "Havale eşleştirme". Order of checks: authz (403
// before reading) → body (422) → the transfer exists (404) → in ONE
// transaction (the Idempotency-Key with it, 06 H-5):
//
//  1. the transfer, locked: unmatched only (409
//     payments.transfer_already_matched / payments.invalid_state; a second
//     match of the same transfer waits on the lock and then gets 409);
//  2. the order (404 when unknown: a field error, the path is the
//     transfer's): its reference code is expected_order_no (422
//     payments.order_reference_mismatch), it waits for payment (409
//     orders.invalid_state) and for money from Koro (409 payments.nothing_due);
//  3. the ledger template of the order's model is built before any write
//     (422 ledger.model_not_enabled for models B and C, T-11);
//  4. the transfer is matched, the payment captured (bank_transfer, the
//     whole transfer amount), orders.ApplyPayment applies what the order
//     waits for (T-14: partial payments accumulate; a full one pays the
//     order and activates its rights);
//  5. the ledger posts payment_received (the whole amount) and, once the
//     order is paid, order_allocated of its lines — the payee lines and
//     Koro's own platform-native lines (Koro's sale and its VAT, BE-067)
//     together; the splits of a paid order are written;
//  6. the rest of an overpayment opens a refund request (overpayment,
//     requested; 03 k27: its approval takes four eyes, BE-043) to the
//     refund IBAN (the sender's when not given; 422 without one);
//  7. audit, payment.captured (O).
//
// Any error rolls everything back: the transfer stays unmatched. The
// commit error passes ledger.TranslateError (a deferred
// ledger_txn_balanced failure is ledger.unbalanced, T-44).
func (s *service) MatchTransfer(ctx context.Context, a authz.Actor, id uuid.UUID, in MatchInput) (MatchResult, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferMatch, adminSurface); err != nil {
		return MatchResult{}, err
	}
	if in.OrderID == uuid.Nil() {
		return MatchResult{}, fieldErr("body.order_id", "is required")
	}
	expected, ok := orders.NormalizeReference(in.ExpectedOrderNo)
	if !ok {
		return MatchResult{}, fieldErr("body.expected_order_no", "must be a reference code (KR + 8 characters)")
	}
	var refundIBAN string
	if in.RefundIBAN != "" {
		var err error
		if refundIBAN, err = cleanIBAN("body.refund_iban", in.RefundIBAN); err != nil {
			return MatchResult{}, err
		}
	}
	if _, err := s.d.Store.GetTransfer(ctx, id, false); err != nil {
		return MatchResult{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op                   string
		Transfer, Order      uuid.UUID
		Expected, RefundIBAN string
	}{"match_transfer", id, in.OrderID, expected, refundIBAN})
	if err != nil {
		return MatchResult{}, err
	}
	res, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (MatchResult, error) {
		return s.match(ctx, a, id, in.OrderID, expected, refundIBAN)
	})
	if err != nil {
		return MatchResult{}, ledger.TranslateError(err)
	}
	return res, nil
}

// match is the capture chain in the transaction carried by ctx.
func (s *service) match(ctx context.Context, a authz.Actor, id, orderID uuid.UUID, expected, refundIBAN string) (MatchResult, error) {
	t, err := s.d.Store.GetTransfer(ctx, id, true)
	if err != nil {
		return MatchResult{}, err
	}
	if t.Status != TransferUnmatched {
		return MatchResult{}, stateErr(t)
	}
	o, err := s.payableOrder(ctx, orderID, expected)
	if err != nil {
		return MatchResult{}, err
	}
	switch {
	case o.Status != orders.StatusAwaitingPayment:
		return MatchResult{}, orders.ErrInvalidState
	case o.DueAmount <= 0:
		return MatchResult{}, ErrNothingDue
	}
	now := s.now()
	paymentID := uuid.NewV7()
	received, err := ledger.PaymentReceived(ledger.Model(o.ModelCode), paymentID, t.Amount, now)
	if err != nil {
		return MatchResult{}, err // ledger.model_not_enabled: models B and C (T-11)
	}
	if refundIBAN, err = overpaymentIBAN(t, t.Amount-min(t.Amount, o.DueAmount), refundIBAN); err != nil {
		return MatchResult{}, err // fail before any write; checked again after the order is locked
	}

	by := a.UserID
	next := t
	next.Status, next.MatchedOrderID, next.MatchedBy, next.MatchedAt = TransferMatched, &o.ID, &by, &now
	matched, err := s.d.Store.UpdateTransfer(ctx, next, TransferUnmatched)
	if err != nil {
		return MatchResult{}, err
	}
	transferID := matched.ID
	p := Payment{
		ID: paymentID, OrderID: o.ID, OrgID: o.OrgID, Provider: ProviderBankTransfer, IncomingTransferID: &transferID,
		Amount: t.Amount, Currency: Currency, Status: PaymentCaptured, CapturedAt: &now, CapturedBy: &by,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.d.Store.InsertPayment(ctx, p); err != nil {
		return MatchResult{}, err
	}
	eff, applied, err := s.applyPayment(ctx, o, p)
	if err != nil {
		return MatchResult{}, err
	}
	over := t.Amount - applied
	if refundIBAN, err = overpaymentIBAN(t, over, refundIBAN); err != nil {
		return MatchResult{}, err
	}
	if _, err := s.d.Ledger.Post(ctx, received); err != nil {
		return MatchResult{}, err
	}
	if eff.Paid {
		if err := s.settle(ctx, p, eff, now); err != nil {
			return MatchResult{}, err
		}
	}
	var refundID *uuid.UUID
	if over > 0 {
		rid := uuid.NewV7()
		if err := s.d.Store.InsertRefund(ctx, Refund{
			ID: rid, PaymentID: p.ID, OrderID: o.ID, Amount: over, Reason: RefundReasonOverpayment, Status: RefundStatusRequested,
			RequestedBy: by, Method: RefundMethodBankEFT, TargetIBAN: &refundIBAN, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return MatchResult{}, err
		}
		refundID = &rid
		if err := s.d.Audit.Log(ctx, refundEvent(a, rid, p, over)); err != nil {
			return MatchResult{}, err
		}
	}
	after, err := s.d.Orders.ForPayment(ctx, o.ID)
	if err != nil {
		return MatchResult{}, err
	}
	meta := map[string]any{"order_id": o.ID.String(), "payment_id": p.ID.String(), "applied_amount": applied,
		"overpayment_amount": over, "order_paid": eff.Paid, "model_code": o.ModelCode}
	if refundID != nil {
		meta["refund_id"] = refundID.String()
	}
	if err := s.d.Audit.Log(ctx, transferEvent(a, authz.PaymentsTransferMatch, matched, "", meta)); err != nil {
		return MatchResult{}, err
	}
	if err := s.emit(ctx, aggregatePayment, p.ID, domainevent.PaymentCaptured{PaymentID: p.ID, OrderID: o.ID, Amount: p.Amount,
		OrderPaid: eff.Paid}); err != nil {
		return MatchResult{}, err
	}
	s.logInfo(ctx, "payment captured", slog.String("payment_id", p.ID.String()), slog.String("transfer_id", t.ID.String()),
		slog.String("order_id", o.ID.String()), slog.Int64("amount", p.Amount), slog.Int64("applied", applied),
		slog.Bool("order_paid", eff.Paid))
	return MatchResult{TransferID: matched.ID, Status: matched.Status, Payment: summaryOf(p), Order: orderSummaryOf(after, eff.Paid),
		AppliedAmount: applied, OverpaymentRefundID: refundID}, nil
}

// applyPayment applies what the order waits for of p (T-14). The order was
// read without a lock: when a concurrent capture of the same order
// committed meanwhile, orders refuses the stale amount (it locks the order
// first) and the amount is computed again from the order as it is now.
func (s *service) applyPayment(ctx context.Context, o orders.PayableOrder, p Payment) (orders.PaymentEffect, int64, error) {
	for attempt := 0; ; attempt++ {
		applied := min(p.Amount, o.DueAmount)
		if applied <= 0 {
			return orders.PaymentEffect{}, 0, ErrNothingDue
		}
		eff, err := s.d.Orders.ApplyPayment(ctx, o.ID, p.ID, applied)
		if !errors.Is(err, orders.ErrAmountExceedsDue) || attempt > 0 {
			return eff, applied, err
		}
		if o, err = s.d.Orders.ForPayment(ctx, o.ID); err != nil {
			return orders.PaymentEffect{}, 0, err
		}
		if o.Status != orders.StatusAwaitingPayment {
			return orders.PaymentEffect{}, 0, orders.ErrInvalidState
		}
	}
}

// overpaymentIBAN is where an overpayment goes back: the requested IBAN, or
// the sender's; without either the match is 422.
func overpaymentIBAN(t IncomingTransfer, over int64, requested string) (string, error) {
	switch {
	case over <= 0 || requested != "":
		return requested, nil
	case t.SenderIBAN == nil:
		return "", fieldErr("body.refund_iban",
			"is required: the transfer pays more than the order waits for and its sender IBAN is unknown")
	}
	return *t.SenderIBAN, nil
}

// payableOrder reads the order of a capture and checks its reference code.
func (s *service) payableOrder(ctx context.Context, orderID uuid.UUID, expected string) (orders.PayableOrder, error) {
	o, err := s.d.Orders.ForPayment(ctx, orderID)
	switch {
	case errors.Is(err, orders.ErrNotFound):
		return orders.PayableOrder{}, fieldErr("body.order_id", "is not an order")
	case err != nil:
		return orders.PayableOrder{}, err
	case o.OrderNo != expected:
		return orders.PayableOrder{}, ErrOrderReferenceMismatch
	}
	return o, nil
}

// settle writes the splits of a paid order on the payment that paid it and
// allocates the order in the ledger (02 §20.4, §21.3 order_allocated).
func (s *service) settle(ctx context.Context, p Payment, eff orders.PaymentEffect, now time.Time) error {
	if err := s.writeSplits(ctx, p, eff, now); err != nil {
		return err
	}
	t, ok, err := allocation(eff, now)
	if err != nil || !ok {
		return err
	}
	_, err = s.d.Ledger.Post(ctx, t)
	return err
}

// writeSplits writes one released split per line of a paid order: a payee
// line splits into the payee's net and Koro's commission with its VAT (the
// buyer fee included); Koro's own platform-native line is Koro's alone. A
// line that moves no money has no split (payment_splits_amount_chk).
func (s *service) writeSplits(ctx context.Context, p Payment, eff orders.PaymentEffect, now time.Time) error {
	ss := make([]Split, 0, len(eff.Lines))
	for _, l := range eff.Lines {
		sp := Split{ID: uuid.NewV7(), PaymentID: p.ID, OrderID: p.OrderID, OrderLineID: l.LineID, Status: SplitReleased,
			CreatedAt: now, UpdatedAt: now}
		if l.PayeeID == nil {
			sp.PlatformAmount = l.PayableAmount
		} else {
			fee, err := money.Add(l.CommissionAmount, l.CommissionVATAmount)
			if err != nil {
				return err
			}
			sp.SellerAmount, sp.PlatformAmount = l.SellerNetAmount, fee
		}
		if sp.SellerAmount+sp.PlatformAmount == 0 {
			continue
		}
		ss = append(ss, sp)
	}
	if len(ss) == 0 {
		return nil
	}
	return s.d.Store.InsertSplits(ctx, ss)
}

// allocation is the order_allocated transaction of a paid order (02 §21.3
// model A): its payee lines and Koro's own platform-native lines (Koro's
// sale, BE-067, T-293) in one transaction. ok is false when no line moves
// money.
func allocation(eff orders.PaymentEffect, now time.Time) (ledger.Txn, bool, error) {
	lines := make([]ledger.AllocationLine, 0, len(eff.Lines))
	var total int64
	for _, l := range eff.Lines {
		lines = append(lines, ledger.AllocationOf(l))
		total += l.PayableAmount // bounded: the order's payable amount fits
	}
	if total == 0 {
		return ledger.Txn{}, false, nil
	}
	t, err := ledger.OrderAllocated(ledger.Model(eff.ModelCode), eff.OrderID, lines, now)
	return t, err == nil, err
}

// refundEvent is the audit record of the overpayment refund a match opens
// (payments.refund.create; the approval is BE-043's four eyes).
func refundEvent(a authz.Actor, refundID uuid.UUID, p Payment, amount int64) audit.Event {
	id := refundID
	return audit.Event{
		Action: string(authz.PaymentsRefundCreate), Outcome: audit.Success, ResourceType: authz.TypeRefund, ResourceID: &id,
		Metadata: map[string]any{"reason": RefundReasonOverpayment, "amount": amount, "payment_id": p.ID.String(),
			"order_id": p.OrderID.String(), "via": "transfer_match"},
	}.By(a)
}

// ConfirmDirectPayment implements Service (fin; action payments.transfer.match,
// Idempotency-Key): model C (06 H-56, T-18, T-227) — a finance admin
// confirms, with the receipt of the buyer's transfer to the payee, that the
// buyer paid the payees directly. Order of checks: authz (403) → body
// (422; the evidence: a transfer_receipt upload) → in one transaction: the
// order (422 unknown, 422 payments.order_reference_mismatch; a model C
// order awaiting payment, else 409 orders.invalid_state) →
// orders.ConfirmDirectPayment (409 orders.order_not_paid while Koro's fee
// is not in; the order is paid, its rights active) → the direct_confirmed
// payment (the direct amount: what the buyer owed the payees), the
// evidence attached → the splits → audit → payment.captured (O). The money
// never reaches Koro's account: the ledger posts nothing (no model C
// template, T-11).
func (s *service) ConfirmDirectPayment(ctx context.Context, a authz.Actor, in DirectInput) (DirectResult, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferMatch, adminSurface); err != nil {
		return DirectResult{}, err
	}
	if in.OrderID == uuid.Nil() {
		return DirectResult{}, fieldErr("body.order_id", "is required")
	}
	expected, ok := orders.NormalizeReference(in.ExpectedOrderNo)
	if !ok {
		return DirectResult{}, fieldErr("body.expected_order_no", "must be a reference code (KR + 8 characters)")
	}
	if err := s.checkFile(ctx, "body.evidence_file_id", in.EvidenceFileID, files.PurposeTransferReceipt); err != nil {
		return DirectResult{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op              string
		Order, Evidence uuid.UUID
		Expected        string
	}{"confirm_direct_payment", in.OrderID, in.EvidenceFileID, expected})
	if err != nil {
		return DirectResult{}, err
	}
	res, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (DirectResult, error) {
		o, err := s.payableOrder(ctx, in.OrderID, expected)
		if err != nil {
			return DirectResult{}, err
		}
		if o.CollectionMethod != orders.CollectDirectToPayee || o.Status != orders.StatusAwaitingPayment {
			return DirectResult{}, orders.ErrInvalidState
		}
		paymentID, now, by := uuid.NewV7(), s.now(), a.UserID
		eff, err := s.d.Orders.ConfirmDirectPayment(ctx, o.ID, paymentID)
		if err != nil {
			return DirectResult{}, err
		}
		var direct int64
		for _, l := range eff.Lines {
			if direct, err = money.Add(direct, l.PayableAmount-l.BuyerFeeAmount); err != nil {
				return DirectResult{}, err
			}
		}
		if direct <= 0 {
			return DirectResult{}, ErrNothingDue
		}
		evidence := in.EvidenceFileID
		p := Payment{
			ID: paymentID, OrderID: o.ID, OrgID: o.OrgID, Provider: ProviderDirectConfirmed, Amount: direct, Currency: Currency,
			Status: PaymentCaptured, CapturedAt: &now, CapturedBy: &by, EvidenceFileID: &evidence, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.d.Store.InsertPayment(ctx, p); err != nil {
			return DirectResult{}, err
		}
		if err := s.attach(ctx, a, evidence, p.ID, files.PurposeTransferReceipt); err != nil {
			return DirectResult{}, err
		}
		if err := s.writeSplits(ctx, p, eff, now); err != nil {
			return DirectResult{}, err
		}
		after, err := s.d.Orders.ForPayment(ctx, o.ID)
		if err != nil {
			return DirectResult{}, err
		}
		pid := p.ID
		if err := s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.PaymentsTransferMatch), Outcome: audit.Success, ResourceType: authz.TypePayment, ResourceID: &pid,
			Metadata: map[string]any{"provider": string(ProviderDirectConfirmed), "order_id": o.ID.String(), "amount": direct,
				"evidence_file_id": evidence.String(), "model_code": o.ModelCode},
		}.By(a)); err != nil {
			return DirectResult{}, err
		}
		if err := s.emit(ctx, aggregatePayment, p.ID, domainevent.PaymentCaptured{PaymentID: p.ID, OrderID: o.ID, Amount: direct,
			OrderPaid: true}); err != nil {
			return DirectResult{}, err
		}
		s.logInfo(ctx, "direct payment confirmed", slog.String("payment_id", p.ID.String()), slog.String("order_id", o.ID.String()),
			slog.Int64("amount", direct))
		return DirectResult{Payment: summaryOf(p), Order: orderSummaryOf(after, true)}, nil
	})
	if err != nil {
		return DirectResult{}, err
	}
	return res, nil
}
