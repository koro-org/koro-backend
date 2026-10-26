package orders

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/textx"
)

// Job and task names (01 §7, §6.2).
const (
	// ExpireJobName ends the unpaid orders whose payment window passed
	// (hourly at :50, default queue).
	ExpireJobName = "orders.expire_unpaid"
	// CloseWindowsJobName fulfils the marked lines whose dispute window
	// passed (hourly at :55, default queue).
	CloseWindowsJobName = "orders.close_dispute_windows"
	// CancelOfferingOrdersTaskName consumes offering.cancelled (O).
	CancelOfferingOrdersTaskName = "orders.cancel_offering_orders"

	expireCron       = "50 * * * *"
	closeWindowsCron = "55 * * * *"
	jobUnique        = 55 * time.Minute
	jobBatch         = 200 // orders or lines per transaction (01 §7: ≤ 1000 rows; an order carries its lines)
)

// Cancellation codes (order.cancelled Reason; orders.cancel_reason of the
// system's cancellations).
const (
	CancelByBuyer        = "buyer"
	CancelByStaff        = "staff"
	CancelOfferingClosed = "offering_cancelled"
)

// maxReason is the length of a reason or note (orders.cancel_reason ≤ 500).
const maxReason = 500

// cleanText normalises a single-line free text (05 §9.5) of at most max
// characters; required rejects an empty one.
func cleanText(loc, s string, maxRunes int, required bool) (string, error) {
	n, err := textx.NormalizeText(s)
	switch {
	case err != nil:
		return "", problem.Field(loc, "contains characters that are not allowed")
	case required && n == "":
		return "", problem.Field(loc, "is required")
	case utf8.RuneCountInString(n) > maxRunes:
		return "", problem.Field(loc, "must be at most "+strconv.Itoa(maxRunes)+" characters")
	}
	return n, nil
}

// CancelOrder implements Service (04 §2.15): an order awaiting payment is
// cancelled by its buyer's admin, a platform or a finance admin; its units
// go back and its pending rights are revoked in the same transaction.
// Money already paid (a partial payment) waits for a refund (RefundsDue).
func (s *service) CancelOrder(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (OrderView, error) {
	o, lines, err := s.loadOrder(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersOrderCancel, orderResource(o, lines)); err != nil {
		return OrderView{}, err
	}
	why, err := cleanText("body.reason", reason, maxReason, true)
	if err != nil {
		return OrderView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Order  uuid.UUID
		Reason string
	}{"cancel_order", id, why})
	if err != nil {
		return OrderView{}, err
	}
	code := CancelByStaff
	if a.HasOrgRole(o.OrgID, authz.MemberCompanyAdmin, authz.MemberCompanyOwner) {
		code = CancelByBuyer
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetOrder(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != StatusAwaitingPayment {
			return uuid.Nil(), ErrInvalidState
		}
		stored, err := s.endUnpaid(ctx, cur, StatusCancelled, &why, revokeCancelled)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, orderEvent(a, authz.OrdersOrderCancel, stored, why, map[string]any{
			"from": string(cur.Status), "cause": code, "paid_amount": stored.PaidAmount,
		})); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.emitOutbox(ctx, auditOrder, stored.ID, domainevent.OrderCancelled{OrderID: stored.ID, Reason: code})
	})
	if err != nil {
		return OrderView{}, err
	}
	return s.fullView(ctx, id)
}

// endUnpaid ends an order awaiting payment (cancelled or expired) in the
// caller's transaction: its lines are cancelled, their units released
// (catalog.Release, in offering ID order like the reservations: lockOrder)
// and the pending rights of its platform-native lines revoked (01 §7
// orders.expire_unpaid).
func (s *service) endUnpaid(ctx context.Context, o Order, to Status, reason *string, revokeReason string) (Order, error) {
	now := s.now()
	next := o
	next.Status = to
	if to == StatusCancelled {
		next.CancelledAt, next.CancelReason = &now, reason
	}
	stored, err := s.d.Store.UpdateOrder(ctx, next, StatusAwaitingPayment)
	if err != nil {
		return Order{}, err
	}
	byOrder, err := s.d.Store.LinesOf(ctx, []uuid.UUID{o.ID})
	if err != nil {
		return Order{}, err
	}
	lines := byOrder[o.ID]
	for _, i := range lockOrder(len(lines), func(i int) uuid.UUID { return lines[i].OfferingID }) {
		l := lines[i]
		native := l.FulfillmentStatus == FulfillNotApplicable
		if l.FulfillmentStatus == FulfillPending || native {
			cancelled := l
			cancelled.FulfillmentStatus = FulfillCancelled
			if _, err := s.d.Store.UpdateLine(ctx, cancelled, l.FulfillmentStatus); err != nil {
				return Order{}, err
			}
		}
		if err := s.d.Offerings.Release(ctx, l.OfferingID, l.Quantity); err != nil {
			return Order{}, fmt.Errorf("orders: release the units of line %s: %w", l.ID, err)
		}
		if native {
			if err := s.d.Entitlements.RevokeBySource(ctx, entitlements.SourceOrderLine, l.ID, revokeReason); err != nil {
				return Order{}, fmt.Errorf("orders: revoke the rights of line %s: %w", l.ID, err)
			}
		}
	}
	return stored, nil
}

