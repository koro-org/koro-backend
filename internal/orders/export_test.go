package orders

// Test-only access to the pure money, reference code and status helpers
// (pricing_test.go and partial_refund_test.go are package orders_test).
type (
	PriceInput  = priceInput
	LineAmounts = lineAmounts
	OrderTotals = orderTotals
)

var (
	ProgressStatus = progressStatus
	PriceLine      = priceLine
	TotalsOf       = totalsOf
	NewReference   = newReference
	RefRe          = refRe
)
