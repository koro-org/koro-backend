package parties

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of parties (05 §2.2) over
// legal_parties, legal_party_documents, payees and payee_bank_accounts
// (02 §17.1–17.4). The GORM store (store_gorm.go) and the in-memory store
// (store_mem.go) pass the same contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any.
//
// Org scope: a party's org is legal_parties.org_id (a company's buyer
// party, a university's own party) or the org of one of its payees
// (payees.org_id: the club a foundation is paid for; R-003 §4.7 seller org ≠
// payee). The org-surface reads (OrgParties, OrgParty) take the org and
// return only its parties with the relation (Scope). Documents carry no
// org_id: they are reached through their party, which the service resolves
// within the org first (06 H-38). The admin and cross-module reads are
// cross-org by nature (platform staff, other modules' contracts).
//
// Rows are never deleted (financial records, 02 §17: app_rw has no DELETE).
type Store interface {
	// InsertParty writes a new party and returns it as stored. 409
	// ErrTaxIDTaken when an active party of the same kind has the VKN or
	// TCKN blind index; ErrPlatformExists for a second platform party.
	InsertParty(ctx context.Context, p LegalParty) (LegalParty, error)
	// InsertPlatformIfAbsent writes p (kind platform) unless a platform
	// party exists; it returns the stored platform party and whether it was
	// written now (ON CONFLICT DO NOTHING: the transaction stays usable).
	InsertPlatformIfAbsent(ctx context.Context, p LegalParty) (LegalParty, bool, error)
	// GetParty returns the party or ErrNotFound.
	GetParty(ctx context.Context, id uuid.UUID) (LegalParty, error)
	// LockParty returns the party locked for the rest of the transaction or
	// ErrNotFound.
	LockParty(ctx context.Context, id uuid.UUID) (LegalParty, error)
	// GetPlatform returns the platform party or ErrNotFound (not seeded).
	GetPlatform(ctx context.Context) (LegalParty, error)
	// UpdateParty writes the mutable columns of p (everything but kind,
	// org_id and created_by: no full-row save) and returns the stored row;
	// updated_at changes on every call (ETag, 04 §1.13). ErrNotFound when
	// the party is gone; 409 ErrTaxIDTaken like InsertParty.
	UpdateParty(ctx context.Context, p LegalParty) (LegalParty, error)

	// OrgParties returns the parties of orgID (not the platform party), by
	// id: its own (org_id) and the legal sides of its payees that are not
	// closed (Scope payee wins when both hold).
	OrgParties(ctx context.Context, orgID uuid.UUID) ([]OrgParty, error)
	// OrgParty returns one party of orgID (as OrgParties) or ErrNotFound
	// when it does not exist or is not the org's; lock locks the party row.
	OrgParty(ctx context.Context, orgID, id uuid.UUID, lock bool) (OrgParty, error)
	// BuyerParties returns the buyer_company parties of a company org, newest
	// first.
	BuyerParties(ctx context.Context, orgID uuid.UUID) ([]LegalParty, error)
	// ListParties returns up to limit parties matching f, newest first,
	// after the position after (nil: the first page).
	ListParties(ctx context.Context, f ListFilter, after *uuid.UUID, limit int) ([]LegalParty, error)
	// PartiesCreatedBy returns the parties a user created, oldest first
	// (KVKK export).
	PartiesCreatedBy(ctx context.Context, userID uuid.UUID) ([]LegalParty, error)

	// InsertDocument writes a document row and returns it as stored; 409
	// when the file already belongs to a document.
	InsertDocument(ctx context.Context, d Document) (Document, error)
	// GetDocument returns the document or ErrNotFound; lock locks it.
	GetDocument(ctx context.Context, id uuid.UUID, lock bool) (Document, error)
	// PartyDocuments returns the documents of the parties, oldest first.
	PartyDocuments(ctx context.Context, partyIDs []uuid.UUID) ([]Document, error)
	// SetDocumentVerified marks a document verified by and at the given
	// values and returns it; ErrNotFound when it is gone.
	SetDocumentVerified(ctx context.Context, id, by uuid.UUID, at time.Time) (Document, error)
	// DocumentsUploadedBy returns the documents a user uploaded, oldest
	// first (KVKK export).
	DocumentsUploadedBy(ctx context.Context, userID uuid.UUID) ([]Document, error)

	PayeeStore
	BankAccountStore
}

