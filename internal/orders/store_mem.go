package orders

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"
)

// MemStore is the in-memory Store of service, wiring and matrix tests (05
// §13.2). It mirrors the rules of the GORM store that the contract tests
// pin (targeted updates of the running columns only, the status guard of
// UpdateOrder/UpdateLine, the reference code's uniqueness); it has no
// locks (db.NopTx) and no foreign keys.
type MemStore struct {
	mu     sync.Mutex
	now    func() time.Time
	orders map[uuid.UUID]Order
	lines  map[uuid.UUID]Line
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory store; now stamps created_at and
// updated_at (a caller's values are ignored, as by the column defaults and
// the touch triggers of gormStore; BE-069).
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{now: now, orders: map[uuid.UUID]Order{}, lines: map[uuid.UUID]Line{}}
}

func (m *MemStore) stamp() time.Time { return m.now().UTC().Truncate(time.Microsecond) }

// touch is the updated_at of a row changed now: never before its last
// stamp (a test clock set back).
func (m *MemStore) touch(prev time.Time) time.Time {
	if t := m.stamp(); t.After(prev) {
		return t
	}
	return prev
}

func cloneLine(l Line) Line {
	l.DeliverablesSnapshot = slices.Clone(l.DeliverablesSnapshot)
	if l.DeliverablesSnapshot == nil {
		l.DeliverablesSnapshot = Deliverables{}
	}
	return l
}

// InsertOrder implements Store.
func (m *MemStore) InsertOrder(_ context.Context, o Order, lines []Line) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o.DirectConfirmedAt != nil && o.ModelCode != ModelC {
		return ErrInvalidState
	}
	if len(lines) == 0 {
		return fmt.Errorf("orders: insert order %s: no lines (orders_totals_match_lines)", o.ID)
	}
	if _, dup := m.orders[o.ID]; dup {
		return fmt.Errorf("orders: insert order %s: duplicate id", o.ID)
	}
	for _, x := range m.orders {
		if x.OrderNo == o.OrderNo {
			return fmt.Errorf("orders: insert order: reference code %s is taken (orders_order_no_uq)", o.OrderNo)
		}
	}
	t := m.stamp()
	o.CreatedAt, o.UpdatedAt = t, t
	if o.PlacedAt.IsZero() {
		o.PlacedAt = t
	}
	if o.Currency == "" {
		o.Currency = currencyTRY
	}
	if o.OrgKind == "" {
		o.OrgKind = orgKindBuyer
	}
	m.orders[o.ID] = o
	for _, l := range lines {
		if l.OrderID != o.ID || l.OrgID != o.OrgID {
			return fmt.Errorf("orders: insert line %s: not of order %s (order_lines_order_id_org_id_fkey)", l.ID, o.ID)
		}
		l.CreatedAt, l.UpdatedAt = t, t
		m.lines[l.ID] = cloneLine(l)
	}
	return nil
}

// OrderNoTaken implements Store.
func (m *MemStore) OrderNoTaken(_ context.Context, orderNo string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.orders {
		if o.OrderNo == orderNo {
			return true, nil
		}
	}
	return false, nil
}

// GetOrder implements Store.
func (m *MemStore) GetOrder(_ context.Context, id uuid.UUID, _ bool) (Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orders[id]
	if !ok {
		return Order{}, ErrNotFound
	}
	return o, nil
}

// OrderByNo implements Store.
func (m *MemStore) OrderByNo(_ context.Context, orderNo string) (Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.orders {
		if o.OrderNo == orderNo {
			return o, nil
		}
	}
	return Order{}, ErrNotFound
}

// OrdersByID implements Store.
func (m *MemStore) OrdersByID(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[uuid.UUID]Order, len(ids))
	for _, id := range ids {
		if o, ok := m.orders[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

// linesOrdered returns the lines matching keep, by (created_at, id).
func (m *MemStore) linesOrdered(keep func(Line) bool) []Line {
	var out []Line
	for _, l := range m.lines {
		if keep(l) {
			out = append(out, cloneLine(l))
		}
	}
	slices.SortFunc(out, func(a, b Line) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return compareUUID(a.ID, b.ID)
	})
	return out
}

// LinesOf implements Store.
func (m *MemStore) LinesOf(_ context.Context, orderIDs []uuid.UUID) (map[uuid.UUID][]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[uuid.UUID][]Line, len(orderIDs))
	for _, l := range m.linesOrdered(func(l Line) bool { return slices.Contains(orderIDs, l.OrderID) }) {
		out[l.OrderID] = append(out[l.OrderID], l)
	}
	return out, nil
}

