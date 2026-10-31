package orders

// Test-only access to the pure money, reference code and status helpers
// (pricing_test.go, partial_refund_test.go and seller_status_test.go are
// package orders_test).
type (
	PriceInput  = priceInput
	LineAmounts = lineAmounts
	OrderTotals = orderTotals
)

var (
	ProgressStatus = progressStatus
	SellerStatus   = sellerStatus
	PriceLine      = priceLine
	TotalsOf       = totalsOf
	NewReference   = newReference
	RefRe          = refRe
)

// LineStateOf is the LineStateDTO of a delivery action's answer (BE-158).
func LineStateOf(v LineView) LineStateDTO { return lineOut(v).Body }
