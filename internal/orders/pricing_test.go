package orders_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"koro/internal/catalog"
	"koro/internal/orders"
	"koro/internal/platform/money"
)

func ptr16(v int16) *int16 { return &v }
func ptr64(v int64) *int64 { return &v }

// TestCreateOrder_LineVATAndCommission_Table pins the server-side line
// amounts of 02 §19.2 under 06 H-6 (platform/money, per line, half away
// from zero): the 02 example, rounding at the half kuruş, both commission
// bases and payers, the policy's minimum and maximum, withholding (T-99:
// half away from zero), exempt / out-of-scope services, a zero commission,
// boundaries (one kuruş, the largest quantity) and the refusals (a
// commission above the seller's share, overflow). Every positive case also
// satisfies the 000102 CHECKs (net, gross, buyer fee, seller net, payable).
func TestCreateOrder_LineVATAndCommission_Table(t *testing.T) {
	t.Parallel()
	pol := func(mut func(*catalog.CommercePolicy)) catalog.CommercePolicy {
		p := catalog.CommercePolicy{ModelCode: catalog.ModelA, CommissionRateBP: 1000, CommissionBasis: catalog.BasisNetExclVAT,
			CommissionPayer: catalog.PayerSeller, CommissionVATRateBP: 2000}
		if mut != nil {
			mut(&p)
		}
		return p
	}
	type want struct{ net, vat, withheld, gross, commission, commissionVAT, buyerFee, sellerNet, payable int64 }
	for _, c := range []struct {
		name string
		in   orders.PriceInput
		want want
		err  error
	}{
		{"02 example: 10 % from the seller, no withholding",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 1_000_000, VATRateBP: 2000, VATClass: catalog.VATStandard, Policy: pol(nil)},
			want{1_000_000, 200_000, 0, 1_200_000, 100_000, 20_000, 0, 1_080_000, 1_200_000}, nil},
		{"quantity multiplies the net",
			orders.PriceInput{Quantity: 3, UnitPriceAmount: 250_000, VATRateBP: 2000, VATClass: catalog.VATStandard, Policy: pol(nil)},
			want{750_000, 150_000, 0, 900_000, 75_000, 15_000, 0, 810_000, 900_000}, nil},
		{"VAT exactly half a kuruş rounds away from zero",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 25, VATRateBP: 1000, VATClass: catalog.VATStandard, Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionRateBP = 0 })},
			want{25, 3, 0, 28, 0, 0, 0, 28, 28}, nil}, // 2.5 → 3
		{"VAT below half a kuruş rounds down",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 2, VATRateBP: 2000, VATClass: catalog.VATStandard, Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionRateBP = 0 })},
			want{2, 0, 0, 2, 0, 0, 0, 2, 2}, nil}, // 0.4 → 0
		{"one kuruş at 20 % and 10 %",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 1, VATRateBP: 2000, VATClass: catalog.VATStandard, Policy: pol(nil)},
			want{1, 0, 0, 1, 0, 0, 0, 1, 1}, nil},
		{"commission on the gross (VAT included)",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 1_000_000, VATRateBP: 2000, VATClass: catalog.VATStandard,
				Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionBasis = catalog.BasisGrossInclVAT })},
			want{1_000_000, 200_000, 0, 1_200_000, 120_000, 24_000, 0, 1_056_000, 1_200_000}, nil},
		{"commission charged to the buyer",
			orders.PriceInput{Quantity: 2, UnitPriceAmount: 250_000, VATRateBP: 2000, VATClass: catalog.VATStandard,
				Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionPayer = catalog.PayerBuyer })},
			want{500_000, 100_000, 0, 600_000, 50_000, 10_000, 60_000, 600_000, 660_000}, nil},
		{"the policy minimum lifts a small commission",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 10_000, VATRateBP: 2000, VATClass: catalog.VATStandard,
				Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionMinAmount = ptr64(5_000) })},
			want{10_000, 2_000, 0, 12_000, 5_000, 1_000, 0, 6_000, 12_000}, nil},
		{"the policy maximum caps a large commission",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 10_000_000, VATRateBP: 2000, VATClass: catalog.VATStandard,
				Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionMaxAmount = ptr64(250_000) })},
			want{10_000_000, 2_000_000, 0, 12_000_000, 250_000, 50_000, 0, 11_700_000, 12_000_000}, nil},
		{"withholding 5/10 of the VAT (tevkifat)",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 1_000_000, VATRateBP: 2000, VATClass: catalog.VATWithheld,
				WithholdingNum: ptr16(5), WithholdingDen: ptr16(10), Policy: pol(nil)},
			want{1_000_000, 200_000, 100_000, 1_200_000, 100_000, 20_000, 0, 980_000, 1_100_000}, nil},
		{"withholding at half a kuruş rounds away from zero (T-99)",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 15, VATRateBP: 2000, VATClass: catalog.VATWithheld,
				WithholdingNum: ptr16(1), WithholdingDen: ptr16(2), Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionRateBP = 0 })},
			want{15, 3, 2, 18, 0, 0, 0, 16, 16}, nil}, // 1.5 → 2
		{"an exempt service has no VAT whatever its rate",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 1_000_000, VATRateBP: 2000, VATClass: catalog.VATExempt, Policy: pol(nil)},
			want{1_000_000, 0, 0, 1_000_000, 100_000, 20_000, 0, 880_000, 1_000_000}, nil},
		{"an out-of-scope service has no VAT",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 500_000, VATRateBP: 1000, VATClass: catalog.VATOutsideScope, Policy: pol(nil)},
			want{500_000, 0, 0, 500_000, 50_000, 10_000, 0, 440_000, 500_000}, nil},
		{"a zero commission leaves the seller everything",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 1_000_000, VATRateBP: 2000, VATClass: catalog.VATStandard,
				Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionRateBP = 0 })},
			want{1_000_000, 200_000, 0, 1_200_000, 0, 0, 0, 1_200_000, 1_200_000}, nil},
		{"a 100 % commission at 0 % VAT takes the whole share",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 1_000, VATRateBP: 0, VATClass: catalog.VATStandard,
				Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionRateBP = 10_000; p.CommissionVATRateBP = 0 })},
			want{1_000, 0, 0, 1_000, 1_000, 0, 0, 0, 1_000}, nil},
		{"a minimum above the seller's share is refused",
			orders.PriceInput{Quantity: 1, UnitPriceAmount: 100, VATRateBP: 2000, VATClass: catalog.VATStandard,
				Policy: pol(func(p *catalog.CommercePolicy) { p.CommissionMinAmount = ptr64(1_000) })},
			want{}, orders.ErrCommissionExceedsShare},
		{"an overflowing net is refused",
			orders.PriceInput{Quantity: 1000, UnitPriceAmount: math.MaxInt64 / 10, VATRateBP: 2000, VATClass: catalog.VATStandard, Policy: pol(nil)},
			want{}, money.ErrOverflow},
		{"a zero quantity is refused",
			orders.PriceInput{Quantity: 0, UnitPriceAmount: 1_000, VATRateBP: 2000, VATClass: catalog.VATStandard, Policy: pol(nil)},
			want{}, money.ErrInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := orders.PriceLine(c.in)
			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Fatalf("error = %v, want %v", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			g := want{got.Net, got.VAT, got.VATWithheld, got.Gross, got.Commission, got.CommissionVAT, got.BuyerFee, got.SellerNet, got.Payable}
			if g != c.want {
				t.Fatalf("amounts = %+v, want %+v", g, c.want)
			}
			// The 000102 CHECKs.
			sellerCost := int64(0)
			if got.CommissionPayer == string(catalog.PayerSeller) {
				sellerCost = got.Commission + got.CommissionVAT
			}
			buyerFee := int64(0)
			if got.CommissionPayer == string(catalog.PayerBuyer) {
				buyerFee = got.Commission + got.CommissionVAT
			}
			switch {
			case got.Net != int64(c.in.Quantity)*c.in.UnitPriceAmount:
				t.Error("order_lines_net_chk")
			case got.Gross != got.Net+got.VAT:
				t.Error("order_lines_gross_chk")
			case got.BuyerFee != buyerFee:
				t.Error("order_lines_buyer_fee_chk")
			case got.SellerNet != got.Gross-got.VATWithheld-sellerCost:
				t.Error("order_lines_seller_net_chk")
			case got.Payable != got.Gross-got.VATWithheld+got.BuyerFee:
				t.Error("order_lines_payable_chk")
			case got.VATWithheld > got.VAT:
				t.Error("order_lines_withheld_chk")
			}
		})
	}
}

