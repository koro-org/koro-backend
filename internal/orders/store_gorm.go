package orders

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// constraints maps the named constraints of 000101/000102 onto module
// errors (05 §6.6): the database is the second layer of the service's
// money and party rules.
var constraints = db.Constraints{
	"orders_buyer_party_id_org_id_fkey": ErrBuyerPartyRequired,
	"orders_paid_limit_chk":             ErrAmountExceedsDue,
	"orders_refunded_limit_chk":         ErrRefundExceedsPaid,
	"order_lines_refunded_limit_chk":    ErrRefundExceedsPaid,
	"order_lines_payout_line_chk":       ErrInvalidState,
	"order_lines_seller_net_amount_chk": ErrCommissionExceedsShare,
}

type gormStore struct{ orm *gorm.DB }

// NewGormStore returns the PostgreSQL Store over the app_rw pool.
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

func lockIf(q *gorm.DB, lock bool) *gorm.DB {
	if lock {
		return q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	return q
}

func take[T any](op string, q *gorm.DB) (T, error) {
	var v T
	err := q.Take(&v).Error
	switch {
	case db.IsNotFound(err):
		return v, ErrNotFound
	case err != nil:
		return v, fmt.Errorf("orders: %s: %w", op, err)
	}
	return v, nil
}

// InsertOrder implements Store: the order and its lines in one
// transaction (a savepoint inside the caller's), so the deferred
// orders_totals_match_lines check sees them together.
//
// cross-org: the order's org is the buyer the service authorised; the
// lines carry the same org (order_lines_order_id_org_id_fkey).
func (s gormStore) InsertOrder(ctx context.Context, o Order, lines []Line) error {
	return db.Conn(ctx, s.orm).Transaction(func(q *gorm.DB) error {
		if err := q.Create(&o).Error; err != nil {
			return fmt.Errorf("orders: insert order: %w", constraints.Translate(err))
		}
		if err := q.Create(&lines).Error; err != nil {
			return fmt.Errorf("orders: insert lines: %w", constraints.Translate(err))
		}
		return nil
	})
}

// OrderNoTaken implements Store.
//
// cross-org: reference codes are unique across every order.
func (s gormStore) OrderNoTaken(ctx context.Context, orderNo string) (bool, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Model(&Order{}).Where("order_no = ?", orderNo).Count(&n).Error; err != nil {
		return false, fmt.Errorf("orders: reference code: %w", err)
	}
	return n > 0, nil
}

// GetOrder implements Store.
//
// cross-org: addressed by ID; the service authorises the loaded order.
func (s gormStore) GetOrder(ctx context.Context, id uuid.UUID, lock bool) (Order, error) {
	return take[Order]("get order", lockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// OrderByNo implements Store.
//
// cross-org: finance's lookup of a transfer reference (payments).
func (s gormStore) OrderByNo(ctx context.Context, orderNo string) (Order, error) {
	return take[Order]("order by reference", db.Conn(ctx, s.orm).Where("order_no = ?", orderNo))
}

// OrdersByID implements Store.
//
// cross-org: the orders of lines the service already scoped.
func (s gormStore) OrdersByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Order, error) {
	out := make(map[uuid.UUID]Order, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []Order
	if err := db.Conn(ctx, s.orm).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("orders: orders by id: %w", err)
	}
	for _, o := range rows {
		out[o.ID] = o
	}
	return out, nil
}

// LinesOf implements Store.
//
// cross-org: the lines of orders the service already loaded.
func (s gormStore) LinesOf(ctx context.Context, orderIDs []uuid.UUID) (map[uuid.UUID][]Line, error) {
	out := make(map[uuid.UUID][]Line, len(orderIDs))
	if len(orderIDs) == 0 {
		return out, nil
	}
	var rows []Line
	if err := db.Conn(ctx, s.orm).Where("order_id IN ?", orderIDs).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("orders: lines: %w", err)
	}
	for _, l := range rows {
		out[l.OrderID] = append(out[l.OrderID], l)
	}
	return out, nil
}

// GetLine implements Store.
//
// cross-org: addressed by ID; the service authorises the loaded line.
func (s gormStore) GetLine(ctx context.Context, id uuid.UUID, lock bool) (Line, error) {
	return take[Line]("get line", lockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// UpdateOrder implements Store: a targeted UPDATE of the running columns
// (06 H-56; orders_frozen keeps the rest).
//
// cross-org: addressed by ID after the service's authorisation.
func (s gormStore) UpdateOrder(ctx context.Context, o Order, from Status) (Order, error) {
	var rows []Order
	err := db.Conn(ctx, s.orm).Raw(`UPDATE orders SET status = @status, paid_amount = @paid_amount,
		refunded_amount = @refunded_amount, paid_at = @paid_at, completed_at = @completed_at,
		cancelled_at = @cancelled_at, cancel_reason = @cancel_reason, direct_confirmed_at = @direct_confirmed_at
		WHERE id = @id AND status = @from
		RETURNING *`, map[string]any{
		"id": o.ID, "from": from, "status": o.Status, "paid_amount": o.PaidAmount, "refunded_amount": o.RefundedAmount,
		"direct_confirmed_at": o.DirectConfirmedAt, "paid_at": o.PaidAt, "completed_at": o.CompletedAt, "cancelled_at": o.CancelledAt, "cancel_reason": o.CancelReason,
	}).Scan(&rows).Error
	if err != nil {
		return Order{}, fmt.Errorf("orders: update order: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetOrder(ctx, o.ID, false); err != nil {
			return Order{}, err
		}
		return Order{}, ErrInvalidState
	}
	return rows[0], nil
}

// UpdateLine implements Store: a targeted UPDATE of the fulfilment columns
// and the refunded amount (order_lines_frozen keeps the rest). The delivery
// evidence is written once: a set one is kept (order_lines_evidence_frozen).
//
// cross-org: addressed by ID after the service's authorisation.
func (s gormStore) UpdateLine(ctx context.Context, l Line, from Fulfillment) (Line, error) {
	var rows []Line
	err := db.Conn(ctx, s.orm).Raw(`UPDATE order_lines SET fulfillment_status = @status,
		fulfillment_marked_at = @marked_at, fulfillment_marked_by = @marked_by,
		dispute_window_ends_at = @window_ends_at, fulfilled_at = @fulfilled_at, fulfilled_via = @fulfilled_via,
		dispute_reason = @dispute_reason, refunded_amount = @refunded_amount,
		fulfillment_evidence_file_id = COALESCE(fulfillment_evidence_file_id, @evidence)
		WHERE id = @id AND fulfillment_status = @from
		RETURNING *`, map[string]any{
		"id": l.ID, "from": from, "status": l.FulfillmentStatus, "marked_at": l.FulfillmentMarkedAt,
		"marked_by": l.FulfillmentMarkedBy, "window_ends_at": l.DisputeWindowEndsAt, "fulfilled_at": l.FulfilledAt,
		"fulfilled_via": l.FulfilledVia, "dispute_reason": l.DisputeReason, "refunded_amount": l.RefundedAmount,
		"evidence": l.FulfillmentEvidenceFileID,
	}).Scan(&rows).Error
	if err != nil {
		return Line{}, fmt.Errorf("orders: update line: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetLine(ctx, l.ID, false); err != nil {
			return Line{}, err
		}
		return Line{}, ErrInvalidState
	}
	return rows[0], nil
}

// keyset adds "(col, id) < (at, id)" for a newest-first page.
func keyset(q *gorm.DB, col string, after *TimeKey) *gorm.DB {
	if after == nil {
		return q
	}
	return q.Where("("+col+", id) < ( ?, ? )", after.At, after.ID)
}

// ListOrgOrders implements Store.
func (s gormStore) ListOrgOrders(ctx context.Context, orgID uuid.UUID, f OrderFilter, after *TimeKey, limit int) ([]Order, error) {
	q := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID))
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	var rows []Order
	if err := keyset(q, "placed_at", after).Order("placed_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("orders: list org orders: %w", err)
	}
	return rows, nil
}

// ListOrders implements Store.
//
// cross-org: the admin list of every order (orders.order.list_admin).
func (s gormStore) ListOrders(ctx context.Context, f OrderFilter, after *TimeKey, limit int) ([]Order, error) {
	q := db.Conn(ctx, s.orm)
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.OrgID != nil {
		q = q.Where("org_id = ?", *f.OrgID)
	}
	if f.OrderNo != "" {
		q = q.Where("order_no = ?", f.OrderNo)
	}
	if f.PlacedFrom != nil {
		q = q.Where("placed_at >= ?", *f.PlacedFrom)
	}
	if f.PlacedUntil != nil {
		q = q.Where("placed_at < ?", *f.PlacedUntil)
	}
	var rows []Order
	if err := keyset(q, "placed_at", after).Order("placed_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("orders: list orders: %w", err)
	}
	return rows, nil
}

// ListSales implements Store.
//
// cross-org: scoped to the seller org (seller_org_id), not the buyer's org_id.
func (s gormStore) ListSales(ctx context.Context, sellerOrgID uuid.UUID, f SalesFilter, after *TimeKey, limit int) ([]Line, error) {
	q := db.Conn(ctx, s.orm).Where("seller_org_id = ?", sellerOrgID)
	if f.Fulfillment != "" {
		q = q.Where("fulfillment_status = ?", f.Fulfillment)
	}
	var rows []Line
	if err := keyset(q, "created_at", after).Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("orders: list sales: %w", err)
	}
	return rows, nil
}

