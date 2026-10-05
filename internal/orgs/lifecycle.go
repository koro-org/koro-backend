package orgs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Worker names (01 §7, §8: <module>.<verb>_<object>).
const (
	// SendInvitationEmailTaskName consumes org_invitation.created.
	SendInvitationEmailTaskName = "orgs.send_invitation_email"
	// EndLapsedAdminRolesTaskName consumes user.edu_verification_lapsed:
	// club_admin ends (verification_lapsed).
	EndLapsedAdminRolesTaskName = "orgs.end_lapsed_admin_roles"
	// EndGraduatedAdminRolesTaskName consumes student.graduated:
	// club_admin ends (graduated).
	EndGraduatedAdminRolesTaskName = "orgs.end_graduated_admin_roles"
	// PurgeSensitiveMembershipsTaskName consumes club.sensitivity_changed:
	// a club that became sensitive loses its ended memberships at once
	// (02 §7.3: membership_ended_sensitive = 0; T-208).
	PurgeSensitiveMembershipsTaskName = "orgs.purge_sensitive_memberships"
	// AdminSweepJobName ends club_admin roles without a valid student
	// verification (or of graduates): the safety net of the two events.
	AdminSweepJobName = "orgs.admin_sweep" // 01:00, default
	// InvitationsExpireJobName closes expired invitations; the closed ones
	// are deleted after their retention (org_invitation: 30 days) by the
	// sweeper (kvkk.retention_sweep, BE-057).
	InvitationsExpireJobName = "orgs.invitations_expire" // hourly :15, low

	// endRolesJob is the system actor of OrgAdmin.EndRoles called by other
	// modules (claim revocation).
	endRolesJob = "orgs.end_roles"

	sweepBatch = 1000 // rows per transaction (01 §7)
)

// clubAdminRoles are the roles that need a valid student verification
// (02 §7.3).
var clubAdminRoles = []authz.MemberRole{authz.MemberClubAdmin}

// Subscribe registers the outbox subscriptions of orgs (01 §6.2).
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicOrgInvitationCreated, SendInvitationEmailTaskName, m.svc.handleInvitationCreated,
		outbox.Queue(queue.Default))
	r.Subscribe(domainevent.TopicUserEduVerificationLapsed, EndLapsedAdminRolesTaskName, m.svc.handleEduLapsed,
		outbox.Queue(queue.Default))
	r.Subscribe(domainevent.TopicStudentGraduated, EndGraduatedAdminRolesTaskName, m.svc.handleGraduated,
		outbox.Queue(queue.Default))
	r.Subscribe(domainevent.TopicClubSensitivityChanged, PurgeSensitiveMembershipsTaskName, m.svc.handleSensitivityChanged,
		outbox.Queue(queue.Default))
}

// Jobs returns the scheduled jobs of 01 §7. The 6-month purge of ended
// memberships (01 §7 orgs.ended_memberships_purge) and the 30-day purge of
// closed invitations are the Sweeper's, run by kvkk.retention_sweep under
// the legal approval of their policies (BE-057); a sensitive club's
// membership is deleted when it ends.
func (m Module) Jobs() []queue.Job {
	return []queue.Job{
		{Name: AdminSweepJobName, Cron: "0 1 * * *", Queue: queue.Default, Unique: 23 * time.Hour,
			Handle: m.svc.adminSweep},
		{Name: InvitationsExpireJobName, Cron: "15 * * * *", Queue: queue.Low, Unique: 55 * time.Minute,
			Handle: m.svc.expireInvitations},
	}
}

// MembershipsOf implements OrgDirectory (hooks.memberships).
func (s *service) MembershipsOf(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error) {
	rows, err := s.d.Store.ActiveMembershipsOfUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]authz.Membership, 0, len(rows))
	for _, m := range rows {
		out = append(out, authz.Membership{OrgID: m.OrgID, OrgKind: m.OrgKind, Role: m.Role})
	}
	return out, nil
}

// HasRole implements OrgDirectory.
func (s *service) HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error) {
	ms, err := s.MembershipsOf(ctx, userID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(ms, func(m authz.Membership) bool {
		return m.OrgID == orgID && slices.Contains(roles, m.Role)
	}), nil
}

// ActiveMemberIDs implements OrgDirectory.
func (s *service) ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	return s.d.Store.ActiveMemberIDs(ctx, orgID)
}

