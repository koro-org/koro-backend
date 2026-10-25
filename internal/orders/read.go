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
		v, err := s.viewOf(ctx, o, lines, false)
		if err == nil {
			v.DirectPayments, v.DirectStatus, err = s.orderDirect(ctx, a, o, lines)
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

// orderDirect is the direct part of getOrder (BE-109). Unlike the payment
// instructions, a payee that cannot be paid directly now (no approved bank
// account: revoked or never approved; a natural person, 06 H-54) does not
// hide the order: the buyer still reads it and may cancel it. The status
// says the transfers are unavailable, and no account is disclosed — not
// even an approved one of another payee, so the direct amount is never
// paid in part. Nothing was opened, so nothing is audited. Any other
// failure (a missing payee, a total that does not match) stays an error.
func (s *service) orderDirect(ctx context.Context, a authz.Actor, o Order, lines []Line) ([]DirectInstruction, DirectStatus, error) {
	if !directVisible(a, o) {
		return nil, "", nil
	}
	out, err := s.directInstructions(ctx, a, o, lines)
	switch {
	case errors.Is(err, parties.ErrNoApprovedAccount) || errors.Is(err, errNaturalPersonPayee):
		return nil, DirectUnavailable, nil
	case err != nil:
		return nil, "", err
	}
	return out, DirectAvailable, nil
}

// directInstructions groups the frozen direct amount by payee (nothing
// unless directVisible). Every payee needs its current approved account
// (409 parties.no_approved_account otherwise): the instructions are all or
// nothing. The audit contains identifiers and amounts, never the IBAN.
func (s *service) directInstructions(ctx context.Context, a authz.Actor, o Order, lines []Line) ([]DirectInstruction, error) {
	if !directVisible(a, o) {
		return nil, nil
	}
	amounts := make(map[uuid.UUID]int64)
	var ids []uuid.UUID
	for _, l := range lines {
		amount := l.PayableAmount - l.BuyerFeeAmount
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
	if len(out) != 0 {
		if err := s.d.Audit.Log(ctx, audit.Event{Action: string(authz.OrdersOrderPaymentInstructions), Outcome: audit.Success,
			ResourceType: authz.TypeOrder, ResourceID: &o.ID,
			Metadata: map[string]any{"via": "direct_payment_instructions", "payee_count": len(out), "direct_amount": total}}.By(a)); err != nil {
			return nil, err
		}
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
// org sold, newest first, with the order's reference, status and the
// buyer's title.
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
