package catalog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/parties"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/money"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// ---- commission arithmetic (06 H-6: platform/money only)

// Commission is the commission of one order line under a policy.
type Commission struct {
	Amount    int64 // the commission (kuruş)
	VATAmount int64 // the commission's VAT (commission_vat_rate_bp)
	BuyerFee  int64 // Amount + VATAmount when the buyer pays it, else 0 (order_lines.buyer_fee_amount)
}

// lineVAT is the VAT of a line: net × rate, half away from zero (06 H-6).
func lineVAT(net int64, rateBP int) (int64, error) { return money.VAT(net, int64(rateBP)) }

// LineCommission computes the commission of one order line of net amount
// net and VAT vat (02 §19.2, 06 H-6): the basis is net (net_excl_vat) or
// net + vat (gross_incl_vat); commission = basis × rate, rounded half away
// from zero, then the policy's min/max; its VAT at commission_vat_rate_bp.
// orders (BE-040) and the buyer fee estimate of getOffering use it, so the
// formula lives in one place.
func (p CommercePolicy) LineCommission(net, vat int64) (Commission, error) {
	basis := net
	if p.CommissionBasis == BasisGrossInclVAT {
		var err error
		if basis, err = money.Add(net, vat); err != nil {
			return Commission{}, err
		}
	}
	c, err := money.Commission(basis, int64(p.CommissionRateBP), p.CommissionMinAmount, p.CommissionMaxAmount)
	if err != nil {
		return Commission{}, err
	}
	cv, err := money.VAT(c, int64(p.CommissionVATRateBP))
	if err != nil {
		return Commission{}, err
	}
	out := Commission{Amount: c, VATAmount: cv}
	if p.CommissionPayer == PayerBuyer {
		if out.BuyerFee, err = money.Add(c, cv); err != nil {
			return Commission{}, err
		}
	}
	return out, nil
}

// ---- the money and invoice direction of the models (02 §18.3 table)

// Party of a money or invoice flow.
type Party string

// Parties of the flows.
const (
	PartyNone        Party = ""
	PartyKoro        Party = "koro"
	PartyKoroOnBehal Party = "koro_on_behalf_of_payee" // A2: Koro issues the payee's invoice
	PartyPayee       Party = "payee"
	PartyBuyer       Party = "buyer"
)

// ModelFlow is one row of the 02 §18.3 model table: who collects, who
// invoices whom, whether Koro pays the payee out. billing plans the
// expected invoices from it (BE-042); the fixed table keeps every model
// in one schema (ADR-0014) while the team decides the invoice direction
// (E-01/E-02): a decision changes the policy row, not the code.
type ModelFlow struct {
	Model                ModelCode
	CollectedBy          Party // the buyer pays Koro (A, A2, B) or the payee (C)
	ServiceInvoiceIssuer Party // who invoices the buyer for the service
	CommissionInvoice    struct{ From, To Party }
	PayeeInvoicesKoro    bool // B: the payee invoices Koro for its share
	Payout               bool // Koro pays the payee out (not in C)
}

// FlowOf returns the model's flow for the commission payer (02 §18.3).
func FlowOf(m ModelCode, payer CommissionPayer) (ModelFlow, error) {
	f := ModelFlow{Model: m}
	commission := struct{ From, To Party }{PartyKoro, PartyPayee}
	if payer == PayerBuyer {
		commission.To = PartyBuyer
	}
	switch m {
	case ModelA:
		f.CollectedBy, f.ServiceInvoiceIssuer, f.CommissionInvoice, f.Payout = PartyKoro, PartyPayee, commission, true
	case ModelA2:
		f.CollectedBy, f.ServiceInvoiceIssuer, f.CommissionInvoice, f.Payout = PartyKoro, PartyKoroOnBehal, commission, true
	case ModelB:
		f.CollectedBy, f.ServiceInvoiceIssuer, f.PayeeInvoicesKoro, f.Payout = PartyKoro, PartyKoro, true, true
	case ModelC:
		f.CollectedBy, f.ServiceInvoiceIssuer = PartyPayee, PartyPayee
		f.CommissionInvoice = struct{ From, To Party }{PartyKoro, PartyBuyer}
	default:
		return ModelFlow{}, fmt.Errorf("catalog: unknown model %q", m)
	}
	return f, nil
}

