package orders

import (
	"context"
	"errors"
	"slices"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/parties"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
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

// viewOf adds the seller names to an order and its (visible) lines; the
// seller view (lines: the seller's own) carries the seller's status of
// the order (Q-31, BE-141).
func (s *service) viewOf(ctx context.Context, o Order, lines []Line, seller bool) (OrderView, error) {
	names, err := s.d.Orgs.GetMany(ctx, sellersOf(lines))
	if err != nil {
		return OrderView{}, err
	}
	v := OrderView{Order: o, Lines: lines, Sellers: make(map[uuid.UUID]string, len(names)), SellerView: seller}
	for id, n := range names {
		v.Sellers[id] = n.Name
	}
	if seller {
		v.SellerStatus = sellerStatus(o, lines)
	}
	return v, nil
}

// fullReader reports whether a reads the whole of an order of buyer (the
// buyer's members and platform staff); anyone else authorised on it acts
// for a seller and sees only its own lines and their status (03 k26, Q-31).
func fullReader(a authz.Actor, buyer uuid.UUID) bool {
	return a.HasOrgRole(buyer, authz.MemberRecruiter, authz.MemberCompanyAdmin, authz.MemberCompanyOwner) || staff(a)
}

// GetOrder implements Service (orders.order.read): the buyer's members and
// platform staff see the whole order; a seller's club_admin,
// university_rep or career_staff sees only its own lines, their status
// (sellerStatus: never another seller's dispute or refund; Q-31) and the
// buyer's title (03 k26: no totals, no payment, no other seller).
func (s *service) GetOrder(ctx context.Context, a authz.Actor, id uuid.UUID) (OrderView, error) {
	o, lines, err := s.loadOrder(ctx, id)
	if err != nil {
		return OrderView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersOrderRead, orderResource(o, lines)); err != nil {
		return OrderView{}, err
	}
	if fullReader(a, o.OrgID) {
		v, err := s.viewOf(ctx, o, lines, false)
		if err == nil {
			v.DirectStatus, err = s.orderDirect(ctx, a, o, lines)
		}
		return v, err
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
// Model C includes the permitted direct instructions, and needs no Koro
// account once its fee due is zero.
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
	var acct CollectionAccount
	if !o.DirectToPayee() || o.DueAmount() > 0 {
		acct, err = s.collectionAccount(ctx)
		if err != nil {
			return Instructions{}, err
		}
	}
	direct, err := s.directInstructions(ctx, a, o, lines)
	if err != nil {
		return Instructions{}, err
	}
	return Instructions{DirectPayments: direct, ReferenceCode: o.OrderNo, AmountDue: o.DueAmount(), Currency: o.Currency, IBAN: acct.IBAN,
		HolderName: acct.HolderName, BankName: acct.BankName, ExpiresAt: o.ExpiresAt}, nil
}

// errNaturalPersonPayee is the cause of the 409 orders.invalid_state of a
// direct transfer to a payee whose legal party is a natural person (06
// H-54: such a payee cannot be opened in the MVP, so this is a broken
// invariant; its account is personal data and never disclosed).
var errNaturalPersonPayee = errors.New("orders: a natural person payee is not paid directly (06 H-54)")

// directVisible reports whether a may see the direct transfers of o (03
// BE-066): an unpaid model C order not yet confirmed as paid directly,
// read by an MFA-verified administrator of the buying company. No admin,
// support or ordinary buyer-member shortcut may disclose another party's
// bank account.
func directVisible(a authz.Actor, o Order) bool {
	return o.DirectToPayee() && o.Status == StatusAwaitingPayment && o.DirectConfirmedAt == nil &&
		a.MFAVerified && a.HasOrgRole(o.OrgID, authz.MemberCompanyAdmin, authz.MemberCompanyOwner)
}

// orderDirect is the direct status of getOrder (BE-109). getOrder never
// carries a payee's bank account (BE-127, data minimisation: the order
// page needs no IBAN, and every account disclosure is an audited access);
// getOrderPaymentInstructions does. A payee that cannot be paid directly
// now (no approved bank account: revoked or never approved; a natural
// person, 06 H-54) does not hide the order: the buyer still reads it and
// may cancel it, the status says the transfers are unavailable. Nothing is
// disclosed, so nothing is audited. Any other failure (a missing payee, a
// total that does not match) stays an error.
func (s *service) orderDirect(ctx context.Context, a authz.Actor, o Order, lines []Line) (DirectStatus, error) {
	if !directVisible(a, o) {
		return "", nil
	}
	_, err := s.directPlan(ctx, o, lines)
	switch {
	case errors.Is(err, parties.ErrNoApprovedAccount) || errors.Is(err, errNaturalPersonPayee):
		return DirectUnavailable, nil
	case err != nil:
		return "", err
	}
	return DirectAvailable, nil
}

// directInstructions is the direct part of the payment instructions
// (nothing unless directVisible): the plan, and the audit of the account
// access with identifiers and amounts, never the IBAN.
func (s *service) directInstructions(ctx context.Context, a authz.Actor, o Order, lines []Line) ([]DirectInstruction, error) {
	if !directVisible(a, o) {
		return nil, nil
	}
	out, err := s.directPlan(ctx, o, lines)
	if err != nil {
		return nil, err
	}
	if len(out) != 0 {
		var total int64
		for _, p := range out {
			total += p.Amount
		}
		if err := s.d.Audit.Log(ctx, audit.Event{Action: string(authz.OrdersOrderPaymentInstructions), Outcome: audit.Success,
			ResourceType: authz.TypeOrder, ResourceID: &o.ID,
			Metadata: map[string]any{"via": "direct_payment_instructions", "payee_count": len(out), "direct_amount": total}}.By(a)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// directPlan groups the frozen direct amount of o by payee with each
// payee's current approved account (409 parties.no_approved_account
// otherwise): the transfers are all or nothing. The caller decides what is
// disclosed.
func (s *service) directPlan(ctx context.Context, o Order, lines []Line) ([]DirectInstruction, error) {
	amounts := make(map[uuid.UUID]int64)
	var ids []uuid.UUID
	for _, l := range liveLines(lines) { // a dropped line is not paid (BE-134): its payee gets no transfer
		amount := directAmount(o, l)
		if amount == 0 {
			continue
		}
		if l.PayeeID == nil {
			return nil, ErrInvalidState
		}
		if _, ok := amounts[*l.PayeeID]; !ok {
			ids = append(ids, *l.PayeeID)
		}
		amounts[*l.PayeeID] += amount
	}
	out := make([]DirectInstruction, 0, len(ids))
	var total int64
	for _, id := range ids {
		py, err := s.d.Payees.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		party, err := s.d.Parties.Get(ctx, py.LegalPartyID)
		if err != nil {
			return nil, err
		}
		if party.LegalType.NaturalPerson() {
			return nil, ErrInvalidState.Wrap(errNaturalPersonPayee)
		}
		acct, err := s.d.Payees.ApprovedAccount(ctx, id)
		if err != nil {
			return nil, err
		}
		bank := ""
		if acct.BankName != nil {
			bank = *acct.BankName
		}
		out = append(out, DirectInstruction{PayeeID: id, Title: party.Title, Amount: amounts[id],
			IBAN: acct.IBAN, HolderName: acct.HolderName, BankName: bank})
		total += amounts[id]
	}
	if total != o.DirectToPayeeAmount {
		return nil, errors.New("orders: direct payment instructions do not match the frozen total")
	}
	return out, nil
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
	if err := authz.Can(ctx, a, authz.OrdersOrderList, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return OrderPage{}, err
	}
	if q.Status != "" && !q.Status.Valid() {
		return OrderPage{}, problem.Field("query.status", "is not an order status")
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
		return OrderPage{}, problem.Field("query.status", "is not an order status")
	}
	if q.OrderNo != "" {
		code, ok := NormalizeReference(q.OrderNo)
		if !ok {
			return OrderPage{}, problem.Field("query.order_no", "is not a reference code (KR + 8 characters)")
		}
		f.OrderNo = code
	}
	if f.PlacedFrom != nil && f.PlacedUntil != nil && !f.PlacedUntil.After(*f.PlacedFrom) {
		return OrderPage{}, problem.Field("query.placed_to", "must be after placed_from")
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
// org sold, newest first, with the order's reference, the buyer's title
// and the order's status as the org sees it: from all of the org's lines
// of the order, not only those on the page, and none of another seller's
// (sellerStatus; Q-31, BE-141).
func (s *service) ListOrgSales(ctx context.Context, a authz.Actor, orgID uuid.UUID, q SalesQuery) (SalesPage, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return SalesPage{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersLineListSales, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return SalesPage{}, err
	}
	if q.Fulfillment != "" && !q.Fulfillment.Valid() {
		return SalesPage{}, problem.Field("query.fulfillment_status", "is not a fulfilment status")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListSales(ctx, o.ID, SalesFilter{Fulfillment: q.Fulfillment}, q.After, limit+1)
	if err != nil {
		return SalesPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	ids := make([]uuid.UUID, 0, len(rows))
	for _, l := range rows {
		if !slices.Contains(ids, l.OrderID) {
			ids = append(ids, l.OrderID)
		}
	}
	orders, err := s.d.Store.OrdersByID(ctx, ids)
	if err != nil {
		return SalesPage{}, err
	}
	byOrder, err := s.d.Store.LinesOf(ctx, ids)
	if err != nil {
		return SalesPage{}, err
	}
	status := make(map[uuid.UUID]Status, len(orders))
	for id, ord := range orders {
		status[id] = sellerStatus(ord, sellerLines(byOrder[id], o.ID))
	}
	out := SalesPage{Items: make([]SalesItem, 0, len(rows))}
	for _, l := range rows {
		ord := orders[l.OrderID]
		out.Items = append(out.Items, SalesItem{Line: l, OrderNo: ord.OrderNo, BuyerTitle: ord.BuyerSnapshot.Title,
			SellerStatus: status[l.OrderID], Currency: ord.Currency})
	}
	if more {
		last := rows[len(rows)-1]
		out.Next = &TimeKey{At: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}
