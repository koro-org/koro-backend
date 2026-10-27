package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"koro/internal/entitlements"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
)

// Revocation reasons of the rights of a line (entitlements.revoke_reason).
const (
	revokeExpired   = "order_expired"
	revokeCancelled = "order_cancelled"
	revokeRefunded  = "order_line_refunded"
)

// ForPayment implements Orders.
func (s *service) ForPayment(ctx context.Context, orderID uuid.UUID) (PayableOrder, error) {
	o, err := s.d.Store.GetOrder(ctx, orderID, false)
	if err != nil {
		return PayableOrder{}, err
	}
	return payableOf(o), nil
}

// FindByReference implements Orders (04 §C.3): the code is normalised
// (separators ignored, upper case, Crockford's O/I/L) before the lookup.
func (s *service) FindByReference(ctx context.Context, ref string) (PayableOrder, bool, error) {
	code, ok := NormalizeReference(ref)
	if !ok {
		return PayableOrder{}, false, nil
	}
	o, err := s.d.Store.OrderByNo(ctx, code)
	switch {
	case errors.Is(err, ErrNotFound):
		return PayableOrder{}, false, nil
	case err != nil:
		return PayableOrder{}, false, err
	}
	return payableOf(o), true, nil
}

// ApplyPayment implements Orders (01 §4.18, §6.3 "Havale eşleştirme"; T-14)
// in the caller's transaction: only an order awaiting payment takes money,
// never more than it still waits for (payments refunds an overpayment).
// When Koro has collected everything (paid_amount = koro_collect_amount, the
// orders_paid_full_chk rule) a model A, A2 or B order is paid (settle). A
// model C order collects only Koro's fee here; it settles once the fee
// is complete and direct payment has been confirmed, in either order.
func (s *service) ApplyPayment(ctx context.Context, orderID, paymentID uuid.UUID, amount int64) (PaymentEffect, error) {
	if amount <= 0 {
		return PaymentEffect{}, problem.Field("amount", "must be positive")
	}
	var eff PaymentEffect
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		o, err := s.d.Store.GetOrder(ctx, orderID, true)
		if err != nil {
			return err
		}
		if o.Status != StatusAwaitingPayment {
			return ErrInvalidState
		}
		if amount > o.DueAmount() {
			return ErrAmountExceedsDue
		}
		next := o
		next.PaidAmount += amount
		paid := next.DueAmount() == 0 && (!o.DirectToPayee() || o.DirectConfirmedAt != nil)
		if eff, err = s.settle(ctx, o, next, paid); err != nil {
			return err
		}
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "order payment applied", slog.String("order_id", o.ID.String()),
			slog.String("payment_id", paymentID.String()), slog.Int64("amount", amount), slog.Bool("paid", paid))
		return nil
	})
	if err != nil {
		return PaymentEffect{}, err
	}
	return eff, nil
}

// ConfirmDirectPayment remembers a finance admin's evidence in the same
// order transaction. Fee capture and direct confirmation may arrive in
// either order; the second one settles exactly once under the order lock.
func (s *service) ConfirmDirectPayment(ctx context.Context, orderID, paymentID uuid.UUID) (PaymentEffect, error) {
	var eff PaymentEffect
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		o, err := s.d.Store.GetOrder(ctx, orderID, true)
		switch {
		case err != nil:
			return err
		case !o.DirectToPayee() || o.Status != StatusAwaitingPayment:
			return ErrInvalidState
		case o.DirectConfirmedAt != nil:
			return ErrInvalidState
		}
		next := o
		now := s.now()
		next.DirectConfirmedAt = &now
		if eff, err = s.settle(ctx, o, next, o.DueAmount() == 0); err != nil {
			return err
		}
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "order direct payment confirmed", slog.String("order_id", o.ID.String()),
			slog.String("payment_id", paymentID.String()), slog.Int64("direct_amount", o.DirectToPayeeAmount))
		return nil
	})
	if err != nil {
		return PaymentEffect{}, err
	}
	return eff, nil
}

