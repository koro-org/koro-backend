package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// Offering cancellation reasons (offering.cancelled Reason: a code, 01 §6.2).
const (
	CancelBySeller       = "seller_cancelled"
	CancelByPlatform     = "platform_cancelled"
	CancelEventCancelled = "event_cancelled"
	cancelEventOfferings = "catalog.cancel_event_offerings" // the system job of the event.cancelled consumer
)

// CreateOffering implements Service: a draft of the org (a club or a
// university for its payee, the platform org for Koro). The service must
// be active (its review is checked at submission), a platform-native
// service is the platform's only, the event and the payee must be the
// seller's (06 H-38, H-55), the VAT rate comes from the service.
func (s *service) CreateOffering(ctx context.Context, a authz.Actor, orgID uuid.UUID, in OfferingInput) (OfferingView, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return OfferingView{}, err
	}
	if err := authz.Can(ctx, a, authz.CatalogOfferingCreate, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return OfferingView{}, err
	}
	if err := writableOrg(a, o); err != nil {
		return OfferingView{}, err
	}
	kind, ok := sellerKindOf(o.Kind)
	if !ok {
		return OfferingView{}, problem.Field("path.org_id", "must be a club, a university or the platform org")
	}
	now := s.now()
	row := Offering{
		ID: uuid.NewV7(), ServiceID: in.ServiceID, SellerKind: kind, OrgID: o.ID, EventID: in.EventID,
		DefaultPayeeID: in.DefaultPayeeID, UnitPriceAmount: in.UnitPriceAmount, Currency: "TRY", CompensationType: "cash",
		QuantityTotal: in.QuantityTotal, PerBuyerLimit: in.PerBuyerLimit, SalesStartAt: utc(in.SalesStartAt),
		SalesEndAt: utc(in.SalesEndAt), FulfilBy: utc(in.FulfilBy), InvoiceTrigger: in.InvoiceTrigger,
		RequiresUniversityApproval: in.RequiresUniversityApproval, UniversityApprovalStatus: undecided(in.RequiresUniversityApproval),
		Status: OfferingDraft, CreatedBy: a.UserID,
	}
	if row.InvoiceTrigger == "" {
		row.InvoiceTrigger = InvoiceOnFulfillment
	}
	if row.Title, err = cleanText("body.title", in.Title, minTitle, maxTitle); err != nil {
		return OfferingView{}, err
	}
	if row.DescriptionJSON, err = descriptionOf(in.DescriptionJSON); err != nil {
		return OfferingView{}, err
	}
	if row.Deliverables, err = cleanDeliverables(in.Deliverables); err != nil {
		return OfferingView{}, err
	}
	if row.BlockedSponsorFlags, err = cleanFlags(in.BlockedSponsorFlags); err != nil {
		return OfferingView{}, err
	}
	if err := checkOffering(row, now); err != nil {
		return OfferingView{}, err
	}
	if row.DefaultPayeeID == nil && kind == SellerOrg && s.d.Payees != nil {
		if py, found, err := s.d.Payees.DefaultFor(ctx, o.ID); err != nil {
			return OfferingView{}, err
		} else if found {
			row.DefaultPayeeID = &py.ID
		}
	}
	hash, err := idempotency.Hash(struct {
		Op  string
		Org uuid.UUID
		In  OfferingInput
	}{"create_offering", o.ID, in})
	if err != nil {
		return OfferingView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		sv, err := s.d.Store.GetService(ctx, row.ServiceID, false)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return uuid.Nil(), problem.Field("body.service_id", "names no service")
			}
			return uuid.Nil(), err
		}
		if err := s.checkParents(ctx, &row, sv); err != nil {
			return uuid.Nil(), err
		}
		row.VATRateBP = sv.DefaultVATRateBP
		stored, err := s.d.Store.InsertOffering(ctx, row)
		if err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.d.Audit.Log(ctx, offeringEvent(a, authz.CatalogOfferingCreate, stored, "", map[string]any{
			"service_id": stored.ServiceID.String(), "seller_kind": string(stored.SellerKind),
			"unit_price_amount": stored.UnitPriceAmount, "vat_rate_bp": stored.VATRateBP,
		}))
	})
	if err != nil {
		return OfferingView{}, err
	}
	return s.reload(ctx, a, id)
}

