package parties

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/idempotency"
)

// payeeCollection is the authz view of an org's payees on its surface
// (03 §3.5: OrgID = the org of the path; 03 §5.13 S cells). Every org-surface
// payee and bank account operation is authorised on it before any row is
// read (no ID probing); the rows are then read within the org (06 H-38).
func payeeCollection(orgID uuid.UUID) authz.Resource {
	org := orgID
	return authz.Resource{Type: authz.TypePayee, OrgID: &org}
}

// payeeEvent is the audit record of a payee write. Metadata carries field
// names and states, never a value of the party.
func payeeEvent(a authz.Actor, act authz.Action, p Payee, meta map[string]any) audit.Event {
	id, org := p.ID, p.OrgID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["legal_party_id"] = p.LegalPartyID.String()
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: authz.TypePayee, ResourceID: &id,
		Metadata: meta,
	}.By(a)
}

// ---- cross-module contract (01 §4.12)

// payees implements Payees over the service (its methods would collide
// with Parties.Get on the service itself).
type payees struct{ s *service }

var _ Payees = payees{}

// Get implements Payees.
func (p payees) Get(ctx context.Context, payeeID uuid.UUID) (Payee, error) {
	return p.s.d.Store.GetPayee(ctx, payeeID, false)
}

// DefaultFor implements Payees.
func (p payees) DefaultFor(ctx context.Context, sellerOrgID uuid.UUID) (Payee, bool, error) {
	py, found, err := p.s.d.Store.DefaultPayee(ctx, sellerOrgID)
	if err != nil || !found || !py.ActiveAt(p.s.now()) {
		return Payee{}, false, err
	}
	return py, true, nil
}

// ApprovedAccount implements Payees.
func (p payees) ApprovedAccount(ctx context.Context, payeeID uuid.UUID) (BankAccount, error) {
	m, err := p.s.d.Store.ApprovedAccounts(ctx, []uuid.UUID{payeeID}, false)
	if err != nil {
		return BankAccount{}, err
	}
	b, ok := m[payeeID]
	if !ok {
		return BankAccount{}, ErrNoApprovedAccount
	}
	return b, nil
}

// IsActive implements Payees.
func (p payees) IsActive(ctx context.Context, payeeID uuid.UUID) (bool, error) {
	py, err := p.s.d.Store.GetPayee(ctx, payeeID, false)
	if err != nil {
		return false, err
	}
	return py.ActiveAt(p.s.now()), nil
}

// ---- org surface

// orgPayee resolves a payee of an org-surface request: the org (404) →
// authz on the org's payees (404 outside the org, 403 without a role) →
// the payee within the org (404: another org's payee too).
func (s *service) orgPayee(ctx context.Context, a authz.Actor, act authz.Action, orgID, payeeID uuid.UUID) (orgs.OrgSummary, Payee, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return orgs.OrgSummary{}, Payee{}, err
	}
	if err := authz.Can(ctx, a, act, payeeCollection(orgID)); err != nil {
		return orgs.OrgSummary{}, Payee{}, err
	}
	p, err := s.d.Store.OrgPayee(ctx, orgID, payeeID, false)
	if err != nil {
		return orgs.OrgSummary{}, Payee{}, err
	}
	return o, p, nil
}

