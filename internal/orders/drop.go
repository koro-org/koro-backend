package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/money"
	"koro/internal/platform/queue"
)

// A line of an unpaid cart whose offering is cancelled (Q-28c, user
// decision 2026-10-26; BE-134).
//
// The rule: an order awaiting payment that took no money (Order.
// LinesDroppable: nothing paid, no transfer matched to it even in part, no
// direct payment confirmed) keeps its other lines and its reference code;
// the line of the cancelled offering drops out (cancelled, dropped_at), its
// units go back to the offering and its pending rights are revoked, and the
// order's amounts become the sums of its other lines — the same per-line
// amounts priceLine froze at placement, summed by totalsOf, so the order is
// what a cart of the other lines would have been. The payment window is
// unchanged. Dropping the last line cancels the order instead.
//
// Why not after money arrived: a matched transfer was paid against the
// order's amount, and a model C direct payment was confirmed for it.
// Shrinking the amount then would leave captured money that no line holds,
// so the order ends whole (cancelUnpaidOrder) and what was paid waits for
// a refund (RefundsDue; BE-043), as before.
//
// The bank transfer race: the buyer may still send the old amount. The
// order row is locked here and by payments' capture (orders.ApplyPayment),
// so a capture either came first — then money arrived and the order ends
// whole — or sees the new amount: payments applies what the order now
// waits for and opens an overpayment refund for the rest (BE-041; the
// unapplied money stays in buyer_advances until it is refunded under four
// eyes). An underpayment stays a partial payment as before. A dropped line
// is never paid, refunded, invoiced, allocated nor paid out: the payment
// effect, CollectedLines, RefundsDue and the refund limits (KoroHeld) leave
// it out.

// NotifyLineDroppedTaskName tells the buyer who placed the order (in-app,
// and an e-mail with the new amount while it waits for payment) and the
// line's seller (in-app) that a line left an unpaid order. A task without a
// topic (mechanism A, queued after the commit of the offering.cancelled
// consumer in the worker; BE-124's pattern), one per dropped line.
const NotifyLineDroppedTaskName = "orders.notify_line_dropped"

// Notice templates of a dropped line (internal/notify/templates/orders).
const (
	tmplLineDropped     = "orders.line_dropped"      // the buyer who placed the order (e-mail + in-app)
	tmplSaleLineDropped = "orders.sale_line_dropped" // the seller's admins (in-app)
)

// revokeDropped is the revocation reason of the rights of a dropped line.
const revokeDropped = "order_line_dropped"

// lineDroppedNotice is the payload of NotifyLineDroppedTaskName (IDs only).
type lineDroppedNotice struct {
	OrderID uuid.UUID `json:"order_id"`
	LineID  uuid.UUID `json:"order_line_id"`
}

// Tasks returns the plain task handlers of orders for the worker.
func (m Module) Tasks() []queue.Task {
	return []queue.Task{{Name: NotifyLineDroppedTaskName, Handle: m.svc.handleLineDropped}}
}

// cancelUnpaidLines lets the lines of a cancelled offering go on an order
// awaiting payment (its row is locked): they drop out while other lines
// stay and the order took no money; otherwise the order is cancelled whole.
// A repeated event finds no live line of the offering and does nothing.
func (s *service) cancelUnpaidLines(ctx context.Context, sys authz.Actor, o Order, p domainevent.OfferingCancelled) error {
	byOrder, err := s.d.Store.LinesOf(ctx, []uuid.UUID{o.ID})
	if err != nil {
		return err
	}
	lines := byOrder[o.ID]
	live := liveLines(lines)
	drop := slices.DeleteFunc(slices.Clone(live), func(l Line) bool { return l.OfferingID != p.OfferingID })
	switch {
	case len(drop) == 0:
		return nil
	case len(drop) < len(live) && o.LinesDroppable():
		return s.dropLines(ctx, sys, o, live, drop, p)
	}
	return s.cancelUnpaidOrder(ctx, sys, o, p)
}