// DueToExpire implements Store.
//
// cross-org: the expiry job of every order.
func (s gormStore) DueToExpire(ctx context.Context, now time.Time, limit int) ([]Order, error) {
	var rows []Order
	err := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate, Options: clause.LockingOptionsSkipLocked}).
		Where("status = 'awaiting_payment' AND expires_at < ?", now).Order("expires_at, id").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("orders: due to expire: %w", err)
	}
	return rows, nil
}

// WindowsElapsed implements Store.
//
// cross-org: the dispute window job of every line.
func (s gormStore) WindowsElapsed(ctx context.Context, now time.Time, limit int) ([]Line, error) {
	var rows []Line
	err := db.Conn(ctx, s.orm).Raw(`SELECT l.* FROM order_lines l JOIN orders o ON o.id = l.order_id
        WHERE l.fulfillment_status = 'marked' AND l.dispute_window_ends_at <= ?
        AND (o.collection_method = 'direct_to_payee' OR l.refunded_amount = 0 OR l.refunded_amount < l.payable_amount)
        ORDER BY l.dispute_window_ends_at, l.id LIMIT ?`, now, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("orders: elapsed dispute windows: %w", err)
	}
	return rows, nil
}

// koroHeldSQL is Order.KoroHeld of line l of order o: a model C order's
// line holds only its buyer fee at Koro (the rest went to the payee).
const koroHeldSQL = `(CASE WHEN o.collection_method = 'direct_to_payee' THEN l.buyer_fee_amount ELSE l.payable_amount END)`