// ---- resolution

// ResolvePolicy implements Offerings (02 §18.3): the policy in effect at at
// for the offering's own scope, its service, its seller org, then the
// default.
func (s *service) ResolvePolicy(ctx context.Context, offeringID uuid.UUID, at time.Time) (CommercePolicy, error) {
	o, err := s.d.Store.GetOffering(ctx, offeringID, false)
	if err != nil {
		return CommercePolicy{}, err
	}
	p, ok, err := s.d.Store.PolicyAt(ctx, o.ID, o.ServiceID, o.OrgID, at.UTC())
	switch {
	case err != nil:
		return CommercePolicy{}, err
	case !ok:
		return CommercePolicy{}, ErrNoPolicy
	}
	return p, nil
}

// ---- admin surface (06 H-17: one person, re-authentication, audit, every admin notified)

// NotifyPolicyChangedTaskName is the task that tells every platform and
// finance admin about a commerce policy change (A; 01 §6.2 has no topic for
// it: doc patch commerce_policy.changed).
const NotifyPolicyChangedTaskName = "catalog.notify_policy_changed"

// policyNotice is the payload of NotifyPolicyChangedTaskName (IDs and codes only).
type policyNotice struct {
	PolicyID uuid.UUID `json:"policy_id"`
	Change   string    `json:"change"` // "created" | "closed"
	By       uuid.UUID `json:"by"`
}

// AdminListPolicies implements Service.
func (s *service) AdminListPolicies(ctx context.Context, a authz.Actor, f PolicyFilter) ([]CommercePolicy, error) {
	if err := authz.Can(ctx, a, authz.CatalogPolicyList, adminSurface); err != nil {
		return nil, err
	}
	if f.Scope != "" && !f.Scope.Valid() {
		return nil, problem.Field("query.scope", "must be default, seller_org, service or offering")
	}
	return s.d.Store.ListPolicies(ctx, f)
}

// AdminCreatePolicy implements Service: authz → body → re-authentication
// (03 §6.2) → the scope exists → insert (an overlapping period of the scope
// is 409 catalog.policy_overlap) → audit → every admin is notified.
func (s *service) AdminCreatePolicy(ctx context.Context, a authz.Actor, in PolicyInput, proof identity.ReauthProof) (CommercePolicy, error) {
	if err := authz.Can(ctx, a, authz.CatalogPolicyManage, adminSurface); err != nil {
		return CommercePolicy{}, err
	}
	now := s.now()
	row, err := policyOf(in, now)
	if err != nil {
		return CommercePolicy{}, err
	}
	kind, err := s.reauth(ctx, a, proof, nil, "created")
	if err != nil {
		return CommercePolicy{}, err
	}
	if err := s.checkScope(ctx, row); err != nil {
		return CommercePolicy{}, err
	}
	row.ID, row.CreatedBy = uuid.NewV7(), a.UserID
	hash, err := idempotency.Hash(struct {
		Op string
		In PolicyInput
	}{"create_commerce_policy", in})
	if err != nil {
		return CommercePolicy{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		stored, err := s.d.Store.InsertPolicy(ctx, row)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, policyEvent(a, stored, "created", kind)); err != nil {
			return uuid.Nil(), err
		}
		s.d.Queue.AfterCommit(ctx, NotifyPolicyChangedTaskName, policyNotice{PolicyID: stored.ID, Change: "created", By: a.UserID},
			"catalog.policy_created:"+stored.ID.String(), queue.Queue(queue.Critical))
		return stored.ID, nil
	})
	if err != nil {
		return CommercePolicy{}, err
	}
	return s.d.Store.GetPolicy(ctx, id, false)
}