// GetLine implements Store.
func (m *MemStore) GetLine(_ context.Context, id uuid.UUID, _ bool) (Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.lines[id]
	if !ok {
		return Line{}, ErrNotFound
	}
	return cloneLine(l), nil
}

// UpdateOrder implements Store: only the running columns change.
func (m *MemStore) UpdateOrder(_ context.Context, o Order, from Status) (Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.orders[o.ID]
	switch {
	case !ok:
		return Order{}, ErrNotFound
	case cur.Status != from:
		return Order{}, ErrInvalidState
	case o.DirectConfirmedAt != nil && cur.ModelCode != ModelC:
		return Order{}, ErrInvalidState
	case cur.DirectConfirmedAt != nil && (o.DirectConfirmedAt == nil || !cur.DirectConfirmedAt.Equal(*o.DirectConfirmedAt)):
		return Order{}, ErrInvalidState
	case cur.DirectConfirmedAt == nil && o.DirectConfirmedAt != nil && cur.Status != StatusAwaitingPayment:
		return Order{}, ErrInvalidState
	case o.PaidAmount > cur.KoroCollectAmount:
		return Order{}, ErrAmountExceedsDue
	case o.RefundedAmount > o.PaidAmount:
		return Order{}, ErrRefundExceedsPaid
	}
	cur.DirectConfirmedAt = o.DirectConfirmedAt
	cur.Status, cur.PaidAmount, cur.RefundedAmount = o.Status, o.PaidAmount, o.RefundedAmount
	cur.PaidAt, cur.CompletedAt, cur.CancelledAt, cur.CancelReason = o.PaidAt, o.CompletedAt, o.CancelledAt, o.CancelReason
	cur.UpdatedAt = m.touch(cur.UpdatedAt)
	m.orders[o.ID] = cur
	return cur, nil
}

// UpdateLine implements Store: only the fulfilment columns and the
// refunded amount change; the delivery evidence is written once.
func (m *MemStore) UpdateLine(_ context.Context, l Line, from Fulfillment) (Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.lines[l.ID]
	switch {
	case !ok:
		return Line{}, ErrNotFound
	case cur.FulfillmentStatus != from:
		return Line{}, ErrInvalidState
	case l.RefundedAmount > cur.PayableAmount:
		return Line{}, ErrRefundExceedsPaid
	case cur.Dropped() && (l.FulfillmentStatus != cur.FulfillmentStatus || l.RefundedAmount != cur.RefundedAmount):
		return Line{}, ErrInvalidState // order_lines_dropped_frozen (BE-134)
	}
	cur.FulfillmentStatus, cur.FulfillmentMarkedAt, cur.FulfillmentMarkedBy = l.FulfillmentStatus, l.FulfillmentMarkedAt, l.FulfillmentMarkedBy
	cur.DisputeWindowEndsAt, cur.FulfilledAt, cur.FulfilledVia = l.DisputeWindowEndsAt, l.FulfilledAt, l.FulfilledVia
	cur.DisputeReason, cur.RefundedAmount = l.DisputeReason, l.RefundedAmount
	if cur.FulfillmentEvidenceFileID == nil && l.FulfillmentEvidenceFileID != nil {
		id := *l.FulfillmentEvidenceFileID
		cur.FulfillmentEvidenceFileID = &id
	}
	cur.UpdatedAt = m.touch(cur.UpdatedAt)
	m.lines[l.ID] = cur
	return cloneLine(cur), nil
}

// ReplaceLineEvidence implements Store.
// It applies order_lines_evidence_open_guard (000166): only an open line
// (marked or disputed, not paid out) takes another evidence.
func (m *MemStore) ReplaceLineEvidence(_ context.Context, lineID, cur, next uuid.UUID, at time.Time) (Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.lines[lineID]
	switch {
	case !ok:
		return Line{}, ErrNotFound
	case l.FulfillmentEvidenceFileID == nil || *l.FulfillmentEvidenceFileID != cur:
		return Line{}, ErrInvalidState
	case l.FulfillmentStatus != FulfillMarked && l.FulfillmentStatus != FulfillDisputed, l.PayoutID != nil, l.PaidOutAt != nil:
		return Line{}, ErrInvalidState
	}
	replaced := at.UTC().Truncate(time.Microsecond)
	l.FulfillmentEvidenceFileID, l.EvidenceReplacedAt = &next, &replaced
	l.UpdatedAt = m.touch(l.UpdatedAt)
	m.lines[lineID] = l
	return cloneLine(l), nil
}