// openStatuses are the states of Status.Open.
const openStatuses = `('awaiting_payment', 'paid', 'in_fulfilment', 'disputed', 'partially_refunded')`

// OpenOrdersWithOffering implements Store.
//
// cross-org: the offering.cancelled consumer reaches every buyer.
func (s gormStore) OpenOrdersWithOffering(ctx context.Context, offeringID uuid.UUID) ([]Order, error) {
	var rows []Order
	err := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("status IN "+openStatuses+" AND id IN ( SELECT order_id FROM order_lines WHERE offering_id = ? )", offeringID).
		Order("id").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("orders: open orders of an offering: %w", err)
	}
	return rows, nil
}

// BuyerHeld implements Store.
func (s gormStore) BuyerHeld(ctx context.Context, offeringID, buyerOrgID uuid.UUID) (int, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Raw(`SELECT coalesce(sum(l.quantity), 0) FROM order_lines l
		JOIN orders o ON o.id = l.order_id AND o.org_id = l.org_id
		WHERE l.org_id = @org_id AND l.offering_id = @offering AND l.fulfillment_status <> 'cancelled'
		  AND o.status IN `+openStatuses, map[string]any{"org_id": buyerOrgID, "offering": offeringID}).Scan(&n).Error
	if err != nil {
		return 0, fmt.Errorf("orders: buyer's units: %w", err)
	}
	return int(n), nil
}

