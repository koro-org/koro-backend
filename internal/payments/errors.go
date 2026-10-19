package payments

import (
	"koro/internal/authz"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// Errors of the payments module (05 §4.2; 04 §1.10, §2.16). Messages and
// field errors never carry a value of the request (an IBAN, a sender's
// name). Meanings other modules already own are their errors (06 H-52):
// parties.iban_invalid (an IBAN that is not a TR IBAN),
// payments.no_active_collection_account (declared by orders, which payments
// fills through hooks.collectionAccount), the order's orders.invalid_state
// and the ledger's refusals (ledger.model_not_enabled, ...).
var (
	ErrCollectionExceedsReceivable = problem.New(problem.Conflict, "payments.collection_exceeds_receivable", "The transfer exceeds the unreserved receivable")
	// ErrTransferAlreadyMatched: the transfer is matched already (04 §1.10);
	// a second match — another key, a concurrent click — captures nothing.
	ErrTransferAlreadyMatched = problem.New(problem.Conflict, "payments.transfer_already_matched",
		"The transfer is already matched")
	// ErrOrderReferenceMismatch: expected_order_no is not the order's
	// reference code (04 §1.10: a guard against matching the wrong order).
	ErrOrderReferenceMismatch = problem.New(problem.Invalid, "payments.order_reference_mismatch",
		"The expected order number is not the order's reference code")
	// ErrInvalidState: the collection account or transfer status does not
	// allow the change (approving a decided account, ignoring a matched
	// transfer, approving a return that was not requested, ...).
	ErrInvalidState = problem.New(problem.Conflict, "payments.invalid_state", "The current status does not allow this change")
	// ErrDuplicateBankReference: the collection account already has a
	// transfer with this bank reference (04 §2.16 adminCreateIncomingTransfer;
	// the import skips such rows instead).
	ErrDuplicateBankReference = problem.New(problem.Conflict, "payments.duplicate_bank_reference",
		"A transfer with this bank reference is already recorded")
	// ErrIBANInUse: another pending or active collection account has the IBAN
	// (collection_accounts_iban_uq).
	ErrIBANInUse = problem.New(problem.Conflict, "payments.iban_in_use", "Another open collection account has this IBAN")
	// ErrNothingDue: the order waits for no money from Koro (a model C order
	// whose fee is paid waits for the direct confirmation only).
	ErrNothingDue = problem.New(problem.Conflict, "payments.nothing_due", "The order does not wait for a payment to Koro")
	// ErrLineInPayout: the order line of a refund is in a payout that is
	// waiting for its approval or its transfer (BE-043, 06 H-59): the
	// refund waits until the payout is paid (the payee's part is then
	// claimed back) or cancelled.
	ErrLineInPayout = problem.New(problem.Conflict, "payments.line_in_payout", "The order line is in an open payout")

	// ErrRefundExceedsPaid is orders' 422 orders.refund_exceeds_paid: the
	// refund is more than the payment, the order or the line still holds
	// (requested and approved refunds count; 06 H-52).
	ErrRefundExceedsPaid = orders.ErrRefundExceedsPaid

	// ErrNoCollectionAccount is orders' 409 payments.no_active_collection_account.
	ErrNoCollectionAccount = orders.ErrNoCollectionAccount
	// ErrIBANInvalid is parties' 422 parties.iban_invalid.
	ErrIBANInvalid = parties.ErrIBANInvalid
	// ErrNotFound: the account or transfer does not exist.
	ErrNotFound = problem.ErrNotFound
	// ErrUnavailable: a dependency the operation needs is not wired; the
	// request fails closed.
	ErrUnavailable = problem.ErrUnavailable
)

// constraints maps the named constraints of 000103–000108 onto module
// errors (05 §6.6). The four-eyes CHECKs answer like authz (03 §6.1: the
// database is the second layer of the same rule); a payment refunded
// beyond its amount is the refund limit's 422.
var constraints = db.Constraints{
	"receivable_collections_transfer_uq":             ErrInvalidState,
	"receivable_collections_receipt_uq":              ErrInvalidState,
	"receivable_collections_source_state_chk":        ErrInvalidState,
	"receivable_collections_limit_chk":               ErrCollectionExceedsReceivable,
	"receivable_collections_four_eyes_chk":           authz.ErrFourEyes,
	"collection_accounts_iban_uq":                    ErrIBANInUse,
	"collection_accounts_iban_chk":                   ErrIBANInvalid,
	"collection_accounts_default_uq":                 ErrInvalidState,
	"collection_accounts_four_eyes_chk":              authz.ErrFourEyes,
	"incoming_bank_transfers_account_reference_uq":   ErrDuplicateBankReference,
	"incoming_bank_transfers_sender_iban_chk":        ErrIBANInvalid,
	"incoming_bank_transfers_return_target_iban_chk": ErrIBANInvalid,
	"incoming_bank_transfers_return_four_eyes_chk":   authz.ErrFourEyes,
	"payments_incoming_transfer_id_uq":               ErrTransferAlreadyMatched,
	"refunds_target_iban_chk":                        ErrIBANInvalid,
	"refunds_four_eyes_chk":                          authz.ErrFourEyes,
	"refunds_closure_four_eyes_chk":                  authz.ErrFourEyes,
	"payments_refunded_limit_chk":                    ErrRefundExceedsPaid,
}

// fieldErr is a 422 validation_failed with one field error.
func fieldErr(loc, msg string) error {
	return problem.ErrValidation.WithFields(problem.FieldError{Location: loc, Message: msg})
}

// fieldErrOf is one field error (for a module error that names its field).
func fieldErrOf(loc, msg string) problem.FieldError {
	return problem.FieldError{Location: loc, Message: msg}
}
