package parties

import "koro/internal/platform/problem"

// Errors of the parties module (05 §4.2; 04 §1.10, §2.13). Messages and
// field errors never carry a tax number, a national ID or an address.
var (
	// ErrTaxIDTaken: another active party of the same kind has this VKN, or
	// (individuals) this national ID (legal_parties_taxno_uq / _tckn_uq; the
	// TCKN is compared through its blind index).
	ErrTaxIDTaken = problem.New(problem.Conflict, "parties.tax_id_taken", "A legal party with this tax ID already exists")
	// ErrPlatformExists: the platform party is a single row (legal_parties_platform_uq).
	ErrPlatformExists = problem.New(problem.Conflict, "parties.platform_exists", "The platform legal party already exists")
	// ErrInvalidState: the party's or document's status does not allow the
	// change (verifying a verified party, changing an inactive one,
	// verifying a document twice).
	ErrInvalidState = problem.New(problem.Conflict, "parties.invalid_state", "Legal party status does not allow this change")
	// ErrNaturalPersonPayee: 06 H-54 — in the MVP a payee is a legal entity
	// (foundation, university, association, institution); an individual or
	// a sole proprietorship cannot be a payee party.
	ErrNaturalPersonPayee = problem.New(problem.Invalid, "parties.natural_person_payee",
		"Payees must be legal entities; natural persons are not supported yet")
	// ErrOrgNotCompany: a buyer party belongs to a company org
	// (createBuyerLegalParty on a club or university).
	ErrOrgNotCompany = problem.New(problem.Invalid, "parties.org_not_company", "Buyer legal parties belong to companies only")
	// ErrVerificationIncomplete: verifying a party without the data an
	// invoice needs (a VKN, unless the party is an individual; a title and
	// an address).
	ErrVerificationIncomplete = problem.New(problem.Invalid, "parties.verification_incomplete",
		"Legal party lacks the tax number or address needed for verification")
	// ErrEmptyPatch: an update without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "parties.empty_patch", "Nothing to update")

	// ErrNotFound: the party, document or org does not exist or is not
	// visible to the caller.
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed
)