// HasOpenCommerce implements Store (the same rule as the in-memory
// sellerOpen and refundDueOrder).
//
// cross-org: the org is the buyer (org_id) or a seller (seller_org_id).
func (s gormStore) HasOpenCommerce(ctx context.Context, orgID uuid.UUID) (bool, error) {
	var open bool
	err := db.Conn(ctx, s.orm).Raw(`SELECT EXISTS (
		  SELECT 1 FROM orders WHERE org_id = @org
		     AND (status IN `+openStatuses+`
		          OR (status IN ('cancelled', 'expired') AND paid_amount > refunded_amount)))
		OR EXISTS (
		  SELECT 1 FROM order_lines l JOIN orders o ON o.id = l.order_id
		   WHERE l.seller_org_id = @org AND o.status NOT IN ('cancelled', 'expired', 'refunded')
		     AND (l.fulfillment_status IN ('pending', 'marked', 'disputed', 'not_applicable')
		          OR (l.fulfillment_status = 'fulfilled' AND l.payee_id IS NOT NULL AND l.payout_id IS NULL AND l.refunded_amount < l.payable_amount
		              AND o.collection_method <> 'direct_to_payee')
		          OR (l.fulfillment_status = 'cancelled' AND o.paid_at IS NOT NULL AND l.refunded_amount < `+koroHeldSQL+`)))`,
		map[string]any{"org": orgID}).Scan(&open).Error
	if err != nil {
		return false, fmt.Errorf("orders: open commerce: %w", err)
	}
	return open, nil
}

// payableStatusSQL are payableStatuses in SQL.
const payableStatusSQL = `('paid', 'in_fulfilment', 'completed', 'disputed', 'partially_refunded')`

// PayableLines implements Store.
//
// cross-org: a payee's lines across its seller org's orders (ledger).
func (s gormStore) PayableLines(ctx context.Context, payeeID uuid.UUID, asOf time.Time) ([]Line, error) {
	var rows []Line
	err := db.Conn(ctx, s.orm).Raw(`SELECT l.* FROM order_lines l JOIN orders o ON o.id = l.order_id
		WHERE l.payee_id = @payee AND l.fulfillment_status = 'fulfilled' AND l.payout_id IS NULL AND l.refunded_amount < l.payable_amount
		  AND l.fulfilled_at + make_interval(days => l.payout_hold_days) <= @as_of
		  AND o.status IN `+payableStatusSQL+` AND o.collection_method <> 'direct_to_payee'
		ORDER BY l.fulfilled_at, l.id`, map[string]any{"payee": payeeID, "as_of": asOf}).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("orders: payable lines: %w", err)
	}
	return rows, nil
}

// LinesOfPayout implements Store.
//
// cross-org: the lines of one payout (billing's commission invoice).
func (s gormStore) LinesOfPayout(ctx context.Context, payoutID uuid.UUID) ([]Line, error) {
	var rows []Line
	if err := db.Conn(ctx, s.orm).Where("payout_id = ?", payoutID).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("orders: lines of payout: %w", err)
	}
	return rows, nil
}