// dropLines takes drop out of the unpaid order o whose live lines are live,
// in the caller's transaction (Q-28c): the order's totals are totalsOf of
// the lines left (kuruş, platform/money), the store writes them with the
// drop, the units of the dropped lines go back (catalog.Release, in
// offering ID order like every release: lockOrder), the pending rights of
// a dropped platform-native line are revoked, every dropped line is
// audited with the order's amounts before and after, and the notices are
// queued after the commit.
func (s *service) dropLines(ctx context.Context, sys authz.Actor, o Order, live, drop []Line, p domainevent.OfferingCancelled) error {
	ids := make([]uuid.UUID, 0, len(drop))
	from := make(map[uuid.UUID]Fulfillment, len(drop))
	for _, l := range drop {
		ids = append(ids, l.ID)
		from[l.ID] = l.FulfillmentStatus
	}
	keep := make([]lineAmounts, 0, len(live)-len(drop))
	for _, l := range live {
		if _, dropped := from[l.ID]; !dropped {
			keep = append(keep, l.amounts())
		}
	}
	t, err := totalsOf(o.ModelCode, keep)
	if err != nil {
		return err
	}
	stored, dropped, err := s.d.Store.DropLines(ctx, o.withTotals(t), ids, s.now())
	if err != nil {
		return err
	}
	for _, i := range lockOrder(len(dropped), func(i int) uuid.UUID { return dropped[i].OfferingID }) {
		l := dropped[i]
		if err := s.d.Offerings.Release(ctx, l.OfferingID, l.Quantity); err != nil {
			return fmt.Errorf("orders: release the units of dropped line %s: %w", l.ID, err)
		}
		if from[l.ID] == FulfillNotApplicable {
			if err := s.d.Entitlements.RevokeBySource(ctx, entitlements.SourceOrderLine, l.ID, revokeDropped); err != nil {
				return fmt.Errorf("orders: revoke the rights of dropped line %s: %w", l.ID, err)
			}
		}
		if err := s.d.Audit.Log(ctx, lineEvent(sys, authz.OrdersOrderCancel, l, "", map[string]any{
			"from": string(from[l.ID]), "cause": CancelOfferingClosed, "offering_cause": p.Reason, "effect": "line_dropped",
			"order_status": string(stored.Status), "order_no": stored.OrderNo,
			"payable_before": o.PayableAmount, "payable_after": stored.PayableAmount,
			"koro_collect_before": o.KoroCollectAmount, "koro_collect_after": stored.KoroCollectAmount,
			"direct_to_payee_before": o.DirectToPayeeAmount, "direct_to_payee_after": stored.DirectToPayeeAmount,
		})); err != nil {
			return err
		}
		s.queueDropNotice(ctx, stored, l)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "order lines dropped", slog.String("order_id", stored.ID.String()),
		slog.String("offering_id", p.OfferingID.String()), slog.Int("lines", len(dropped)),
		slog.Int64("payable_before", o.PayableAmount), slog.Int64("payable_after", stored.PayableAmount))
	return nil
}

// liveLines are the lines of an order that did not drop out of it.
func liveLines(lines []Line) []Line {
	return slices.DeleteFunc(slices.Clone(lines), Line.Dropped)
}

// queueDropNotice queues the notices of a dropped line once the
// transaction commits (one task per line; the dedupe ID makes a repeat a
// no-op). Without a queue (a module test of another package) the notice is
// left out and logged.
func (s *service) queueDropNotice(ctx context.Context, o Order, l Line) {
	if s.d.Queue == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "order line dropped: no queue, its notices are not sent",
			slog.String("order_id", o.ID.String()), slog.String("order_line_id", l.ID.String()))
		return
	}
	s.d.Queue.AfterCommit(ctx, NotifyLineDroppedTaskName, lineDroppedNotice{OrderID: o.ID, LineID: l.ID},
		NotifyLineDroppedTaskName+":"+l.ID.String(), queue.Queue(queue.Default))
}

// handleLineDropped sends the notices of a dropped line: in-app to the
// buyer who placed the order and, while the order still waits for payment,
// an e-mail with the reference code, the new amount due and the due date;
// in-app to the admins of the line's seller. A stale task (no such order or
// line, or a line that did not drop) is skipped.
func (s *service) handleLineDropped(ctx context.Context, payload []byte) error {
	var n lineDroppedNotice
	if err := json.Unmarshal(payload, &n); err != nil || n.OrderID == uuid.Nil() || n.LineID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("orders: line dropped notice payload: %w", errors.Join(err, errors.New("order_id and order_line_id required"))))
	}
	o, lines, ok, err := s.noticeOrder(ctx, n.OrderID)
	if err != nil || !ok {
		return err
	}
	i := slices.IndexFunc(lines, func(l Line) bool { return l.ID == n.LineID && l.Dropped() })
	if i < 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "line dropped notice: no such dropped line", slog.String("order_id", o.ID.String()),
			slog.String("order_line_id", n.LineID.String()))
		return nil
	}
	l := lines[i]
	key := "line:" + l.ID.String()
	errs := []error{s.notifyInApp(ctx, o, o.OrgID, tmplLineDropped, key, []uuid.UUID{o.PlacedByUserID}, notify.OrderLink(o.ID))}
	if o.Status == StatusAwaitingPayment {
		errs = append(errs, s.mailLineDropped(ctx, o, l, key))
	}
	admins, err := s.membersWith(ctx, l.SellerOrgID, authz.MemberClubAdmin, authz.MemberUniversityRep)
	if err != nil {
		return err
	}
	errs = append(errs, s.notifyInApp(ctx, o, l.SellerOrgID, tmplSaleLineDropped, key, admins, notify.OrgSalesLink(l.SellerOrgID)))
	return errors.Join(errs...)
}

// mailLineDropped e-mails the placer the new amount of the order: the same
// reference code, what Koro now waits for and the due date (the transfer
// details stay on the order page and in the payment instructions).
func (s *service) mailLineDropped(ctx context.Context, o Order, l Line, key string) error {
	u := o.PlacedByUserID
	c, ok, err := s.contact(ctx, u)
	if err != nil || !ok {
		return err
	}
	if s.d.Sender == nil {
		return errors.New("orders: notify is not wired")
	}
	org := o.OrgID
	_, err = s.d.Sender.Send(ctx, notify.Message{
		UserID: &u, ToEmail: c.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: tmplLineDropped, Locale: c.Locale, SourceOrgID: &org,
		DedupeKey: tmplLineDropped + ":" + key + ":email",
		Data: map[string]any{
			"name": c.FirstName, "reference_code": o.OrderNo, "line_title": l.TitleSnapshot,
			"amount": money.Decimal(o.DueAmount()), "currency": o.Currency,
			"expires_at": o.ExpiresAt.In(clock.Istanbul()).Format("02.01.2006 15:04"),
		},
	})
	return s.sendErr(ctx, tmplLineDropped, o.ID, u, err)
}