// settle writes next (o with its new paid amount) and, when paid, makes it
// paid: the rights of its platform-native lines are activated and those
// lines fulfilled (auto_platform, order_line.fulfilled O), and the effect
// carries every line for payment_splits and the ledger — but a line that
// dropped out before payment (BE-134): the buyer did not pay for it.
func (s *service) settle(ctx context.Context, o, next Order, paid bool) (PaymentEffect, error) {
	var lines []Line
	if paid {
		now := s.now()
		var err error
		next.Status, next.PaidAt = StatusPaid, &now
		if lines, err = s.payLines(ctx, o, now); err != nil {
			return PaymentEffect{}, err
		}
		next = next.moveTo(progressStatus(next, lines), now)
	}
	stored, err := s.d.Store.UpdateOrder(ctx, next, StatusAwaitingPayment)
	if err != nil {
		return PaymentEffect{}, err
	}
	eff := PaymentEffect{OrderID: stored.ID, OrgID: stored.OrgID, ModelCode: stored.ModelCode, PaidAmount: stored.PaidAmount,
		DueAmount: stored.DueAmount(), Paid: paid}
	for _, l := range liveLines(lines) {
		eff.Lines = append(eff.Lines, paidLineOf(l))
	}
	return eff, nil
}

// payLines runs at the payment of o: the rights of its platform-native
// lines are activated and those lines fulfilled (02 §19.2 "hak ödeme anında
// aktifleşir, fulfilled_via='auto_platform'"). It returns every line.
func (s *service) payLines(ctx context.Context, o Order, now time.Time) ([]Line, error) {
	byOrder, err := s.d.Store.LinesOf(ctx, []uuid.UUID{o.ID})
	if err != nil {
		return nil, err
	}
	lines := byOrder[o.ID]
	for i, l := range lines {
		if l.FulfillmentStatus != FulfillNotApplicable {
			continue
		}
		if err := s.d.Entitlements.ActivateBySource(ctx, entitlements.SourceOrderLine, l.ID); err != nil {
			return nil, fmt.Errorf("orders: activate the rights of line %s: %w", l.ID, err)
		}
		via := ViaAutoPlatform
		next := l
		next.FulfillmentStatus, next.FulfilledAt, next.FulfilledVia = FulfillFulfilled, &now, &via
		stored, err := s.d.Store.UpdateLine(ctx, next, FulfillNotApplicable)
		if err != nil {
			return nil, err
		}
		lines[i] = stored
		if err := s.emitFulfilled(ctx, stored); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

// emitFulfilled writes order_line.fulfilled (O) for billing and the seller's
// notice; a platform line has no payee (the zero ID).
func (s *service) emitFulfilled(ctx context.Context, l Line) error {
	p := domainevent.OrderLineFulfilled{OrderLineID: l.ID, OrderID: l.OrderID}
	if l.PayeeID != nil {
		p.PayeeID = *l.PayeeID
	}
	return s.emitOutbox(ctx, auditLine, l.ID, p)
}

// progressStatus is the running status of a paid order from its lines:
// completed once every line is final and one was fulfilled. A line is final
// when it is fulfilled, cancelled (dropped lines included) or its service
// was refunded in full through Koro (fullyRefundedService: such a line is
// never delivered, T-309). Until then a partially refunded order stays
// partially_refunded, and any other one is disputed while a line is
// disputed, in_fulfilment once a line is marked or fulfilled, paid before.
// A partial refund does not hold a finished order open (T-382, BE-135): a
// completed order keeps its refunds in refunded_amount (the order's and its
// lines'). Any other status (unpaid, cancelled, expired, refunded) is kept.
func progressStatus(o Order, lines []Line) Status {
	switch o.Status {
	case StatusPaid, StatusInFulfilment, StatusDisputed, StatusCompleted, StatusPartiallyRefunded:
	default:
		return o.Status
	}
	disputed, allDone, fulfilled, started := false, true, false, false
	for _, l := range lines {
		switch {
		case l.FulfillmentStatus == FulfillFulfilled:
			fulfilled, started = true, true
		case l.FulfillmentStatus == FulfillCancelled, fullyRefundedService(o, l):
		case l.FulfillmentStatus == FulfillDisputed:
			disputed, allDone, started = true, false, true
		case l.FulfillmentStatus == FulfillMarked:
			allDone, started = false, true
		default:
			allDone = false
		}
	}
	switch {
	case allDone && fulfilled:
		return StatusCompleted
	case o.Status == StatusPartiallyRefunded:
		return StatusPartiallyRefunded
	case disputed:
		return StatusDisputed
	case started:
		return StatusInFulfilment
	}
	return StatusPaid
}

// moveTo is o in status st: an order that completes for the first time
// takes now as completed_at; one that completed before keeps it (a refund
// of a completed order leaves it completed, BE-135).
func (o Order) moveTo(st Status, now time.Time) Order {
	o.Status = st
	if st == StatusCompleted && o.CompletedAt == nil {
		o.CompletedAt = &now
	}
	return o
}

// advance writes the running status of a paid order after a change of one
// of its lines (the order row is locked first).
func (s *service) advance(ctx context.Context, orderID uuid.UUID, now time.Time) (Order, error) {
	o, err := s.d.Store.GetOrder(ctx, orderID, true)
	if err != nil {
		return Order{}, err
	}
	byOrder, err := s.d.Store.LinesOf(ctx, []uuid.UUID{o.ID})
	if err != nil {
		return Order{}, err
	}
	next := o.moveTo(progressStatus(o, byOrder[o.ID]), now)
	if next.Status == o.Status {
		return o, nil
	}
	return s.d.Store.UpdateOrder(ctx, next, o.Status)
}

// ApplyRefund implements Orders (BE-043, in its transaction): the refunded
// amount of the order (and of the line) grows, never beyond what was paid
// nor beyond what Koro holds of the line (its buyer fee on a model C order;
// 422 orders.refund_exceeds_paid); a paid order becomes refunded once all
// its money went back, otherwise partially_refunded while it still runs and
// completed once every line is final (progressStatus; a completed order
// stays completed, BE-135); a fully refunded line, or every line of a fully
// refunded order, loses its rights.
func (s *service) ApplyRefund(ctx context.Context, orderID uuid.UUID, lineID *uuid.UUID, amount int64) error {
	if amount <= 0 {
		return problem.Field("amount", "must be positive")
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		o, err := s.d.Store.GetOrder(ctx, orderID, true)
		if err != nil {
			return err
		}
		if o.RefundedAmount+amount > o.PaidAmount {
			return ErrRefundExceedsPaid
		}
		var revoke []uuid.UUID
		if lineID != nil {
			l, err := s.d.Store.GetLine(ctx, *lineID, true)
			switch {
			case err != nil:
				return err
			case l.OrderID != o.ID:
				return ErrNotFound
			case amount > o.Refundable(l):
				return ErrRefundExceedsPaid
			}
			next := l
			next.RefundedAmount += amount
			if _, err := s.d.Store.UpdateLine(ctx, next, l.FulfillmentStatus); err != nil {
				return err
			}
			if next.RefundedAmount == o.KoroHeld(next) {
				revoke = append(revoke, l.ID)
			}
		}
		next := o
		next.RefundedAmount += amount
		if o.Status.PaidState() {
			byOrder, err := s.d.Store.LinesOf(ctx, []uuid.UUID{o.ID})
			if err != nil {
				return err
			}
			if next.RefundedAmount == next.PaidAmount {
				next.Status = StatusRefunded
				for _, l := range byOrder[o.ID] {
					if !slices.Contains(revoke, l.ID) {
						revoke = append(revoke, l.ID)
					}
				}
			} else {
				next.Status = StatusPartiallyRefunded
				next = next.moveTo(progressStatus(next, byOrder[o.ID]), s.now())
			}
		}
		if _, err := s.d.Store.UpdateOrder(ctx, next, o.Status); err != nil {
			return err
		}
		for _, id := range revoke {
			if err := s.d.Entitlements.RevokeBySource(ctx, entitlements.SourceOrderLine, id, revokeRefunded); err != nil {
				return fmt.Errorf("orders: revoke the rights of line %s: %w", id, err)
			}
		}
		return nil
	})
}

// ForBilling implements Orders (BE-042): the order and every line, read
// in the caller's transaction.
func (s *service) ForBilling(ctx context.Context, orderID uuid.UUID) (Order, []Line, error) {
	o, err := s.d.Store.GetOrder(ctx, orderID, false)
	if err != nil {
		return Order{}, nil, err
	}
	byOrder, err := s.d.Store.LinesOf(ctx, []uuid.UUID{o.ID})
	if err != nil {
		return Order{}, nil, err
	}
	return o, byOrder[o.ID], nil
}

// PayoutLines implements Orders (BE-042).
func (s *service) PayoutLines(ctx context.Context, payoutID uuid.UUID) ([]Line, error) {
	return s.d.Store.LinesOfPayout(ctx, payoutID)
}

// GetLine implements Orders.
func (s *service) GetLine(ctx context.Context, lineID uuid.UUID) (Line, error) {
	return s.d.Store.GetLine(ctx, lineID, false)
}

// PayableLines implements Orders (01 §4.18; BE-044).
func (s *service) PayableLines(ctx context.Context, payeeID uuid.UUID, asOf time.Time) ([]PayableLine, error) {
	lines, err := s.d.Store.PayableLines(ctx, payeeID, asOf.UTC())
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.OrderID)
	}
	orders, err := s.d.Store.OrdersByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]PayableLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, PayableLine{LineID: l.ID, OrderID: l.OrderID, OrderNo: orders[l.OrderID].OrderNo, PayeeID: *l.PayeeID,
			SellerOrgID: l.SellerOrgID, SellerNetAmount: l.SellerNetAmount, FulfilledAt: *l.FulfilledAt})
	}
	return out, nil
}

