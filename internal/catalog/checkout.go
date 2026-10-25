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
// (its own without one) the offering row is locked, the per-buyer limit is
// checked against the buyer's open orders (BuyerHeld, read under the lock,
// so two orders of one buyer cannot both pass), then one conditional
// UPDATE takes the units; the last one turns the offering sold_out.
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
		if err := reserveRefusal(cur, qty, now); err != nil {
			return err
		}
		if cur.PerBuyerLimit != nil {
			held := 0
			if s.d.BuyerHeld != nil {
				if held, err = s.d.BuyerHeld(ctx, cur.ID, buyerOrgID); err != nil {
					return fmt.Errorf("catalog: buyer's units: %w", err)
				}
			}
			if held+qty > *cur.PerBuyerLimit {
				return ErrPerBuyerLimit
			}
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
			if err := reserveRefusal(again, qty, now); err != nil {
				return err
			}
			return ErrSoldOut
		}
		return nil
	})
}

// reserveRefusal is why qty units of o cannot be reserved at now (nil when
// they can, as far as the row tells).
func reserveRefusal(o Offering, qty int, now time.Time) error {
	if o.Status == OfferingSoldOut {
		return ErrSoldOut
	}
	if !o.OnSaleAt(now) {
		return ErrNotOnSale
	}
	if r := o.Remaining(); r != nil && *r < qty {
		return ErrSoldOut
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