// checkParents checks the parents of a draft: an active service (a
// platform-native one is the platform's only), an event of the seller that
// is still open, a payee of the seller that is not closed, the university
// approval only for a club with a university.
func (s *service) checkParents(ctx context.Context, o *Offering, sv ServiceItem) error {
	if !sv.Active {
		return problem.Field("body.service_id", "names an inactive service")
	}
	if sv.FulfillmentType == FulfillPlatformNative && o.SellerKind != SellerPlatform {
		return problem.Field("body.service_id", "a platform-native service is sold by the platform only")
	}
	if o.EventID != nil {
		if s.d.Events == nil {
			return ErrUnavailable
		}
		e, err := s.d.Events.Get(ctx, *o.EventID)
		switch {
		case isEventNotFound(err):
			return problem.Field("body.event_id", "names no event of this organization")
		case err != nil:
			return err
		case e.OrgID != o.OrgID:
			return ErrParentMismatch.WithFields(fieldError("body.event_id", "is an event of another organization"))
		case !openEvent(e):
			return problem.Field("body.event_id", "names a cancelled or completed event")
		}
	}
	if o.DefaultPayeeID != nil {
		if o.SellerKind == SellerPlatform {
			return problem.Field("body.default_payee_id", "the platform sells without a payee")
		}
		if s.d.Payees == nil {
			return ErrUnavailable
		}
		py, err := s.d.Payees.Get(ctx, *o.DefaultPayeeID)
		switch {
		case errors.Is(err, parties.ErrNotFound):
			return problem.Field("body.default_payee_id", "names no payee of this organization")
		case err != nil:
			return err
		case py.OrgID != o.OrgID:
			return ErrParentMismatch.WithFields(fieldError("body.default_payee_id", "is a payee of another organization"))
		case py.Status == parties.PayeeClosed:
			return problem.Field("body.default_payee_id", "names a closed payee")
		}
	}
	if o.RequiresUniversityApproval {
		f, err := s.universityOf(ctx, o.OrgID)
		if err != nil {
			return err
		}
		if f.universityOrgID == uuid.Nil() {
			return problem.Field("body.requires_university_approval", "only a club of a university needs its approval")
		}
	}
	return nil
}

// descriptionOf validates the Tiptap description (nil: none).
func descriptionOf(m map[string]any) (JSON, error) {
	if m == nil {
		return nil, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, problem.Field("body.description_json", "must be a Tiptap document")
	}
	doc, err := sanitizeDescription(raw)
	if err != nil {
		return nil, problem.Field("body.description_json", "must be a Tiptap document of paragraphs, headings 2-3, lists, quotes, bold, italic and https links")
	}
	return doc, nil
}

// UpdateOffering implements Service: drafts only, If-Match (04 §2.14).
func (s *service) UpdateOffering(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p OfferingPatch) (OfferingView, error) {
	cur, err := s.authorized(ctx, a, authz.CatalogOfferingUpdate, id)
	if err != nil {
		return OfferingView{}, err
	}
	if p.empty() {
		return OfferingView{}, ErrEmptyPatch
	}
	for _, c := range p.Clear {
		if !slices.Contains(offeringClearable, c) {
			return OfferingView{}, problem.Field("body.clear", "names a field that cannot be cleared")
		}
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.GetOffering(ctx, cur.ID, true)
		if err != nil {
			return err
		}
		if err := httpx.CheckIfMatch(ifMatch, httpx.ETag(cur.UpdatedAt)); err != nil {
			return err
		}
		if cur.Status != OfferingDraft {
			return ErrInvalidState
		}
		if err := s.sellerWritable(ctx, a, cur); err != nil {
			return err
		}
		next, fields, err := s.patchOffering(cur, p)
		if err != nil {
			return err
		}
		sv, err := s.d.Store.GetService(ctx, next.ServiceID, false)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return problem.Field("body.service_id", "names no service")
			}
			return err
		}
		if err := s.checkParents(ctx, &next, sv); err != nil {
			return err
		}
		if next.ServiceID != cur.ServiceID {
			next.VATRateBP = sv.DefaultVATRateBP
		}
		stored, err := s.d.Store.UpdateOffering(ctx, next, OfferingDraft)
		if err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, offeringEvent(a, authz.CatalogOfferingUpdate, stored, "", map[string]any{"fields": fields}))
	})
	if err != nil {
		return OfferingView{}, err
	}
	return s.reload(ctx, a, id)
}

