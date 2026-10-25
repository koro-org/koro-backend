package catalog

import (
	"context"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
)

// Outcomes of offering.decided (01 §6.2: a code).
const (
	OutcomeApproved           = "approved"            // on sale
	OutcomeAwaitingUniversity = "awaiting_university" // the platform approved; the university has not decided yet
	OutcomeUniversityApproved = "university_approved" // the university approved; the platform has not decided yet
	OutcomeRejected           = "rejected"            // back to draft (platform)
	OutcomeUniversityRejected = "university_rejected" // back to draft (university)
)

// AdminDecideOffering implements Service (catalog.offering.decide, 03 §6.1
// "Teklif onayı"): a platform admin other than the creator approves or
// rejects an offering waiting for the platform decision. An approval puts
// it on sale (activate) unless the university still has to decide (T-21:
// it waits, 04 §2.14); a rejection sends it back to draft with the reason.
func (s *service) AdminDecideOffering(ctx context.Context, a authz.Actor, id uuid.UUID, d Decision) (OfferingView, error) {
	if err := authz.Can(ctx, a, authz.CatalogOfferingReviewQueue, adminSurface); err != nil {
		return OfferingView{}, err // 403 before reading (03 §7.2-3)
	}
	why, err := cleanReason("body.rejection_reason", d.Reason, !d.Approve)
	if err != nil {
		return OfferingView{}, err
	}
	if _, err := s.authorized(ctx, a, authz.CatalogOfferingDecide, id); err != nil {
		return OfferingView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Offering uuid.UUID
		D        Decision
	}{"decide_offering", id, d})
	if err != nil {
		return OfferingView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetOffering(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		// The row lock fixes the facts: authz again on the locked row.
		f, err := s.facts(ctx, cur)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := authz.Can(ctx, a, authz.CatalogOfferingDecide, offeringResource(cur, f)); err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != OfferingPendingApproval || cur.ApprovedBy != nil {
			return uuid.Nil(), ErrInvalidState
		}
		next, outcome := cur, OutcomeRejected
		if d.Approve {
			now := s.now()
			next.ApprovedBy, next.ApprovedAt = &a.UserID, &now
			outcome = OutcomeAwaitingUniversity
			if !cur.RequiresUniversityApproval || cur.UniversityApprovalStatus == UniApproved {
				if err := s.activate(ctx, &next); err != nil {
					return uuid.Nil(), err
				}
				outcome = OutcomeApproved
			}
		} else {
			reject(&next, why)
		}
		return s.decided(ctx, a, authz.CatalogOfferingDecide, cur, next, outcome, why)
	})
	if err != nil {
		return OfferingView{}, err
	}
	return s.reload(ctx, a, id)
}

// DecideUniversityApproval implements Service (catalog.offering.university_
// decide, T-21): the university_rep of the seller club's university
// approves or rejects an offering that requires it. An approval puts the
// offering on sale when the platform has approved it too; a rejection
// sends it back to draft (the platform approval is void).
func (s *service) DecideUniversityApproval(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, d Decision) (OfferingView, error) {
	uni, err := s.org(ctx, orgID)
	if err != nil {
		return OfferingView{}, err
	}
	// The gate: the university's surface, before the offering is read.
	if err := authz.Can(ctx, a, authz.CatalogOfferingUniversityDecide, authz.OrgResource(uni.ID, string(uni.Kind), string(uni.Status))); err != nil {
		return OfferingView{}, err
	}
	why, err := cleanReason("body.note", d.Reason, false)
	if err != nil {
		return OfferingView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Org      uuid.UUID
		Offering uuid.UUID
		D        Decision
	}{"decide_university_offering", orgID, id, d})
	if err != nil {
		return OfferingView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetOffering(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		f, err := s.facts(ctx, cur)
		if err != nil {
			return uuid.Nil(), err
		}
		if f.universityOrgID != uni.ID || !cur.RequiresUniversityApproval {
			return uuid.Nil(), ErrNotFound // not one of this university's offerings (06 H-38)
		}
		if err := authz.Can(ctx, a, authz.CatalogOfferingUniversityDecide, offeringResource(cur, f)); err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != OfferingPendingApproval || cur.UniversityApprovalStatus != UniPending {
			return uuid.Nil(), ErrInvalidState
		}
		now := s.now()
		next := cur
		next.UniversityApprovedBy, next.UniversityApprovedAt = &a.UserID, &now
		outcome := OutcomeUniversityRejected
		if d.Approve {
			next.UniversityApprovalStatus, outcome = UniApproved, OutcomeUniversityApproved
			if cur.ApprovedBy != nil {
				if err := s.activate(ctx, &next); err != nil {
					return uuid.Nil(), err
				}
				outcome = OutcomeApproved
			}
		} else {
			next.UniversityApprovalStatus = UniRejected
			reject(&next, why)
		}
		return s.decided(ctx, a, authz.CatalogOfferingUniversityDecide, cur, next, outcome, why)
	})
	if err != nil {
		return OfferingView{}, err
	}
	return s.reload(ctx, a, id)
}

// activate puts an approved offering on sale after the checks of
// submission hold again (the payee may have lost its account meanwhile)
// and a commerce policy applies (T-10: nothing is sold without one).
func (s *service) activate(ctx context.Context, o *Offering) error {
	sv, err := s.d.Store.GetService(ctx, o.ServiceID, false)
	if err != nil {
		return err
	}
	if err := s.checkSellable(ctx, *o, sv); err != nil {
		return err
	}
	if _, ok, err := s.d.Store.PolicyAt(ctx, o.ID, o.ServiceID, o.OrgID, s.now()); err != nil {
		return err
	} else if !ok {
		return ErrNoPolicy
	}
	o.Status = OfferingActive
	if o.QuantityTotal != nil && o.QuantitySold >= *o.QuantityTotal {
		o.Status = OfferingSoldOut
	}
	return nil
}

// reject sends an offering back to draft with the reason: every approval
// is void (the seller edits and submits again).
func reject(o *Offering, why *string) {
	o.Status, o.RejectionReason = OfferingDraft, why
	o.ApprovedBy, o.ApprovedAt = nil, nil
}

// decided writes a decision: the row (still pending_approval when read),
// the audit record and offering.decided (A).
func (s *service) decided(ctx context.Context, a authz.Actor, act authz.Action, cur, next Offering, outcome string, why *string) (uuid.UUID, error) {
	stored, err := s.d.Store.UpdateOffering(ctx, next, cur.Status)
	if err != nil {
		return uuid.Nil(), err
	}
	reason := ""
	if why != nil {
		reason = *why
	}
	if err := s.d.Audit.Log(ctx, offeringEvent(a, act, stored, reason, map[string]any{"outcome": outcome})); err != nil {
		return uuid.Nil(), err
	}
	return stored.ID, s.emitAfterCommit(ctx, stored, domainevent.OfferingDecided{OfferingID: stored.ID, OrgID: stored.OrgID,
		Outcome: outcome})
}
