package orgs

import (
	"context"
	"strings"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/idempotency"
)

// AdminVerifyOrg implements Service: pending_review → active, verified_at
// and verified_by set (04 §2.2). Another status is 409 orgs.invalid_state.
func (s *service) AdminVerifyOrg(ctx context.Context, a authz.Actor, id uuid.UUID, note string) (OrgView, error) {
	if err := authz.Can(ctx, a, authz.OrgsOrgVerify, adminSurface); err != nil {
		return OrgView{}, err
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > maxReason {
		return OrgView{}, fieldErr("body.note", "must be at most 500 characters")
	}
	return s.transition(ctx, a, id, transition{
		op: "verify", action: authz.OrgsOrgVerify, from: StatusPendingReview, to: StatusActive, reason: note,
	})
}

// AdminSuspendOrg implements Service (R): active → suspended. The platform
// org is never suspended.
func (s *service) AdminSuspendOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof reauthProof) (OrgView, error) {
	return s.adminR(ctx, a, id, reason, proof, transition{op: "suspend", from: StatusActive, to: StatusSuspended})
}

// AdminReactivateOrg implements Service (R): suspended → active.
func (s *service) AdminReactivateOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof reauthProof) (OrgView, error) {
	return s.adminR(ctx, a, id, reason, proof, transition{op: "reactivate", from: StatusSuspended, to: StatusActive})
}

// adminR is the R flow of suspend/reactivate (03 §6.2): authz (403 before
// reading) → reason → re-authentication → the transition.
func (s *service) adminR(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof reauthProof, t transition) (OrgView, error) {
	if err := authz.Can(ctx, a, authz.OrgsOrgSuspend, adminSurface); err != nil {
		return OrgView{}, err
	}
	reason, err := cleanReason("body.reason", reason)
	if err != nil {
		return OrgView{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.OrgsOrgSuspend, id, false, map[string]any{"transition": t.op}))
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
		o, err := s.d.Store.LockOrg(ctx, id)
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
		if _, err := s.d.Store.UpdateOrg(ctx, o); err != nil {
			return uuid.Nil(), err
		}
		if err := s.emitOrgUpdated(ctx, o, []string{"status"}); err != nil {
			return uuid.Nil(), err
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
