package orders

import (
	"context"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// maxDisputeReason is order_lines.dispute_reason's length (≤ 1000).
const maxDisputeReason = 1000

// Delivery (04 §2.15, §C.5): the seller marks a line of a paid order
// delivered, which opens the buyer's dispute window (the frozen
// dispute_window_days); the buyer confirms or disputes it inside the
// window; orders.close_dispute_windows fulfils it when the window passes;
// a platform or finance admin resolves a dispute. Every change locks the
// order first, then the line (one lock order for every writer), and moves
// the order's running status on (progressStatus).

// MarkLineFulfilled implements Service (orders.line.mark_fulfilled). Order:
// the line (404) → authz on it (404/403) → the note (422) → the evidence
// file, when given (a delivery_evidence upload of the line's seller org:
// 422; rejected or infected: 409; no files module: 503) → in one
// transaction: the order and the line locked, the states (409), the
// evidence attached to the line (files.Attach), the line marked with it,
// audit, order_line.fulfillment_marked (A).
func (s *service) MarkLineFulfilled(ctx context.Context, a authz.Actor, lineID uuid.UUID, in MarkInput) (LineView, error) {
	l, err := s.d.Store.GetLine(ctx, lineID, false)
	if err != nil {
		return LineView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersLineMarkFulfilled, lineResource(l)); err != nil {
		return LineView{}, err
	}
	note, err := cleanText("body.note", in.Note, maxReason, false)
	if err != nil {
		return LineView{}, err
	}
	if in.EvidenceFileID != nil {
		if err := s.checkEvidence(ctx, l, *in.EvidenceFileID); err != nil {
			return LineView{}, err
		}
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Line     uuid.UUID
		Note     string
		Evidence *uuid.UUID
	}{"mark_order_line_fulfilled", lineID, note, in.EvidenceFileID})
	if err != nil {
		return LineView{}, err
	}
	// The seller's admin reads its own lines' status back, never the order's
	// (Q-31, BE-141); a platform admin marking for the seller reads the order's.
	seller := !fullReader(a, l.OrgID)
	return s.lineChange(ctx, hash, lineID, seller, func(ctx context.Context, o Order, cur Line, now time.Time) (Line, error) {
		switch {
		case o.Status == StatusAwaitingPayment:
			return Line{}, ErrOrderNotPaid
		case !deliverable(o.Status) || fullyRefundedService(o, cur):
			return Line{}, ErrInvalidState
		case cur.FulfillmentStatus != FulfillPending:
			return Line{}, ErrInvalidState
		}
		next := cur
		ends := now.AddDate(0, 0, int(cur.DisputeWindowDays))
		by := a.UserID
		next.FulfillmentStatus, next.FulfillmentMarkedAt, next.FulfillmentMarkedBy, next.DisputeWindowEndsAt = FulfillMarked, &now, &by, &ends
		if in.EvidenceFileID != nil {
			// The same transaction binds the file to the line: it stops being an
			// orphan and no other record may take it (files.already_attached).
			if err := s.d.Files.Attach(ctx, a, *in.EvidenceFileID, evidenceOwner(cur.ID)); err != nil {
				return Line{}, evidenceAttachErr(err)
			}
			file := *in.EvidenceFileID
			next.FulfillmentEvidenceFileID = &file
		}
		stored, err := s.d.Store.UpdateLine(ctx, next, FulfillPending)
		if err != nil {
			return Line{}, err
		}
		meta := map[string]any{"from": string(cur.FulfillmentStatus), "dispute_window_ends_at": ends.Format(time.RFC3339)}
		if in.EvidenceFileID != nil {
			meta["evidence_file_id"] = in.EvidenceFileID.String()
		}
		if err := s.d.Audit.Log(ctx, lineEvent(a, authz.OrdersLineMarkFulfilled, stored, note, meta)); err != nil {
			return Line{}, err
		}
		return stored, s.emitAfterCommit(ctx, auditLine, stored.ID,
			domainevent.OrderLineFulfillmentMarked{OrderLineID: stored.ID, OrderID: stored.OrderID})
	})
}

