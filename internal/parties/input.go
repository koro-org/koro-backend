package parties

import (
	"bytes"
	"context"
	"encoding/hex"
	"uuid"

	"koro/internal/platform/problem"
)

// Envelope purposes of an individual's national ID (02 §0.4). The sealed
// value is bound to its row (TCKNSealPurposePrefix + party id), so a
// ciphertext copied to another row does not open; crypto.rewrap rebuilds the
// purpose from the same prefix and the id column.
const (
	TCKNSealPurposePrefix = "parties.legal_party.tckn:"
	TCKNIndexPurpose      = "parties.legal_party.tckn"
)

// PartyInput is the body of createBuyerLegalParty and adminCreateLegalParty
// (04 §2.13). Kind, OrgID, VATStatus, EInvoiceAlias and TCKN are admin
// fields: the org route never reads them.
type PartyInput struct {
	Kind          Kind       // admin: payee | platform
	LegalType     LegalType  // required
	OrgID         *uuid.UUID // admin, payee only: the university org the party is
	Title         string     // invoice title (unvan), 2–300 characters
	TaxOffice     string
	TaxNo         string // VKN
	TCKN          string // admin, individual only (06 H-54: closed in the MVP)
	MersisNo      string
	Address       *Address
	Email         string
	Phone         string
	VATStatus     VATStatus // admin; "" → unknown
	EInvoiceAlias string    // admin
}

// PartyPatch is the body of updateLegalParty and adminUpdateLegalParty: nil
// leaves a field as it is, an empty string clears an optional one. The
// admin-only fields are refused on the org route.
type PartyPatch struct {
	LegalType     *LegalType
	Title         *string
	TaxOffice     *string
	TaxNo         *string
	TCKN          *string // admin, individual parties only
	MersisNo      *string
	Address       *Address
	Email         *string
	Phone         *string
	VATStatus     *VATStatus // admin
	EInvoiceAlias *string    // admin
	Deactivate    bool       // admin: status inactive (not the platform party)
}

func (p PartyPatch) empty() bool {
	return p.LegalType == nil && p.Title == nil && p.TaxOffice == nil && p.TaxNo == nil && p.TCKN == nil &&
		p.MersisNo == nil && p.Address == nil && p.Email == nil && p.Phone == nil && p.VATStatus == nil &&
		p.EInvoiceAlias == nil && !p.Deactivate
}

func (p PartyPatch) adminFields() bool {
	return p.TCKN != nil || p.VATStatus != nil || p.EInvoiceAlias != nil || p.Deactivate
}

// DocumentInput is the body of uploadLegalPartyDocument.
type DocumentInput struct {
	Kind   DocumentKind
	FileID uuid.UUID // a party_document upload (createUpload)
}

// Decision is the body of adminVerifyLegalParty.
type Decision struct {
	Decision Status // StatusVerified | StatusRejected
	Note     string // optional, audited
}

// AdminQuery narrows adminListLegalParties (04 §2.13: kind, status, q).
type AdminQuery struct {
	Kind   Kind
	Status Status
	Q      string
	Limit  int
	After  *uuid.UUID
}

// PartyPage is a page of parties; Next is the keyset position of the next
// page (nil: last page).
type PartyPage struct {
	Items []PartyView
	Next  *uuid.UUID
}

