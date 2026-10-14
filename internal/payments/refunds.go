package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/money"
	"koro/internal/platform/page"
)

// Refunds (BE-043; 03 §5.16, §6.1 "İade"; 02 §20.5; 01 §6.3): money going
// back to the buyer, a money exit, so four eyes in three steps.
//
//	1. request  (adminCreateRefund, pad/fin, Idempotency-Key) → requested: the
//	            payment, the line (a paid order's) or none (an overpayment,
//	            an unpaid order's money), the amount, the reason, the target IBAN;
//	2. decision (adminApproveRefund, fin·4·R) → approved, by a finance admin
//	            other than the requester; or adminRejectRefund (fin) → rejected;
//	3. receipt  (adminCompleteRefund, fin) → completed, after the EFT: in one
//	            transaction the payment, the order (orders.ApplyRefund), the
//	            ledger (refund_completed) and payment.refunded (O).
//
// Money pools. A refund draws from one payment (payments_refunded_limit_chk)
// and from the order:
//
//   - overpayment: the part of the order's transfers the order did not wait
//     for (Σ captured bank-transfer payments − paid_amount; 03 k27, T-14).
//     It never was the order's money: orders.ApplyRefund is not called and
//     the ledger takes it from buyer_advances.
//   - an order cancelled or expired before it was paid in full: what was
//     paid (paid_amount − refunded_amount), refunded as a whole (no line);
//     never allocated, so from buyer_advances as well.
//   - a line of a paid order: what Koro holds of it (its payable amount; its
//     buyer fee only in model C, the Model C upper bound) minus what was
//     refunded. A payee line was allocated: its shares come back in
//     proportion to what is left of them (platform/money Allocate over the
//     ledger's refunded shares, so the last refund of the line takes the
//     exact remainder); a line already paid out gives its seller part back
//     from payee_receivable (06 H-59). Koro's own platform-native line gives
//     back its sale, buyer fee and VAT the same way (BE-067; a line paid
//     before its template is recognised first, in the same transaction).
//
// Every limit of a request counts the open (requested, approved) refunds,
// so two requests never promise the same money; the completion checks
// again under the row locks (orders.ApplyRefund, the payment row) and is the
// authoritative one.
//
// Model C (T-11, BE-066): the upper bound applies (Koro holds the fee
// only), but the ledger has no model C template: its completion is 422
// ledger.model_not_enabled until BE-066 writes the reversal.

// Limits of a refund (000108 CHECKs).
const (
	maxRefundNote      = 1000
	maxRejectionReason = 1000
	// maxDueScan bounds the refunds-due rows read from orders at once.
	maxDueScan = 1000
)

// aggregateRefund is the outbox aggregate and the files owner of a refund.
const aggregateRefund = "refund"

// Valid reports whether s is a payments.status value.
func (s PaymentStatus) Valid() bool {
	switch s {
	case PaymentInitiated, PaymentCaptured, PaymentFailed, PaymentPartiallyRefunded, PaymentRefunded, PaymentChargeback:
		return true
	}
	return false
}

// refundable reports whether a payment can still give money back: a
// captured bank transfer (a direct_confirmed payment never reached Koro).
func (p Payment) refundable() bool {
	return p.Provider == ProviderBankTransfer && (p.Status == PaymentCaptured || p.Status == PaymentPartiallyRefunded)
}

// refundResource is the authz view of a refund for the second eye (03 §3.5
// refund: status, requested_by).
func refundResource(r Refund) authz.Resource {
	return authz.Resource{Type: authz.TypeRefund, ID: r.ID, Attrs: map[string]string{
		authz.AttrStatus: string(r.Status), authz.AttrRequestedBy: r.RequestedBy.String(),
	}}
}

// refundAudit is the audit record of a refund action: IDs, amounts and
// states; the target IBAN masked only.
func refundAudit(a authz.Actor, act authz.Action, r Refund, reason string, meta map[string]any) audit.Event {
	id := r.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["status"] = string(r.Status)
	meta["amount"] = r.Amount
	meta["reason"] = r.Reason
	meta["payment_id"] = r.PaymentID.String()
	meta["order_id"] = r.OrderID.String()
	if r.OrderLineID != nil {
		meta["order_line_id"] = r.OrderLineID.String()
	}
	if m := r.MaskedIBAN(); m != "" {
		meta["target_iban_masked"] = m
	}
	return audit.Event{
		Action: string(act), Outcome: audit.Success, ResourceType: authz.TypeRefund, ResourceID: &id, Reason: reason, Metadata: meta,
	}.By(a)
}