// deliverable reports whether lines of an order in status st may move on
// (paid and not fully refunded).
func deliverable(st Status) bool {
	switch st {
	case StatusPaid, StatusInFulfilment, StatusDisputed, StatusPartiallyRefunded:
		return true
	}
	return false
}

// ConfirmLine implements Service (orders.line.confirm): the buyer confirms
// a marked line; it is fulfilled (buyer_confirmed) and order_line.fulfilled
// (O) plans its invoice and tells the seller.
func (s *service) ConfirmLine(ctx context.Context, a authz.Actor, lineID uuid.UUID) (LineView, error) {
	l, err := s.d.Store.GetLine(ctx, lineID, false)
	if err != nil {
		return LineView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersLineConfirm, lineResource(l)); err != nil {
		return LineView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		Line uuid.UUID
	}{"confirm_order_line", lineID})
	if err != nil {
		return LineView{}, err
	}
	return s.lineChange(ctx, hash, lineID, false, func(ctx context.Context, o Order, cur Line, now time.Time) (Line, error) {
		if !deliverable(o.Status) || fullyRefundedService(o, cur) || cur.FulfillmentStatus != FulfillMarked {
			return Line{}, ErrInvalidState
		}
		return s.fulfil(ctx, a, authz.OrdersLineConfirm, cur, ViaBuyerConfirmed, now, "")
	})
}

// fulfil moves a line to fulfilled (via), audits it and writes
// order_line.fulfilled (O).
func (s *service) fulfil(ctx context.Context, a authz.Actor, act authz.Action, cur Line, via FulfilledVia, now time.Time, note string) (Line, error) {
	next := cur
	v := via
	next.FulfillmentStatus, next.FulfilledAt, next.FulfilledVia = FulfillFulfilled, &now, &v
	stored, err := s.d.Store.UpdateLine(ctx, next, cur.FulfillmentStatus)
	if err != nil {
		return Line{}, err
	}
	if err := s.d.Audit.Log(ctx, lineEvent(a, act, stored, note, map[string]any{
		"from": string(cur.FulfillmentStatus), "fulfilled_via": string(via),
	})); err != nil {
		return Line{}, err
	}
	return stored, s.emitFulfilled(ctx, stored)
}

// DisputeLine implements Service (orders.line.dispute): the buyer disputes
// a marked line inside its dispute window (after it: 409
// orders.dispute_window_closed); a platform or finance admin resolves it.
func (s *service) DisputeLine(ctx context.Context, a authz.Actor, lineID uuid.UUID, reason string) (LineView, error) {
	l, err := s.d.Store.GetLine(ctx, lineID, false)
	if err != nil {
		return LineView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersLineDispute, lineResource(l)); err != nil {
		return LineView{}, err
	}
	why, err := cleanText("body.dispute_reason", reason, maxDisputeReason, true)
	if err != nil {
		return LineView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Line   uuid.UUID
		Reason string
	}{"dispute_order_line", lineID, why})
	if err != nil {
		return LineView{}, err
	}
	return s.lineChange(ctx, hash, lineID, false, func(ctx context.Context, o Order, cur Line, now time.Time) (Line, error) {
		switch {
		case cur.FulfillmentStatus == FulfillFulfilled,
			cur.FulfillmentStatus == FulfillMarked && (cur.DisputeWindowEndsAt == nil || !now.Before(*cur.DisputeWindowEndsAt)):
			return Line{}, ErrDisputeWindowClosed
		case !deliverable(o.Status), cur.FulfillmentStatus != FulfillMarked:
			return Line{}, ErrInvalidState
		}
		next := cur
		next.FulfillmentStatus, next.DisputeReason = FulfillDisputed, &why
		stored, err := s.d.Store.UpdateLine(ctx, next, FulfillMarked)
		if err != nil {
			return Line{}, err
		}
		return stored, s.d.Audit.Log(ctx, lineEvent(a, authz.OrdersLineDispute, stored, "", map[string]any{
			"from": string(cur.FulfillmentStatus),
		}))
	})
}

