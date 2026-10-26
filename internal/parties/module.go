// Package parties is the legal parties module (01 §4.12): the legal
// persons of money and invoices — Koro's own party (kind platform, one row,
// seeded as draft until Koro's legal entity exists: T-31), the invoice
// titles of buying companies (buyer_company) and the legal side of payees
// (payee: the foundation, university or association paid on behalf of a
// club) — their evidence documents and their verification (BE-036); the
// payees themselves (which legal party is paid for which seller org) and
// their bank accounts (IBAN) under four eyes (BE-037).
//
// Rules the package enforces:
//
//   - Org isolation (03 §3.6, §7.6): /orgs/{org_id}/legal-parties is the
//     org's management surface: invisible (404) to anyone who is not an
//     active member of the org or platform staff; a party of another org is
//     404 too. A party belongs to an org by legal_parties.org_id (a
//     company's buyer party) or through one of the org's payees (03 k21: the
//     club sees its foundation; the seller org is never the party's org_id,
//     R-003 §4.7). Scope columns come from the rows, never from the request
//     (06 H-38).
//   - k22: a company admin creates and changes only buyer_company parties of
//     its own company; payee and platform parties are finance and platform
//     admins' (the /admin operations).
//   - 06 H-54: a payee party is a legal entity in the MVP; an individual or a
//     sole proprietorship as a payee is 422 parties.natural_person_payee.
//     The national-ID path (individuals) is built — envelope-encrypted
//     tckn_enc plus its blind index for uniqueness — and stays closed until
//     a ticket lifts H-54 (Deps.AllowNaturalPersonPayees).
//   - Tax identifiers: the VKN is checked with its check digit, the TCKN
//     with its two check digits, the MERSİS number by its form (16 digits).
//     No response ever carries a TCKN (03 k21, 04 §2.13); the org roles see
//     the VKN and MERSİS masked, finance and platform admins in full. None of
//     them is ever logged or put into an audit row, an idempotency hash or an
//     error message (05 §11.2).
//   - A change to a verified (or rejected) party sends it back to
//     pending_verification; orders keep their own snapshot (04 §2.13).
//   - Documents pass the files pipeline (purpose party_document: allow-list,
//     scan, presigned URLs); a download is a 5-minute URL and is audited
//     every time. A document is verified by a finance or platform admin who
//     did not upload it.
//   - Personal data: an individual's party is personal data, and documents
//     may be. They are financial records kept 10 years (financial_records):
//     account erasure retains them (privacy.go).
//   - Koro's own party is seeded as a draft by `koro seed bootstrap-commerce`
//     (NewSeeder; idempotent, never over HTTP).
//   - Payee bank accounts (03 §6.1 "Payee IBAN"): money only ever goes to an
//     approved account, and an account is approved by four eyes. An org
//     submission (club_admin, university_rep: R) is verified by a finance or
//     platform admin A ≠ submitter (evidence + holder = the party's title,
//     T-19) and approved by B ≠ submitter, ≠ A (R); an admin submission (R)
//     is approved by another admin. authz checks the eyes (Attrs
//     submitted_by, verified_by) and the database again (payee_bank_accounts
//     CHECKs, answered as 403 authz.four_eyes_violation). A change is a new
//     row: approving it supersedes the approved one in the same transaction;
//     the IBAN, holder and evidence of a row never change. The IBAN is
//     checked with platform/iban (422 parties.iban_invalid) and the database
//     function koro_iban_tr_valid, stored in clear (06 H-54: payees are legal
//     entities), shown masked to org roles, in full only to finance and
//     platform admins, audited (03 §6.5), and never logged.
//   - Security notices (01 §6.2): a submission tells the payee org's admins
//     (an IBAN change they did not make is an account takeover) and every
//     finance admin; approval, rejection and revocation are notified too.
package parties

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// Parties is the read contract of other modules (01 §4.12). The returned
// parties never carry the encrypted national ID or its blind index.
type Parties interface {
	// Get returns a party (any kind and status) or ErrNotFound.
	Get(ctx context.Context, partyID uuid.UUID) (LegalParty, error)
	// Platform returns Koro's own party (kind platform) or ErrNotFound when
	// `koro seed bootstrap-commerce` has not run. It may still be a draft
	// (T-31): callers that invoice need status verified.
	Platform(ctx context.Context) (LegalParty, error)
	// BuyerPartyOf returns the newest verified buyer party of a company org;
	// found is false when the company has none (orders: 422
	// orders.buyer_party_required).
	BuyerPartyOf(ctx context.Context, companyOrgID uuid.UUID) (LegalParty, bool, error)
}

