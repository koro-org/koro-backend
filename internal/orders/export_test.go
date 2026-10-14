package orders

// Test-only access to the pure money and reference code helpers
// (pricing_test.go is package orders_test).
type (
	PriceInput  = priceInput
	LineAmounts = lineAmounts
	OrderTotals = orderTotals
)

var (
	PriceLine    = priceLine
	TotalsOf     = totalsOf
	NewReference = newReference
	RefRe        = refRe
)
