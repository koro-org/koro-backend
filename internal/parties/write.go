package parties

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/idempotency"
)

// createCmd is the idempotency hash input of a party creation: the cleaned
// fields and, instead of a national ID, its blind index.
type createCmd struct {
	Op        string
	Org       *uuid.UUID
	Kind      Kind
	LegalType LegalType
	Title     string
	TaxOffice *string
	TaxNo     *string
	TCKNIndex string
	MersisNo  *string
	Address   Address
	Email     *string
	Phone     *string
	VAT       VATStatus
	EInvoice  *string
}

func cmdOf(op string, org *uuid.UUID, p LegalParty, tcknIndex string) createCmd {
	return createCmd{
		Op: op, Org: org, Kind: p.Kind, LegalType: p.LegalType, Title: p.Title, TaxOffice: p.TaxOffice, TaxNo: p.TaxNo,
		TCKNIndex: tcknIndex, MersisNo: p.MersisNo, Address: p.Address, Email: p.Email, Phone: p.Phone, VAT: p.VATStatus,
		EInvoice: p.EInvoiceAlias,
	}
}

// CreateBuyerParty implements Service. Order: the org (404) → authz
// (404/403; k22) → org status (403 orgs.org_suspended) → company org (422
// parties.org_not_company) → body → in one transaction: the party
// (pending_verification), audit.
func (s *service) CreateBuyerParty(ctx context.Context, a authz.Actor, orgID uuid.UUID, in PartyInput) (PartyView, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return PartyView{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyCreate, newBuyerResource(orgID)); err != nil {
		return PartyView{}, err
	}
	if err := writableOrg(a, o); err != nil {
		return PartyView{}, err
	}
	if o.Kind != orgs.KindCompany {
		return PartyView{}, ErrOrgNotCompany
	}
	in.Kind, in.OrgID, in.TCKN, in.VATStatus, in.EInvoiceAlias = KindBuyerCompany, nil, "", "", "" // org-route body
	p, _, err := cleanInput(in)
	if err != nil {
		return PartyView{}, err
	}
	if err := s.checkKindRules(KindBuyerCompany, p.LegalType); err != nil {
		return PartyView{}, err
	}
	if err := requireBuyerFields(p); err != nil {
		return PartyView{}, err
	}
	org := orgID
	p.Kind, p.OrgID, p.Status = KindBuyerCompany, &org, StatusPendingVerification
	id, err := s.insert(ctx, a, &org, p, "", cmdOf("create_buyer", &org, p, ""))
	if err != nil {
		return PartyView{}, err
	}
	return s.reload(ctx, id, fullTaxIDs(a))
}

// AdminCreateParty implements Service: a payee or platform party
// (pending_verification). 06 H-54: a natural-person payee is 422. A payee
// may name the university org it is (never a club: the club is the seller
// org of a payee row, R-003 §4.7); the platform party has no org.
func (s *service) AdminCreateParty(ctx context.Context, a authz.Actor, in PartyInput) (PartyView, error) {
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyCreate, adminSurface); err != nil {
		return PartyView{}, err
	}
	if in.Kind != KindPayee && in.Kind != KindPlatform {
		return PartyView{}, fieldErr("body.kind", "must be payee or platform (buyer parties are created by their company)")
	}
	if !in.LegalType.Valid() {
		return PartyView{}, fieldErr("body.legal_type", "is required")
	}
	if err := s.checkKindRules(in.Kind, in.LegalType); err != nil {
		return PartyView{}, err
	}
	p, tckn, err := cleanInput(in)
	if err != nil {
		return PartyView{}, err
	}
	if in.OrgID != nil {
		if in.Kind == KindPlatform {
			return PartyView{}, fieldErr("body.org_id", "the platform party has no org")
		}
		o, err := s.d.Orgs.Get(ctx, *in.OrgID)
		if errors.Is(err, orgs.ErrNotFound) || err == nil && o.Kind != orgs.KindUniversity {
			return PartyView{}, fieldErr("body.org_id", "must be a university organization")
		}
		if err != nil {
			return PartyView{}, err
		}
		org := *in.OrgID
		p.OrgID = &org
	}
	index, err := s.tcknIndex(tckn)
	if err != nil {
		return PartyView{}, err
	}
	p.Kind, p.Status = in.Kind, StatusPendingVerification
	id, err := s.insert(ctx, a, nil, p, tckn, cmdOf("admin_create", p.OrgID, p, index))
	if err != nil {
		return PartyView{}, err
	}
	return s.reload(ctx, id, true)
}

// insert writes a new party once per Idempotency-Key (06 H-5): the
// national ID is sealed for the new row inside the transaction.
func (s *service) insert(ctx context.Context, a authz.Actor, orgID *uuid.UUID, p LegalParty, tckn string, cmd createCmd) (uuid.UUID, error) {
	hash, err := idempotency.Hash(cmd)
	if err != nil {
		return uuid.Nil(), err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		p.ID = uuid.NewV7()
		by := a.UserID
		p.CreatedBy = &by
		if tckn != "" {
			enc, bidx, err := s.sealTCKN(ctx, p.ID, tckn)
			if err != nil {
				return uuid.Nil(), err
			}
			p.TCKNEnc, p.TCKNBidx = enc, bidx
		}
		stored, err := s.d.Store.InsertParty(ctx, p)
		if err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{"legal_type": string(stored.LegalType), "status": string(stored.Status)}
		return stored.ID, s.d.Audit.Log(ctx, partyEvent(a, authz.PartiesLegalPartyCreate, orgID, stored, "", meta))
	})
	return id, err
}

