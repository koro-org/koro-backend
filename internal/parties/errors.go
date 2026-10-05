package parties

import "koro/internal/platform/problem"

// Errors of the parties module (05 §4.2; 04 §1.10, §2.13). Messages and
// field errors never carry a tax number, a national ID, an IBAN or an
// address.
var (
	// ErrTaxIDTaken: another active party of the same kind has this VKN, or
	// (individuals) this national ID (legal_parties_taxno_uq / _tckn_uq; the
	// TCKN is compared through its blind index). 06 H-60: a platform admin
	// corrects a wrong owner with adminCorrectLegalPartyOwnership.
	ErrTaxIDTaken = problem.New(problem.Conflict, "parties.tax_id_taken", "A legal party with this tax ID already exists")
	// ErrPlatformExists: the platform party is a single row (legal_parties_platform_uq).
	ErrPlatformExists = problem.New(problem.Conflict, "parties.platform_exists", "The platform legal party already exists")
	// ErrInvalidState: the status of the party, document, payee or bank
	// account does not allow the change (verifying a verified party,
	// changing an inactive one, verifying a document twice, approving an
	// account that is not pending approval, writing to a closed payee).
	ErrInvalidState = problem.New(problem.Conflict, "parties.invalid_state", "The current status does not allow this change")
	// ErrNaturalPersonPayee: 06 H-54 — in the MVP a payee is a legal entity
	// (foundation, university, association, institution); an individual or
	// a sole proprietorship cannot be a payee party or a payee.
	ErrNaturalPersonPayee = problem.New(problem.Invalid, "parties.natural_person_payee",
		"Payees must be legal entities; natural persons are not supported yet")
	// ErrOrgNotCompany: a buyer party belongs to a company org
	// (createBuyerLegalParty on a club or university).
	ErrOrgNotCompany = problem.New(problem.Invalid, "parties.org_not_company", "Buyer legal parties belong to companies only")
	// ErrVerificationIncomplete: verifying a party without the data an
	// invoice needs (a VKN, unless the party is an individual; a title and
	// an address), or a payee whose legal party is not verified.
	ErrVerificationIncomplete = problem.New(problem.Invalid, "parties.verification_incomplete",
		"Legal party lacks the tax number or address needed for verification")
	// ErrEmptyPatch: an update without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "parties.empty_patch", "Nothing to update")

	// ErrIBANInvalid: not a Turkish IBAN (TR + 24 digits, ISO 7064 mod-97;
	// platform/iban and koro_iban_tr_valid, payee_bank_accounts_iban_chk).
	ErrIBANInvalid = problem.New(problem.Invalid, "parties.iban_invalid", "The IBAN is not a valid Turkish IBAN")
	// ErrBankAccountPending: the payee already has a bank account waiting
	// for verification or approval (payee_bank_accounts_pending_uq): it is
	// decided (approved or rejected) before another one is submitted.
	ErrBankAccountPending = problem.New(problem.Conflict, "parties.bank_account_pending",
		"The payee already has a bank account awaiting verification or approval")
	// ErrNoApprovedAccount: the payee has no approved bank account (01
	// §4.12 Payees.ApprovedAccount; 04 submitOffering, payouts).
	ErrNoApprovedAccount = problem.New(problem.Conflict, "parties.no_approved_account", "The payee has no approved bank account")
	// ErrPayeeExists: the org already has an open payee for this legal party
	// (payees_org_party_uq).
	ErrPayeeExists = problem.New(problem.Conflict, "parties.payee_exists", "The organization already has a payee for this legal party")

	// ErrNotFound: the party, document, payee, bank account or org does not
	// exist or is not visible to the caller.
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed
	// ErrUnavailable: a dependency the operation needs is not wired (files,
	// re-authentication); the request fails closed.
	ErrUnavailable = problem.ErrUnavailable
)