// MarkLinesPaidOut implements Orders (01 §6.3 "Payout ödendi"; BE-044): in
// the caller's transaction, every line gets the payout once; a line that is
// not fulfilled, has no payee or is already paid out fails the whole call
// (409 orders.invalid_state).
func (s *service) MarkLinesPaidOut(ctx context.Context, lineIDs []uuid.UUID, payoutID uuid.UUID) error {
	if len(lineIDs) == 0 || payoutID == uuid.Nil() {
		return errors.New("orders: mark paid out: lines and payout are required")
	}
	ids := slices.Clone(lineIDs)
	slices.SortFunc(ids, compareUUID)
	if len(slices.Compact(ids)) != len(lineIDs) {
		return errors.New("orders: mark paid out: a line is listed twice")
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		n, err := s.d.Store.MarkLinesPaidOut(ctx, lineIDs, payoutID, s.now())
		if err != nil {
			return err
		}
		if n != len(lineIDs) {
			return ErrInvalidState
		}
		return nil
	})
}

// maxRefundsDue bounds one RefundsDue page.
const maxRefundsDue = 1000

// RefundsDue implements Orders.
func (s *service) RefundsDue(ctx context.Context, limit int) ([]RefundDue, error) {
	if limit < 1 || limit > maxRefundsDue {
		limit = maxRefundsDue
	}
	return s.d.Store.RefundsDue(ctx, limit)
}