// PayeeStore is the payees part of Store (02 §17.3). A payee's org is
// payees.org_id: the org-surface reads take the org and return only its
// payees (06 H-38: a bank account's org comes from its payee row, never
// from the request).
type PayeeStore interface {
	// InsertPayee writes a new payee and returns it as stored. 409
	// ErrPayeeExists when the org has an open payee for the legal party;
	// 422 when the legal party is not a payee party (composite FK).
	InsertPayee(ctx context.Context, p Payee) (Payee, error)
	// GetPayee returns the payee or ErrNotFound; lock locks it for the rest
	// of the transaction.
	GetPayee(ctx context.Context, id uuid.UUID, lock bool) (Payee, error)
	// OrgPayee returns a payee of orgID or ErrNotFound (another org's payee
	// too); lock locks it.
	OrgPayee(ctx context.Context, orgID, id uuid.UUID, lock bool) (Payee, error)
	// OrgPayees returns the payees of orgID (closed ones included), by id.
	OrgPayees(ctx context.Context, orgID uuid.UUID) ([]Payee, error)
	// DefaultPayee returns the active default payee of a seller org; found
	// is false when it has none.
	DefaultPayee(ctx context.Context, orgID uuid.UUID) (Payee, bool, error)
	// UpdatePayee writes the mutable columns of p (status, is_default,
	// payout_method, invoicing_capability, university_protocol_ref,
	// verified_at/by, valid_to: never the party, the org or the creator)
	// and returns the stored row; updated_at changes (ETag).
	UpdatePayee(ctx context.Context, p Payee) (Payee, error)
	// ClearDefault unsets is_default on the active payees of orgID other
	// than keep and returns their IDs (the default moves in one
	// transaction, payees_default_uq).
	ClearDefault(ctx context.Context, orgID, keep uuid.UUID) ([]uuid.UUID, error)
	// PayeesCreatedBy returns the payees a user created, oldest first
	// (KVKK export).
	PayeesCreatedBy(ctx context.Context, userID uuid.UUID) ([]Payee, error)
}

// BankAccountStore is the payee_bank_accounts part of Store (02 §17.4).
// Rows are never deleted and their IBAN, holder, payee, evidence and
// submitter never change (payee_bank_accounts_frozen): only the state
// columns are written, by targeted UPDATEs (06 H-56: no full-row save).
type BankAccountStore interface {
	// InsertBankAccount writes a new account (status pending_verification or
	// pending_approval, payee_bank_accounts_initial_status) and returns it
	// as stored. 409 ErrBankAccountPending when the payee has a pending
	// account; 422 ErrIBANInvalid when the database refuses the IBAN.
	InsertBankAccount(ctx context.Context, b BankAccount) (BankAccount, error)
	// GetBankAccount returns the account or ErrNotFound; lock locks it.
	GetBankAccount(ctx context.Context, id uuid.UUID, lock bool) (BankAccount, error)
	// PayeeBankAccounts returns the accounts of a payee, newest first.
	PayeeBankAccounts(ctx context.Context, payeeID uuid.UUID) ([]BankAccount, error)
	// ApprovedAccounts returns the approved account of each payee that has
	// one; lock locks them (the approval that supersedes one).
	ApprovedAccounts(ctx context.Context, payeeIDs []uuid.UUID, lock bool) (map[uuid.UUID]BankAccount, error)
	// ListBankAccounts returns up to limit accounts in statuses (all when
	// empty), oldest first (the four-eyes queue), after the position after.
	ListBankAccounts(ctx context.Context, statuses []BankAccountStatus, after *uuid.UUID, limit int) ([]BankAccount, error)
	// TransitionBankAccount writes the state columns of b (status and the
	// verified_*, approved_*, rejected_*, superseded_*, revoked_* columns)
	// when the stored status is from, and returns the stored row.
	// ErrInvalidState when the status is no longer from (a concurrent
	// decision); the four-eyes CHECKs answer 403 authz.four_eyes_violation.
	TransitionBankAccount(ctx context.Context, b BankAccount, from BankAccountStatus) (BankAccount, error)
	// BankAccountsSubmittedBy returns the accounts a user submitted, oldest
	// first (KVKK export).
	BankAccountsSubmittedBy(ctx context.Context, userID uuid.UUID) ([]BankAccount, error)
}

// ListFilter narrows ListParties. Zero fields do not filter.
type ListFilter struct {
	Kind   Kind
	Status Status
	// Q matches part of the title (Turkish-insensitive) or, when it is a
	// 10-digit number, the VKN exactly.
	Q string
}