// ListOrgPayees implements Service: the org (404) → authz (404/403) → the
// org's payees with their party and approved account (masked).
func (s *service) ListOrgPayees(ctx context.Context, a authz.Actor, orgID uuid.UUID) ([]PayeeView, error) {
	if _, err := s.org(ctx, orgID); err != nil {
		return nil, err
	}
	if err := authz.Can(ctx, a, authz.PartiesPayeeList, payeeCollection(orgID)); err != nil {
		return nil, err
	}
	ps, err := s.d.Store.OrgPayees(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return s.payeeViews(ctx, ps)
}

// GetOrgPayee implements Service.
func (s *service) GetOrgPayee(ctx context.Context, a authz.Actor, orgID, payeeID uuid.UUID) (PayeeView, error) {
	_, p, err := s.orgPayee(ctx, a, authz.PartiesPayeeRead, orgID, payeeID)
	if err != nil {
		return PayeeView{}, err
	}
	return s.payeeView(ctx, p)
}

// payeeViews builds the views of ps: their parties and approved accounts.
func (s *service) payeeViews(ctx context.Context, ps []Payee) ([]PayeeView, error) {
	out := make([]PayeeView, len(ps))
	if len(ps) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	approved, err := s.d.Store.ApprovedAccounts(ctx, ids, false)
	if err != nil {
		return nil, err
	}
	partyOf := map[uuid.UUID]LegalParty{}
	for i, p := range ps {
		lp, ok := partyOf[p.LegalPartyID]
		if !ok {
			if lp, err = s.d.Store.GetParty(ctx, p.LegalPartyID); err != nil {
				return nil, err
			}
			lp = lp.withoutSecrets()
			partyOf[p.LegalPartyID] = lp
		}
		out[i] = PayeeView{Payee: p, Party: lp, ETag: etagOf(p.UpdatedAt)}
		if b, ok := approved[p.ID]; ok {
			out[i].Approved = &b
		}
	}
	return out, nil
}

func (s *service) payeeView(ctx context.Context, p Payee) (PayeeView, error) {
	vs, err := s.payeeViews(ctx, []Payee{p})
	if err != nil {
		return PayeeView{}, err
	}
	return vs[0], nil
}

// ---- admin surface

// AdminCreatePayee implements Service: authz (403 before reading) → body →
// the seller org (a club or a university: 422; closed: 409) → the legal
// party (kind payee: 422; a natural person: 422 parties.natural_person_payee,
// 06 H-54; inactive: 409) → in one transaction: the payee (draft), audit.
func (s *service) AdminCreatePayee(ctx context.Context, a authz.Actor, in PayeeInput) (PayeeView, error) {
	if err := authz.Can(ctx, a, authz.PartiesPayeeManage, adminSurface); err != nil {
		return PayeeView{}, err
	}
	p := Payee{
		LegalPartyID: in.LegalPartyID, OrgID: in.OrgID, IsDefault: in.IsDefault, PayoutMethod: PayoutBankEFT,
		InvoicingCapability: InvoicingUnknown, Status: PayeeDraft,
	}
	if in.PayoutMethod != "" {
		p.PayoutMethod = in.PayoutMethod
	}
	if in.InvoicingCapability != "" {
		p.InvoicingCapability = in.InvoicingCapability
	}
	if err := checkPayeeFields(p); err != nil {
		return PayeeView{}, err
	}
	ref, err := cleanLine("body.university_protocol_ref", in.UniversityProtocolRef, maxProtocolRef)
	if err != nil {
		return PayeeView{}, err
	}
	p.UniversityProtocolRef = ref
	o, err := s.d.Orgs.Get(ctx, in.OrgID)
	switch {
	case errors.Is(err, orgs.ErrNotFound) || err == nil && !isSellerKind(o.Kind):
		return PayeeView{}, fieldErr("body.org_id", "must be a club or university organization")
	case err != nil:
		return PayeeView{}, err
	case o.Status == orgs.StatusClosed:
		return PayeeView{}, orgs.ErrInvalidState
	}
	party, err := s.d.Store.GetParty(ctx, in.LegalPartyID)
	switch {
	case errors.Is(err, ErrNotFound) || err == nil && party.Kind != KindPayee:
		return PayeeView{}, fieldErr("body.legal_party_id", "must be a payee legal party (kind payee)")
	case err != nil:
		return PayeeView{}, err
	}
	if err := s.checkPayeeParty(party); err != nil {
		return PayeeView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op string
		P  Payee
	}{"create_payee", p})
	if err != nil {
		return PayeeView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		p.ID, p.CreatedBy, p.ValidFrom = uuid.NewV7(), a.UserID, s.now()
		stored, err := s.d.Store.InsertPayee(ctx, p)
		if err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{"operation": "create", "status": string(stored.Status), "is_default": stored.IsDefault,
			"payout_method": string(stored.PayoutMethod)}
		return stored.ID, s.d.Audit.Log(ctx, payeeEvent(a, authz.PartiesPayeeManage, stored, meta))
	})
	if err != nil {
		return PayeeView{}, err
	}
	return s.reloadPayee(ctx, id)
}

// checkPayeeParty is the legal-party rule of a payee: a legal entity (06
// H-54) that is not retired.
func (s *service) checkPayeeParty(p LegalParty) error {
	if p.LegalType.NaturalPerson() && !s.d.AllowNaturalPersonPayees {
		return ErrNaturalPersonPayee
	}
	if p.Status == StatusInactive {
		return ErrInvalidState
	}
	return nil
}

// checkPayeeFields validates the enumerations of a payee body (iyzico is
// phase 2: it needs a sub-merchant key the API does not take).
func checkPayeeFields(p Payee) error {
	if !p.PayoutMethod.Valid() || p.PayoutMethod == PayoutIyzico {
		return fieldErr("body.payout_method", "must be bank_eft or none")
	}
	if !p.InvoicingCapability.Valid() {
		return fieldErr("body.invoicing_capability", "must be can_issue, cannot_issue or unknown")
	}
	return nil
}

// AdminUpdatePayee implements Service: authz (403 before reading) → in one
// transaction: the payee, locked (404) → closed (409) → If-Match (428/412)
// → the patch → audit. Making an active payee the default moves the
// default in the same transaction; suspending ends its default effect
// (payees_default_uq counts active rows only); closing ends its validity.
func (s *service) AdminUpdatePayee(ctx context.Context, a authz.Actor, payeeID uuid.UUID, ifMatch string, pt PayeePatch) (PayeeView, error) {
	if err := authz.Can(ctx, a, authz.PartiesPayeeManage, adminSurface); err != nil {
		return PayeeView{}, err
	}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.GetPayee(ctx, payeeID, true)
		if err != nil {
			return err
		}
		if cur.Status == PayeeClosed {
			return ErrInvalidState
		}
		if err := checkIfMatch(ifMatch, etagOf(cur.UpdatedAt)); err != nil {
			return err
		}
		if pt.empty() {
			return ErrEmptyPatch
		}
		next, changed, err := s.applyPayeePatch(cur, pt)
		if err != nil || len(changed) == 0 {
			return err
		}
		meta := map[string]any{"operation": "update", "fields": changed, "from": string(cur.Status), "to": string(next.Status)}
		wasDefault := cur.IsDefault && cur.Status == PayeeActive
		if next.IsDefault && next.Status == PayeeActive && !wasDefault {
			prev, err := s.d.Store.ClearDefault(ctx, next.OrgID, next.ID)
			if err != nil {
				return err
			}
			meta["previous_default_payee_ids"] = idStrings(prev)
		}
		stored, err := s.d.Store.UpdatePayee(ctx, next)
		if err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, payeeEvent(a, authz.PartiesPayeeManage, stored, meta))
	})
	if err != nil {
		return PayeeView{}, err
	}
	return s.reloadPayee(ctx, payeeID)
}

