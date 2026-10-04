// Package parties is the legal parties module, first half (01 §4.12;
// BE-036): the legal persons of money and invoices — Koro's own party
// (kind platform, one row, seeded as draft until Koro's legal entity exists:
// T-31), the invoice titles of buying companies (buyer_company) and the
// legal side of payees (payee: the foundation, university or association
// paid on behalf of a club) — their evidence documents and their
// verification. Payees and their bank accounts come with BE-037.
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
package parties

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/privacy"
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

	// AdminListParties is adminListLegalParties (parties.legal_party.read).
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
	// proprietorship payee parties). false in the MVP: cmd/koro never sets
	// it; a later ticket does, together with the payee IBAN encryption.
	AllowNaturalPersonPayees bool
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Audit", d.Audit != nil},
		{"Idempotency", d.Idempotency != nil}, {"Orgs", d.Orgs != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
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
	// Privacy is the KVKK provider (parties and documents a user created).
	Privacy privacy.Provider
}

// New builds the parties module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Privacy: privacyProvider{s: s}}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