// DropLines implements Store (BE-134) with the rules of 000157: the order
// takes no more money, the lines are pending or not yet active lines of it,
// at least one line stays, and the new amounts are the sums of the lines
// left (Koro's part the model's). It writes only when everything holds.
func (m *MemStore) DropLines(_ context.Context, o Order, lineIDs []uuid.UUID, at time.Time) (Order, []Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.orders[o.ID]
	switch {
	case !ok:
		return Order{}, nil, ErrNotFound
	case !cur.LinesDroppable() || len(lineIDs) == 0:
		return Order{}, nil, ErrInvalidState
	}
	dropping := map[uuid.UUID]bool{}
	for _, id := range lineIDs {
		l, found := m.lines[id]
		if !found || l.OrderID != o.ID || l.Dropped() || dropping[id] ||
			l.FulfillmentStatus != FulfillPending && l.FulfillmentStatus != FulfillNotApplicable {
			return Order{}, nil, ErrInvalidState
		}
		dropping[id] = true
	}
	var live int
	var net, vat, withheld, fee, payable int64
	for _, l := range m.lines {
		if l.OrderID != o.ID || l.Dropped() || dropping[l.ID] {
			continue
		}
		live++
		net, vat, withheld, fee, payable = net+l.NetAmount, vat+l.VATAmount, withheld+l.VATWithheldAmount, fee+l.BuyerFeeAmount,
			payable+l.PayableAmount
	}
	collect := payable
	if cur.DirectToPayee() {
		collect = fee
	}
	if live == 0 || o.SubtotalAmount != net || o.VATAmount != vat || o.VATWithheldAmount != withheld || o.BuyerFeeAmount != fee ||
		o.PayableAmount != payable || o.KoroCollectAmount != collect || o.TotalAmount != net+vat+fee ||
		o.DirectToPayeeAmount != payable-collect {
		return Order{}, nil, ErrInvalidState // orders_amounts_guard, orders_totals_match_lines
	}
	cur.SubtotalAmount, cur.VATAmount, cur.VATWithheldAmount, cur.BuyerFeeAmount = net, vat, withheld, fee
	cur.TotalAmount, cur.PayableAmount, cur.KoroCollectAmount, cur.DirectToPayeeAmount = o.TotalAmount, payable, collect, o.DirectToPayeeAmount
	cur.UpdatedAt = m.touch(cur.UpdatedAt)
	m.orders[o.ID] = cur
	when := at
	for id := range dropping {
		l := m.lines[id]
		l.FulfillmentStatus, l.DroppedAt, l.UpdatedAt = FulfillCancelled, &when, m.touch(l.UpdatedAt)
		m.lines[id] = l
	}
	return cur, m.linesOrdered(func(l Line) bool { return dropping[l.ID] }), nil
}

// before reports whether (at, id) comes after the keyset position k in a
// newest-first list (the next page holds the older rows).
func before(at time.Time, id uuid.UUID, k *TimeKey) bool {
	if k == nil {
		return true
	}
	if c := at.Compare(k.At); c != 0 {
		return c < 0
	}
	return compareUUID(id, k.ID) < 0
}

func newestFirst(a, b Order) int {
	if c := b.PlacedAt.Compare(a.PlacedAt); c != 0 {
		return c
	}
	return compareUUID(b.ID, a.ID)
}

func (m *MemStore) orderPage(keep func(Order) bool, after *TimeKey, limit int) []Order {
	var out []Order
	for _, o := range m.orders {
		if keep(o) && before(o.PlacedAt, o.ID, after) {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, newestFirst)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ListOrgOrders implements Store.
func (m *MemStore) ListOrgOrders(_ context.Context, orgID uuid.UUID, f OrderFilter, after *TimeKey, limit int) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.orderPage(func(o Order) bool { return o.OrgID == orgID && (f.Status == "" || o.Status == f.Status) }, after, limit), nil
}

// ListOrders implements Store.
func (m *MemStore) ListOrders(_ context.Context, f OrderFilter, after *TimeKey, limit int) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.orderPage(func(o Order) bool {
		return (f.Status == "" || o.Status == f.Status) && (f.OrgID == nil || o.OrgID == *f.OrgID) &&
			(f.OrderNo == "" || o.OrderNo == f.OrderNo) && (f.PlacedFrom == nil || !o.PlacedAt.Before(*f.PlacedFrom)) &&
			(f.PlacedUntil == nil || o.PlacedAt.Before(*f.PlacedUntil))
	}, after, limit), nil
}