// Service is the parties service: the cross-module contract plus the HTTP
// use cases of 04 §2.13. Every method that takes an actor calls authz.Can
// first (05 §5.3).
type Service interface {
	Parties

	// ListOrgParties is listOrgLegalParties (parties.legal_party.read).
	ListOrgParties(ctx context.Context, a authz.Actor, orgID uuid.UUID) ([]PartyView, error)
	// GetOrgParty is getOrgLegalParty (parties.legal_party.read).
	GetOrgParty(ctx context.Context, a authz.Actor, orgID, partyID uuid.UUID) (PartyView, error)
	// CreateBuyerParty is createBuyerLegalParty (parties.legal_party.create):
	// a buyer_company party of the company org, pending_verification.
	CreateBuyerParty(ctx context.Context, a authz.Actor, orgID uuid.UUID, in PartyInput) (PartyView, error)
	// UpdateOrgParty is updateLegalParty (parties.legal_party.update):
	// ifMatch is the If-Match header (missing → 428, stale → 412).
	UpdateOrgParty(ctx context.Context, a authz.Actor, orgID, partyID uuid.UUID, ifMatch string, p PartyPatch) (PartyView, error)
	// UploadDocument is uploadLegalPartyDocument (parties.document.upload).
	UploadDocument(ctx context.Context, a authz.Actor, orgID, partyID uuid.UUID, in DocumentInput) (Document, error)
	// DownloadDocument is downloadLegalPartyDocument (parties.document.read):
	// a 5-minute URL, audited.
	DownloadDocument(ctx context.Context, a authz.Actor, orgID, partyID, documentID uuid.UUID) (DocumentLink, error)

	// AdminListParties is adminQueryLegalParties and adminListLegalParties (parties.legal_party.read).
	AdminListParties(ctx context.Context, a authz.Actor, q AdminQuery) (PartyPage, error)
	// AdminCreateParty is adminCreateLegalParty (parties.legal_party.create):
	// a payee or platform party, pending_verification.
	AdminCreateParty(ctx context.Context, a authz.Actor, in PartyInput) (PartyView, error)
	// AdminUpdateParty is adminUpdateLegalParty (parties.legal_party.update).
	AdminUpdateParty(ctx context.Context, a authz.Actor, partyID uuid.UUID, ifMatch string, p PartyPatch) (PartyView, error)
	// AdminVerifyParty is adminVerifyLegalParty (parties.legal_party.verify).
	AdminVerifyParty(ctx context.Context, a authz.Actor, partyID uuid.UUID, d Decision) (PartyView, error)
	// AdminVerifyDocument is adminVerifyLegalPartyDocument
	// (parties.document.verify): never by the document's uploader.
	AdminVerifyDocument(ctx context.Context, a authz.Actor, documentID uuid.UUID) (Document, error)
	// AdminGetParty is adminGetLegalParty (parties.legal_party.read; T-234).
	AdminGetParty(ctx context.Context, a authz.Actor, partyID uuid.UUID) (PartyView, error)
	// AdminUploadDocument is adminUploadLegalPartyDocument
	// (parties.document.upload; T-234): a document of any party, for
	// example a foundation that has no payee yet.
	AdminUploadDocument(ctx context.Context, a authz.Actor, partyID uuid.UUID, in DocumentInput) (Document, error)
	// AdminDownloadDocument is adminDownloadLegalPartyDocument
	// (parties.document.read; T-234): a 5-minute URL, audited.
	AdminDownloadDocument(ctx context.Context, a authz.Actor, documentID uuid.UUID) (DocumentLink, error)
	// AdminCorrectOwnership is adminCorrectLegalPartyOwnership
	// (parties.legal_party.verify, platform admins only; 06 H-60): a party
	// that holds another owner's tax ID is retired with the evidence.
	AdminCorrectOwnership(ctx context.Context, a authz.Actor, partyID uuid.UUID, in OwnershipCorrection) (PartyView, error)

	PayeeService
}

