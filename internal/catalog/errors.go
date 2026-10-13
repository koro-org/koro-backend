package catalog

import "koro/internal/platform/problem"

// Errors of the catalog module (05 §4.2; 04 §1.10, §2.14). Messages and
// field errors never carry a value of the request.
var (
	// ErrInvalidState: the status of the service, offering or policy does not
	// allow the change (editing a submitted offering, deciding a decided
	// one, cancelling a cancelled one, closing a closed policy).
	ErrInvalidState = problem.New(problem.Conflict, "catalog.invalid_state", "The current status does not allow this change")
	// ErrEmptyPatch: an update without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "catalog.empty_patch", "Nothing to update")
	// ErrServiceCodeTaken: another service has this code (services_code_uq).
	ErrServiceCodeTaken = problem.New(problem.Conflict, "catalog.service_code_taken", "A service with this code already exists")
	// ErrServiceNotSellable: the offering's service is inactive or not yet
	// reviewed by the accountant (02 §18.1 accounting_reviewed_at), so the
	// offering cannot be submitted or go on sale.
	ErrServiceNotSellable = problem.New(problem.Conflict, "catalog.service_not_sellable",
		"The service is inactive or not reviewed by the accountant")
	// ErrOfferingIncomplete: the offering lacks what submission or approval
	// needs (the event of an event-bound service, a payee of the seller, a
	// sales window that has not ended, an event that is still open); the
	// field errors name what is missing.
	ErrOfferingIncomplete = problem.New(problem.Invalid, "catalog.offering_incomplete",
		"The offering lacks what submission needs")
	// ErrParentMismatch: a referenced payee or event does not belong to the
	// offering's seller org (offerings_default_payee_id_org_id_fkey,
	// offerings_event_id_org_id_fkey; 06 H-38, H-55).
	ErrParentMismatch = problem.New(problem.Invalid, "catalog.parent_mismatch",
		"The payee or event does not belong to the seller organization")
	// ErrPayeeNotActive: the offering's payee is not active (01 §4.12
	// Payees.IsActive): it cannot be submitted or go on sale. A payee
	// without an approved bank account is parties.no_approved_account.
	ErrPayeeNotActive = problem.New(problem.Conflict, "catalog.payee_not_active", "The payee of the offering is not active")

	// ErrNotOnSale: the offering is not active or outside its sales window
	// (ForCheckout, Reserve).
	ErrNotOnSale = problem.New(problem.Conflict, "catalog.not_on_sale", "The offering is not on sale")
	// ErrSoldOut: the stock does not cover the requested quantity (Reserve).
	ErrSoldOut = problem.New(problem.Conflict, "catalog.sold_out", "Not enough stock left")
	// ErrPerBuyerLimit: the buyer would exceed the offering's per_buyer_limit.
	ErrPerBuyerLimit = problem.New(problem.Conflict, "catalog.per_buyer_limit", "The per-buyer limit of the offering is reached")

	// ErrNoPolicy: no commerce policy applies to the offering at that time
	// (no offering, service, seller org or default row; T-10: the live
	// commission rate is entered only once the team decides it).
	ErrNoPolicy = problem.New(problem.Conflict, "catalog.no_commerce_policy", "No commerce policy applies")
	// ErrPolicyOverlap: the validity period overlaps another policy of the
	// same scope (commerce_policies_no_overlap_uq, T-43).
	ErrPolicyOverlap = problem.New(problem.Conflict, "catalog.policy_overlap",
		"The validity period overlaps another policy of the same scope")

	// ErrNotFound: the service, offering, policy or org does not exist or is
	// not visible to the caller.
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed
	// ErrUnavailable: a dependency the operation needs is not wired
	// (re-authentication, parties); the request fails closed.
	ErrUnavailable = problem.ErrUnavailable
)
