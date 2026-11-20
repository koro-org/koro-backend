package orgs

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// The party rule of the staff decisions on an org (BE-208, Q-63; BE-212,
// Q-65): a platform admin who is a member of the org (any role) decides
// nothing about it. Every decision authorises twice: first the admin surface
// (403 before anything is read), then the org decided on (authz: the org
// resource of orgs.org.verify / orgs.org.suspend gives a member no grant),
// before any proof is asked for. The decisions:
//
//	adminVerifyOrg            orgs.org.verify   the registration's approval
//	closeOrg (the rejection)  orgs.org.verify   the registration's rejection: staff close a pending_review org
//	adminSuspendOrg           orgs.org.suspend
//	adminReactivateOrg        orgs.org.suspend
//	adminApproveTaxIdChange   orgs.org.verify   (tax_review.go)
//	adminRejectTaxIdChange    orgs.org.suspend  (tax_review.go)
//
// The party still sees the review queues and their notices (nothing in them
// is secret from a member); only the decision is refused.

// reasonOrgMember is the Reason of the denied record of the party rule
// (BE-208): the decider is a member of the org decided on.
const reasonOrgMember = "org_member"

// canDecide is the party rule of a staff decision on o (authz: the org
// resource of act). A platform admin who is a member of o (any role) is
// refused 403, recorded as denied in its own transaction (the refusal must
// stay on record) with o's scope: o was read (06 H-38). meta names the
// decision refused, never business data.
func (s *service) canDecide(ctx context.Context, a authz.Actor, act authz.Action, o Org, meta map[string]any) error {
	err := authz.Can(ctx, a, act, authz.OrgResource(o.ID, string(o.Kind), string(o.Status)))
	if err == nil || !errors.Is(err, authz.ErrForbidden) {
		return err
	}
	id := o.ID
	if lerr := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(act), Outcome: audit.Denied, OrgID: &id, ResourceType: resourceOrg, ResourceID: &id,
			Reason: reasonOrgMember, Metadata: meta,
		}.By(a))
	}); lerr != nil {
		return lerr
	}
	return err
}

// AdminVerifyOrg implements Service: authz (403 before reading) → note →
// the org (404) and the party rule (403, BE-212) → pending_review → active,
// verified_at and verified_by set (04 §2.2); org.review_decided (verified)
// tells the org's admins (BE-144). Another status is 409 orgs.invalid_state.
func (s *service) AdminVerifyOrg(ctx context.Context, a authz.Actor, id uuid.UUID, note string) (OrgView, error) {
	if err := authz.Can(ctx, a, authz.OrgsOrgVerify, adminSurface); err != nil {
		return OrgView{}, err
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxReason {
		return OrgView{}, problem.Field("body.note", "must be at most 500 characters")
	}
	o, err := s.orgs().GetOrg(ctx, id)
	if err != nil {
		return OrgView{}, err
	}
	t := transition{op: "verify", action: authz.OrgsOrgVerify, from: StatusPendingReview, to: StatusActive, reason: note}
	if err := s.canDecide(ctx, a, t.action, o, map[string]any{"transition": t.op}); err != nil {
		return OrgView{}, err
	}
	return s.transition(ctx, a, id, t)
}

// AdminSuspendOrg implements Service (R): active → suspended. The platform
// org is never suspended.
func (s *service) AdminSuspendOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof reauthProof) (OrgView, error) {
	return s.adminR(ctx, a, id, reason, proof, transition{op: "suspend", from: StatusActive, to: StatusSuspended})
}

// AdminReactivateOrg implements Service (R): suspended → active; a company's
// tax identifier rejection ends with the suspension (BE-208).
func (s *service) AdminReactivateOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof reauthProof) (OrgView, error) {
	return s.adminR(ctx, a, id, reason, proof, transition{op: "reactivate", from: StatusSuspended, to: StatusActive})
}

// adminR is the R flow of suspend/reactivate (03 §6.2): authz (403 before
// reading) → reason → the org (404) and the party rule (403, BE-212: no
// proof is asked of a party) → re-authentication → the transition.
func (s *service) adminR(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof reauthProof, t transition) (OrgView, error) {
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
	meta := map[string]any{"transition": t.op}
	if err := s.canDecide(ctx, a, authz.OrgsOrgSuspend, o, meta); err != nil {
		return OrgView{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.OrgsOrgSuspend, o.ID, meta))
	if err != nil {
		return OrgView{}, err
	}
	t.action, t.reason, t.reauth = authz.OrgsOrgSuspend, reason, kind
	return s.transition(ctx, a, id, t)
}

// transition is one admin status change.
type transition struct {
	op       string // audit metadata "transition"
	action   authz.Action
	from, to Status
	reason   string
	reauth   string // "" without re-authentication
}

func (s *service) transition(ctx context.Context, a authz.Actor, id uuid.UUID, t transition) (OrgView, error) {
	hash, err := idempotency.Hash(struct {
		Op     string
		Org    uuid.UUID
		Reason string
	}{t.op, id, t.reason})
	if err != nil {
		return OrgView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		o, err := s.orgs().LockOrg(ctx, id)
		if err != nil {
			return uuid.Nil(), err
		}
		if o.Status != t.from || o.Kind == KindPlatform {
			return uuid.Nil(), ErrInvalidState
		}
		prev := o.Status
		o.Status = t.to
		if t.to == StatusActive && t.from == StatusPendingReview {
			now, by := s.now(), a.UserID
			o.VerifiedAt, o.VerifiedBy = &now, &by
		}
		if _, err := s.orgs().UpdateOrg(ctx, o); err != nil {
			return uuid.Nil(), err
		}
		if err := s.emitOrgUpdated(ctx, o, []string{"status"}); err != nil {
			return uuid.Nil(), err
		}
		if t.from == StatusSuspended { // the suspension a tax identifier rejection explained ends (BE-208)
			if err := s.endTaxIDRejection(ctx, o); err != nil {
				return uuid.Nil(), err
			}
		}
		if t.from == StatusPendingReview && t.to == StatusActive {
			if err := s.emitReviewDecided(ctx, o, domainevent.OrgReviewVerified); err != nil {
				return uuid.Nil(), err
			}
		}
		meta := map[string]any{"transition": t.op, "from": string(prev), "to": string(t.to), "org_kind": string(o.Kind)}
		if t.reauth != "" {
			meta["reauth"] = t.reauth
		}
		return o.ID, s.d.Audit.Log(ctx, auditEvent(a, t.action, o.ID, t.reason, meta))
	})
	if err != nil {
		return OrgView{}, err
	}
	return s.reload(ctx, id)
}
