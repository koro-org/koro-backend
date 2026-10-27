package orders

import (
	"context"
	"uuid"

	"koro/internal/authz"
)

// QuoteOrder implements Service (quoteOrder, BE-136): the orders createOrder
// would place now for the same body, priced and split by createOrder's own
// code (checkout, cart, orderOf), written nowhere — no stock taken, no
// order, no right, no audit record, no event, no Idempotency-Key. The
// cart is refused exactly as createOrder refuses it (same status, code and
// errors[].location body.lines[i].offering_id), except that the stock is
// only read (stockLeft): a placement in between may still take the last
// units, and createOrder decides. The views are unsaved: their IDs are
// throwaway, they have no reference code, and placed_at / expires_at are
// what a placement now would get.
func (s *service) QuoteOrder(ctx context.Context, a authz.Actor, orgID uuid.UUID, in CreateInput) ([]OrderView, error) {
	org, party, err := s.checkout(ctx, a, authz.OrdersOrderQuote, orgID, in)
	if err != nil {
		return nil, err
	}
	now := s.now()
	drafts, err := s.cart(ctx, in.Lines, now, s.stockLeft(org.ID))
	if err != nil {
		return nil, err
	}
	out := make([]OrderView, 0, len(drafts))
	for _, d := range drafts {
		o, lines := s.orderOf(a, org, party, d, now)
		v, err := s.viewOf(ctx, o, lines, false)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// stockLeft is quoteOrder's read of what catalog.Reserve (02 §18.2) would
// refuse a line for buyer, without the row lock and without taking the
// units: catalog's one stock rule (Stock.Refusal, BE-139) applied to the
// checkout read. ForCheckout has already refused an offering that is sold
// out or not on sale; the rule checks the units left against the line's
// quantity (catalog.sold_out) and the per-buyer limit against the units
// the buyer holds on its open orders (catalog.per_buyer_limit; BuyerHeld,
// the count catalog reads under the lock).
func (s *service) stockLeft(buyer uuid.UUID) func(context.Context, pricedLine) error {
	return func(ctx context.Context, p pricedLine) error {
		return p.offer.Stock().Refusal(p.in.Quantity, func() (int, error) {
			return s.d.Store.BuyerHeld(ctx, p.offer.ID, buyer)
		})
	}
}
