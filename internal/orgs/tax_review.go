package orgs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// The review of a company's new tax identifier (BE-206; user decision Q-55,
// T-415). BE-183 sent an active company back to pending_review when a member
// replaced its tax identifier (public profile, new orders and messages
// stopped). Now the company stays active: updateOrg flags it
// (companies.tax_id_review_requested_at) and writes org.review_requested
// (the worker tells the platform admins of what the org awaits: an active
// company is not a registration), and a platform admin decides:
//
//	adminApproveTaxIdChange  orgs.org.verify (pad)     the flag goes, nothing else changes
//	adminRejectTaxIdChange   orgs.org.suspend (pad·R)  the flag goes and the company is suspended
//
// Both read the company as the admin saw it: If-Match is the org's ETag,
// which every updateOrg changes, so a newer identifier is never decided
// unseen (412). No four eyes: the review is not a money movement (06 H-60:
// document reviews take one admin), and both actions are the single-admin
// cells of 03. Neither the old nor the new number is copied anywhere: the
// flag is a time, the audit records name no number.

// The tax_id_review values of the audit metadata.
const (
	taxReviewRequested = "requested" // updateOrg: the company is flagged now
	taxReviewPending   = "pending"   // updateOrg: another new identifier while flagged
	taxReviewApproved  = "approved"
	taxReviewRejected  = "rejected"
)

// Notice templates of the tax identifier review (internal/notify/templates/orgs).
const (
	tmplTaxIDReviewRequested = "orgs.tax_id_review_requested" // platform admins: in-app
	tmplTaxIDApproved        = "orgs.tax_id_approved"         // the company's admins: in-app
	tmplTaxIDRejected        = "orgs.tax_id_rejected"         // the company's admins: in-app + e-mail
)

// AdminApproveTaxIDChange implements Service: authz (403 before reading) →
// note → in one transaction: the pending review (409 orgs.invalid_state),
// If-Match (428/412), the flag cleared, org.review_decided (tax_id_approved),
// audit. The company keeps its status and verification.
func (s *service) AdminApproveTaxIDChange(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch, note string) (OrgView, error) {
	if err := authz.Can(ctx, a, authz.OrgsOrgVerify, adminSurface); err != nil {
		return OrgView{}, err
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxReason {
		return OrgView{}, problem.Field("body.note", "must be at most 500 characters")
	}
	return s.decideTaxID(ctx, a, id, ifMatch, taxDecision{
		action: authz.OrgsOrgVerify, outcome: taxReviewApproved, event: domainevent.OrgReviewTaxIDApproved, reason: note,
	})
}

// AdminRejectTaxIDChange implements Service (R): authz (403 before reading)
// → reason → the pending review (409) and If-Match (428/412) → the proof,
// outside the transaction (its denied record must outlive the refusal) → in
// one transaction: both checks again, the flag cleared, an active company
// suspended (org.updated), org.review_decided (tax_id_rejected), audit with
// the reason. A suspended company stays suspended. The reason is the staff's
// (audit only): the company's notice says what happened, not why.
func (s *service) AdminRejectTaxIDChange(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch, reason string,
	proof reauthProof) (OrgView, error) {
	if err := authz.Can(ctx, a, authz.OrgsOrgSuspend, adminSurface); err != nil {
		return OrgView{}, err
	}
	reason, err := cleanReason("body.reason", reason)
	if err != nil {
		return OrgView{}, err
	}
	o, err := s.orgs().GetOrg(ctx, id)
	if err != nil {
		return OrgView{}, err
	}
	if _, err := s.pendingTaxReview(ctx, o, ifMatch); err != nil {
		return OrgView{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.OrgsOrgSuspend, o.ID, true,
		map[string]any{"tax_id_review": taxReviewRejected}))
	if err != nil {
		return OrgView{}, err
	}
	return s.decideTaxID(ctx, a, id, ifMatch, taxDecision{
		action: authz.OrgsOrgSuspend, outcome: taxReviewRejected, event: domainevent.OrgReviewTaxIDRejected, reason: reason,
		reauth: kind,
	})
}

// taxDecision is one decision on a pending tax identifier review.
type taxDecision struct {
	action  authz.Action
	outcome string // taxReviewApproved | taxReviewRejected
	event   string // the org.review_decided outcome
	reason  string // the audit reason: the approval's note, the rejection's reason
	reauth  string // "" without re-authentication
}

// decideTaxID writes a decision in one transaction (idempotency.Do: a
// replayed Idempotency-Key returns the first answer).
func (s *service) decideTaxID(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, d taxDecision) (OrgView, error) {
	hash, err := idempotency.Hash(struct {
		Op              string
		Org             uuid.UUID
		IfMatch, Reason string
	}{"tax_id_" + d.outcome, id, ifMatch, d.reason})
	if err != nil {
		return OrgView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		o, err := s.orgs().LockOrg(ctx, id)
		if err != nil {
			return uuid.Nil(), err
		}
		c, err := s.pendingTaxReview(ctx, o, ifMatch)
		if err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{"tax_id_review": d.outcome, "waited_seconds": waited(*c.TaxIDReviewRequestedAt, s.now())}
		c.TaxIDReviewRequestedAt = nil
		if _, err := s.companies().UpdateCompany(ctx, c); err != nil {
			return uuid.Nil(), err
		}
		if d.outcome == taxReviewRejected && o.Status == StatusActive {
			prev := o.Status
			o.Status = StatusSuspended
			if _, err := s.orgs().UpdateOrg(ctx, o); err != nil {
				return uuid.Nil(), err
			}
			if err := s.emitOrgUpdated(ctx, o, []string{"status"}); err != nil {
				return uuid.Nil(), err
			}
			meta["from"], meta["to"] = string(prev), string(o.Status)
		}
		if err := s.emitReviewDecided(ctx, o, d.event); err != nil {
			return uuid.Nil(), err
		}
		if d.reauth != "" {
			meta["reauth"] = d.reauth
		}
		return o.ID, s.d.Audit.Log(ctx, auditEvent(a, d.action, o.ID, d.reason, meta))
	})
	if err != nil {
		return OrgView{}, err
	}
	v, err := s.reload(ctx, id)
	v.FullTaxNo = fullTaxNo(a) // the reviewer sees the number decided on
	return v, err
}