// patchOffering applies p to cur and returns the changed field names.
func (s *service) patchOffering(cur Offering, p OfferingPatch) (Offering, []string, error) {
	next, fields := cur, []string{}
	set := func(name string) { fields = append(fields, name) }
	var err error
	if p.ServiceID != nil {
		next.ServiceID = *p.ServiceID
		set("service_id")
	}
	if p.EventID != nil {
		next.EventID = p.EventID
		set("event_id")
	}
	if p.DefaultPayeeID != nil {
		next.DefaultPayeeID = p.DefaultPayeeID
		set("default_payee_id")
	}
	if p.Title != nil {
		if next.Title, err = cleanText("body.title", *p.Title, minTitle, maxTitle); err != nil {
			return Offering{}, nil, err
		}
		set("title")
	}
	if p.DescriptionJSON != nil {
		if next.DescriptionJSON, err = descriptionOf(p.DescriptionJSON); err != nil {
			return Offering{}, nil, err
		}
		set("description_json")
	}
	if p.Deliverables != nil {
		if next.Deliverables, err = cleanDeliverables(*p.Deliverables); err != nil {
			return Offering{}, nil, err
		}
		set("deliverables")
	}
	if p.UnitPriceAmount != nil {
		next.UnitPriceAmount = *p.UnitPriceAmount
		set("unit_price_amount")
	}
	if p.QuantityTotal != nil {
		next.QuantityTotal = p.QuantityTotal
		set("quantity_total")
	}
	if p.PerBuyerLimit != nil {
		next.PerBuyerLimit = p.PerBuyerLimit
		set("per_buyer_limit")
	}
	if p.SalesStartAt != nil {
		next.SalesStartAt = utc(p.SalesStartAt)
		set("sales_start_at")
	}
	if p.SalesEndAt != nil {
		next.SalesEndAt = utc(p.SalesEndAt)
		set("sales_end_at")
	}
	if p.FulfilBy != nil {
		next.FulfilBy = utc(p.FulfilBy)
		set("fulfil_by")
	}
	if p.InvoiceTrigger != nil {
		next.InvoiceTrigger = *p.InvoiceTrigger
		set("invoice_trigger")
	}
	if p.BlockedSponsorFlags != nil {
		if next.BlockedSponsorFlags, err = cleanFlags(*p.BlockedSponsorFlags); err != nil {
			return Offering{}, nil, err
		}
		set("blocked_sponsor_flags")
	}
	if p.RequiresUniversityApproval != nil {
		next.RequiresUniversityApproval = *p.RequiresUniversityApproval
		set("requires_university_approval")
	}
	for _, c := range p.Clear {
		switch c {
		case "event_id":
			next.EventID = nil
		case "default_payee_id":
			next.DefaultPayeeID = nil
		case "description_json":
			next.DescriptionJSON = nil
		case "quantity_total":
			next.QuantityTotal = nil
		case "per_buyer_limit":
			next.PerBuyerLimit = nil
		case "sales_start_at":
			next.SalesStartAt = nil
		case "sales_end_at":
			next.SalesEndAt = nil
		case "fulfil_by":
			next.FulfilBy = nil
		}
		set(c)
	}
	// An edited draft has no university decision (000099: pending while required).
	next.UniversityApprovalStatus = undecided(next.RequiresUniversityApproval)
	next.UniversityApprovedBy, next.UniversityApprovedAt = nil, nil
	return next, fields, checkOffering(next, s.now())
}

// SubmitOffering implements Service: draft → pending_approval. The
// offering must be sellable (checkSellable: BE-037's active payee with an
// approved IBAN, 409 parties.no_approved_account); its VAT rate is
// refreshed from the service; a university's approval is requested when
// required (T-21). offering.submitted (A) notifies the approvers.
func (s *service) SubmitOffering(ctx context.Context, a authz.Actor, id uuid.UUID) (OfferingView, error) {
	if _, err := s.authorized(ctx, a, authz.CatalogOfferingSubmit, id); err != nil {
		return OfferingView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Offering uuid.UUID
	}{"submit_offering", id})
	if err != nil {
		return OfferingView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetOffering(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != OfferingDraft {
			return uuid.Nil(), ErrInvalidState
		}
		if err := s.sellerWritable(ctx, a, cur); err != nil {
			return uuid.Nil(), err
		}
		sv, err := s.d.Store.GetService(ctx, cur.ServiceID, false)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.checkSellable(ctx, cur, sv); err != nil {
			return uuid.Nil(), err
		}
		now := s.now()
		next := cur
		next.Status, next.SubmittedAt, next.VATRateBP = OfferingPendingApproval, &now, sv.DefaultVATRateBP
		next.ApprovedBy, next.ApprovedAt, next.RejectionReason = nil, nil, nil
		next.UniversityApprovalStatus = undecided(next.RequiresUniversityApproval)
		next.UniversityApprovedBy, next.UniversityApprovedAt = nil, nil
		stored, err := s.d.Store.UpdateOffering(ctx, next, OfferingDraft)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, offeringEvent(a, authz.CatalogOfferingSubmit, stored, "", map[string]any{
			"vat_rate_bp": stored.VATRateBP, "requires_university_approval": stored.RequiresUniversityApproval,
		})); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.emitAfterCommit(ctx, stored, domainevent.OfferingSubmitted{OfferingID: stored.ID, OrgID: stored.OrgID})
	})
	if err != nil {
		return OfferingView{}, err
	}
	return s.reload(ctx, a, id)
}