// AdminResolveDispute implements Service (orders.line.resolve_dispute; 04
// §2.15): the admin surface is the gate (403 before the line is read), then
// the disputed line is fulfilled (admin) or cancelled for a refund: what
// Koro holds of it waits in RefundsDue and a finance admin opens the refund
// under four eyes (BE-043). The refund is the whole line: refund_amount,
// when given, must be that amount (more → 422 orders.refund_exceeds_paid,
// less → 422; a partial refund would leave the rest to pay out, T-291).
func (s *service) AdminResolveDispute(ctx context.Context, a authz.Actor, lineID uuid.UUID, in ResolveInput) (LineView, error) {
	if err := authz.Can(ctx, a, authz.OrdersLineResolveDispute, adminSurface); err != nil {
		return LineView{}, err
	}
	l, err := s.d.Store.GetLine(ctx, lineID, false)
	if err != nil {
		return LineView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersLineResolveDispute, lineResource(l)); err != nil {
		return LineView{}, err
	}
	note, err := cleanText("body.note", in.Note, maxDisputeReason, true)
	if err != nil {
		return LineView{}, err
	}
	switch in.Outcome {
	case OutcomeFulfilled:
		if in.RefundAmount != nil {
			return LineView{}, problem.Field("body.refund_amount", "applies to a refund only")
		}
	case OutcomeRefund:
		if in.RefundAmount != nil && *in.RefundAmount < 1 {
			return LineView{}, problem.Field("body.refund_amount", "must be positive")
		}
	default:
		return LineView{}, problem.Field("body.outcome", "must be fulfilled or refund")
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Line    uuid.UUID
		Outcome string
		Note    string
		Amount  *int64
	}{"resolve_order_line_dispute", lineID, in.Outcome, note, in.RefundAmount})
	if err != nil {
		return LineView{}, err
	}
	return s.lineChange(ctx, hash, lineID, false, func(ctx context.Context, o Order, cur Line, now time.Time) (Line, error) {
		if cur.FulfillmentStatus != FulfillDisputed {
			return Line{}, ErrInvalidState
		}
		if in.Outcome == OutcomeFulfilled {
			if fullyRefundedService(o, cur) {
				return Line{}, ErrInvalidState
			}
			return s.fulfil(ctx, a, authz.OrdersLineResolveDispute, cur, ViaAdmin, now, note)
		}
		amount := o.Refundable(cur)
		if in.RefundAmount != nil {
			switch {
			case *in.RefundAmount > amount:
				return Line{}, ErrRefundExceedsPaid
			case *in.RefundAmount < amount:
				// A partial refund would leave the rest of the line to pay out,
				// which a cancelled line never is (T-291; BE-043/BE-044).
				return Line{}, problem.Field("body.refund_amount", "must be the line's whole refundable amount: partial refunds are not supported")
			}
		}
		next := cur
		next.FulfillmentStatus = FulfillCancelled
		stored, err := s.d.Store.UpdateLine(ctx, next, FulfillDisputed)
		if err != nil {
			return Line{}, err
		}
		return stored, s.d.Audit.Log(ctx, lineEvent(a, authz.OrdersLineResolveDispute, stored, note, map[string]any{
			"from": string(cur.FulfillmentStatus), "outcome": OutcomeRefund, "refund_amount": amount,
		}))
	})
}

