package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/entitlements"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// Limits of one createOrder request.
const (
	maxLines     = 20   // lines of one request (all models together)
	maxQuantity  = 1000 // units of one line
	maxRefDraws  = 5    // reference codes drawn before giving up (40 random bits each)
	currencyTRY  = "TRY"
	orgKindBuyer = "company"
	grantDays    = 365 // validity of a platform-native right whose template has no valid_days
	maxGrantSpan = 3 * 366 * 24 * time.Hour
)

// CreateOrder implements Service (04 §2.15, §C.2; 01 §6.3 "Sipariş
// verme"): authz (cma S of the buying company), the company's state and
// its verified buyer party (k25), then — in one transaction with the
// Idempotency-Key — the checkout of every offering, the policy in effect,
// the payee, the stock, the server-side amounts, one order per model
// (T-12), the pending rights of platform-native lines and order.placed (O).
func (s *service) CreateOrder(ctx context.Context, a authz.Actor, orgID uuid.UUID, in CreateInput) ([]OrderView, error) {
	org, err := s.org(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if err := authz.Can(ctx, a, authz.OrdersOrderCreate, authz.OrgResource(org.ID, string(org.Kind), string(org.Status))); err != nil {
		return nil, err
	}
	if err := buyerOrgUsable(org); err != nil {
		return nil, err
	}
	if err := checkCreate(in); err != nil {
		return nil, err
	}
	party, err := s.buyerParty(ctx, org.ID, in.BuyerPartyID)
	if err != nil {
		return nil, err
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Org   uuid.UUID
		Party uuid.UUID
		Lines []LineInput
	}{"create_order", org.ID, party.ID, in.Lines})
	if err != nil {
		return nil, err
	}
	ids, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) ([]uuid.UUID, error) {
		return s.place(ctx, a, org, party, in.Lines)
	})
	if err != nil {
		return nil, err
	}
	out := make([]OrderView, 0, len(ids))
	for _, id := range ids {
		v, err := s.fullView(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// buyerOrgUsable is 03 §2.3 for the buying company: only an active company
// orders (a pending_review one is not verified yet, a suspended one is
// blocked, a closed one takes nothing).
func buyerOrgUsable(o orgs.OrgSummary) error {
	if o.Kind != orgs.KindCompany {
		return ErrNotFound
	}
	switch o.Status {
	case orgs.StatusActive:
		return nil
	case orgs.StatusPendingReview:
		return orgs.ErrOrgNotVerified
	case orgs.StatusSuspended:
		return orgs.ErrOrgSuspended
	}
	return orgs.ErrInvalidState
}

// checkCreate validates the shape of a createOrder body (huma checks the
// JSON schema; the service checks again for in-process callers).
func checkCreate(in CreateInput) error {
	switch {
	case in.BuyerPartyID == uuid.Nil():
		return problem.Field("body.buyer_party_id", "is required")
	case len(in.Lines) == 0 || len(in.Lines) > maxLines:
		return problem.Field("body.lines", fmt.Sprintf("must have 1-%d lines", maxLines))
	}
	seen := map[uuid.UUID]bool{}
	for i, l := range in.Lines {
		loc := fmt.Sprintf("body.lines[%d]", i)
		switch {
		case l.OfferingID == uuid.Nil():
			return problem.Field(loc+".offering_id", "is required")
		case seen[l.OfferingID]:
			// One line per offering: the per-buyer limit of catalog.Reserve
			// counts the buyer's committed lines, not a second line of the
			// same request.
			return problem.Field(loc+".offering_id", "appears in another line: one line per offering")
		case l.Quantity < 1 || l.Quantity > maxQuantity:
			return problem.Field(loc+".quantity", fmt.Sprintf("must be 1-%d", maxQuantity))
		}
		seen[l.OfferingID] = true
	}
	return nil
}

// buyerParty is 03 k25: the billed party is a verified buyer_company party
// of the buying company (anything else, another company's party included,
// is 422 orders.buyer_party_required).
func (s *service) buyerParty(ctx context.Context, orgID, partyID uuid.UUID) (parties.LegalParty, error) {
	p, err := s.d.Parties.Get(ctx, partyID)
	switch {
	case errors.Is(err, parties.ErrNotFound):
		return parties.LegalParty{}, ErrBuyerPartyRequired
	case err != nil:
		return parties.LegalParty{}, err
	case p.Kind != parties.KindBuyerCompany || p.OrgID == nil || *p.OrgID != orgID || p.Status != parties.StatusVerified:
		return parties.LegalParty{}, ErrBuyerPartyRequired
	}
	return p, nil
}

// snapshotOf is the billed party as it is now (orders.buyer_snapshot).
func snapshotOf(p parties.LegalParty) BuyerSnapshot {
	str := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	return BuyerSnapshot{
		Title: p.Title, TaxOffice: str(p.TaxOffice), TaxNo: str(p.TaxNo), Email: str(p.Email),
		Address: SnapshotAddress{Line1: p.Address.Line1, District: p.Address.District, City: p.Address.City,
			PostalCode: p.Address.PostalCode, Country: p.Address.Country},
	}
}

// pricedLine is one request line after checkout and pricing.
type pricedLine struct {
	in     LineInput
	offer  catalog.CheckoutOffering
	policy catalog.CommercePolicy
	payee  *uuid.UUID
	amount lineAmounts
	grant  *rightTemplate // platform-native lines only
}

// rightTemplate is services.entitlement_template (catalog validates it):
// the right one unit of a platform-native offering grants.
type rightTemplate struct {
	Feature         string `json:"feature"`
	Kind            string `json:"kind"`
	QuantityPerUnit int    `json:"quantity_per_unit"`
	ValidDays       int    `json:"valid_days"`
}

// place runs in the idempotent transaction: it returns the IDs of the
// orders it wrote, in the order of their first line.
func (s *service) place(ctx context.Context, a authz.Actor, org orgs.OrgSummary, party parties.LegalParty, in []LineInput) ([]uuid.UUID, error) {
	now := s.now()
	priced := make([]pricedLine, 0, len(in))
	for i, li := range in {
		p, err := s.priceRequestLine(ctx, i, li, now)
		if err != nil {
			return nil, err
		}
		priced = append(priced, p)
	}
	// The stock of every line, under each offering's row lock (the per-buyer
	// limit reads the buyer's committed lines through BuyerHeld).
	for _, p := range priced {
		if err := s.d.Offerings.Reserve(ctx, p.offer.ID, p.in.Quantity, org.ID); err != nil {
			return nil, err
		}
	}
	type group struct {
		model, method string
		lines         []pricedLine
	}
	var groups []*group
	for _, p := range priced {
		model, method := string(p.policy.ModelCode), string(p.policy.CollectionMethod)
		var g *group
		for _, x := range groups {
			if x.model == model && x.method == method {
				g = x
			}
		}
		if g == nil {
			g = &group{model: model, method: method}
			groups = append(groups, g)
		}
		g.lines = append(g.lines, p)
	}
	used := map[string]bool{}
	ids := make([]uuid.UUID, 0, len(groups))
	for _, g := range groups {
		id, err := s.writeOrder(ctx, a, org, party, g.model, g.method, g.lines, now, used)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// priceRequestLine checks out one offering and prices its line.
func (s *service) priceRequestLine(ctx context.Context, i int, li LineInput, now time.Time) (pricedLine, error) {
	co, err := s.d.Offerings.ForCheckout(ctx, li.OfferingID)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		return pricedLine{}, problem.Field(fmt.Sprintf("body.lines[%d].offering_id", i), "is not an offering")
	case err != nil:
		return pricedLine{}, err
	case co.Currency != currencyTRY:
		return pricedLine{}, fmt.Errorf("orders: offering %s is priced in %q", co.ID, co.Currency)
	}
	pol, err := s.d.Offerings.ResolvePolicy(ctx, co.ID, now)
	if err != nil {
		return pricedLine{}, err // catalog.ErrNoPolicy: 409 catalog.no_commerce_policy (T-10)
	}
	payee, err := s.payeeFor(ctx, co)
	if err != nil {
		return pricedLine{}, err
	}
	if pol.ModelCode == catalog.ModelC && payee == nil {
		return pricedLine{}, problem.Field(fmt.Sprintf("body.lines[%d].offering_id", i), "model C requires a direct payee; platform products use model A")
	}
	amt, err := priceLine(priceInput{
		Quantity: li.Quantity, UnitPriceAmount: co.UnitPriceAmount, VATRateBP: co.VATRateBP, VATClass: co.VATClass,
		WithholdingNum: co.VATWithholdingNum, WithholdingDen: co.VATWithholdingDen, Policy: pol,
	})
	if err != nil {
		return pricedLine{}, err
	}
	p := pricedLine{in: li, offer: co, policy: pol, payee: payee, amount: amt}
	if co.FulfillmentType == catalog.FulfillPlatformNative {
		if p.grant, err = templateOf(co); err != nil {
			return pricedLine{}, err
		}
	}
	return p, nil
}

// payeeFor is the payee frozen on a line (01 §6.3: Payees.DefaultFor /
// IsActive): the offering's payee, or the seller's default one; it must
// be active now (409 catalog.payee_not_active). A platform offering has
// no payee.
func (s *service) payeeFor(ctx context.Context, co catalog.CheckoutOffering) (*uuid.UUID, error) {
	if co.SellerKind == catalog.SellerPlatform {
		return nil, nil
	}
	var id uuid.UUID
	if co.DefaultPayeeID != nil {
		id = *co.DefaultPayeeID
	} else {
		p, found, err := s.d.Payees.DefaultFor(ctx, co.SellerOrgID)
		switch {
		case err != nil:
			return nil, err
		case !found || p.OrgID != co.SellerOrgID:
			return nil, catalog.ErrPayeeNotActive
		}
		id = p.ID
	}
	ok, err := s.d.Payees.IsActive(ctx, id)
	switch {
	case errors.Is(err, parties.ErrNotFound):
		return nil, catalog.ErrPayeeNotActive
	case err != nil:
		return nil, err
	case !ok:
		return nil, catalog.ErrPayeeNotActive
	}
	return &id, nil
}

// templateOf reads the entitlement template of a platform-native offering.
func templateOf(co catalog.CheckoutOffering) (*rightTemplate, error) {
	var t rightTemplate
	if err := json.Unmarshal(co.EntitlementTemplate, &t); err != nil {
		return nil, fmt.Errorf("orders: entitlement template of service %s: %w", co.ServiceID, err)
	}
	f := entitlements.Feature(t.Feature)
	if !f.Valid() || t.QuantityPerUnit < 1 || t.ValidDays < 0 {
		return nil, fmt.Errorf("orders: entitlement template of service %s is not a known right", co.ServiceID)
	}
	return &t, nil
}

// writeOrder writes one order of one model with its lines, the pending
// rights of its platform-native lines, its audit record and order.placed.
func (s *service) writeOrder(ctx context.Context, a authz.Actor, org orgs.OrgSummary, party parties.LegalParty, model, method string,
	priced []pricedLine, now time.Time, used map[string]bool) (uuid.UUID, error) {
	amounts := make([]lineAmounts, len(priced))
	for i, p := range priced {
		amounts[i] = p.amount
	}
	t, err := totalsOf(model, amounts)
	if err != nil {
		return uuid.Nil(), err
	}
	no, err := s.newOrderNo(ctx, used)
	if err != nil {
		return uuid.Nil(), err
	}
	o := Order{
		ID: uuid.NewV7(), OrderNo: no, OrgID: org.ID, OrgKind: orgKindBuyer, PlacedByUserID: a.UserID, BuyerPartyID: party.ID,
		BuyerSnapshot: snapshotOf(party), ModelCode: model, CollectionMethod: method, Status: StatusAwaitingPayment,
		Currency: currencyTRY, SubtotalAmount: t.Subtotal, VATAmount: t.VAT, VATWithheldAmount: t.VATWithheld,
		BuyerFeeAmount: t.BuyerFee, TotalAmount: t.Total, PayableAmount: t.Payable, KoroCollectAmount: t.KoroCollect,
		DirectToPayeeAmount: t.DirectToPayee, ExpiresAt: now.Add(s.d.Config.PaymentWindow), PlacedAt: now,
	}
	lines := make([]Line, len(priced))
	for i, p := range priced {
		lines[i] = lineOf(o, p, now)
	}
	if err := s.d.Store.InsertOrder(ctx, o, lines); err != nil {
		return uuid.Nil(), err
	}
	for i, p := range priced {
		if p.grant == nil {
			continue
		}
		if err := s.grantPending(ctx, o, lines[i], *p.grant, now); err != nil {
			return uuid.Nil(), err
		}
	}
	if err := s.d.Audit.Log(ctx, orderEvent(a, authz.OrdersOrderCreate, o, "", map[string]any{
		"order_no": o.OrderNo, "model_code": o.ModelCode, "lines": len(lines), "payable_amount": o.PayableAmount,
	})); err != nil {
		return uuid.Nil(), err
	}
	if err := s.emitOutbox(ctx, auditOrder, o.ID, domainevent.OrderPlaced{OrderID: o.ID, OrgID: o.OrgID, ModelCode: o.ModelCode}); err != nil {
		return uuid.Nil(), err
	}
	return o.ID, nil
}

// lineOf builds the frozen line of a priced request line.
func lineOf(o Order, p pricedLine, now time.Time) Line {
	a := p.amount
	status := FulfillPending
	if p.grant != nil {
		status = FulfillNotApplicable
	}
	deliverables := make(Deliverables, len(p.offer.Deliverables))
	for i, d := range p.offer.Deliverables {
		deliverables[i] = Deliverable{Key: d.Key, Label: d.Label}
	}
	return Line{
		ID: uuid.NewV7(), OrderID: o.ID, OrgID: o.OrgID, SellerOrgID: p.offer.SellerOrgID, PayeeID: p.payee,
		OfferingID: p.offer.ID, ServiceID: p.offer.ServiceID, EventID: p.offer.EventID, CommercePolicyID: p.policy.ID,
		TitleSnapshot: p.offer.Title, DeliverablesSnapshot: deliverables, Quantity: p.in.Quantity,
		UnitPriceAmount: p.offer.UnitPriceAmount, NetAmount: a.Net, VATRateBP: a.VATRateBP, VATAmount: a.VAT,
		VATWithholdingNum: a.WithholdingNum, VATWithholdingDen: a.WithholdingDen, VATWithheldAmount: a.VATWithheld,
		GrossAmount: a.Gross, CommissionRateBP: a.CommissionRateBP, CommissionBasis: a.CommissionBasis,
		CommissionPayer: a.CommissionPayer, CommissionAmount: a.Commission, CommissionVATRateBP: a.CommissionVATRateBP,
		CommissionVATAmount: a.CommissionVAT, DisputeWindowDays: p.policy.DisputeWindowDays, PayoutHoldDays: p.policy.PayoutHoldDays,
		BuyerFeeAmount: a.BuyerFee, SellerNetAmount: a.SellerNet, PayableAmount: a.Payable, FulfillmentStatus: status,
	}
}

// grantPending writes the pending right of a platform-native line (01
// §6.3): quantity_per_unit × quantity units (quota) or an access right,
// valid from now for valid_days (365 when the template has none) counted
// from the end of the payment window, so a late payment loses no day; at
// most three years (entitlements' limit).
func (s *service) grantPending(ctx context.Context, o Order, l Line, t rightTemplate, now time.Time) error {
	f := entitlements.Feature(t.Feature)
	days := t.ValidDays
	if days == 0 {
		days = grantDays
	}
	validTo := o.ExpiresAt.AddDate(0, 0, days)
	if validTo.Sub(now) > maxGrantSpan {
		validTo = now.Add(maxGrantSpan)
	}
	src := l.ID
	in := entitlements.GrantInput{
		OrgID: o.OrgID, Feature: f, ValidFrom: now, ValidTo: validTo, Status: entitlements.StatusPending,
		SourceType: entitlements.SourceOrderLine, SourceID: &src,
	}
	if f.Kind() == entitlements.KindQuota {
		q := t.QuantityPerUnit * l.Quantity
		in.Quantity = &q
	}
	if _, err := s.d.Entitlements.Grant(ctx, in); err != nil {
		return fmt.Errorf("orders: pending right of line %s: %w", l.ID, err)
	}
	return nil
}

// newOrderNo draws a reference code that no order (and no other order of
// this request) has.
func (s *service) newOrderNo(ctx context.Context, used map[string]bool) (string, error) {
	for range maxRefDraws {
		no, err := newReference()
		if err != nil {
			return "", err
		}
		if used[no] {
			continue
		}
		taken, err := s.d.Store.OrderNoTaken(ctx, no)
		if err != nil {
			return "", err
		}
		if !taken {
			used[no] = true
			return no, nil
		}
	}
	return "", errors.New("orders: no free reference code after several draws")
}