// pendingTaxReview returns the company profile of o while its new tax
// identifier waits for a decision; otherwise (no company, no flag, a closed
// org) 409 orgs.invalid_state. Then If-Match: the org as the admin saw it
// (428 missing, 412 stale).
func (s *service) pendingTaxReview(ctx context.Context, o Org, ifMatch string) (Company, error) {
	if o.Kind != KindCompany {
		return Company{}, ErrInvalidState
	}
	c, err := s.companies().GetCompany(ctx, o.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Company{}, ErrInvalidState
	case err != nil:
		return Company{}, err
	case !c.TaxIDReviewPending(o.Status):
		return Company{}, ErrInvalidState
	}
	return c, httpx.CheckIfMatch(ifMatch, httpx.ETag(o.UpdatedAt))
}

// waited is the whole seconds a review waited (audit: the queue's latency,
// never negative).
func waited(since, now time.Time) int64 { return max(int64(now.Sub(since)/time.Second), 0) }

// notifyTaxIDReview is orgs.notify_review_requested of an org past its
// registration: while its new tax identifier waits, one in-app notice per
// platform admin, linking the admin list of the companies awaiting a tax
// identifier review; a review decided meanwhile (or none) needs none. The
// dedupe keys hold the review (the org and the flag's time), not the event:
// a registration's event delivered after the flag finds the same review and
// sends nothing twice.
func (s *service) notifyTaxIDReview(ctx context.Context, o Org) error {
	at, err := s.taxReviewSince(ctx, o)
	if err != nil || at == nil {
		return err
	}
	if s.d.Sender == nil || s.d.PlatformAdmins == nil {
		return errors.New("orgs: the review notices are not wired")
	}
	admins, err := s.d.PlatformAdmins(ctx)
	if err != nil {
		return fmt.Errorf("orgs: platform admins: %w", err)
	}
	key := fmt.Sprintf("%s:%s:%d", tmplTaxIDReviewRequested, o.ID, at.UnixMicro())
	return s.notifyKeyed(ctx, key, o, tmplTaxIDReviewRequested, admins, notify.AdminTaxIDReviewsLink(), nil)
}

// taxReviewSince is the time o's new tax identifier waits since, nil when
// nothing waits.
func (s *service) taxReviewSince(ctx context.Context, o Org) (*time.Time, error) {
	if o.Kind != KindCompany {
		return nil, nil
	}
	c, err := s.companies().GetCompany(ctx, o.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case !c.TaxIDReviewPending(o.Status):
		return nil, nil
	}
	return c.TaxIDReviewRequestedAt, nil
}

// notifyTaxIDDecided tells the company's active admins (adminRoles) of a
// decision on its new tax identifier, linking its panel: an approval in-app
// while the org is open, a rejection in-app and by e-mail while the company
// is still suspended (a reactivation before the task ran makes the notice
// wrong). The staff's reason is never sent.
func (s *service) notifyTaxIDDecided(ctx context.Context, e domainevent.Envelope, o Org, outcome string) error {
	tmpl, mail := tmplTaxIDApproved, map[string]any(nil)
	switch outcome {
	case domainevent.OrgReviewTaxIDApproved:
		if o.Status == StatusClosed {
			return nil
		}
	case domainevent.OrgReviewTaxIDRejected:
		if o.Status != StatusSuspended {
			return nil
		}
		tmpl, mail = tmplTaxIDRejected, map[string]any{"org_name": o.Name}
	}
	to, err := s.activeAdmins(ctx, o)
	if err != nil {
		return err
	}
	return s.notifyEach(ctx, e, o, tmpl, to, notify.OrgLink(o.ID), mail)
}