// GrantRole implements OrgAdmin. It joins the caller's transaction.
func (s *service) GrantRole(ctx context.Context, orgID, userID uuid.UUID, role authz.MemberRole, _ string) error {
	if role == authz.MemberClubAdmin {
		ok, err := s.adultStudent(ctx, userID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrClubAdminNeedsAdultStudent
		}
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		o, err := s.d.Store.LockOrg(ctx, orgID)
		if err != nil {
			return err
		}
		if o.Status == StatusClosed {
			return ErrInvalidState
		}
		if err := checkNewRole(o.Kind, role); err != nil {
			return err
		}
		sens, err := s.sensitive(ctx, o.ID, o.Kind)
		if err != nil {
			return err
		}
		now := s.now()
		switch cur, err := s.d.Store.LockOpenMember(ctx, o.ID, userID); {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		case cur.Status == MemberActive && cur.Role == role:
			return nil
		default:
			if _, err := s.endMember(ctx, cur, EndRoleChanged, sens, now); err != nil {
				return err
			}
		}
		m, err := s.d.Store.InsertMember(ctx, Membership{
			ID: uuid.NewV7(), OrgID: o.ID, UserID: userID, Role: role, Status: MemberActive, Source: SourceClaim, JoinedAt: &now,
		})
		if err != nil {
			return err
		}
		s.emitStarted(ctx, m)
		return nil
	})
}

// EndRoles implements OrgAdmin.
func (s *service) EndRoles(ctx context.Context, userID uuid.UUID, roles []authz.MemberRole, reason EndReason) (int, error) {
	return s.endRoles(ctx, authz.SystemActor(endRolesJob), userID, roles, reason)
}

// endRoles ends userID's active memberships with one of roles in every org
// (system action orgs.member.end_role), in one transaction, each with an
// audit record of the system job; sensitive clubs' rows are deleted at
// once. The last-admin rule does not apply: the role must go.
func (s *service) endRoles(ctx context.Context, a authz.Actor, userID uuid.UUID, roles []authz.MemberRole, reason EndReason) (int, error) {
	if !reason.Valid() || len(roles) == 0 {
		return 0, fmt.Errorf("orgs: end roles: reason %q, %d roles: %w", reason, len(roles), ErrMemberState)
	}
	if err := authz.Can(ctx, a, authz.OrgsMemberEndRole, authz.Resource{Type: authz.TypeOrgMember}); err != nil {
		return 0, err
	}
	n := 0
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		rows, err := s.d.Store.LockUserActiveByRoles(ctx, userID, roles)
		if err != nil {
			return err
		}
		now := s.now()
		for _, m := range rows {
			sens, err := s.sensitive(ctx, m.OrgID, m.OrgKind)
			if err != nil {
				return err
			}
			e, err := s.endMember(ctx, m, reason, sens, now)
			if err != nil {
				return err
			}
			if err := s.d.Audit.Log(ctx, memberAudit(a, authz.OrgsMemberEndRole, e, sens, "",
				map[string]any{"end_reason": string(reason)})); err != nil {
				return err
			}
		}
		n = len(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// handleEduLapsed is orgs.end_lapsed_admin_roles. A verification renewed
// meanwhile keeps the role (the event may arrive late); otherwise every
// club_admin role of the user ends (verification_lapsed). Idempotent: a
// second run finds nothing active.
func (s *service) handleEduLapsed(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.UserEduVerificationLapsed](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if s.d.Students != nil {
		if _, active, err := s.d.Students.Active(ctx, p.UserID); err != nil || active {
			return err
		}
	}
	n, err := s.endRoles(ctx, authz.SystemActor(EndLapsedAdminRolesTaskName), p.UserID, clubAdminRoles, EndVerificationLapsed)
	if err == nil && n > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "club_admin roles ended: verification lapsed",
			slog.String("user_id", p.UserID.String()), slog.Int("ended", n))
	}
	return err
}