// applyPayeePatch validates pt against cur and returns the new row and the
// names of the fields that change.
func (s *service) applyPayeePatch(cur Payee, pt PayeePatch) (Payee, []string, error) {
	next, changed := cur, []string{}
	if pt.Status != nil && *pt.Status != cur.Status {
		switch *pt.Status {
		case PayeeSuspended:
			if cur.Status != PayeeActive {
				return Payee{}, nil, ErrInvalidState
			}
		case PayeeClosed:
			if now := s.now(); now.After(cur.ValidFrom) {
				next.ValidTo = &now
			}
		default:
			return Payee{}, nil, fieldErr("body.status", "must be suspended or closed (adminVerifyPayee activates)")
		}
		next.Status = *pt.Status
		changed = append(changed, "status")
	}
	if pt.IsDefault != nil && *pt.IsDefault != cur.IsDefault {
		next.IsDefault = *pt.IsDefault
		changed = append(changed, "is_default")
	}
	if pt.PayoutMethod != nil && *pt.PayoutMethod != cur.PayoutMethod {
		next.PayoutMethod = *pt.PayoutMethod
		changed = append(changed, "payout_method")
	}
	if pt.InvoicingCapability != nil && *pt.InvoicingCapability != cur.InvoicingCapability {
		next.InvoicingCapability = *pt.InvoicingCapability
		changed = append(changed, "invoicing_capability")
	}
	if err := checkPayeeFields(next); err != nil {
		return Payee{}, nil, err
	}
	if pt.UniversityProtocolRef != nil {
		ref, err := cleanLine("body.university_protocol_ref", *pt.UniversityProtocolRef, maxProtocolRef)
		if err != nil {
			return Payee{}, nil, err
		}
		if !equalPtr(ref, cur.UniversityProtocolRef) {
			next.UniversityProtocolRef = ref
			changed = append(changed, "university_protocol_ref")
		}
	}
	return next, changed, nil
}

// AdminVerifyPayee implements Service: draft, pending_verification or
// suspended → active (verified by and at). The legal party must be
// verified (422 parties.verification_incomplete) and a legal entity (06
// H-54); a default payee takes the default over in the same transaction.
// Anything else is 409 parties.invalid_state.
func (s *service) AdminVerifyPayee(ctx context.Context, a authz.Actor, payeeID uuid.UUID) (PayeeView, error) {
	if err := authz.Can(ctx, a, authz.PartiesPayeeVerify, adminSurface); err != nil {
		return PayeeView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Payee uuid.UUID
	}{"verify_payee", payeeID})
	if err != nil {
		return PayeeView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetPayee(ctx, payeeID, true)
		if err != nil {
			return uuid.Nil(), err
		}
		switch cur.Status {
		case PayeeDraft, PayeePendingVerification, PayeeSuspended:
		default:
			return uuid.Nil(), ErrInvalidState
		}
		party, err := s.d.Store.GetParty(ctx, cur.LegalPartyID)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.checkPayeeParty(party); err != nil {
			return uuid.Nil(), err
		}
		if party.Status != StatusVerified {
			return uuid.Nil(), ErrVerificationIncomplete
		}
		meta := map[string]any{"operation": "verify", "from": string(cur.Status), "to": string(PayeeActive)}
		if cur.IsDefault {
			prev, err := s.d.Store.ClearDefault(ctx, cur.OrgID, cur.ID)
			if err != nil {
				return uuid.Nil(), err
			}
			meta["previous_default_payee_ids"] = idStrings(prev)
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.VerifiedAt, next.VerifiedBy = PayeeActive, &now, &by
		stored, err := s.d.Store.UpdatePayee(ctx, next)
		if err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.d.Audit.Log(ctx, payeeEvent(a, authz.PartiesPayeeVerify, stored, meta))
	})
	if err != nil {
		return PayeeView{}, err
	}
	return s.reloadPayee(ctx, payeeID)
}

func (s *service) reloadPayee(ctx context.Context, id uuid.UUID) (PayeeView, error) {
	p, err := s.d.Store.GetPayee(ctx, id, false)
	if err != nil {
		return PayeeView{}, err
	}
	return s.payeeView(ctx, p)
}

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
