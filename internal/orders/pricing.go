package orders

import (
	"errors"
	"fmt"

	"koro/internal/catalog"
	"koro/internal/platform/money"
)

// Server-side pricing (06 H-6, 02 §19.2): every amount of an order is
// computed here from the offering (price, VAT rate), its service (VAT
// class, withholding) and the commerce policy in effect — never taken from
// the request. Integer kuruş arithmetic of platform/money only, per line,
// half away from zero; the order totals are the sums of the lines (the
// database checks the same at commit: orders_totals_match_lines).

// priceInput is what one line is priced from.
type priceInput struct {
	Quantity        int
	UnitPriceAmount int64 // kuruş, excluding VAT (offerings.unit_price_amount)
	VATRateBP       int   // the offering's rate (copied from the service)
	VATClass        catalog.VATClass
	WithholdingNum  *int16 // tevkifat fraction of a withheld service
	WithholdingDen  *int16
	Policy          catalog.CommercePolicy
}

// lineAmounts are the frozen amounts of one line (02 §19.2 columns).
type lineAmounts struct {
	Net                 int64
	VATRateBP           int // the effective rate: 0 for an exempt or out-of-scope service
	VAT                 int64
	WithholdingNum      *int16
	WithholdingDen      *int16
	VATWithheld         int64
	Gross               int64
	CommissionRateBP    int
	CommissionBasis     string
	CommissionPayer     string
	Commission          int64
	CommissionVATRateBP int
	CommissionVAT       int64
	BuyerFee            int64
	SellerNet           int64
	Payable             int64
}

// priceLine computes the amounts of one line (02 §19.2, H-6):
//
//	net          = quantity × unit price
//	vat          = net × rate (half away from zero); 0 for exempt / outside_scope
//	vat_withheld = vat × num / den (withheld services; H-6 rounding, T-99)
//	gross        = net + vat
//	commission   = basis × rate, then the policy's min/max; its VAT at commission_vat_rate_bp
//	buyer_fee    = commission + its VAT when the buyer pays it, else 0
//	seller_net   = gross − withheld − (commission + its VAT when the seller pays it)
//	payable      = gross − withheld + buyer_fee
//
// A commission larger than the seller's share is ErrCommissionExceedsShare.
func priceLine(in priceInput) (lineAmounts, error) {
	if in.Quantity < 1 || in.UnitPriceAmount < 1 {
		return lineAmounts{}, fmt.Errorf("orders: price: quantity %d, unit price %d: %w", in.Quantity, in.UnitPriceAmount, money.ErrInvalid)
	}
	p := in.Policy
	out := lineAmounts{
		VATRateBP: in.VATRateBP, CommissionRateBP: p.CommissionRateBP, CommissionBasis: string(p.CommissionBasis),
		CommissionPayer: string(p.CommissionPayer), CommissionVATRateBP: p.CommissionVATRateBP,
	}
	var err error
	if out.Net, err = money.Mul(in.UnitPriceAmount, int64(in.Quantity)); err != nil {
		return lineAmounts{}, err
	}
	switch in.VATClass {
	case catalog.VATExempt, catalog.VATOutsideScope:
		out.VATRateBP = 0
	case catalog.VATWithheld:
		if in.WithholdingNum == nil || in.WithholdingDen == nil {
			return lineAmounts{}, errors.New("orders: price: a withheld service without its withholding fraction")
		}
	}
	if out.VAT, err = money.VAT(out.Net, int64(out.VATRateBP)); err != nil {
		return lineAmounts{}, err
	}
	if in.VATClass == catalog.VATWithheld {
		out.WithholdingNum, out.WithholdingDen = in.WithholdingNum, in.WithholdingDen
		if out.VATWithheld, err = money.VATWithheld(out.VAT, int64(*in.WithholdingNum), int64(*in.WithholdingDen)); err != nil {
			return lineAmounts{}, err
		}
	}
	if out.Gross, err = money.Add(out.Net, out.VAT); err != nil {
		return lineAmounts{}, err
	}
	c, err := p.LineCommission(out.Net, out.VAT)
	if err != nil {
		return lineAmounts{}, err
	}
	out.Commission, out.CommissionVAT, out.BuyerFee = c.Amount, c.VATAmount, c.BuyerFee
	sellerCost := int64(0)
	if p.CommissionPayer == catalog.PayerSeller {
		if sellerCost, err = money.Add(c.Amount, c.VATAmount); err != nil {
			return lineAmounts{}, err
		}
	}
	if out.SellerNet, err = money.Sum(out.Gross, -out.VATWithheld, -sellerCost); err != nil {
		return lineAmounts{}, err
	}
	if out.SellerNet < 0 {
		return lineAmounts{}, ErrCommissionExceedsShare
	}
	if out.Payable, err = money.Sum(out.Gross, -out.VATWithheld, out.BuyerFee); err != nil {
		return lineAmounts{}, err
	}
	return out, nil
}

// orderTotals are the frozen amounts of one order (02 §19.1).
type orderTotals struct {
	Subtotal      int64
	VAT           int64
	VATWithheld   int64
	BuyerFee      int64
	Total         int64
	Payable       int64
	KoroCollect   int64
	DirectToPayee int64
}

// totalsOf sums the lines of one order. What Koro collects depends on the
// model (02 §18.3): in A, A2 and B the buyer pays everything to Koro's
// account; in C it pays the payee directly and Koro only its fee (the
// buyer fee: model C charges the commission to the buyer).
func totalsOf(model string, lines []lineAmounts) (orderTotals, error) {
	var t orderTotals
	var err error
	for _, l := range lines {
		for _, f := range []struct {
			sum *int64
			add int64
		}{{&t.Subtotal, l.Net}, {&t.VAT, l.VAT}, {&t.VATWithheld, l.VATWithheld}, {&t.BuyerFee, l.BuyerFee}, {&t.Payable, l.Payable}} {
			if *f.sum, err = money.Add(*f.sum, f.add); err != nil {
				return orderTotals{}, err
			}
		}
	}
	if t.Total, err = money.Sum(t.Subtotal, t.VAT, t.BuyerFee); err != nil {
		return orderTotals{}, err
	}
	t.KoroCollect = t.Payable
	if model == ModelC {
		t.KoroCollect = t.BuyerFee
		t.DirectToPayee = t.Payable - t.BuyerFee
	}
	return t, nil
}
