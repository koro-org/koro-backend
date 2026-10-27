package catalog

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// ForCheckout implements Offerings: the offering must be on sale now with
// stock left; the service's VAT profile is joined in (BE-040 freezes both).
func (s *service) ForCheckout(ctx context.Context, offeringID uuid.UUID) (CheckoutOffering, error) {
	o, err := s.d.Store.GetOffering(ctx, offeringID, false)
	if err != nil {
		return CheckoutOffering{}, err
	}
	if o.Status == OfferingSoldOut {
		return CheckoutOffering{}, ErrSoldOut
	}
	if !o.OnSaleAt(s.now()) {
		return CheckoutOffering{}, ErrNotOnSale
	}
	if r := o.Remaining(); r != nil && *r <= 0 {
		return CheckoutOffering{}, ErrSoldOut
	}
	sv, err := s.d.Store.GetService(ctx, o.ServiceID, false)
	if err != nil {
		return CheckoutOffering{}, err
	}
	return checkoutOf(o, sv), nil
}

// Reserve implements Offerings (02 §18.2): in the caller's transaction
// (its own without one) the offering row is locked, the stock rule is
// applied to the locked row (Stock.Refusal: the per-buyer limit against
// the buyer's open orders, BuyerHeld, read under the lock, so two orders
// of one buyer cannot both pass), then one conditional UPDATE takes the
// units; the last one turns the offering sold_out.
func (s *service) Reserve(ctx context.Context, offeringID uuid.UUID, qty int, buyerOrgID uuid.UUID) error {
	if qty <= 0 || qty > maxQuantity {
		return problem.Field("quantity", "must be 1-100000")
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.GetOffering(ctx, offeringID, true)
		if err != nil {
			return err
		}
		now := s.now()
		if err := saleRefusal(cur, now); err != nil {
			return err
		}
		held := func() (int, error) {
			if s.d.BuyerHeld == nil {
				return 0, nil
			}
			n, err := s.d.BuyerHeld(ctx, cur.ID, buyerOrgID)
			if err != nil {
				return 0, fmt.Errorf("catalog: buyer's units: %w", err)
			}
			return n, nil
		}
		if err := cur.Stock().Refusal(qty, held); err != nil {
			return err
		}
		_, ok, err := s.d.Store.Reserve(ctx, cur.ID, qty, now)
		if err != nil {
			return err
		}
		if !ok {
			// The lock holds the row: only a caller outside a transaction
			// (or a store without locks) gets here. Name the reason.
			again, err := s.d.Store.GetOffering(ctx, cur.ID, false)
			if err != nil {
				return err
			}
			if err := saleRefusal(again, now); err != nil {
				return err
			}
			return ErrSoldOut
		}
		return nil
	})
}

// saleRefusal is why o cannot be reserved at all at now (nil when it is on
// sale, as far as the row tells): sold out, or not on sale.
func saleRefusal(o Offering, now time.Time) error {
	if o.Status == OfferingSoldOut {
		return ErrSoldOut
	}
	if !o.OnSaleAt(now) {
		return ErrNotOnSale
	}
	return nil
}

// Stock is what the stock rule of an offering reads (02 §18.2): the units
// left (nil: unlimited) and the per-buyer limit (nil: none).
type Stock struct {
	Remaining     *int
	PerBuyerLimit *int
}

// Stock is the offering's stock as the row tells it now.
func (o Offering) Stock() Stock {
	return Stock{Remaining: o.Remaining(), PerBuyerLimit: o.PerBuyerLimit}
}

// Stock is the stock of the offering as ForCheckout read it.
func (c CheckoutOffering) Stock() Stock {
	return Stock{Remaining: c.QuantityRemaining, PerBuyerLimit: c.PerBuyerLimit}
}

// Refusal is the stock rule of an offering on sale (02 §18.2), the one
// place it is written (BE-139): qty more units for a buyer are refused
// with ErrSoldOut when fewer than qty units are left, and with
// ErrPerBuyerLimit when the units the buyer already holds on its open
// orders (held; read only when the offering has a limit, nil → 0) plus qty
// exceed per_buyer_limit. nil: the units can be taken. Reserve applies it
// to the locked row before it takes the units; orders' quoteOrder applies
// it to the checkout read, taking nothing.
func (st Stock) Refusal(qty int, held func() (int, error)) error {
	if r := st.Remaining; r != nil && *r < qty {
		return ErrSoldOut
	}
	if st.PerBuyerLimit == nil {
		return nil
	}
	n := 0
	if held != nil {
		var err error
		if n, err = held(); err != nil {
			return err
		}
	}
	if n+qty > *st.PerBuyerLimit {
		return ErrPerBuyerLimit
	}
	return nil
}

// Release implements Offerings: qty units back (an expired or cancelled
// order, BE-040); a sold-out offering is on sale again.
func (s *service) Release(ctx context.Context, offeringID uuid.UUID, qty int) error {
	if qty <= 0 || qty > maxQuantity {
		return problem.Field("quantity", "must be 1-100000")
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		_, ok, err := s.d.Store.Release(ctx, offeringID, qty)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("catalog: release %d units of offering %s: %w", qty, offeringID, ErrInvalidState)
		}
		return nil
	})
}