// exceeds is the 422 orders.refund_exceeds_paid of a request's amount.
func exceeds(msg string) error {
	return ErrRefundExceedsPaid.WithFields(fieldErrOf("body.amount", msg))
}

// ---- reads

// ListPayments implements Service (pad, fin): authz (403) → the query (422)
// → a page, newest first.
func (s *service) ListPayments(ctx context.Context, a authz.Actor, q PaymentQuery) (PaymentPage, error) {
	if err := authz.Can(ctx, a, authz.PaymentsPaymentList, adminSurface); err != nil {
		return PaymentPage{}, err
	}
	if q.invalid != nil {
		return PaymentPage{}, q.invalid
	}
	if q.Filter.Status != "" && !q.Filter.Status.Valid() {
		return PaymentPage{}, fieldErr("query.status", "is not a payment status")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListPayments(ctx, q.Filter, q.After, limit+1)
	if err != nil {
		return PaymentPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := PaymentPage{Items: rows}
	if more {
		last := rows[len(rows)-1]
		out.Next = &TimeCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// GetPaymentView implements Service (pad, fin): authz (403) → the payment
// (404) → its order's reference code, its splits and its refunds.
func (s *service) GetPaymentView(ctx context.Context, a authz.Actor, id uuid.UUID) (PaymentView, error) {
	if err := authz.Can(ctx, a, authz.PaymentsPaymentRead, adminSurface); err != nil {
		return PaymentView{}, err
	}
	p, err := s.d.Store.GetPayment(ctx, id, false)
	if err != nil {
		return PaymentView{}, err
	}
	o, err := s.d.Orders.ForPayment(ctx, p.OrderID)
	if err != nil {
		return PaymentView{}, err
	}
	splits, err := s.d.Store.SplitsOf(ctx, p.ID)
	if err != nil {
		return PaymentView{}, err
	}
	all, err := s.d.Store.RefundsOfOrder(ctx, p.OrderID)
	if err != nil {
		return PaymentView{}, err
	}
	v := PaymentView{Payment: p, OrderNo: o.OrderNo, Splits: splits}
	for _, r := range all {
		if r.PaymentID == p.ID {
			v.Refunds = append(v.Refunds, r)
		}
	}
	return v, nil
}

// ListRefunds implements Service (pad, fin): authz (403) → the query (422)
// → a page, newest first.
func (s *service) ListRefunds(ctx context.Context, a authz.Actor, q RefundQuery) (RefundPage, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundList, adminSurface); err != nil {
		return RefundPage{}, err
	}
	if q.invalid != nil {
		return RefundPage{}, q.invalid
	}
	if q.Filter.Status != "" && !q.Filter.Status.Valid() {
		return RefundPage{}, fieldErr("query.status", "is not a refund status")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListRefunds(ctx, q.Filter, q.After, limit+1)
	if err != nil {
		return RefundPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := RefundPage{Items: rows}
	if more {
		last := rows[len(rows)-1]
		out.Next = &TimeCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// orderMoney is what a refund decision needs of an order: the order, its
// payments and its refunds.
type orderMoney struct {
	order    orders.PayableOrder
	payments []Payment
	refunds  []Refund
}

func (s *service) orderMoney(ctx context.Context, orderID uuid.UUID) (orderMoney, error) {
	o, err := s.d.Orders.ForPayment(ctx, orderID)
	if err != nil {
		return orderMoney{}, err
	}
	ps, err := s.d.Store.PaymentsOfOrder(ctx, orderID)
	if err != nil {
		return orderMoney{}, err
	}
	rs, err := s.d.Store.RefundsOfOrder(ctx, orderID)
	if err != nil {
		return orderMoney{}, err
	}
	return orderMoney{order: o, payments: ps, refunds: rs}, nil
}

// held sums the amounts of the refunds other than self that match and
// still hold their money: open ones, and completed ones when done is set
// (a completed refund of the order money is already in refunded_amount).
func (m orderMoney) held(self uuid.UUID, done bool, match func(Refund) bool) int64 {
	var n int64
	for _, r := range m.refunds {
		if r.ID != self && (r.Status.Open() || done && r.Status == RefundStatusCompleted) && match(r) {
			n += r.Amount
		}
	}
	return n
}

// paymentLeft is what payment p can still give back: not refunded and not
// held by an open refund other than self.
func (m orderMoney) paymentLeft(p Payment, self uuid.UUID) int64 {
	return p.Amount - p.RefundedAmount - m.held(self, false, func(r Refund) bool { return r.PaymentID == p.ID })
}

// overpaymentLeft is the order's unapplied money (its captured transfers
// beyond what it waited for) not refunded nor promised by another
// overpayment refund.
func (m orderMoney) overpaymentLeft(self uuid.UUID) int64 {
	var in int64
	for _, p := range m.payments {
		if p.Provider == ProviderBankTransfer && p.CapturedAt != nil {
			in += p.Amount
		}
	}
	return in - m.order.PaidAmount - m.held(self, true, Refund.Overpayment)
}

// orderLeft is the order money not refunded nor promised by another open
// refund of the order money.
func (m orderMoney) orderLeft(self uuid.UUID) int64 {
	return m.order.PaidAmount - m.order.RefundedAmount - m.held(self, false, func(r Refund) bool { return !r.Overpayment() })
}

// lineLeft is what Koro holds of line l (the buyer fee only in model C)
// not refunded nor promised by another open refund of the line.
func (m orderMoney) lineLeft(l orders.Line, self uuid.UUID) int64 {
	held := l.PayableAmount
	if m.order.CollectionMethod == orders.CollectDirectToPayee {
		held = l.BuyerFeeAmount
	}
	return held - l.RefundedAmount - m.held(self, false, func(r Refund) bool { return r.OrderLineID != nil && *r.OrderLineID == l.ID })
}

// ListRefundsDue implements Service (pad, fin; payments.refund.list): the
// money orders.RefundsDue lists (oldest first) minus the open refunds that
// already cover it, with the payments that can fund it; at most limit
// rows (the oldest; no cursor — finance works the queue from the top).
func (s *service) ListRefundsDue(ctx context.Context, a authz.Actor, limit int) ([]RefundDue, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundList, adminSurface); err != nil {
		return nil, err
	}
	limit = page.ClampLimit(limit)
	dues, err := s.d.Orders.RefundsDue(ctx, maxDueScan)
	if err != nil {
		return nil, err
	}
	byOrder := map[uuid.UUID]orderMoney{}
	out := []RefundDue{}
	for _, d := range dues {
		m, ok := byOrder[d.OrderID]
		if !ok {
			if m, err = s.orderMoney(ctx, d.OrderID); err != nil {
				return nil, err
			}
			byOrder[d.OrderID] = m
		}
		open := m.held(uuid.Nil(), false, func(r Refund) bool {
			return !r.Overpayment() && (d.LineID == nil && r.OrderLineID == nil || d.LineID != nil && r.OrderLineID != nil && *r.OrderLineID == *d.LineID)
		})
		if d.Amount-open <= 0 {
			continue
		}
		item := RefundDue{OrderID: d.OrderID, OrderNo: m.order.OrderNo, OrgID: d.OrgID, LineID: d.LineID, Amount: d.Amount - open,
			Since: d.Since.UTC(), Payments: []FundingPayment{}}
		for _, p := range m.payments {
			if left := m.paymentLeft(p, uuid.Nil()); p.refundable() && left > 0 {
				item.Payments = append(item.Payments, FundingPayment{PaymentID: p.ID, Available: left})
			}
		}
		out = append(out, item)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ---- request

// refundDraft is a validated request (and its idempotency hash input).
type refundDraft struct {
	PaymentID  uuid.UUID
	LineID     *uuid.UUID
	Amount     int64
	Reason     string
	Note       string
	Method     string
	TargetIBAN string
}

// cleanRefund validates the body of adminCreateRefund (422).
func cleanRefund(in RefundInput) (refundDraft, error) {
	d := refundDraft{PaymentID: in.PaymentID, LineID: in.OrderLineID, Amount: in.Amount, Reason: in.Reason, Method: in.Method}
	if d.Method == "" {
		d.Method = RefundMethodBankEFT
	}
	switch {
	case d.PaymentID == uuid.Nil():
		return refundDraft{}, fieldErr("body.payment_id", "is required")
	case d.LineID != nil && *d.LineID == uuid.Nil():
		return refundDraft{}, fieldErr("body.order_line_id", "must be an order line ID")
	case d.Amount <= 0:
		return refundDraft{}, fieldErr("body.amount", "must be a positive amount in kuruş")
	case !slices.Contains(RefundReasons(), d.Reason):
		return refundDraft{}, fieldErr("body.reason", "is not a refund reason")
	case d.Reason == RefundReasonOverpayment && d.LineID != nil:
		return refundDraft{}, fieldErr("body.order_line_id", "must be empty: an overpayment is not the money of a line")
	case d.Method != RefundMethodBankEFT:
		return refundDraft{}, fieldErr("body.method", "must be bank_eft (iyzico refunds are not available yet)")
	}
	var err error
	if d.Note, err = cleanText("body.reason_note", in.ReasonNote, 0, maxRefundNote); err != nil {
		return refundDraft{}, err
	}
	if in.TargetIBAN == "" {
		return refundDraft{}, fieldErr("body.target_iban", "is required for a bank EFT")
	}
	if d.TargetIBAN, err = cleanIBAN("body.target_iban", in.TargetIBAN); err != nil {
		return refundDraft{}, err
	}
	return d, nil
}

// CreateRefund implements Service (pad, fin; Idempotency-Key): authz (403)
// → body (422) → the payment (422 unknown) → in one transaction with the
// key (06 H-5): the payment locked (a captured bank transfer, else 409
// payments.invalid_state) → the limits, open refunds counted (422
// orders.refund_exceeds_paid): the payment, then the pool of the reason —
// an overpayment the order's unapplied money; an unpaid (cancelled,
// expired) order its paid money, as a whole (409 orders.invalid_state while
// it still waits for payment); a paid order one of its lines (required)
// and what Koro holds of it → a payee line in an open payout is 409
// payments.line_in_payout → the request (requested) → audit.
func (s *service) CreateRefund(ctx context.Context, a authz.Actor, in RefundInput) (Refund, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundCreate, adminSurface); err != nil {
		return Refund{}, err
	}
	d, err := cleanRefund(in)
	if err != nil {
		return Refund{}, err
	}
	if _, err := s.d.Store.GetPayment(ctx, d.PaymentID, false); err != nil {
		if isNotFound(err) {
			return Refund{}, fieldErr("body.payment_id", "is not a payment")
		}
		return Refund{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op string
		D  refundDraft
	}{"create_refund", d})
	if err != nil {
		return Refund{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		return s.createRefund(ctx, a, d)
	})
	if err != nil {
		return Refund{}, err
	}
	return s.d.Store.GetRefund(ctx, id, false)
}

func (s *service) createRefund(ctx context.Context, a authz.Actor, d refundDraft) (uuid.UUID, error) {
	p, err := s.d.Store.GetPayment(ctx, d.PaymentID, true)
	if err != nil {
		return uuid.Nil(), err
	}
	if !p.refundable() {
		return uuid.Nil(), ErrInvalidState.WithFields(fieldErrOf("body.payment_id", "is not a captured bank transfer payment"))
	}
	m, err := s.orderMoney(ctx, p.OrderID)
	if err != nil {
		return uuid.Nil(), err
	}
	if d.Amount > m.paymentLeft(p, uuid.Nil()) {
		return uuid.Nil(), exceeds("exceeds what the payment still holds (requested and approved refunds count)")
	}
	if err := s.checkPool(ctx, m, d.Reason, d.LineID, d.Amount, uuid.Nil()); err != nil {
		return uuid.Nil(), err
	}
	now, by, note := s.now(), a.UserID, optional(d.Note)
	r := Refund{
		ID: uuid.NewV7(), PaymentID: p.ID, OrderID: p.OrderID, OrderLineID: d.LineID, Amount: d.Amount, Reason: d.Reason,
		ReasonNote: note, Status: RefundStatusRequested, RequestedBy: by, Method: d.Method, TargetIBAN: &d.TargetIBAN,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.d.Store.InsertRefund(ctx, r); err != nil {
		return uuid.Nil(), err
	}
	if err := s.d.Audit.Log(ctx, refundAudit(a, authz.PaymentsRefundCreate, r, d.Note, map[string]any{"via": "api"})); err != nil {
		return uuid.Nil(), err
	}
	s.logInfo(ctx, "refund requested", slog.String("refund_id", r.ID.String()), slog.String("payment_id", p.ID.String()),
		slog.Int64("amount", r.Amount))
	return r.ID, nil
}

// checkPool checks a request's amount against the money pool of its reason
// (see the package comment of this file).
func (s *service) checkPool(ctx context.Context, m orderMoney, reason string, lineID *uuid.UUID, amount int64, self uuid.UUID) error {
	o := m.order
	switch {
	case reason == RefundReasonOverpayment:
		if amount > m.overpaymentLeft(self) {
			return exceeds("exceeds the order's overpayment (the transfers beyond what the order waited for)")
		}
		return nil
	case o.Status == orders.StatusAwaitingPayment:
		return orders.ErrInvalidState.WithFields(fieldErrOf("body.payment_id",
			"the order still waits for its payment: cancel it to refund what was paid, or refund an overpayment"))
	case lineID == nil && o.Status.PaidState():
		return fieldErr("body.order_line_id", "is required: the order is paid, a refund gives back the money of one of its lines")
	case lineID != nil && !o.Status.PaidState():
		return fieldErr("body.order_line_id", "must be empty: the order was not paid in full, its money is refunded as a whole")
	}
	if amount > m.orderLeft(self) {
		return exceeds("exceeds what the order still holds (requested and approved refunds count)")
	}
	if lineID == nil {
		return nil
	}
	l, err := s.d.Orders.GetLine(ctx, *lineID)
	switch {
	case errors.Is(err, orders.ErrNotFound) || err == nil && l.OrderID != o.ID:
		return fieldErr("body.order_line_id", "is not a line of the payment's order")
	case err != nil:
		return err
	case amount > m.lineLeft(l, self):
		return exceeds("exceeds what Koro still holds of the line (requested and approved refunds count)")
	}
	_, err = s.inOpenPayout(ctx, l)
	return err
}

// inOpenPayout reports a payee line that is not paid out yet: its
// payout check runs (409 payments.line_in_payout when a payout waits for
// approval or for its transfer). A paid-out line or Koro's own line has
// none (checked=false).
func (s *service) inOpenPayout(ctx context.Context, l orders.Line) (checked bool, err error) {
	if l.PayeeID == nil || l.PayoutID != nil {
		return false, nil
	}
	if s.d.LedgerLines == nil {
		return false, ErrUnavailable
	}
	in, err := s.d.LedgerLines.LinesInPayouts(ctx, []uuid.UUID{l.ID})
	if err != nil {
		return false, err
	}
	if _, ok := in[l.ID]; ok {
		return true, ErrLineInPayout
	}
	return true, nil
}

// ---- decision

// ApproveRefund implements Service, the second eye (03 §6.1, fin·4·R): the
// gate (403 before reading: payments.refund.list on the admin surface) →
// the refund (404) → the eyes (403 authz.four_eyes_violation for its
// requester; a platform admin 403: fin only) → the re-authentication proof
// (403 identity.reauth_required / reauth_failed) → in one transaction: the
// refund locked, requested only (409 payments.invalid_state), the eyes again
// on the locked row → approved by, at (refunds_four_eyes_chk refuses the
// requester too) → audit.
func (s *service) ApproveRefund(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof) (Refund, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundList, adminSurface); err != nil {
		return Refund{}, err
	}
	r, err := s.d.Store.GetRefund(ctx, id, false)
	if err != nil {
		return Refund{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsRefundApprove, refundResource(r)); err != nil {
		return Refund{}, err
	}
	kind, err := s.reauth(ctx, a, proof)
	if err != nil {
		return Refund{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Refund uuid.UUID
	}{"approve_refund", id})
	if err != nil {
		return Refund{}, err
	}
	return s.transitionRefund(ctx, hash, id, func(ctx context.Context, cur Refund) (Refund, error) {
		if cur.Status != RefundStatusRequested {
			return Refund{}, ErrInvalidState
		}
		if err := authz.Can(ctx, a, authz.PaymentsRefundApprove, refundResource(cur)); err != nil {
			return Refund{}, err
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.ApprovedBy, next.ApprovedAt = RefundStatusApproved, &by, &now
		stored, err := s.d.Store.UpdateRefund(ctx, next, RefundStatusRequested)
		if err != nil {
			return Refund{}, err
		}
		return stored, s.d.Audit.Log(ctx, refundAudit(a, authz.PaymentsRefundApprove, stored, "",
			map[string]any{"requested_by": cur.RequestedBy.String(), audit.MetaReauth: kind}))
	})
}

// RejectRefund implements Service (fin): authz (403) → the note (422) → the
// refund (404) → in one transaction: requested only (409
// payments.invalid_state; an approved refund is completed after its EFT)
// → rejected by, at, with the note → audit. The requester may withdraw
// their own request this way (no money moves).
func (s *service) RejectRefund(ctx context.Context, a authz.Actor, id uuid.UUID, note string) (Refund, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundReject, adminSurface); err != nil {
		return Refund{}, err
	}
	why, err := cleanText("body.note", note, 1, maxRejectionReason)
	if err != nil {
		return Refund{}, err
	}
	if _, err := s.d.Store.GetRefund(ctx, id, false); err != nil {
		return Refund{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op, Note string
		Refund   uuid.UUID
	}{"reject_refund", why, id})
	if err != nil {
		return Refund{}, err
	}
	return s.transitionRefund(ctx, hash, id, func(ctx context.Context, cur Refund) (Refund, error) {
		if cur.Status != RefundStatusRequested {
			return Refund{}, ErrInvalidState
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.RejectedBy, next.RejectedAt, next.RejectionReason = RefundStatusRejected, &by, &now, &why
		stored, err := s.d.Store.UpdateRefund(ctx, next, RefundStatusRequested)
		if err != nil {
			return Refund{}, err
		}
		return stored, s.d.Audit.Log(ctx, refundAudit(a, authz.PaymentsRefundReject, stored, why,
			map[string]any{"requested_by": cur.RequestedBy.String()}))
	})
}

// transitionRefund runs apply on the locked refund in one transaction with
// the Idempotency-Key, then reads the result back.
func (s *service) transitionRefund(ctx context.Context, hash []byte, id uuid.UUID,
	apply func(ctx context.Context, cur Refund) (Refund, error)) (Refund, error) {
	_, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetRefund(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		stored, err := apply(ctx, cur)
		return stored.ID, err
	})
	if err != nil {
		return Refund{}, err
	}
	return s.d.Store.GetRefund(ctx, id, false)
}

// ---- completion

// CompleteRefund implements Service (fin; 01 §6.3 "İade tamamlama"): authz
// (403) → body (422: the bank reference; the receipt, a refund_receipt
// upload) → the refund (404) → in ONE transaction (the Idempotency-Key with
// it, 06 H-5):
//
//  1. the refund locked: approved only (409 payments.invalid_state);
//  2. the payment locked: a captured bank transfer that still holds the
//     amount (409 / 422 orders.refund_exceeds_paid);
//  3. the ledger template of the order's model (422 ledger.model_not_enabled:
//     models B and C, T-11 / BE-066) before any write;
//  4. the order: an overpayment is checked against the order's unapplied
//     money (orders untouched); otherwise orders.ApplyRefund (the order and
//     the line locked; 422 orders.refund_exceeds_paid; a payee line in an
//     open payout 409 payments.line_in_payout);
//  5. the payment's refunded amount, the refund completed with the receipt
//     (attached) and the bank reference;
//  6. ledger.Post(refund_completed) — a paid-out line's seller part to
//     payee_receivable (06 H-59) — then the payout check again, now that the
//     ledger holds the payee's lock (a payout created meanwhile is seen);
//  7. audit, payment.refunded (O).
//
// Any error rolls everything back: the refund stays approved. The commit
// error passes ledger.TranslateError (T-44).
func (s *service) CompleteRefund(ctx context.Context, a authz.Actor, id uuid.UUID, in RefundReceipt) (RefundResult, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundComplete, adminSurface); err != nil {
		return RefundResult{}, err
	}
	ref, err := cleanText("body.bank_reference", in.BankReference, 1, maxBankReference)
	if err != nil {
		return RefundResult{}, err
	}
	if err := s.checkFile(ctx, "body.receipt_file_id", in.ReceiptFileID, files.PurposeRefundReceipt); err != nil {
		return RefundResult{}, err
	}
	if _, err := s.d.Store.GetRefund(ctx, id, false); err != nil {
		return RefundResult{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op, Ref         string
		Refund, Receipt uuid.UUID
	}{"complete_refund", ref, id, in.ReceiptFileID})
	if err != nil {
		return RefundResult{}, err
	}
	res, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (RefundResult, error) {
		return s.complete(ctx, a, id, ref, in.ReceiptFileID)
	})
	if err != nil {
		return RefundResult{}, ledger.TranslateError(err)
	}
	return res, nil
}

// complete is the completion chain in the transaction carried by ctx.
func (s *service) complete(ctx context.Context, a authz.Actor, id uuid.UUID, ref string, receipt uuid.UUID) (RefundResult, error) {
	r, err := s.d.Store.GetRefund(ctx, id, true)
	if err != nil {
		return RefundResult{}, err
	}
	if r.Status != RefundStatusApproved {
		return RefundResult{}, ErrInvalidState
	}
	p, err := s.d.Store.GetPayment(ctx, r.PaymentID, true)
	switch {
	case err != nil:
		return RefundResult{}, err
	case !p.refundable():
		return RefundResult{}, ErrInvalidState
	case r.Amount > p.Amount-p.RefundedAmount:
		return RefundResult{}, ErrRefundExceedsPaid
	}
	o, err := s.d.Orders.ForPayment(ctx, r.OrderID)
	if err != nil {
		return RefundResult{}, err
	}
	now := s.now()
	model := ledger.Model(o.ModelCode)
	if _, err := ledger.RefundCompleted(model, r.ID, r.Amount, nil, now); err != nil {
		return RefundResult{}, err // ledger.model_not_enabled: models B and C (T-11; the C reversal is BE-066's)
	}
	shares, err := s.applyToOrder(ctx, r, model, now)
	if err != nil {
		return RefundResult{}, err
	}
	t, err := ledger.RefundCompleted(model, r.ID, shares.unallocated, shares.lines, now)
	if err != nil {
		return RefundResult{}, err
	}
	if _, err := s.d.Store.AddPaymentRefund(ctx, p.ID, r.Amount); err != nil {
		return RefundResult{}, err
	}
	by := a.UserID
	next := r
	next.Status, next.CompletedAt, next.BankReference, next.ReceiptFileID = RefundStatusCompleted, &now, &ref, &receipt
	stored, err := s.d.Store.UpdateRefund(ctx, next, RefundStatusApproved)
	if err != nil {
		return RefundResult{}, err
	}
	if err := s.attach(ctx, a, receipt, stored.ID, files.PurposeRefundReceipt); err != nil {
		return RefundResult{}, err
	}
	txnID, err := s.d.Ledger.Post(ctx, t)
	if err != nil {
		return RefundResult{}, err
	}
	if shares.payoutLine != nil {
		// Again under the payee's lock (Post took it for the seller part):
		// a payout created after the first check committed before it.
		if _, err := s.inOpenPayout(ctx, *shares.payoutLine); err != nil {
			return RefundResult{}, err
		}
	}
	if err := s.d.Audit.Log(ctx, refundAudit(a, authz.PaymentsRefundComplete, stored, "", map[string]any{
		"receipt_file_id": receipt.String(), "ledger_transaction_id": txnID.String(), "unallocated_amount": shares.unallocated,
		"clawback_amount": shares.clawback, "completed_by": by.String(),
	})); err != nil {
		return RefundResult{}, err
	}
	ev := domainevent.PaymentRefunded{RefundID: r.ID, PaymentID: r.PaymentID, OrderID: r.OrderID, Amount: r.Amount}
	if r.OrderLineID != nil {
		ev.OrderLineID = *r.OrderLineID
	}
	if err := s.emit(ctx, aggregateRefund, r.ID, ev); err != nil {
		return RefundResult{}, err
	}
	s.logInfo(ctx, "refund completed", slog.String("refund_id", r.ID.String()), slog.String("payment_id", r.PaymentID.String()),
		slog.String("order_id", r.OrderID.String()), slog.Int64("amount", r.Amount), slog.Int64("clawback", shares.clawback))
	return RefundResult{Refund: stored, TransactionID: txnID, Unallocated: shares.unallocated, Clawback: shares.clawback}, nil
}

// refundShares is how a completed refund leaves the ledger.
type refundShares struct {
	unallocated int64               // from buyer_advances
	lines       []ledger.RefundLine // an allocated payee line's shares
	clawback    int64               // the seller part claimed back from the payee (06 H-59)
	payoutLine  *orders.Line        // a payee line not paid out: checked again after Post
}

// applyToOrder records the refund on the order (not for an overpayment)
// and returns its ledger shares.
func (s *service) applyToOrder(ctx context.Context, r Refund, model ledger.Model, now time.Time) (refundShares, error) {
	if r.Overpayment() {
		m, err := s.orderMoney(ctx, r.OrderID)
		if err != nil {
			return refundShares{}, err
		}
		if r.Amount > m.overpaymentLeft(r.ID) {
			return refundShares{}, ErrRefundExceedsPaid
		}
		return refundShares{unallocated: r.Amount}, nil
	}
	if r.OrderLineID == nil {
		// An order cancelled or expired before it was paid in full: never
		// allocated (order_allocated runs once the order is paid).
		return refundShares{unallocated: r.Amount}, s.d.Orders.ApplyRefund(ctx, r.OrderID, nil, r.Amount)
	}
	before, err := s.d.Orders.GetLine(ctx, *r.OrderLineID)
	if err != nil {
		return refundShares{}, err
	}
	if _, err := s.inOpenPayout(ctx, before); err != nil {
		return refundShares{}, err
	}
	if err := s.d.Orders.ApplyRefund(ctx, r.OrderID, r.OrderLineID, r.Amount); err != nil {
		return refundShares{}, err
	}
	// Read again behind the line lock ApplyRefund took: a payout cannot mark
	// the line paid out until this transaction ends.
	l, err := s.d.Orders.GetLine(ctx, *r.OrderLineID)
	if err != nil {
		return refundShares{}, err
	}
	if s.d.LedgerLines == nil {
		return refundShares{}, ErrUnavailable
	}
	if l.PayeeID == nil {
		return s.platformRefund(ctx, r, before, l, model, now)
	}
	done, err := s.d.LedgerLines.RefundedShares(ctx, l.ID)
	if err != nil {
		return refundShares{}, err
	}
	rl, err := lineRefund(l, ledger.AllocationOf(l.Paid()), done, r.Amount)
	if err != nil {
		return refundShares{}, err
	}
	out := refundShares{lines: []ledger.RefundLine{rl}}
	if rl.PaidOut {
		out.clawback = rl.SellerAmount
	} else {
		out.payoutLine = &l
	}
	return out, nil
}

// platformRefund is the refund of Koro's own platform-native line (BE-067,
// T-293): its sale was allocated at payment (order_allocated) — or, for a
// line paid before the template, is recognised now, in this transaction,
// with the same idempotent adjustment ledger.recognize_platform_sales posts
// (a line already refunded through buyer_advances before is left to
// finance: 409). The refund takes back the sale, the buyer fee and the VAT
// in proportion to what is left of them, as a payee line's shares.
func (s *service) platformRefund(ctx context.Context, r Refund, before, l orders.Line, model ledger.Model, now time.Time) (refundShares, error) {
	a := ledger.AllocationOf(l.Paid())
	alloc, err := s.d.LedgerLines.LineAllocation(ctx, l.ID)
	switch {
	case err != nil:
		return refundShares{}, err
	case alloc == 0:
		if before.RefundedAmount != 0 {
			return refundShares{}, ErrInvalidState.WithFields(fieldErrOf("body.order_line_id",
				"Koro's own line was refunded before its sale was recognised: finance corrects the ledger by hand"))
		}
		t, err := ledger.PlatformSaleRecognized(model, a, now)
		if err != nil {
			return refundShares{}, err
		}
		if _, err := s.d.Ledger.Post(ctx, t); err != nil {
			return refundShares{}, err
		}
	case alloc != l.PayableAmount:
		return refundShares{}, fmt.Errorf("payments: line %s: the ledger allocated %d of %d", l.ID, alloc, l.PayableAmount)
	}
	done, err := s.d.LedgerLines.RefundedShares(ctx, l.ID)
	if err != nil {
		return refundShares{}, err
	}
	rl, err := lineRefund(l, a, done, r.Amount)
	if err != nil {
		return refundShares{}, err
	}
	return refundShares{lines: []ledger.RefundLine{rl}}, nil
}

// lineRefund splits amount over what is left of an allocated line's shares
// (a: the line's allocation; done: what refunds took back so far): a payee
// line's seller net, commission and commission VAT, Koro's own line's sale,
// buyer fee and VAT (BE-067). platform/money Allocate, so every share stays
// within what is left and the last refund of the line takes the exact
// remainder; the zero shares of the other kind of line take nothing.
func lineRefund(l orders.Line, a ledger.AllocationLine, done ledger.LineShares, amount int64) (ledger.RefundLine, error) {
	sh := a.Shares()
	left := []int64{sh.Seller - done.Seller, sh.Sales - done.Sales, sh.Commission - done.Commission, sh.CommissionVAT - done.CommissionVAT}
	var total int64
	for _, v := range left {
		if v < 0 {
			return ledger.RefundLine{}, fmt.Errorf("payments: line %s: the ledger refunded more than the line's shares (%+v)", l.ID, done)
		}
		total += v
	}
	if amount > total {
		return ledger.RefundLine{}, ErrRefundExceedsPaid
	}
	parts, err := money.Allocate(amount, left)
	if err != nil {
		return ledger.RefundLine{}, fmt.Errorf("payments: line %s: split the refund: %w", l.ID, err)
	}
	rl := ledger.RefundLine{
		OrderLineID: l.ID, SellerAmount: parts[0], SalesAmount: parts[1], CommissionAmount: parts[2], CommissionVATAmount: parts[3],
		PaidOut: l.PayoutID != nil,
	}
	if l.PayeeID != nil {
		rl.PayeeID = *l.PayeeID
	}
	return rl, nil
}
