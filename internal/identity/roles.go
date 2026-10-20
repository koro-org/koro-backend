package identity

import (
	"context"
	"log/slog"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Platform role assignment with four eyes (06 H-17, 02 §5.7, 03 §5.1 k3).
//
//	privileged roles (platform_admin, finance_admin, talent_auditor):
//	  grant   admin A: adminGrantPlatformRole → pending (requested_by A)
//	          admin B ≠ A: adminApprovePlatformRole → active (approved_by B)
//	  revoke  admin A: adminRevokePlatformRole → revoke_pending (revoke_requested_by A)
//	          admin B ≠ A: adminRevokePlatformRole again → revoked (revoked_by B)
//	moderator, support, dpo: one admin grants (active) and revokes (revoked).
//	a pending request: adminRevokePlatformRole rejects or withdraws it (rejected).
//
// The rule is enforced twice: authz (Attrs requested_by → 403
// authz.four_eyes_violation, OwnerUserID = target → Y6 403) and the DB
// CHECKs (store → the same problem codes). A role is in effect only while
// its row is active or revoke_pending (06 H-33): a pending request grants
// nothing. Every change that puts a role in or out of effect closes the
// target's sessions (role_change, 03 §2.2-6); every step alerts all
// platform admins. Decisions run in serializable transactions, so the
// talent_auditor limit and the last-admin rule hold under concurrency.

// RoleGrant is the body of adminGrantPlatformRole.
type RoleGrant struct {
	UserID uuid.UUID
	Role   authz.PlatformRole
	Reason string
	Proof  ReauthProof
}

// String keeps the proof out of %v.
func (g RoleGrant) String() string { return "identity.RoleGrant{" + string(g.Role) + "}" }

// RolePage is one page of adminListPlatformRoles with the names of the
// users it mentions.
type RolePage struct {
	Items []RoleAssignment
	Users map[uuid.UUID]UserSummary
	More  bool
}

// talentAuditorMax is 03 §1.1 row 16 ("en çok 2 aktif").
const talentAuditorMax = 2

// roleResource is the authz view of an assignment: the target user owns
// it (Y6) and requestedBy is the user whose request the actor would
// complete (four eyes; nil on a first step).
func roleResource(id, target uuid.UUID, role authz.PlatformRole, requestedBy *uuid.UUID) authz.Resource {
	t := target
	req := ""
	if requestedBy != nil {
		req = requestedBy.String()
	}
	return authz.Resource{Type: authz.TypePlatformRoleAssignment, ID: id, OwnerUserID: &t,
		Attrs: map[string]string{"role": string(role), authz.AttrRequestedBy: req}}
}

// roleGate is the decision taken before an assignment is read (03 §3.2:
// a caller without the role learns nothing about the ID): the target is
// not known yet, so it is nobody (the nil UUID), and there is no earlier
// step. The real resource is decided again once the row is locked.
func roleGate(id uuid.UUID) authz.Resource { return roleResource(id, uuid.Nil(), "", nil) }

// PlatformRoleAssignments implements Service (adminListPlatformRoles).
func (s *service) PlatformRoleAssignments(ctx context.Context, a authz.Actor, q RoleQuery) (RolePage, error) {
	if err := authz.Can(ctx, a, authz.IdentityPlatformRoleList, adminSurface); err != nil {
		return RolePage{}, err
	}
	if q.Role != "" && !validPlatformRole(q.Role) {
		return RolePage{}, invalid(problem.ErrValidation, "query.role", "must be a platform role")
	}
	limit := page.ClampLimit(q.Limit)
	q.Limit = limit + 1
	rows, err := s.d.Store.RoleAssignments(ctx, q)
	if err != nil {
		return RolePage{}, err
	}
	items, more := page.Cut(rows, limit)
	var ids []uuid.UUID
	for _, r := range items {
		ids = append(ids, r.UserID)
	}
	users, err := s.GetMany(ctx, ids)
	if err != nil {
		return RolePage{}, err
	}
	return RolePage{Items: items, Users: users, More: more}, nil
}

// GrantPlatformRole implements Service (adminGrantPlatformRole, R).
func (s *service) GrantPlatformRole(ctx context.Context, a authz.Actor, in RoleGrant) (RoleAssignment, error) {
	if err := authz.Can(ctx, a, authz.IdentityPlatformRoleGrant, roleResource(uuid.Nil(), in.UserID, in.Role, nil)); err != nil {
		return RoleAssignment{}, err
	}
	if !validPlatformRole(in.Role) {
		return RoleAssignment{}, invalid(problem.ErrValidation, "body.role", "must be a platform role")
	}
	reason, err := cleanReason(in.Reason, "body.reason")
	if err != nil {
		return RoleAssignment{}, err
	}
	kind, err := s.reauth(ctx, a, in.Proof, reauthDenied(authz.IdentityPlatformRoleGrant, resourceRole, nil, map[string]any{"step": "request"}))
	if err != nil {
		return RoleAssignment{}, err
	}
	var out RoleAssignment
	err = s.d.Tx.WithSerializableTx(ctx, func(ctx context.Context) error {
		u, err := s.d.Store.User(ctx, in.UserID)
		switch {
		case isNotFound(err):
			return invalid(problem.ErrValidation, "body.user_id", "unknown user")
		case err != nil:
			return err
		}
		if u.Status != StatusActive {
			return ErrUserInvalidState
		}
		if in.Role == authz.PlatformTalentAuditor {
			eff, pend, _, err := s.d.Store.CountRoleAssignments(ctx, in.Role)
			if err != nil {
				return err
			}
			if eff+pend >= talentAuditorMax {
				return ErrTalentAuditorLimit
			}
		}
		now, me := s.now(), a.UserID
		ra := RoleAssignment{ID: uuid.NewV7(), UserID: u.ID, Role: in.Role, Reason: reason, Status: RolePending,
			RequestedBy: &me, RequestedAt: now}
		alert := AlertRoleRequested
		if !privilegedRole(in.Role) { // moderator, support, dpo: one admin is enough (02 §5.7)
			ra.Status, ra.GrantedAt, alert = RoleActive, &now, AlertRoleGranted
		}
		if err := s.d.Store.InsertRoleAssignment(ctx, ra); err != nil {
			return err
		}
		n := 0
		if ra.Status == RoleActive {
			if n, err = s.d.Store.RevokeUserSessions(ctx, u.ID, nil, now, RevokeRoleChange); err != nil {
				return err
			}
		}
		out = ra
		s.alertAdmins(ctx, ra.ID, alert)
		return s.auditRole(ctx, a, authz.IdentityPlatformRoleGrant, ra, reason, map[string]any{
			audit.MetaReauth: kind, "result": string(ra.Status), "sessions_revoked": n})
	})
	if err != nil {
		return RoleAssignment{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "platform role granted or requested", slog.String("assignment_id", out.ID.String()),
		slog.String("role", string(out.Role)), slog.String("status", string(out.Status)), slog.String("by", a.UserID.String()))
	return out, nil
}

// ApprovePlatformRole implements Service (adminApprovePlatformRole, R; the
// second eye of 06 H-17, action identity.platform_role.grant).
func (s *service) ApprovePlatformRole(ctx context.Context, a authz.Actor, id uuid.UUID, proof ReauthProof) (RoleAssignment, error) {
	if err := authz.Can(ctx, a, authz.IdentityPlatformRoleGrant, roleGate(id)); err != nil {
		return RoleAssignment{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityPlatformRoleGrant, resourceRole, &id, map[string]any{"step": "approve"}))
	if err != nil {
		return RoleAssignment{}, err
	}
	var out RoleAssignment
	err = s.d.Tx.WithSerializableTx(ctx, func(ctx context.Context) error {
		ra, err := s.d.Store.LockRoleAssignment(ctx, id)
		if err != nil {
			return err
		}
		if err := authz.Can(ctx, a, authz.IdentityPlatformRoleGrant, roleResource(ra.ID, ra.UserID, ra.Role, ra.RequestedBy)); err != nil {
			return err
		}
		if ra.Status != RolePending {
			return ErrRoleInvalidState
		}
		u, err := s.d.Store.User(ctx, ra.UserID)
		if err != nil {
			return err
		}
		if u.Status != StatusActive {
			return ErrUserInvalidState
		}
		if ra.Role == authz.PlatformTalentAuditor {
			eff, _, _, err := s.d.Store.CountRoleAssignments(ctx, ra.Role)
			if err != nil {
				return err
			}
			if eff >= talentAuditorMax {
				return ErrTalentAuditorLimit
			}
		}
		now, me := s.now(), a.UserID
		ra.Status, ra.ApprovedBy, ra.ApprovedAt, ra.GrantedAt = RoleActive, &me, &now, &now
		if err := s.d.Store.UpdateRoleAssignment(ctx, ra); err != nil {
			return err
		}
		n, err := s.d.Store.RevokeUserSessions(ctx, ra.UserID, nil, now, RevokeRoleChange)
		if err != nil {
			return err
		}
		out = ra
		s.alertAdmins(ctx, ra.ID, AlertRoleGranted)
		meta := map[string]any{audit.MetaReauth: kind, "result": "approved", "sessions_revoked": n}
		if ra.RequestedBy != nil {
			meta["requested_by"] = ra.RequestedBy.String()
		}
		return s.auditRole(ctx, a, authz.IdentityPlatformRoleGrant, ra, "", meta)
	})
	if err != nil {
		return RoleAssignment{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "platform role approved", slog.String("assignment_id", id.String()),
		slog.String("role", string(out.Role)), slog.String("by", a.UserID.String()))
	return out, nil
}

// RevokePlatformRole implements Service (adminRevokePlatformRole, R).
func (s *service) RevokePlatformRole(ctx context.Context, a authz.Actor, id uuid.UUID, rawReason string, proof ReauthProof) (RoleAssignment, error) {
	if err := authz.Can(ctx, a, authz.IdentityPlatformRoleRevoke, roleGate(id)); err != nil {
		return RoleAssignment{}, err
	}
	reason, err := cleanReason(rawReason, "body.reason")
	if err != nil {
		return RoleAssignment{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityPlatformRoleRevoke, resourceRole, &id, nil))
	if err != nil {
		return RoleAssignment{}, err
	}
	var out RoleAssignment
	err = s.d.Tx.WithSerializableTx(ctx, func(ctx context.Context) error {
		ra, err := s.d.Store.LockRoleAssignment(ctx, id)
		if err != nil {
			return err
		}
		var earlier *uuid.UUID // four eyes: the revocation's requester
		if ra.Status == RoleRevokePending {
			earlier = ra.RevokeRequestedBy
		}
		if err := authz.Can(ctx, a, authz.IdentityPlatformRoleRevoke, roleResource(ra.ID, ra.UserID, ra.Role, earlier)); err != nil {
			return err
		}
		now, me := s.now(), a.UserID
		var alert string
		n := 0
		switch ra.Status {
		case RolePending: // a request is rejected or withdrawn: nothing was in effect
			ra.Status, ra.RejectedBy, ra.RejectedAt, alert = RoleRejected, &me, &now, AlertRoleRejected
		case RoleActive:
			if err := s.keepOneAdmin(ctx, ra, 1); err != nil {
				return err
			}
			ra.RevokeReason = reason
			if privilegedRole(ra.Role) {
				ra.Status, ra.RevokeRequestedBy, ra.RevokeRequestedAt, alert = RoleRevokePending, &me, &now, AlertRoleRevokeRequested
			} else {
				ra.Status, ra.RevokedBy, ra.RevokedAt, alert = RoleRevoked, &me, &now, AlertRoleRevoked
			}
		case RoleRevokePending:
			if err := s.keepOneAdmin(ctx, ra, 0); err != nil {
				return err
			}
			ra.Status, ra.RevokedBy, ra.RevokedAt, alert = RoleRevoked, &me, &now, AlertRoleRevoked
		default:
			return ErrRoleInvalidState
		}
		if err := s.d.Store.UpdateRoleAssignment(ctx, ra); err != nil {
			return err
		}
		if ra.Status == RoleRevoked {
			if n, err = s.d.Store.RevokeUserSessions(ctx, ra.UserID, nil, now, RevokeRoleChange); err != nil {
				return err
			}
		}
		out = ra
		s.alertAdmins(ctx, ra.ID, alert)
		meta := map[string]any{audit.MetaReauth: kind, "result": string(ra.Status), "sessions_revoked": n}
		if earlier != nil {
			meta["revoke_requested_by"] = earlier.String()
		}
		return s.auditRole(ctx, a, authz.IdentityPlatformRoleRevoke, ra, reason, meta)
	})
	if err != nil {
		return RoleAssignment{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "platform role revocation step", slog.String("assignment_id", id.String()),
		slog.String("role", string(out.Role)), slog.String("status", string(out.Status)), slog.String("by", a.UserID.String()))
	return out, nil
}

// keepOneAdmin refuses a revocation step of platform_admin that would leave
// no admin in effect who is not on the way out (03 k3: the last active
// platform_admin cannot be revoked). leaving is how many more admins this
// step takes out of the "active, not revoke_pending" set (1 for a new
// revocation request, 0 when a pending one is confirmed).
func (s *service) keepOneAdmin(ctx context.Context, ra RoleAssignment, leaving int) error {
	if ra.Role != authz.PlatformAdmin {
		return nil
	}
	eff, _, revokePending, err := s.d.Store.CountRoleAssignments(ctx, authz.PlatformAdmin)
	if err != nil {
		return err
	}
	if eff-revokePending-leaving < 1 {
		return ErrLastPlatformAdmin
	}
	return nil
}

// auditRole writes the audit event of a role step (metadata: role, target).
func (s *service) auditRole(ctx context.Context, a authz.Actor, act authz.Action, ra RoleAssignment, reason string, meta map[string]any) error {
	meta["role"] = string(ra.Role)
	meta[audit.MetaSubjectUserID] = ra.UserID.String()
	id := ra.ID
	return s.d.Audit.Log(ctx, audit.Event{Action: string(act), Outcome: audit.Success, ResourceType: resourceRole,
		ResourceID: &id, Reason: reason, Metadata: meta}.By(a))
}