// cleanInput validates the fields of a new party. It returns the party
// without id, kind and status, and the national ID digits (individuals).
func cleanInput(in PartyInput) (LegalParty, string, error) {
	var p LegalParty
	var err error
	if !in.LegalType.Valid() {
		return LegalParty{}, "", fieldErr("body.legal_type", "is required")
	}
	p.LegalType = in.LegalType
	if p.Title, err = cleanTitle("body.title", in.Title); err != nil {
		return LegalParty{}, "", err
	}
	if p.TaxOffice, err = cleanLine("body.tax_office", in.TaxOffice, maxTaxOffice); err != nil {
		return LegalParty{}, "", err
	}
	if p.TaxNo, err = cleanVKN("body.tax_no", in.TaxNo); err != nil {
		return LegalParty{}, "", err
	}
	if p.MersisNo, err = cleanMERSIS("body.mersis_no", in.MersisNo); err != nil {
		return LegalParty{}, "", err
	}
	if p.Email, err = cleanEmail("body.email", in.Email); err != nil {
		return LegalParty{}, "", err
	}
	if p.Phone, err = cleanPhone("body.phone", in.Phone); err != nil {
		return LegalParty{}, "", err
	}
	if in.Address != nil {
		if p.Address, err = cleanAddress("body.address", *in.Address); err != nil {
			return LegalParty{}, "", err
		}
	}
	p.VATStatus = VATUnknown
	if in.VATStatus != "" {
		if !in.VATStatus.Valid() {
			return LegalParty{}, "", fieldErr("body.vat_status", "must be vat_payer, vat_exempt, not_vat_payer or unknown")
		}
		p.VATStatus = in.VATStatus
	}
	if p.EInvoiceAlias, err = cleanLine("body.e_invoice_alias", in.EInvoiceAlias, maxEInvoice); err != nil {
		return LegalParty{}, "", err
	}
	var tckn string
	switch {
	case p.LegalType == LegalIndividual:
		if tckn, err = cleanTCKN("body.tckn", in.TCKN); err != nil {
			return LegalParty{}, "", err
		}
	case in.TCKN != "":
		return LegalParty{}, "", fieldErr("body.tckn", "applies to individual parties only")
	}
	return p, tckn, nil
}

// requireBuyerFields is the createBuyerLegalParty body rule (04 §2.13):
// tax office, VKN, address and e-mail are required for an invoice title.
func requireBuyerFields(p LegalParty) error {
	switch {
	case p.TaxOffice == nil:
		return fieldErr("body.tax_office", "is required")
	case p.TaxNo == nil:
		return fieldErr("body.tax_no", "is required")
	case p.Address.Empty():
		return fieldErr("body.address", "is required")
	case p.Email == nil:
		return fieldErr("body.email", "is required")
	}
	return nil
}

// checkKindRules applies the legal type rules of a kind: buyers buy under
// a VKN (no individual); a payee is a legal entity in the MVP (06 H-54);
// Koro is not a natural person without a business (no individual).
func (s *service) checkKindRules(k Kind, t LegalType) error {
	switch k {
	case KindBuyerCompany:
		return validLegalType("body.legal_type", t, buyerTypes)
	case KindPayee:
		if t.NaturalPerson() && !s.d.AllowNaturalPersonPayees {
			return ErrNaturalPersonPayee
		}
	case KindPlatform:
		if t == LegalIndividual {
			return fieldErr("body.legal_type", "is not an allowed legal type here")
		}
	}
	return nil
}

// sealTCKN encrypts the national ID for the party id and returns the
// ciphertext and its blind index (nothing is stored in clear: without a
// sealer or indexer the request fails closed).
func (s *service) sealTCKN(ctx context.Context, partyID uuid.UUID, tckn string) (enc, bidx []byte, err error) {
	if s.d.Sealer == nil || s.d.Indexer == nil {
		return nil, nil, problem.ErrUnavailable
	}
	enc, err = s.d.Sealer.Seal(ctx, TCKNSealPurposePrefix+partyID.String(), []byte(tckn))
	if err != nil {
		return nil, nil, problem.ErrUnavailable.Wrap(err)
	}
	return enc, s.d.Indexer.BlindIndex(TCKNIndexPurpose, []byte(tckn)), nil
}

// tcknIndex is the blind index of a national ID, for idempotency hashes
// (the hash must never hold the clear value: 11 digits are brute-forced
// from a plain SHA-256).
func (s *service) tcknIndex(tckn string) (string, error) {
	if tckn == "" {
		return "", nil
	}
	if s.d.Indexer == nil {
		return "", problem.ErrUnavailable
	}
	return hex.EncodeToString(s.d.Indexer.BlindIndex(TCKNIndexPurpose, []byte(tckn))), nil
}

// patchResult is what applyPatch changed.
type patchResult struct {
	next    LegalParty
	changed []string // field names (audit metadata: never values)
}