// maxCollectedLines bounds one CollectedLines page.
const maxCollectedLines = 1000

// CollectedLines implements Orders (BE-067).
func (s *service) CollectedLines(ctx context.Context, after uuid.UUID, platformOnly bool, limit int) ([]CollectedLine, error) {
	if limit < 1 || limit > maxCollectedLines {
		limit = maxCollectedLines
	}
	lines, err := s.d.Store.CollectedLines(ctx, after, platformOnly, limit)
	if err != nil || len(lines) == 0 {
		return []CollectedLine{}, err
	}
	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.OrderID)
	}
	byID, err := s.d.Store.OrdersByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]CollectedLine, 0, len(lines))
	for _, l := range lines {
		o, ok := byID[l.OrderID]
		if !ok {
			return nil, fmt.Errorf("orders: collected line %s: order %s not found", l.ID, l.OrderID)
		}
		out = append(out, CollectedLine{PaidLine: paidLineOf(l), OrderID: l.OrderID, ModelCode: o.ModelCode, RefundedAmount: l.RefundedAmount})
	}
	return out, nil
}

// ForAllocation implements Orders. Derive the parent from the stored line,
// then lock parent before child (the same order as ApplyRefund). No ledger
// header may be inserted before these locks: its order_line FK would wait
// behind a refund that is waiting to insert the same recognition header.
func (s *service) ForAllocation(ctx context.Context, lineID uuid.UUID) (CollectedLine, error) {
	if !db.InTx(ctx) {
		return CollectedLine{}, errors.New("orders: ForAllocation needs a transaction")
	}
	l, err := s.d.Store.GetLine(ctx, lineID, false)
	if err != nil {
		return CollectedLine{}, err
	}
	o, err := s.d.Store.GetOrder(ctx, l.OrderID, true)
	if err != nil {
		return CollectedLine{}, err
	}
	if o.PaidAt == nil || o.DirectToPayee() || l.Dropped() {
		return CollectedLine{}, ErrInvalidState // a dropped line was never collected (BE-134)
	}
	l, err = s.d.Store.GetLine(ctx, lineID, true)
	if err != nil {
		return CollectedLine{}, err
	}
	return CollectedLine{PaidLine: paidLineOf(l), OrderID: o.ID, ModelCode: o.ModelCode, RefundedAmount: l.RefundedAmount}, nil
}
