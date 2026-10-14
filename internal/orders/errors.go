package orders

import "koro/internal/platform/problem"

// Errors of the orders module (05 §4.2; 04 §1.10, §2.15, 03 k25). Messages
// and field errors never carry a value of the request. The catalog's
// refusals of a checkout (catalog.not_on_sale, catalog.sold_out,
// catalog.per_buyer_limit, catalog.no_commerce_policy,
// catalog.payee_not_active) are returned as catalog defines them (06 H-52).
var (
	// ErrBuyerPartyRequired: 03 k25 — the order needs a verified
	// buyer_company legal party of the buying company (the billed party).
	ErrBuyerPartyRequired = problem.New(problem.Invalid, "orders.buyer_party_required",
		"A verified buyer legal party of the company is required")
	// ErrInvalidState: the order or line status does not allow the change
	// (cancelling a paid order, confirming a line that is not marked,
	// resolving a line that is not disputed).
	ErrInvalidState = problem.New(problem.Conflict, "orders.invalid_state", "The current status does not allow this change")
	// ErrOrderNotPaid: a line is marked fulfilled only once its order is paid
	// in full (04 §2.15 markOrderLineFulfilled).
	ErrOrderNotPaid = problem.New(problem.Conflict, "orders.order_not_paid", "The order is not paid yet")
	// ErrDisputeWindowClosed: the buyer disputes a line after its dispute
	// window ended (04 §2.15 disputeOrderLine "Pencere dışı 409").
	ErrDisputeWindowClosed = problem.New(problem.Conflict, "orders.dispute_window_closed", "The dispute window of the line has ended")
	// ErrCommissionExceedsShare: the commission of the policy in effect
	// (with its minimum) is larger than the seller's share of a line.
	ErrCommissionExceedsShare = problem.New(problem.Conflict, "orders.commission_exceeds_share",
		"The commission exceeds the seller's share of a line")
	// ErrAmountExceedsDue: ApplyPayment with more than the order still waits
	// for (payments opens an overpayment refund for the rest, T-14).
	ErrAmountExceedsDue = problem.New(problem.Conflict, "orders.amount_exceeds_due", "The amount exceeds what the order still waits for")
	// ErrRefundExceedsPaid: ApplyRefund with more than was paid (on the
	// order) or than the line's payable amount left (BE-043).
	ErrRefundExceedsPaid = problem.New(problem.Invalid, "orders.refund_exceeds_paid", "The refund exceeds the paid amount")
	// ErrNoCollectionAccount: payment instructions without an active Koro
	// collection account (04 §2.15). The code is payments' name (BE-041
	// reuses this error instead of declaring it again, 06 H-52): payments
	// is a higher layer orders cannot import.
	ErrNoCollectionAccount = problem.New(problem.Conflict, "payments.no_active_collection_account",
		"No active collection account is configured")

	// ErrNotFound: the order, line or org does not exist or is not visible
	// to the caller.
	ErrNotFound = problem.ErrNotFound
	// ErrUnavailable: a dependency the operation needs is not wired; the
	// request fails closed.
	ErrUnavailable = problem.ErrUnavailable
)

// fieldErr is a 422 validation_failed with one field error.
func fieldErr(loc, msg string) error {
	return problem.ErrValidation.WithFields(problem.FieldError{Location: loc, Message: msg})
}