// expireJob is orders.expire_unpaid (01 §7).
func (s *service) expireJob() queue.Job {
	return queue.Job{Name: ExpireJobName, Cron: expireCron, Queue: queue.Default, Unique: jobUnique, Handle: s.expire}
}

// expire ends the unpaid orders whose payment window passed, in batches;
// each order, its released units, its revoked rights, its audit record and
// order.expired (A) share one transaction per batch. Idempotent: an order
// that is no longer awaiting payment is not picked again.
func (s *service) expire(ctx context.Context) error {
	sys := authz.SystemActor(ExpireJobName)
	if err := authz.Can(ctx, sys, authz.OrdersOrderExpire, authz.Resource{Type: authz.TypeOrder}); err != nil {
		return queue.Permanent(err)
	}
	now := s.now()
	total, err := db.Batches(ctx, s.d.Tx, jobBatch, func(ctx context.Context) (int, error) {
		due, err := s.d.Store.DueToExpire(ctx, now, jobBatch)
		if err != nil {
			return 0, err
		}
		for _, o := range due {
			stored, err := s.endUnpaid(ctx, o, StatusExpired, nil, revokeExpired)
			if err != nil {
				return 0, err
			}
			if err := s.d.Audit.Log(ctx, orderEvent(sys, authz.OrdersOrderExpire, stored, "", map[string]any{
				"from": string(o.Status), "paid_amount": stored.PaidAmount,
			})); err != nil {
				return 0, err
			}
			if err := s.emitAfterCommit(ctx, auditOrder, stored.ID, domainevent.OrderExpired{OrderID: stored.ID}); err != nil {
				return 0, err
			}
		}
		return len(due), nil
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "unpaid orders expired", slog.Int("count", total))
	return nil
}

// handleOfferingCancelled consumes offering.cancelled (01 §6.2; O): as the
// system, in one transaction, every unpaid order with a line of the
// offering is cancelled (its units back, its rights revoked, order.cancelled
// O), and the undelivered lines of the offering on paid orders are
// cancelled: their money waits for a refund (RefundsDue; BE-043 refunds).
// Delivered lines stay. Idempotent: cancelled orders and lines are skipped.
func (s *service) handleOfferingCancelled(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OfferingCancelled](env)
	if err != nil {
		return queue.Permanent(err)
	}
	sys := authz.SystemActor(CancelOfferingOrdersTaskName)
	why := CancelOfferingClosed
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		open, err := s.d.Store.OpenOrdersWithOffering(ctx, p.OfferingID)
		if err != nil {
			return err
		}
		for _, o := range open {
			if o.Status == StatusAwaitingPayment {
				stored, err := s.endUnpaid(ctx, o, StatusCancelled, &why, revokeCancelled)
				if err != nil {
					return err
				}
				if err := s.d.Audit.Log(ctx, orderEvent(sys, authz.OrdersOrderCancel, stored, "", map[string]any{
					"from": string(o.Status), "cause": CancelOfferingClosed, "offering_id": p.OfferingID.String(),
					"offering_cause": p.Reason, "paid_amount": stored.PaidAmount,
				})); err != nil {
					return err
				}
				if err := s.emitOutbox(ctx, auditOrder, stored.ID, domainevent.OrderCancelled{OrderID: stored.ID, Reason: CancelOfferingClosed}); err != nil {
					return err
				}
				continue
			}
			if err := s.cancelPaidLines(ctx, sys, o, p); err != nil {
				return err
			}
		}
		return nil
	})
}

// cancelPaidLines cancels the undelivered lines of the offering on a paid
// order (their money waits for a refund) and moves the order on.
func (s *service) cancelPaidLines(ctx context.Context, sys authz.Actor, o Order, p domainevent.OfferingCancelled) error {
	byOrder, err := s.d.Store.LinesOf(ctx, []uuid.UUID{o.ID})
	if err != nil {
		return err
	}
	changed := false
	for _, l := range byOrder[o.ID] {
		if l.OfferingID != p.OfferingID {
			continue
		}
		switch l.FulfillmentStatus {
		case FulfillPending, FulfillMarked, FulfillDisputed:
		default:
			continue
		}
		next := l
		next.FulfillmentStatus = FulfillCancelled
		stored, err := s.d.Store.UpdateLine(ctx, next, l.FulfillmentStatus)
		if err != nil {
			return err
		}
		if err := s.d.Audit.Log(ctx, lineEvent(sys, authz.OrdersOrderCancel, stored, "", map[string]any{
			"from": string(l.FulfillmentStatus), "cause": CancelOfferingClosed, "offering_cause": p.Reason,
			"refund_due_amount": o.Refundable(stored),
		})); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	_, err = s.advance(ctx, o.ID, s.now())
	return err
}
