package orders

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence of orders (05 §2): orders and their lines. Two
// implementations pass the same contract (store_contract_test.go): the
// GORM store over PostgreSQL and the in-memory store of service, wiring and
// matrix tests (05 §13.2). Writes join the transaction carried by ctx
// (db.Conn). Frozen columns are never written again (06 H-56: targeted
// UPDATEs, never Save).
type Store interface {
	// InsertOrder writes a new order with its lines (the database checks the
	// totals at commit: orders_totals_match_lines). The store stamps their
	// created_at and updated_at; the callers' are ignored (BE-069).
	InsertOrder(ctx context.Context, o Order, lines []Line) error
	// OrderNoTaken reports whether a reference code is in use.
	OrderNoTaken(ctx context.Context, orderNo string) (bool, error)
	// GetOrder returns an order (any org) or ErrNotFound; lock takes the row
	// lock (FOR UPDATE) in the caller's transaction.
	GetOrder(ctx context.Context, id uuid.UUID, lock bool) (Order, error)
	// OrderByNo returns the order of a canonical reference code or ErrNotFound.
	OrderByNo(ctx context.Context, orderNo string) (Order, error)
	// OrdersByID returns the orders among ids that exist.
	OrdersByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Order, error)
	// LinesOf returns the lines of the orders, by order, in creation order.
	LinesOf(ctx context.Context, orderIDs []uuid.UUID) (map[uuid.UUID][]Line, error)
	// GetLine returns a line or ErrNotFound; lock takes its row lock.
	GetLine(ctx context.Context, id uuid.UUID, lock bool) (Line, error)
	// UpdateOrder writes the running columns of o (status, paid and refunded
	// amounts, paid_at, completed_at, cancelled_at, cancel_reason) when its
	// status is still from; ErrInvalidState when the row moved on.
	UpdateOrder(ctx context.Context, o Order, from Status) (Order, error)
	// UpdateLine writes the fulfilment columns and the refunded amount of l
	// when its fulfilment is still from; ErrInvalidState otherwise.
	UpdateLine(ctx context.Context, l Line, from Fulfillment) (Line, error)
	// ReplaceLineEvidence writes next as the delivery evidence of a line
	// whose evidence is still cur (BE-154; order_lines_evidence_frozen lets
	// only a rejected file go); ErrInvalidState when the line holds another
	// evidence now (or none).
	ReplaceLineEvidence(ctx context.Context, lineID, cur, next uuid.UUID) (Line, error)
	// DropLines takes the lines out of their unpaid order (Q-28c, BE-134):
	// each is cancelled with dropped_at = at, then the order's amounts
	// become o's (the service's totals of the lines left). Only while the
	// order awaits payment and took no money (Order.LinesDroppable), only
	// pending or not-yet-active lines of the order not dropped before, and
	// never the last live one; the amounts must be the sums of the live
	// lines (orders_amounts_guard, orders_totals_match_lines). Anything
	// else is ErrInvalidState and writes nothing.
	DropLines(ctx context.Context, o Order, lineIDs []uuid.UUID, at time.Time) (Order, []Line, error)

	// ListOrgOrders returns a page of the buyer's orders, newest placement
	// first by (placed_at, id).
	ListOrgOrders(ctx context.Context, orgID uuid.UUID, f OrderFilter, after *TimeKey, limit int) ([]Order, error)
	// ListOrders returns a page of every order (admin), newest first.
	ListOrders(ctx context.Context, f OrderFilter, after *TimeKey, limit int) ([]Order, error)
	// ListSales returns a page of the seller's lines, newest first by
	// (created_at, id).
	ListSales(ctx context.Context, sellerOrgID uuid.UUID, f SalesFilter, after *TimeKey, limit int) ([]Line, error)

	// DueToExpire locks up to limit awaiting_payment orders whose expires_at
	// is before now (FOR UPDATE SKIP LOCKED), oldest first.
	DueToExpire(ctx context.Context, now time.Time, limit int) ([]Order, error)
	// WindowsElapsed returns up to limit marked lines whose dispute window
	// ended before now, oldest window first (candidates: the job locks each
	// order, then its line, like every other delivery change).
	WindowsElapsed(ctx context.Context, now time.Time, limit int) ([]Line, error)
	// OpenOrdersWithOffering locks (in id order) the orders that are not
	// cancelled, expired, completed or refunded and have a line of the
	// offering.
	OpenOrdersWithOffering(ctx context.Context, offeringID uuid.UUID) ([]Order, error)
	// BuyerHeld sums the quantity of the buyer's lines of the offering on
	// open orders (lines not cancelled): catalog's per_buyer_limit.
	BuyerHeld(ctx context.Context, offeringID, buyerOrgID uuid.UUID) (int, error)
	// HasOpenCommerce reports whether the org has an open order as a buyer
	// (or one with money still to refund to it: a cancelled or expired
	// order paid in part, a cancelled line of a paid order), or as a seller
	// a line still to deliver, to refund or to pay out (a model C line is
	// never paid out: its payee was paid directly).
	HasOpenCommerce(ctx context.Context, orgID uuid.UUID) (bool, error)

	// PayableLines returns the payee's lines a payout may take as of asOf
	// (01 §4.18): order paid, line fulfilled with its payout hold elapsed,
	// nothing refunded, no payout yet, Koro holding its money (not model C);
	// oldest fulfilment first.
	PayableLines(ctx context.Context, payeeID uuid.UUID, asOf time.Time) ([]Line, error)
	// MarkLinesPaidOut records the payout of the lines (only fulfilled lines
	// of a payee without a payout); it returns how many were marked.
	MarkLinesPaidOut(ctx context.Context, lineIDs []uuid.UUID, payoutID uuid.UUID, at time.Time) (int, error)
	// LinesOfPayout returns the lines a payout paid out, by (created_at, id).
	LinesOfPayout(ctx context.Context, payoutID uuid.UUID) ([]Line, error)
	// RefundsDue returns up to limit money that waits for a refund: paid
	// lines that were cancelled (what Koro holds of them, Order.Refundable;
	// never a line dropped before payment)
	// and orders cancelled or expired with money paid, not refunded yet;
	// oldest first.
	RefundsDue(ctx context.Context, limit int) ([]RefundDue, error)
	// CollectedLines returns up to limit lines of the orders Koro collected in
	// full (paid_at set, collection not direct_to_payee) with an ID greater
	// than after, in ID order, leaving out the lines dropped before payment
	// (BE-134: never collected); platformOnly keeps the lines without a payee.
	CollectedLines(ctx context.Context, after uuid.UUID, platformOnly bool, limit int) ([]Line, error)

	// OrdersPlacedBy returns the orders a user placed (KVKK export).
	OrdersPlacedBy(ctx context.Context, userID uuid.UUID) ([]Order, error)
	// LinesMarkedBy returns the lines a user marked fulfilled (KVKK export).
	LinesMarkedBy(ctx context.Context, userID uuid.UUID) ([]Line, error)
}

// TimeKey is a keyset position (time, id) of a page.
type TimeKey struct {
	At time.Time
	ID uuid.UUID
}

// OrderFilter narrows the order lists.
type OrderFilter struct {
	Status      Status     // "" = any
	OrgID       *uuid.UUID // admin list: one buyer
	OrderNo     string     // admin list: one canonical reference code
	PlacedFrom  *time.Time // admin list: placed_at >= PlacedFrom
	PlacedUntil *time.Time // admin list: placed_at < PlacedUntil
}

// SalesFilter narrows ListSales.
type SalesFilter struct {
	Fulfillment Fulfillment // "" = any
}

// RefundDue is money that waits for a refund (BE-043 opens refunds from
// it; orders cannot write payments' table): a cancelled line of a paid
// order (LineID set; offering cancelled or dispute resolved with a
// refund), or an order cancelled or expired with money paid (LineID nil).
type RefundDue struct {
	OrderID uuid.UUID
	LineID  *uuid.UUID
	OrgID   uuid.UUID // the buyer
	Amount  int64     // what is still refundable through Koro (kuruş; a model C line: its buyer fee)
	Since   time.Time // when the line or order was last changed
}
