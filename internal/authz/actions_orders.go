// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the orders module (03 §5.15), in table order.
const (
	OrdersOrderCreate              Action = "orders.order.create"
	OrdersOrderQuote               Action = "orders.order.quote"
	OrdersOrderList                Action = "orders.order.list"
	OrdersOrderRead                Action = "orders.order.read"
	OrdersOrderPaymentInstructions Action = "orders.order.payment_instructions"
	OrdersOrderCancel              Action = "orders.order.cancel"
	OrdersOrderListAdmin           Action = "orders.order.list_admin"
	OrdersLineListSales            Action = "orders.line.list_sales"
	OrdersLineMarkFulfilled        Action = "orders.line.mark_fulfilled"
	OrdersLineReadEvidence         Action = "orders.line.read_evidence"
	OrdersLineConfirm              Action = "orders.line.confirm"
	OrdersLineDispute              Action = "orders.line.dispute"
	OrdersLineResolveDispute       Action = "orders.line.resolve_dispute"
)

// System-only actions of the orders module (03 §3.7): only SystemActor may perform them.
const (
	OrdersOrderExpire     Action = "orders.order.expire"
	OrdersLineAutoFulfill Action = "orders.line.auto_fulfill"
)

// ordersActions lists the HTTP actions of orders in 03 §5.15 order.
var ordersActions = []Action{
	OrdersOrderCreate,
	OrdersOrderQuote,
	OrdersOrderList,
	OrdersOrderRead,
	OrdersOrderPaymentInstructions,
	OrdersOrderCancel,
	OrdersOrderListAdmin,
	OrdersLineListSales,
	OrdersLineMarkFulfilled,
	OrdersLineReadEvidence,
	OrdersLineConfirm,
	OrdersLineDispute,
	OrdersLineResolveDispute,
}
