package orders

import (
	"context"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/page"
)

// loadOrder reads an order with its lines (404 when it does not exist).
func (s *service) loadOrder(ctx context.Context, id uuid.UUID) (Order, []Line, error) {
	o, err := s.d.Store.GetOrder(ctx, id, false)
	if err != nil {
		return Order{}, nil, err
	}
	byOrder, err := s.d.Store.LinesOf(ctx, []uuid.UUID{o.ID})
	if err != nil {
		return Order{}, nil, err
	}
	return o, byOrder[o.ID], nil
}

// fullView is the buyer's and staff's view of an order.
func (s *service) fullView(ctx context.Context, id uuid.UUID) (OrderView, error) {
	o, lines, err := s.loadOrder(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	return s.viewOf(ctx, o, lines, false)
}

// viewOf adds the seller names to an order and its (visible) lines.
func (s *service) viewOf(ctx context.Context, o Order, lines []Line, seller bool) (OrderView, error) {
	names, err := s.d.Orgs.GetMany(ctx, sellersOf(lines))
	if err != nil {
		return OrderView{}, err
	}
	v := OrderView{Order: o, Lines: lines, Sellers: make(map[uuid.UUID]string, len(names)), SellerView: seller}
	for id, n := range names {
		v.Sellers[id] = n.Name
	}
	return v, nil
}

// GetOrder implements Service (orders.order.read): the buyer's members and
// platform staff see the whole order; a seller's club_admin,
// university_rep or career_staff sees only its own lines and the buyer's
// title (03 k26: no totals, no payment, no other seller).
func (s *service) GetOrder(ctx context.Context, a authz.Actor, id uuid.UUID) (OrderView, error) {
	o, lines, err := s.loadOrder(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersOrderRead, orderResource(o, lines)); err != nil {
		return OrderView{}, err
	}
	if a.HasOrgRole(o.OrgID, authz.MemberRecruiter, authz.MemberCompanyAdmin, authz.MemberCompanyOwner) || staff(a) {
		return s.viewOf(ctx, o, lines, false)
	}
	own := slices.DeleteFunc(slices.Clone(lines), func(l Line) bool { return !a.HasOrgRole(l.SellerOrgID, sellerRoles...) })
	if len(own) == 0 { // authz granted a seller path, so this is unreachable; fail closed
		return OrderView{}, ErrNotFound
	}
	return s.viewOf(ctx, o, own, true)
}

// PaymentInstructions implements Service (04 §C.3): the reference code,
// the amount still due and Koro's active collection account (none → 409
// payments.no_active_collection_account) of an order awaiting payment.
func (s *service) PaymentInstructions(ctx context.Context, a authz.Actor, id uuid.UUID) (Instructions, error) {
	o, lines, err := s.loadOrder(ctx, id)
	if err != nil {
		return Instructions{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersOrderPaymentInstructions, orderResource(o, lines)); err != nil {
		return Instructions{}, err
	}
	if o.Status != StatusAwaitingPayment {
		return Instructions{}, ErrInvalidState
	}
	acct, err := s.collectionAccount(ctx)
	if err != nil {
		return Instructions{}, err
	}
	return Instructions{ReferenceCode: o.OrderNo, AmountDue: o.DueAmount(), Currency: o.Currency, IBAN: acct.IBAN,
		HolderName: acct.HolderName, BankName: acct.BankName, ExpiresAt: o.ExpiresAt}, nil
}

// collectionAccount returns Koro's active collection account or 409.
func (s *service) collectionAccount(ctx context.Context) (CollectionAccount, error) {
	if s.d.CollectionAccount == nil {
		return CollectionAccount{}, ErrNoCollectionAccount
	}
	acct, found, err := s.d.CollectionAccount(ctx)
	switch {
	case err != nil:
		return CollectionAccount{}, err
	case !found || acct.IBAN == "":
		return CollectionAccount{}, ErrNoCollectionAccount
	}
	return acct, nil
}

// ListOrgOrders implements Service (orders.order.list): the buyer's orders,
// newest placement first.
func (s *service) ListOrgOrders(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (OrderPage, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return OrderPage{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersOrderList, orgResource(o)); err != nil {
		return OrderPage{}, err
	}
	if q.Status != "" && !q.Status.Valid() {
		return OrderPage{}, fieldErr("query.status", "is not an order status")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListOrgOrders(ctx, o.ID, OrderFilter{Status: q.Status}, q.After, limit+1)
	if err != nil {
		return OrderPage{}, err
	}
	return orderPage(rows, limit), nil
}

// AdminListOrders implements Service (orders.order.list_admin): every
// order, filtered by status, buyer, reference code and placement window.
func (s *service) AdminListOrders(ctx context.Context, a authz.Actor, q ListQuery) (OrderPage, error) {
	if err := authz.Can(ctx, a, authz.OrdersOrderListAdmin, adminSurface); err != nil {
		return OrderPage{}, err
	}
	f := OrderFilter{Status: q.Status, OrgID: q.OrgID, PlacedFrom: q.PlacedFrom, PlacedUntil: q.PlacedUntil}
	if q.Status != "" && !q.Status.Valid() {
		return OrderPage{}, fieldErr("query.status", "is not an order status")
	}
	if q.OrderNo != "" {
		code, ok := NormalizeReference(q.OrderNo)
		if !ok {
			return OrderPage{}, fieldErr("query.order_no", "is not a reference code (KR + 8 characters)")
		}
		f.OrderNo = code
	}
	if f.PlacedFrom != nil && f.PlacedUntil != nil && !f.PlacedUntil.After(*f.PlacedFrom) {
		return OrderPage{}, fieldErr("query.placed_to", "must be after placed_from")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListOrders(ctx, f, q.After, limit+1)
	if err != nil {
		return OrderPage{}, err
	}
	return orderPage(rows, limit), nil
}

func orderPage(rows []Order, limit int) OrderPage {
	rows, more := page.Cut(rows, limit)
	out := OrderPage{Items: rows}
	if more {
		last := rows[len(rows)-1]
		out.Next = &TimeKey{At: last.PlacedAt, ID: last.ID}
	}
	return out
}

// ListOrgSales implements Service (orders.line.list_sales): the lines the
// org sold, newest first, with the order's reference, status and the
// buyer's title.
func (s *service) ListOrgSales(ctx context.Context, a authz.Actor, orgID uuid.UUID, q SalesQuery) (SalesPage, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return SalesPage{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersLineListSales, orgResource(o)); err != nil {
		return SalesPage{}, err
	}
	if q.Fulfillment != "" && !q.Fulfillment.Valid() {
		return SalesPage{}, fieldErr("query.fulfillment_status", "is not a fulfilment status")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListSales(ctx, o.ID, SalesFilter{Fulfillment: q.Fulfillment}, q.After, limit+1)
	if err != nil {
		return SalesPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	ids := make([]uuid.UUID, 0, len(rows))
	for _, l := range rows {
		ids = append(ids, l.OrderID)
	}
	orders, err := s.d.Store.OrdersByID(ctx, ids)
	if err != nil {
		return SalesPage{}, err
	}
	out := SalesPage{Items: make([]SalesItem, 0, len(rows))}
	for _, l := range rows {
		ord := orders[l.OrderID]
		out.Items = append(out.Items, SalesItem{Line: l, OrderNo: ord.OrderNo, BuyerTitle: ord.BuyerSnapshot.Title, OrderStatus: ord.Status})
	}
	if more {
		last := rows[len(rows)-1]
		out.Next = &TimeKey{At: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}