// applyPatch validates p against cur and returns the new row. Only real
// changes count: a PATCH that repeats the stored values changes nothing.
// Any data change of a verified or rejected party sends it back to
// pending_verification (04 §2.13 updateLegalParty).
func (s *service) applyPatch(ctx context.Context, cur LegalParty, p PartyPatch) (patchResult, error) {
	next, changed := cur, []string{}
	mark := func(name string, same bool) {
		if !same {
			changed = append(changed, name)
		}
	}
	if p.LegalType != nil {
		t := *p.LegalType
		if !t.Valid() {
			return patchResult{}, fieldErr("body.legal_type", "is not an allowed legal type here")
		}
		if (t == LegalIndividual) != (cur.LegalType == LegalIndividual) {
			return patchResult{}, fieldErr("body.legal_type", "cannot change between individual and other legal types")
		}
		if err := s.checkKindRules(cur.Kind, t); err != nil {
			return patchResult{}, err
		}
		next.LegalType = t
		mark("legal_type", t == cur.LegalType)
	}
	if p.Title != nil {
		t, err := cleanTitle("body.title", *p.Title)
		if err != nil {
			return patchResult{}, err
		}
		next.Title = t
		mark("title", t == cur.Title)
	}
	type optional struct {
		name string
		in   *string
		dst  **string
		fn   func(loc, s string) (*string, error)
	}
	for _, f := range []optional{
		{"tax_office", p.TaxOffice, &next.TaxOffice, func(l, v string) (*string, error) { return cleanLine(l, v, maxTaxOffice) }},
		{"tax_no", p.TaxNo, &next.TaxNo, cleanVKN},
		{"mersis_no", p.MersisNo, &next.MersisNo, cleanMERSIS},
		{"email", p.Email, &next.Email, cleanEmail},
		{"phone", p.Phone, &next.Phone, cleanPhone},
		{"e_invoice_alias", p.EInvoiceAlias, &next.EInvoiceAlias, func(l, v string) (*string, error) { return cleanLine(l, v, maxEInvoice) }},
	} {
		if f.in == nil {
			continue
		}
		v, err := f.fn("body."+f.name, *f.in)
		if err != nil {
			return patchResult{}, err
		}
		before := *f.dst
		*f.dst = v
		mark(f.name, equalPtr(before, v))
	}
	if p.Address != nil {
		a, err := cleanAddress("body.address", *p.Address)
		if err != nil {
			return patchResult{}, err
		}
		next.Address = a
		mark("address", a == cur.Address)
	}
	if p.VATStatus != nil {
		if !p.VATStatus.Valid() {
			return patchResult{}, fieldErr("body.vat_status", "must be vat_payer, vat_exempt, not_vat_payer or unknown")
		}
		next.VATStatus = *p.VATStatus
		mark("vat_status", next.VATStatus == cur.VATStatus)
	}
	if p.TCKN != nil {
		if cur.LegalType != LegalIndividual {
			return patchResult{}, fieldErr("body.tckn", "applies to individual parties only")
		}
		d, err := cleanTCKN("body.tckn", *p.TCKN)
		if err != nil {
			return patchResult{}, err
		}
		enc, bidx, err := s.sealTCKN(ctx, cur.ID, d)
		if err != nil {
			return patchResult{}, err
		}
		if !bytes.Equal(bidx, cur.TCKNBidx) {
			next.TCKNEnc, next.TCKNBidx = enc, bidx
			changed = append(changed, "tckn")
		}
	}
	if cur.Kind == KindBuyerCompany {
		if err := requireBuyerFields(next); err != nil {
			return patchResult{}, err
		}
	}
	if len(changed) > 0 && (cur.Status == StatusVerified || cur.Status == StatusRejected) {
		next.Status, next.VerifiedAt, next.VerifiedBy = StatusPendingVerification, nil, nil
	}
	if p.Deactivate && cur.Status != StatusInactive {
		if cur.Kind == KindPlatform {
			return patchResult{}, ErrInvalidState
		}
		next.Status, next.VerifiedAt, next.VerifiedBy = StatusInactive, nil, nil
		changed = append(changed, "status")
	}
	return patchResult{next: next, changed: changed}, nil
}

// verificationReady reports whether p carries what an invoice needs: a VKN
// (or, for an individual, the national ID) and an address with line and
// city.
func verificationReady(p LegalParty) bool {
	id := p.TaxNo != nil
	if p.LegalType == LegalIndividual {
		id = p.HasTCKN()
	}
	return id && p.Address.Line1 != "" && p.Address.City != ""
}