// AdminClosePolicy implements Service: an open policy gets its valid_to
// (the only change a row takes). valid_to is now when not given, never in
// the past (past resolutions stay as they were) and after valid_from; a
// policy that has not started is closed at its start + 1 µs (it never
// applies).
func (s *service) AdminClosePolicy(ctx context.Context, a authz.Actor, id uuid.UUID, validTo *time.Time, proof identity.ReauthProof) (CommercePolicy, error) {
	if err := authz.Can(ctx, a, authz.CatalogPolicyManage, adminSurface); err != nil {
		return CommercePolicy{}, err
	}
	now := s.now()
	if validTo != nil && validTo.Before(now) {
		return CommercePolicy{}, problem.Field("body.valid_to", "must not be in the past")
	}
	kind, err := s.reauth(ctx, a, proof, &id, "closed")
	if err != nil {
		return CommercePolicy{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op      string
		Policy  uuid.UUID
		ValidTo *time.Time
	}{"close_commerce_policy", id, validTo})
	if err != nil {
		return CommercePolicy{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetPolicy(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if !cur.Open() {
			return uuid.Nil(), ErrInvalidState
		}
		end := now
		if validTo != nil {
			end = validTo.UTC().Truncate(time.Microsecond)
		}
		if !end.After(cur.ValidFrom) {
			if validTo != nil {
				return uuid.Nil(), problem.Field("body.valid_to", "must be after the policy's valid_from")
			}
			end = cur.ValidFrom.Add(time.Microsecond) // not started: it never applies
		}
		stored, err := s.d.Store.ClosePolicy(ctx, cur.ID, end)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, policyEvent(a, stored, "closed", kind)); err != nil {
			return uuid.Nil(), err
		}
		s.d.Queue.AfterCommit(ctx, NotifyPolicyChangedTaskName, policyNotice{PolicyID: stored.ID, Change: "closed", By: a.UserID},
			"catalog.policy_closed:"+stored.ID.String(), queue.Queue(queue.Critical))
		return stored.ID, nil
	})
	if err != nil {
		return CommercePolicy{}, err
	}
	return s.d.Store.GetPolicy(ctx, id, false)
}

