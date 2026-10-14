package billing

import (
	"errors"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// Errors of the billing module (05 §4.2; 04 §1.10).
var (
	// ErrInvalidState: the invoice's state does not allow the operation
	// (upload to an invoice that is not planned or found mismatched, verify
	// without an upload waiting, cancel a cancelled invoice or an issued
	// credit note, download an unissued PDF without the uploader's right).
	ErrInvalidState = problem.New(problem.Conflict, "billing.invalid_state", "Invoice state does not allow this")
	// ErrInvoiceNoTaken: the issuer already has an invoice with this number
	// (invoices_no_uq).
	ErrInvoiceNoTaken = problem.New(problem.Conflict, "billing.invoice_no_taken",
		"The issuer already has an invoice with this number")

	// ErrNotFound: the invoice does not exist (or is not visible).
	ErrNotFound = problem.ErrNotFound
)

// Errors of the InvoiceProvider contract (adapters return them wrapped).
var (
	// ErrProviderNotFound: the provider does not know the reference or job.
	ErrProviderNotFound = errors.New("billing: provider reference not found")
	// ErrProviderNotReady: the document is not issued (yet) at the provider.
	ErrProviderNotReady = errors.New("billing: provider document not issued yet")
	// ErrProviderInvalid: the provider rejected the request (missing field,
	// bad tax number); not retried.
	ErrProviderInvalid = errors.New("billing: provider rejected the request")
)

// constraints maps the named constraints of 000116/000117 onto the module
// errors. The four-eyes CHECK answers like the policy (03 §6.1: the
// database is the second line of authz.four_eyes_violation, 06 H-19).
var constraints = db.Constraints{
	"invoices_verifier_chk":        authz.ErrFourEyes,
	"invoices_no_uq":               ErrInvoiceNoTaken,
	"invoices_idempotency_key_uq":  ErrInvalidState,
	"invoices_issued_chk":          ErrInvalidState,
	"invoices_issued_at_chk":       ErrInvalidState,
	"invoices_uploaded_chk":        ErrInvalidState,
	"invoices_manual_verified_chk": ErrInvalidState,
	"invoices_verified_pair_chk":   ErrInvalidState,
	"invoices_credit_note_chk":     ErrInvalidState,
}

// translate maps a billing database error.
func translate(err error) error {
	if err == nil {
		return nil
	}
	return constraints.Translate(err)
}