// CancelOffering implements Service: any status but cancelled → cancelled
// with the reason (shown to the seller; no personal data). offering.cancelled
// (O) lets orders cancel the unpaid orders and queue the paid ones for
// refund (BE-040).
func (s *service) CancelOffering(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (OfferingView, error) {
	if _, err := s.authorized(ctx, a, authz.CatalogOfferingCancel, id); err != nil {
		return OfferingView{}, err
	}
	why, err := cleanReason("body.cancel_reason", reason, true)
	if err != nil {
		return OfferingView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Offering uuid.UUID
		Reason   string
	}{"cancel_offering", id, *why})
	if err != nil {
		return OfferingView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetOffering(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		code := CancelBySeller
		if !a.HasOrgRole(cur.OrgID, authz.MemberClubAdmin, authz.MemberUniversityRep) {
			code = CancelByPlatform
		}
		return cur.ID, s.cancel(ctx, a, cur, why, code)
	})
	if err != nil {
		return OfferingView{}, err
	}
	return s.reload(ctx, a, id)
}

// cancel moves o to cancelled in the transaction carried by ctx, audits it
// (the reason as the audit reason) and writes offering.cancelled (O).
func (s *service) cancel(ctx context.Context, a authz.Actor, cur Offering, why *string, code string) error {
	if cur.Status == OfferingCancelled {
		return ErrInvalidState
	}
	now := s.now()
	next := cur
	next.Status, next.CancelledAt, next.CancelReason = OfferingCancelled, &now, why
	stored, err := s.d.Store.UpdateOffering(ctx, next, cur.Status)
	if err != nil {
		return err
	}
	reason := ""
	if why != nil {
		reason = *why
	}
	if err := s.d.Audit.Log(ctx, offeringEvent(a, authz.CatalogOfferingCancel, stored, reason, map[string]any{
		"from": string(cur.Status), "cause": code,
	})); err != nil {
		return err
	}
	return s.emitOutbox(ctx, stored, domainevent.OfferingCancelled{OfferingID: stored.ID, OrgID: stored.OrgID, Reason: code})
}

// authorized reads an offering for an action on the resource surface: the
// offering (404), its university (authz Attrs), then authz.Can (404 when
// invisible, 403 without the grant).
func (s *service) authorized(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (Offering, error) {
	o, err := s.d.Store.GetOffering(ctx, id, false)
	if err != nil {
		return Offering{}, err
	}
	f, err := s.facts(ctx, o)
	if err != nil {
		return Offering{}, err
	}
	if err := authz.Can(ctx, a, act, offeringResource(o, f)); err != nil {
		return Offering{}, err
	}
	return o, nil
}

// sellerWritable applies the seller org's status (03 §2.3) to a write.
func (s *service) sellerWritable(ctx context.Context, a authz.Actor, o Offering) error {
	org, err := s.d.Orgs.Get(ctx, o.OrgID)
	if err != nil {
		if errors.Is(err, orgs.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return writableOrg(a, org)
}

// reload returns the view of an offering after a write.
func (s *service) reload(ctx context.Context, a authz.Actor, id uuid.UUID) (OfferingView, error) {
	o, err := s.d.Store.GetOffering(ctx, id, false)
	if err != nil {
		return OfferingView{}, err
	}
	return s.view(ctx, a, o)
}

// handleEventCancelled cancels the offerings bound to a cancelled event
// (01 §6.2: event.cancelled → catalog) as the system: each gets
// offering.cancelled (O, reason event_cancelled), so orders refunds or
// cancels their orders. Idempotent: a cancelled offering is skipped; an
// event that is not cancelled (a stale or forged payload) changes nothing.
func (s *service) handleEventCancelled(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.EventCancelled](env)
	if err != nil {
		return queue.Permanent(err)
	}
	if s.d.Events != nil {
		e, err := s.d.Events.Get(ctx, p.EventID)
		switch {
		case isEventNotFound(err):
			return nil
		case err != nil:
			return err
		case e.OrgID != p.OrgID || e.Status != events.StatusCancelled:
			return nil
		}
	}
	sys := authz.SystemActor(cancelEventOfferings)
	why := "event_cancelled"
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		// Every live offering of the event in one transaction: an event has a
		// handful of packages, and a partial cancellation must never leave one
		// on sale for a cancelled event.
		os, err := s.d.Store.LiveEventOfferings(ctx, p.OrgID, p.EventID)
		if err != nil {
			return err
		}
		for _, o := range os {
			if err := s.cancel(ctx, sys, o, &why, CancelEventCancelled); err != nil {
				return err
			}
		}
		return nil
	})
}

// undecided is the university approval state of an offering without a
// decision: pending when the university must approve it (000099
// offerings_university_required_chk), not_required otherwise.
func undecided(required bool) UniversityApproval {
	if required {
		return UniPending
	}
	return UniNotRequired
}