// reauth verifies the proof of catalog.policy.manage (03 §6.2, 06 H-17).
// A refused proof is audited as denied with the policy (nil before it
// exists) and the change; an error without a verdict (overload, a
// dependency down) writes nothing (audit.ReauthFailed, BE-074).
func (s *service) reauth(ctx context.Context, a authz.Actor, proof identity.ReauthProof, policyID *uuid.UUID, change string) (string, error) {
	if s.d.Reauth == nil {
		return "", ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		denied := audit.Event{Action: string(authz.CatalogPolicyManage), ResourceType: auditPolicy, ResourceID: policyID,
			Metadata: map[string]any{"change": change}}.By(a)
		return "", audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	return kind, nil
}

// checkScope checks that the scope row exists: a seller org (club,
// university or the platform), a service, an offering.
func (s *service) checkScope(ctx context.Context, p CommercePolicy) error {
	bad := problem.Field("body.scope_id", "names no "+string(p.Scope))
	switch p.Scope {
	case ScopeSellerOrg:
		o, err := s.d.Orgs.Get(ctx, *p.ScopeID)
		if err != nil {
			if errors.Is(err, ErrNotFound) || isOrgNotFound(err) {
				return bad
			}
			return err
		}
		if _, ok := sellerKindOf(o.Kind); !ok {
			return problem.Field("body.scope_id", "must be a club, a university or the platform org")
		}
	case ScopeService:
		if _, err := s.d.Store.GetService(ctx, *p.ScopeID, false); err != nil {
			if errors.Is(err, ErrNotFound) {
				return bad
			}
			return err
		}
	case ScopeOffering:
		if _, err := s.d.Store.GetOffering(ctx, *p.ScopeID, false); err != nil {
			if errors.Is(err, ErrNotFound) {
				return bad
			}
			return err
		}
	}
	return nil
}

// policyEvent is the audit record of a policy change: every parameter
// (they are configuration, never personal data) and the reauth kind.
func policyEvent(a authz.Actor, p CommercePolicy, change, reauthKind string) audit.Event {
	id := p.ID
	meta := map[string]any{
		"change": change, "scope": string(p.Scope), "model_code": string(p.ModelCode),
		"collection_method": string(p.CollectionMethod), "commission_rate_bp": p.CommissionRateBP,
		"commission_basis": string(p.CommissionBasis), "commission_payer": string(p.CommissionPayer),
		"commission_vat_rate_bp": p.CommissionVATRateBP, "invoice_automation": string(p.InvoiceAutomation),
		"dispute_window_days": p.DisputeWindowDays, "payout_hold_days": p.PayoutHoldDays,
		"valid_from": p.ValidFrom.UTC().Format(time.RFC3339Nano),
	}
	if reauthKind != "" {
		meta[audit.MetaReauth] = reauthKind
	}
	if p.ScopeID != nil {
		meta["scope_id"] = p.ScopeID.String()
	}
	if p.ValidTo != nil {
		meta["valid_to"] = p.ValidTo.UTC().Format(time.RFC3339Nano)
	}
	if p.CommissionMinAmount != nil {
		meta["commission_min_amount"] = strconv.FormatInt(*p.CommissionMinAmount, 10)
	}
	if p.CommissionMaxAmount != nil {
		meta["commission_max_amount"] = strconv.FormatInt(*p.CommissionMaxAmount, 10)
	}
	return audit.Event{Action: string(authz.CatalogPolicyManage), Outcome: audit.Success, ResourceType: auditPolicy,
		ResourceID: &id, Metadata: meta}.By(a)
}

// ---- checks shared by submission and approval

// checkSellable checks what an offering needs to be submitted or go on
// sale: a sellable service, an open event, a sales window that has not
// ended, and for an org seller an active payee of its own with an approved
// bank account (BE-037).
func (s *service) checkSellable(ctx context.Context, o Offering, sv ServiceItem) error {
	now := s.now()
	if !sv.Sellable() {
		return ErrServiceNotSellable
	}
	missing := func(loc, msg string) error {
		return ErrOfferingIncomplete.WithFields(fieldError(loc, msg))
	}
	if sv.FulfillmentType == FulfillEventBound && o.EventID == nil {
		return missing("offering.event_id", "an event-bound service is sold at an event of the seller")
	}
	if o.EventID != nil {
		if s.d.Events == nil {
			return ErrUnavailable
		}
		e, err := s.d.Events.Get(ctx, *o.EventID)
		if err != nil && !isEventNotFound(err) {
			return err
		}
		if err != nil || e.OrgID != o.OrgID || !openEvent(e) {
			return missing("offering.event_id", "the event is cancelled, completed or gone")
		}
	}
	if o.SalesEndAt != nil && !o.SalesEndAt.After(now) {
		return missing("offering.sales_end_at", "the sales window has ended")
	}
	if o.SellerKind == SellerPlatform {
		return nil
	}
	if o.DefaultPayeeID == nil {
		return missing("offering.default_payee_id", "an organization sells for a payee")
	}
	if s.d.Payees == nil {
		return ErrUnavailable
	}
	py, err := s.d.Payees.Get(ctx, *o.DefaultPayeeID)
	switch {
	case errors.Is(err, parties.ErrNotFound):
		return missing("offering.default_payee_id", "the payee is gone")
	case err != nil:
		return err
	case py.OrgID != o.OrgID:
		return ErrParentMismatch
	}
	active, err := s.d.Payees.IsActive(ctx, py.ID)
	if err != nil {
		return err
	}
	if !active {
		return ErrPayeeNotActive
	}
	if _, err := s.d.Payees.ApprovedAccount(ctx, py.ID); err != nil {
		return err // 409 parties.no_approved_account
	}
	return nil
}