// PayeeService is the payee and bank account half of Service (04 §2.13,
// BE-037).
type PayeeService interface {
	// ListOrgPayees is listOrgPayees (parties.payee.list).
	ListOrgPayees(ctx context.Context, a authz.Actor, orgID uuid.UUID) ([]PayeeView, error)
	// GetOrgPayee is getOrgPayee (parties.payee.read).
	GetOrgPayee(ctx context.Context, a authz.Actor, orgID, payeeID uuid.UUID) (PayeeView, error)
	// ListPayeeBankAccounts is listPayeeBankAccounts
	// (parties.bank_account.list): masked for org roles; a full read by
	// finance or platform admins is audited.
	ListPayeeBankAccounts(ctx context.Context, a authz.Actor, orgID, payeeID uuid.UUID) ([]BankAccountView, error)
	// SubmitPayeeBankAccount is submitPayeeBankAccount
	// (parties.bank_account.submit, R).
	SubmitPayeeBankAccount(ctx context.Context, a authz.Actor, orgID, payeeID uuid.UUID, in BankAccountInput, proof identity.ReauthProof) (BankAccountView, error)

	// AdminCreatePayee is adminCreatePayee (parties.payee.manage).
	AdminCreatePayee(ctx context.Context, a authz.Actor, in PayeeInput) (PayeeView, error)
	// AdminUpdatePayee is adminUpdatePayee (parties.payee.manage; If-Match).
	AdminUpdatePayee(ctx context.Context, a authz.Actor, payeeID uuid.UUID, ifMatch string, p PayeePatch) (PayeeView, error)
	// AdminVerifyPayee is adminVerifyPayee (parties.payee.verify; If-Match): active.
	AdminVerifyPayee(ctx context.Context, a authz.Actor, payeeID uuid.UUID, ifMatch string) (PayeeView, error)
	// AdminQueryPayees is adminQueryPayees (parties.payee.list on the admin
	// surface; BE-129): every org's payees, newest first, with their org.
	AdminQueryPayees(ctx context.Context, a authz.Actor, q PayeeQuery) (PayeePage, error)

	// AdminListBankAccounts is adminListBankAccounts
	// (parties.bank_account.list): the four-eyes queue, the IBAN masked.
	AdminListBankAccounts(ctx context.Context, a authz.Actor, q BankAccountQuery) (BankAccountPage, error)
	// AdminSubmitBankAccount is adminSubmitBankAccount
	// (parties.bank_account.submit, R): pending_approval, the submitter is
	// the first eye.
	AdminSubmitBankAccount(ctx context.Context, a authz.Actor, payeeID uuid.UUID, in BankAccountInput, proof identity.ReauthProof) (BankAccountView, error)
	// AdminVerifyBankAccount is adminVerifyBankAccount
	// (parties.bank_account.verify): the first eye of an org submission.
	AdminVerifyBankAccount(ctx context.Context, a authz.Actor, id uuid.UUID, in VerifyInput) (BankAccountView, error)
	// AdminApproveBankAccount is adminApproveBankAccount
	// (parties.bank_account.approve, R): the second eye; the approved
	// account of the payee is superseded in the same transaction.
	AdminApproveBankAccount(ctx context.Context, a authz.Actor, id uuid.UUID, proof identity.ReauthProof) (BankAccountView, error)
	// AdminRejectBankAccount is adminRejectBankAccount
	// (parties.bank_account.reject).
	AdminRejectBankAccount(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (BankAccountView, error)
	// AdminRevokeBankAccount is adminRevokeBankAccount
	// (parties.bank_account.revoke, R): an approved account stops being
	// payable.
	AdminRevokeBankAccount(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof identity.ReauthProof) (BankAccountView, error)
	// AdminDownloadBankAccountEvidence is
	// adminDownloadBankAccountEvidence (parties.bank_account.list): the
	// evidence the eyes check, a 5-minute URL, audited.
	AdminDownloadBankAccountEvidence(ctx context.Context, a authz.Actor, id uuid.UUID) (DocumentLink, error)
	// AdminRevealBankAccountIBAN is adminRevealBankAccountIban
	// (parties.iban.reveal; BE-129): the account's full IBAN, audited with
	// the access reason.
	AdminRevealBankAccountIBAN(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (string, error)
}

// Payees is the payee contract of other modules (01 §4.12: catalog,
// orders, ledger). It reads without an actor (trusted in-process calls).
type Payees interface {
	// Get returns a payee (any status) or ErrNotFound.
	Get(ctx context.Context, payeeID uuid.UUID) (Payee, error)
	// DefaultFor returns the default payee of a seller org that is active
	// now; found is false when it has none.
	DefaultFor(ctx context.Context, sellerOrgID uuid.UUID) (Payee, bool, error)
	// ApprovedAccount returns the approved bank account of a payee, the only
	// account money may go to (with its IBAN in full: payouts snapshot it),
	// or 409 ErrNoApprovedAccount.
	ApprovedAccount(ctx context.Context, payeeID uuid.UUID) (BankAccount, error)
	// IsActive reports whether the payee may be paid now (status active
	// within its validity); ErrNotFound for an unknown payee.
	IsActive(ctx context.Context, payeeID uuid.UUID) (bool, error)
}

// Deps are the dependencies of the parties module. Store, Tx, Audit,
// Idempotency, Orgs, Clock and Log are required (New returns an error
// otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Audit       AuditLogger         // every write and every document download, same transaction
	Idempotency *idempotency.Keeper // optional Idempotency-Key of the POST operations (06 H-5)
	Orgs        Orgs                // the org of an org-surface request
	Clock       clock.Clock
	Log         *slog.Logger

	// Files attaches and serves the documents. nil → the document
	// operations fail closed with 503.
	Files Files
	// Sealer and Indexer protect an individual's national ID. nil → an
	// individual party is refused with 503 (nothing is stored in clear).
	Sealer  Sealer
	Indexer Indexer
	// AllowNaturalPersonPayees lifts 06 H-54 (individual and sole
	// proprietorship payee parties and payees). false in the MVP: cmd/koro
	// never sets it; a later ticket does, together with the payee IBAN
	// encryption.
	AllowNaturalPersonPayees bool

	// Outbox writes payee_bank_account.submitted/approved (O) in the
	// business transaction and .rejected (A) after it (01 §6.2).
	Outbox outbox.Writer
	// Queue enqueues the revocation notice after commit (A; 01 §6.2 has no
	// topic for it).
	Queue queue.Enqueuer
	// Reauth verifies the proof of the R operations (submit, approve,
	// revoke). nil → they fail closed with 503.
	Reauth Reauthenticator
	// Sender, Users and FinanceAdmins serve the security notices in the
	// worker: the payee org's admins come from Orgs, every finance admin in
	// effect from FinanceAdmins (identity, 06 H-33), their addresses from
	// Users. nil Sender or FinanceAdmins → the notice task fails and is
	// retried; nil Users → in-app notices only.
	Sender        Sender
	Users         Users
	FinanceAdmins func(ctx context.Context) ([]uuid.UUID, error)
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Audit", d.Audit != nil},
		{"Idempotency", d.Idempotency != nil}, {"Orgs", d.Orgs != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
		{"Outbox", d.Outbox != nil}, {"Queue", d.Queue != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("parties.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what parties hands to the composition root. The draft
// platform party is written by NewSeeder (`koro seed bootstrap-commerce`).
type Module struct {
	Service Service
	// Payees is the payee contract of other modules (01 §4.12).
	Payees Payees
	// Privacy is the KVKK provider (parties, documents, payees and bank
	// accounts a user created).
	Privacy privacy.Provider

	svc *service
}

// New builds the parties module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Payees: payees{s: s}, Privacy: privacyProvider{s: s}, svc: s}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