// TestOrderTotals_ModelSplit pins totalsOf (02 §19.1): totals are the sums
// of the lines; model C collects only the buyer fee at Koro, the rest goes
// to the payee directly.
func TestOrderTotals_ModelSplit(t *testing.T) {
	t.Parallel()
	a := orders.LineAmounts{Net: 1_000_000, VAT: 200_000, Gross: 1_200_000, Payable: 1_200_000}
	b := orders.LineAmounts{Net: 500_000, VAT: 100_000, VATWithheld: 50_000, Gross: 600_000, BuyerFee: 60_000, Payable: 610_000}
	got, err := orders.TotalsOf(orders.ModelA, []orders.LineAmounts{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if want := (orders.OrderTotals{Subtotal: 1_500_000, VAT: 300_000, VATWithheld: 50_000, BuyerFee: 60_000, Total: 1_860_000,
		Payable: 1_810_000, KoroCollect: 1_810_000}); got != want {
		t.Fatalf("model A totals = %+v, want %+v", got, want)
	}
	if got.Payable != got.Total-got.VATWithheld {
		t.Error("orders_payable_chk")
	}
	c, err := orders.TotalsOf(orders.ModelC, []orders.LineAmounts{b})
	if err != nil {
		t.Fatal(err)
	}
	if c.KoroCollect != 60_000 || c.DirectToPayee != 550_000 || c.KoroCollect+c.DirectToPayee != c.Payable {
		t.Fatalf("model C split = %+v", c)
	}
}

// TestNewReference_Crockford: a drawn code is KR + 8 Crockford characters
// (orders_order_no_chk) and draws do not repeat in practice (40 bits).
func TestNewReference_Crockford(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 200 {
		code, err := orders.NewReference()
		if err != nil {
			t.Fatal(err)
		}
		if !orders.RefRe.MatchString(code) || strings.ContainsAny(code[2:], "ILOU") {
			t.Fatalf("drawn code %q is not KR + 8 Crockford characters", code)
		}
		seen[code] = true
	}
	if len(seen) < 199 {
		t.Fatalf("200 draws gave only %d distinct codes", len(seen))
	}
}