// handleGraduated is orgs.end_graduated_admin_roles (student.graduated):
// every club_admin role of the user ends (graduated).
func (s *service) handleGraduated(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.StudentGraduated](e)
	if err != nil {
		return queue.Permanent(err)
	}
	n, err := s.endRoles(ctx, authz.SystemActor(EndGraduatedAdminRolesTaskName), p.UserID, clubAdminRoles, EndGraduated)
	if err == nil && n > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "club_admin roles ended: graduated",
			slog.String("user_id", p.UserID.String()), slog.Int("ended", n))
	}
	return err
}

// handleSensitivityChanged is orgs.purge_sensitive_memberships: when a club
// becomes sensitive its ended membership rows (kept for the 6-month dispute
// window of a plain club) are special-category data now and are deleted at
// once, in batches; open memberships end-delete as usual from now on.
// Clearing the flag changes nothing. Idempotent.
func (s *service) handleSensitivityChanged(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ClubSensitivityChanged](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if !p.Sensitive {
		return nil
	}
	n, err := s.batches(ctx, func(ctx context.Context) (int, error) {
		return s.d.Store.DeleteEndedMembers(ctx, p.ClubOrgID, sweepBatch)
	})
	if err != nil {
		return fmt.Errorf("%s: %w", PurgeSensitiveMembershipsTaskName, err)
	}
	if n > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "ended memberships of a sensitive club deleted",
			slog.String("org_id", p.ClubOrgID.String()), slog.Int("deleted", n))
	}
	return nil
}

// adminSweep is orgs.admin_sweep (01 §7): it walks the active club_admin
// memberships in batches and ends the roles of users without an active
// student verification (verification_lapsed) or with a graduated profile
// (graduated): the safety net for missed events. Without a verification
// source it does nothing (it cannot tell).
func (s *service) adminSweep(ctx context.Context) error {
	if s.d.Students == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "admin sweep skipped: no student verification source")
		return nil
	}
	sys := authz.SystemActor(AdminSweepJobName)
	after, ended := uuid.Nil(), 0
	for ctx.Err() == nil {
		rows, err := s.d.Store.ActiveByRole(ctx, authz.MemberClubAdmin, after, sweepBatch)
		if err != nil {
			return fmt.Errorf("%s: %w", AdminSweepJobName, err)
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].ID
		var users []uuid.UUID
		for _, m := range rows {
			if !slices.Contains(users, m.UserID) {
				users = append(users, m.UserID)
			}
		}
		verified, err := s.d.Students.ActiveMany(ctx, users)
		if err != nil {
			return fmt.Errorf("%s: %w", AdminSweepJobName, err)
		}
		graduated := map[uuid.UUID]bool{}
		if s.d.Graduated != nil {
			if graduated, err = s.d.Graduated(ctx, users); err != nil {
				return fmt.Errorf("%s: %w", AdminSweepJobName, err)
			}
		}
		for _, u := range users {
			reason := EndReason("")
			switch _, ok := verified[u]; {
			case graduated[u]:
				reason = EndGraduated
			case !ok:
				reason = EndVerificationLapsed
			}
			if reason == "" {
				continue
			}
			n, err := s.endRoles(ctx, sys, u, clubAdminRoles, reason)
			if err != nil {
				return fmt.Errorf("%s: %w", AdminSweepJobName, err)
			}
			ended += n
		}
		if len(rows) < sweepBatch {
			break
		}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "admin sweep done", slog.Int("ended", ended))
	return ctx.Err()
}

// expireInvitations is orgs.invitations_expire: open invitations past
// expires_at are closed (revoked_at = expires_at), in batches (idempotent).
// Deleting the closed ones after their retention is the Sweeper's.
func (s *service) expireInvitations(ctx context.Context) error {
	now := s.now()
	closed, err := s.batches(ctx, func(ctx context.Context) (int, error) {
		return s.d.Store.CloseExpiredInvitations(ctx, now, sweepBatch)
	})
	if err != nil {
		return fmt.Errorf("%s: close: %w", InvitationsExpireJobName, err)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "invitations expired", slog.Int("closed", closed))
	return ctx.Err()
}

// batches runs fn in its own transaction until it handles fewer than
// sweepBatch rows.
func (s *service) batches(ctx context.Context, fn func(context.Context) (int, error)) (int, error) {
	total := 0
	for ctx.Err() == nil {
		var n int
		if err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = fn(ctx)
			return err
		}); err != nil {
			return total, err
		}
		total += n
		if n < sweepBatch {
			break
		}
	}
	return total, nil
}