// lineChange runs change in the idempotent transaction with the order and
// then the line locked, moves the order on and returns the line's view:
// with the order's status, or for a seller (seller) the status of the
// line's seller's lines (sellerStatus; Q-31, BE-141).
func (s *service) lineChange(ctx context.Context, hash []byte, lineID uuid.UUID, seller bool,
	change func(ctx context.Context, o Order, cur Line, now time.Time) (Line, error)) (LineView, error) {
	v, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (LineView, error) {
		l, err := s.d.Store.GetLine(ctx, lineID, false)
		if err != nil {
			return LineView{}, err
		}
		o, err := s.d.Store.GetOrder(ctx, l.OrderID, true)
		if err != nil {
			return LineView{}, err
		}
		cur, err := s.d.Store.GetLine(ctx, lineID, true)
		if err != nil {
			return LineView{}, err
		}
		now := s.now()
		stored, err := change(ctx, o, cur, now)
		if err != nil {
			return LineView{}, err
		}
		if o, err = s.advance(ctx, o.ID, now); err != nil {
			return LineView{}, err
		}
		if !seller {
			return LineView{Line: stored, OrderNo: o.OrderNo, OrderStatus: o.Status}, nil
		}
		byOrder, err := s.d.Store.LinesOf(ctx, []uuid.UUID{o.ID})
		if err != nil {
			return LineView{}, err
		}
		return LineView{Line: stored, OrderNo: o.OrderNo, OrderStatus: sellerStatus(o, sellerLines(byOrder[o.ID], stored.SellerOrgID)),
			SellerView: true}, nil
	})
	return v, err
}

// closeWindowsJob is orders.close_dispute_windows (01 §7).
func (s *service) closeWindowsJob() queue.Job {
	return queue.Job{Name: CloseWindowsJobName, Cron: closeWindowsCron, Queue: queue.Default, Unique: jobUnique, Handle: s.closeWindows}
}

// closeWindows fulfils (window_elapsed) the marked lines whose dispute
// window passed, in batches: each line, its order's status, its audit
// record and order_line.fulfilled (O) share the batch's transaction; a
// line that moved on meanwhile (confirmed, disputed) is skipped.
func (s *service) closeWindows(ctx context.Context) error {
	sys := authz.SystemActor(CloseWindowsJobName)
	if err := authz.Can(ctx, sys, authz.OrdersLineAutoFulfill, authz.Resource{Type: authz.TypeOrderLine}); err != nil {
		return queue.Permanent(err)
	}
	// total counts the fulfilled lines (read only when every batch
	// committed); a batch that fulfils none ends the loop, since its
	// skipped lines would come back unchanged.
	now, total := s.now(), 0
	_, err := db.Batches(ctx, s.d.Tx, jobBatch, func(ctx context.Context) (int, error) {
		due, err := s.d.Store.WindowsElapsed(ctx, now, jobBatch)
		if err != nil {
			return 0, err
		}
		done := 0
		for _, l := range due {
			o, err := s.d.Store.GetOrder(ctx, l.OrderID, true)
			if err != nil {
				return 0, err
			}
			cur, err := s.d.Store.GetLine(ctx, l.ID, true)
			if err != nil {
				return 0, err
			}
			if fullyRefundedService(o, cur) || cur.FulfillmentStatus != FulfillMarked || cur.DisputeWindowEndsAt == nil || cur.DisputeWindowEndsAt.After(now) {
				continue
			}
			if _, err := s.fulfil(ctx, sys, authz.OrdersLineAutoFulfill, cur, ViaWindowElapsed, now, ""); err != nil {
				return 0, err
			}
			if _, err := s.advance(ctx, cur.OrderID, now); err != nil {
				return 0, err
			}
			done++
		}
		total += done
		if done == 0 {
			return 0, nil
		}
		return len(due), nil
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "dispute windows closed", slog.Int("count", total))
	return nil
}

// fullyRefundedService only identifies money Koro collected for the service.
// Model C's fee refund does not establish that the directly paid service was
// refunded; its existing order status and rights rules remain unchanged.
// Such a line is final: its order may complete (progressStatus) and its
// seller has nothing left on it (BE-135; fullyRefundedSQL says the same).
func fullyRefundedService(o Order, l Line) bool {
	return !o.DirectToPayee() && l.RefundedAmount > 0 && l.RefundedAmount >= l.PayableAmount
}