// ListSales implements Store.
func (m *MemStore) ListSales(_ context.Context, sellerOrgID uuid.UUID, f SalesFilter, after *TimeKey, limit int) ([]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Line
	for _, l := range m.lines {
		if l.SellerOrgID == sellerOrgID && (f.Fulfillment == "" || l.FulfillmentStatus == f.Fulfillment) && before(l.CreatedAt, l.ID, after) {
			out = append(out, cloneLine(l))
		}
	}
	slices.SortFunc(out, func(a, b Line) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return compareUUID(b.ID, a.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DueToExpire implements Store.
func (m *MemStore) DueToExpire(_ context.Context, now time.Time, limit int) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Order
	for _, o := range m.orders {
		if o.Status == StatusAwaitingPayment && o.ExpiresAt.Before(now) {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b Order) int {
		if c := a.ExpiresAt.Compare(b.ExpiresAt); c != 0 {
			return c
		}
		return compareUUID(a.ID, b.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// WindowsElapsed implements Store.
func (m *MemStore) WindowsElapsed(_ context.Context, now time.Time, limit int) ([]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Line
	for _, l := range m.lines {
		if l.FulfillmentStatus == FulfillMarked && l.DisputeWindowEndsAt != nil && !l.DisputeWindowEndsAt.After(now) && !fullyRefundedService(m.orders[l.OrderID], l) {
			out = append(out, cloneLine(l))
		}
	}
	slices.SortFunc(out, func(a, b Line) int {
		if c := a.DisputeWindowEndsAt.Compare(*b.DisputeWindowEndsAt); c != 0 {
			return c
		}
		return compareUUID(a.ID, b.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// OpenOrdersWithOffering implements Store.
func (m *MemStore) OpenOrdersWithOffering(_ context.Context, offeringID uuid.UUID) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[uuid.UUID]bool{}
	var out []Order
	for _, l := range m.lines {
		o := m.orders[l.OrderID]
		if l.OfferingID == offeringID && o.Status.Open() && !seen[o.ID] {
			seen[o.ID] = true
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b Order) int { return compareUUID(a.ID, b.ID) })
	return out, nil
}

// BuyerHeld implements Store.
func (m *MemStore) BuyerHeld(_ context.Context, offeringID, buyerOrgID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, l := range m.lines {
		if l.OfferingID == offeringID && l.OrgID == buyerOrgID && l.FulfillmentStatus != FulfillCancelled &&
			m.orders[l.OrderID].Status.Open() {
			n += l.Quantity
		}
	}
	return n, nil
}

// sellerOpen is the seller half of HasOpenCommerce for line l of order o
// (the GORM store's SQL says the same): a model C line is never paid out; a
// line refunded in full is final, nothing is left to deliver or pay out of
// it (BE-135).
func sellerOpen(o Order, l Line) bool {
	switch o.Status {
	case StatusCancelled, StatusExpired, StatusRefunded:
		return false
	}
	switch l.FulfillmentStatus {
	case FulfillPending, FulfillMarked, FulfillDisputed, FulfillNotApplicable:
		return !fullyRefundedService(o, l)
	case FulfillFulfilled:
		return l.PayeeID != nil && l.PayoutID == nil && l.RefundedAmount < l.PayableAmount && !o.DirectToPayee()
	case FulfillCancelled:
		return o.Status.PaidState() && o.Refundable(l) > 0
	}
	return false
}

// HasOpenCommerce implements Store.
func (m *MemStore) HasOpenCommerce(_ context.Context, orgID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.orders {
		if o.OrgID == orgID && (o.Status.Open() || refundDueOrder(o)) {
			return true, nil
		}
	}
	for _, l := range m.lines {
		o := m.orders[l.OrderID]
		if (l.SellerOrgID == orgID && sellerOpen(o, l)) || (o.OrgID == orgID && refundDueLine(o, l)) {
			return true, nil
		}
	}
	return false, nil
}

// refundDueOrder reports an order cancelled or expired with money paid
// and not refunded yet.
func refundDueOrder(o Order) bool {
	return (o.Status == StatusCancelled || o.Status == StatusExpired) && o.PaidAmount > o.RefundedAmount
}

// refundDueLine reports a cancelled line of a paid order whose money Koro
// still holds (RefundsDue; the buyer's open commerce, BE-131).
func refundDueLine(o Order, l Line) bool {
	return l.FulfillmentStatus == FulfillCancelled && o.Status.PaidState() && o.Status != StatusRefunded && o.Refundable(l) > 0
}

// payableStatuses are the order states whose fulfilled lines may be paid out.
var payableStatuses = []Status{StatusPaid, StatusInFulfilment, StatusCompleted, StatusDisputed, StatusPartiallyRefunded}

// PayableLines implements Store.
func (m *MemStore) PayableLines(_ context.Context, payeeID uuid.UUID, asOf time.Time) ([]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Line
	for _, l := range m.lines {
		o := m.orders[l.OrderID]
		if l.PayeeID == nil || *l.PayeeID != payeeID || l.FulfillmentStatus != FulfillFulfilled || l.PayoutID != nil ||
			l.RefundedAmount >= l.PayableAmount || l.FulfilledAt == nil || !slices.Contains(payableStatuses, o.Status) || o.DirectToPayee() {
			continue
		}
		if l.FulfilledAt.AddDate(0, 0, int(l.PayoutHoldDays)).After(asOf) {
			continue
		}
		out = append(out, cloneLine(l))
	}
	slices.SortFunc(out, func(a, b Line) int {
		if c := a.FulfilledAt.Compare(*b.FulfilledAt); c != 0 {
			return c
		}
		return compareUUID(a.ID, b.ID)
	})
	return out, nil
}

// LinesOfPayout implements Store.
func (m *MemStore) LinesOfPayout(_ context.Context, payoutID uuid.UUID) ([]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.linesOrdered(func(l Line) bool { return l.PayoutID != nil && *l.PayoutID == payoutID }), nil
}

// MarkLinesPaidOut implements Store. It writes only when every line
// qualifies: the service rolls a partial payout back, which db.NopTx
// cannot do (the count is the same as the GORM store's).
func (m *MemStore) MarkLinesPaidOut(_ context.Context, lineIDs []uuid.UUID, payoutID uuid.UUID, at time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ok []uuid.UUID
	for _, id := range lineIDs {
		l, found := m.lines[id]
		if found && l.PayeeID != nil && l.FulfillmentStatus == FulfillFulfilled && l.PayoutID == nil && !slices.Contains(ok, id) {
			ok = append(ok, id)
		}
	}
	if len(ok) != len(lineIDs) {
		return len(ok), nil
	}
	for _, id := range ok {
		l := m.lines[id]
		p, t := payoutID, at
		l.PayoutID, l.PaidOutAt, l.UpdatedAt = &p, &t, m.touch(l.UpdatedAt)
		m.lines[id] = l
	}
	return len(ok), nil
}

// CollectedLines implements Store.
func (m *MemStore) CollectedLines(_ context.Context, after uuid.UUID, platformOnly bool, limit int) ([]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Line{}
	for _, l := range m.lines {
		o := m.orders[l.OrderID]
		if compareUUID(l.ID, after) <= 0 || o.PaidAt == nil || o.DirectToPayee() || l.Dropped() || platformOnly && l.PayeeID != nil {
			continue
		}
		out = append(out, cloneLine(l))
	}
	slices.SortFunc(out, func(a, b Line) int { return compareUUID(a.ID, b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RefundsDue implements Store.
func (m *MemStore) RefundsDue(_ context.Context, limit int) ([]RefundDue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RefundDue
	for _, l := range m.lines {
		o := m.orders[l.OrderID]
		if refundDueLine(o, l) {
			id := l.ID
			out = append(out, RefundDue{OrderID: o.ID, LineID: &id, OrgID: o.OrgID, Amount: o.Refundable(l), Since: l.UpdatedAt})
		}
	}
	for _, o := range m.orders {
		if refundDueOrder(o) {
			out = append(out, RefundDue{OrderID: o.ID, OrgID: o.OrgID, Amount: o.PaidAmount - o.RefundedAmount, Since: o.UpdatedAt})
		}
	}
	slices.SortFunc(out, func(a, b RefundDue) int {
		if c := a.Since.Compare(b.Since); c != 0 {
			return c
		}
		if c := compareUUID(a.OrderID, b.OrderID); c != 0 {
			return c
		}
		return cmp.Compare(lineKey(a.LineID), lineKey(b.LineID))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func lineKey(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// OrdersPlacedBy implements Store.
func (m *MemStore) OrdersPlacedBy(_ context.Context, userID uuid.UUID) ([]Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Order
	for _, o := range m.orders {
		if o.PlacedByUserID == userID {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b Order) int { return -newestFirst(a, b) })
	return out, nil
}

// LinesMarkedBy implements Store.
func (m *MemStore) LinesMarkedBy(_ context.Context, userID uuid.UUID) ([]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.linesOrdered(func(l Line) bool { return l.FulfillmentMarkedBy != nil && *l.FulfillmentMarkedBy == userID }), nil
}