// UpdateOrgParty implements Service. Order: the org (404) → authz on a
// buyer party of the org (404 outside the org, 403 without a role: no ID
// probing; k22) → in one transaction: the party within the org, locked
// (404) → authz on the party (k22) → org status (403) → party status (409)
// → If-Match (428/412) → the patch → audit.
func (s *service) UpdateOrgParty(ctx context.Context, a authz.Actor, orgID, partyID uuid.UUID, ifMatch string, p PartyPatch) (PartyView, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return PartyView{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyUpdate, newBuyerResource(orgID)); err != nil {
		return PartyView{}, err
	}
	org := orgID
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		op, err := s.d.Store.OrgParty(ctx, orgID, partyID, true)
		if err != nil {
			return err
		}
		if err := authz.Can(ctx, a, authz.PartiesLegalPartyUpdate, partyResource(orgID, op)); err != nil {
			return err
		}
		if err := writableOrg(a, o); err != nil {
			return err
		}
		if p.adminFields() {
			return fieldErr("body", "national ID, VAT status, e-invoice alias and deactivation are admin fields")
		}
		return s.update(ctx, a, &org, op.Party, ifMatch, p)
	})
	if err != nil {
		return PartyView{}, err
	}
	return s.reload(ctx, partyID, fullTaxIDs(a))
}

// AdminUpdateParty implements Service: authz (403 before reading) → in one
// transaction: the party, locked (404) → status (409) → If-Match → patch.
func (s *service) AdminUpdateParty(ctx context.Context, a authz.Actor, partyID uuid.UUID, ifMatch string, p PartyPatch) (PartyView, error) {
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyUpdate, adminSurface); err != nil {
		return PartyView{}, err
	}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockParty(ctx, partyID)
		if err != nil {
			return err
		}
		return s.update(ctx, a, nil, cur, ifMatch, p)
	})
	if err != nil {
		return PartyView{}, err
	}
	return s.reload(ctx, partyID, true)
}

// update applies a patch to a locked party (both surfaces).
func (s *service) update(ctx context.Context, a authz.Actor, orgID *uuid.UUID, cur LegalParty, ifMatch string, p PartyPatch) error {
	if cur.Status == StatusInactive {
		return ErrInvalidState
	}
	if err := checkIfMatch(ifMatch, etagOf(cur.UpdatedAt)); err != nil {
		return err
	}
	if p.empty() {
		return ErrEmptyPatch
	}
	res, err := s.applyPatch(ctx, cur, p)
	if err != nil {
		return err
	}
	if len(res.changed) == 0 {
		return nil // the stored values already: nothing to write, no new version
	}
	stored, err := s.d.Store.UpdateParty(ctx, res.next)
	if err != nil {
		return err
	}
	meta := map[string]any{"fields": res.changed, "from": string(cur.Status), "to": string(stored.Status)}
	return s.d.Audit.Log(ctx, partyEvent(a, authz.PartiesLegalPartyUpdate, orgID, stored, "", meta))
}

// AdminVerifyParty implements Service: draft, pending_verification or
// rejected → verified (the party must carry its tax ID and address: 422
// parties.verification_incomplete); draft or pending_verification →
// rejected. Anything else is 409 parties.invalid_state.
func (s *service) AdminVerifyParty(ctx context.Context, a authz.Actor, partyID uuid.UUID, d Decision) (PartyView, error) {
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyVerify, adminSurface); err != nil {
		return PartyView{}, err
	}
	if d.Decision != StatusVerified && d.Decision != StatusRejected {
		return PartyView{}, fieldErr("body.decision", "must be verified or rejected")
	}
	note, err := cleanNote("body.note", d.Note)
	if err != nil {
		return PartyView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Party    uuid.UUID
		Decision Status
		Note     string
	}{"verify", partyID, d.Decision, note})
	if err != nil {
		return PartyView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.LockParty(ctx, partyID)
		if err != nil {
			return uuid.Nil(), err
		}
		next := cur
		switch {
		case d.Decision == StatusVerified && (cur.Status == StatusDraft || cur.Status == StatusPendingVerification ||
			cur.Status == StatusRejected):
			if !verificationReady(cur) {
				return uuid.Nil(), ErrVerificationIncomplete
			}
			now, by := s.now(), a.UserID
			next.Status, next.VerifiedAt, next.VerifiedBy = StatusVerified, &now, &by
		case d.Decision == StatusRejected && (cur.Status == StatusDraft || cur.Status == StatusPendingVerification):
			next.Status, next.VerifiedAt, next.VerifiedBy = StatusRejected, nil, nil
		default:
			return uuid.Nil(), ErrInvalidState
		}
		stored, err := s.d.Store.UpdateParty(ctx, next)
		if err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{"decision": string(d.Decision), "from": string(cur.Status), "to": string(stored.Status)}
		return stored.ID, s.d.Audit.Log(ctx, partyEvent(a, authz.PartiesLegalPartyVerify, nil, stored, note, meta))
	})
	if err != nil {
		return PartyView{}, err
	}
	return s.reload(ctx, partyID, true)
}