// MarkLinesPaidOut implements Store.
//
// cross-org: the lines of one payout (ledger); the payout's payee is
// pinned by order_lines_payout_id_payee_id_fkey.
func (s gormStore) MarkLinesPaidOut(ctx context.Context, lineIDs []uuid.UUID, payoutID uuid.UUID, at time.Time) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE order_lines SET payout_id = @payout, paid_out_at = @at
		WHERE id IN @ids AND payout_id IS NULL AND payee_id IS NOT NULL AND fulfillment_status = 'fulfilled'`,
		map[string]any{"payout": payoutID, "at": at, "ids": lineIDs})
	if res.Error != nil {
		return 0, fmt.Errorf("orders: mark paid out: %w", constraints.Translate(res.Error))
	}
	return int(res.RowsAffected), nil
}

// RefundsDue implements Store.
//
// cross-org: finance's refund queue of every buyer.
func (s gormStore) RefundsDue(ctx context.Context, limit int) ([]RefundDue, error) {
	type row struct {
		OrderID uuid.UUID  `gorm:"column:order_id"`
		LineID  *uuid.UUID `gorm:"column:line_id"`
		OrgID   uuid.UUID  `gorm:"column:org_id"`
		Amount  int64      `gorm:"column:amount"`
		Since   time.Time  `gorm:"column:since"`
	}
	var rows []row
	err := db.Conn(ctx, s.orm).Raw(`SELECT * FROM (
		  SELECT l.order_id, l.id AS line_id, l.org_id, `+koroHeldSQL+` - l.refunded_amount AS amount, l.updated_at AS since
		    FROM order_lines l JOIN orders o ON o.id = l.order_id
		   WHERE l.fulfillment_status = 'cancelled' AND l.refunded_amount < `+koroHeldSQL+`
		     AND o.status IN ('paid', 'in_fulfilment', 'completed', 'disputed', 'partially_refunded')
		  UNION ALL
		  SELECT id, NULL, org_id, paid_amount - refunded_amount, updated_at
		    FROM orders WHERE status IN ('cancelled', 'expired') AND paid_amount > refunded_amount
		) due ORDER BY since, order_id, coalesce(line_id::text, '') LIMIT @limit`, map[string]any{"limit": limit}).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("orders: refunds due: %w", err)
	}
	out := make([]RefundDue, len(rows))
	for i, r := range rows {
		out[i] = RefundDue(r)
	}
	return out, nil
}

// CollectedLines implements Store (BE-067).
//
// cross-org: the ledger's allocation check over every order Koro collected.
func (s gormStore) CollectedLines(ctx context.Context, after uuid.UUID, platformOnly bool, limit int) ([]Line, error) {
	var rows []Line
	err := db.Conn(ctx, s.orm).Raw(`SELECT l.* FROM order_lines l JOIN orders o ON o.id = l.order_id
		WHERE l.id > @after AND o.paid_at IS NOT NULL AND o.collection_method <> 'direct_to_payee'
		  AND (NOT @platform OR l.payee_id IS NULL)
		ORDER BY l.id LIMIT @limit`, map[string]any{"after": after, "platform": platformOnly, "limit": limit}).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("orders: collected lines: %w", err)
	}
	return rows, nil
}

// OrdersPlacedBy implements Store.
//
// cross-org: a data subject's own records (KVKK export).
func (s gormStore) OrdersPlacedBy(ctx context.Context, userID uuid.UUID) ([]Order, error) {
	var rows []Order
	if err := db.Conn(ctx, s.orm).Where("placed_by_user_id = ?", userID).Order("placed_at, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("orders: orders placed by a user: %w", err)
	}
	return rows, nil
}

// LinesMarkedBy implements Store.
//
// cross-org: a data subject's own records (KVKK export).
func (s gormStore) LinesMarkedBy(ctx context.Context, userID uuid.UUID) ([]Line, error) {
	var rows []Line
	if err := db.Conn(ctx, s.orm).Where("fulfillment_marked_by = ?", userID).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("orders: lines marked by a user: %w", err)
	}
	return rows, nil
}
