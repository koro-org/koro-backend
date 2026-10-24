package orgs

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/authz"
	"koro/internal/platform/db"
)

// Membership lifecycle and invitations (BE-019) of the GORM store.

const (
	invitationCols = `id, org_id, org_kind, email, email_normalized, role, key_id, token_hash, invited_by, expires_at,
		accepted_at, accepted_user_id, revoked_at, created_at`
	// sinceExpr is the SortMemberJoined key (Membership.since).
	sinceExpr = "COALESCE(joined_at, requested_at, created_at)"
)

func roleStrings(roles []authz.MemberRole) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}

func statusStrings(sts []MemberStatus) []string {
	if len(sts) == 0 {
		return []string{string(MemberPending), string(MemberActive)}
	}
	out := make([]string, len(sts))
	for i, st := range sts {
		out[i] = string(st)
	}
	return out
}

func takeMember(op string, q *gorm.DB) (Membership, error) {
	var m Membership
	err := q.Select(memberCols).Take(&m).Error
	switch {
	case db.IsNotFound(err):
		return Membership{}, ErrNotFound
	case err != nil:
		return Membership{}, wrap(op, err)
	}
	return m, nil
}

func locked(q *gorm.DB) *gorm.DB {
	return q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
}

// GetMember implements Store.
func (s gormStore) GetMember(ctx context.Context, orgID, id uuid.UUID) (Membership, error) {
	return takeMember("get member", db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("id = ?", id))
}

// LockMember implements Store.
func (s gormStore) LockMember(ctx context.Context, orgID, id uuid.UUID) (Membership, error) {
	return s.scoped(ctx, "lock member", orgID, "id = ?", id)
}

// LockMemberOfUser implements Store.
//
// cross-org: /me/memberships/{id} addresses the caller's own row, scoped by
// user_id instead of an org.
func (s gormStore) LockMemberOfUser(ctx context.Context, userID, id uuid.UUID) (Membership, error) {
	return takeMember("lock own member", locked(db.Conn(ctx, s.orm)).Where("id = ? AND user_id = ?", id, userID))
}

// LockOpenMember implements Store.
func (s gormStore) LockOpenMember(ctx context.Context, orgID, userID uuid.UUID) (Membership, error) {
	return s.scoped(ctx, "lock open member", orgID, "user_id = ? AND status IN ('pending', 'active')", userID)
}

// LockActiveByRoles implements Store.
func (s gormStore) LockActiveByRoles(ctx context.Context, orgID uuid.UUID, roles []authz.MemberRole) ([]Membership, error) {
	var out []Membership
	err := locked(db.Conn(ctx, s.orm)).Scopes(db.ScopeOrg(orgID)).Select(memberCols).
		Where("status = 'active' AND role IN ?", roleStrings(roles)).Order("id").Find(&out).Error
	if err != nil {
		return nil, wrap("lock admins", err)
	}
	return out, nil
}

// LockUserActiveByRoles implements Store.
//
// cross-org: a user's roles end in every org at once (EndRoles: lapsed
// verification, graduation, claim revocation).
func (s gormStore) LockUserActiveByRoles(ctx context.Context, userID uuid.UUID, roles []authz.MemberRole) ([]Membership, error) {
	var out []Membership
	err := locked(db.Conn(ctx, s.orm)).Select(memberCols).
		Where("user_id = ? AND status = 'active' AND role IN ?", userID, roleStrings(roles)).Order("id").Find(&out).Error
	if err != nil {
		return nil, wrap("lock user roles", err)
	}
	return out, nil
}

// ActivateMember implements Store.
func (s gormStore) ActivateMember(ctx context.Context, orgID, id uuid.UUID, now time.Time, approvedBy uuid.UUID) (Membership, error) {
	var rows []Membership
	err := db.Conn(ctx, s.orm).Raw(`UPDATE org_members SET status = 'active', joined_at = @now, approved_by = @by
		WHERE id = @id AND org_id = @org_id AND status = 'pending'
		RETURNING `+memberCols, map[string]any{"now": now, "by": approvedBy, "id": id, "org_id": orgID}).Scan(&rows).Error
	if err != nil {
		return Membership{}, wrap("activate member", err)
	}
	if len(rows) == 0 {
		return Membership{}, ErrNotFound
	}
	return rows[0], nil
}

// DeleteMember implements Store.
func (s gormStore) DeleteMember(ctx context.Context, orgID, id uuid.UUID) error {
	if err := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("id = ?", id).Delete(&Membership{}).Error; err != nil {
		return wrap("delete member", err)
	}
	return nil
}

// DeleteEndedMembers implements Store.
func (s gormStore) DeleteEndedMembers(ctx context.Context, orgID uuid.UUID, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM org_members WHERE id IN (SELECT id FROM org_members
		WHERE org_id = @org_id AND status = 'ended' ORDER BY id LIMIT @limit FOR UPDATE SKIP LOCKED)`,
		map[string]any{"org_id": orgID, "limit": limit})
	if res.Error != nil {
		return 0, wrap("delete ended members", res.Error)
	}
	return int(res.RowsAffected), nil
}

// ListMembers implements Store.
func (s gormStore) ListMembers(ctx context.Context, orgID uuid.UUID, f MemberFilter, after *MemberAfter, limit int) ([]Membership, error) {
	q := db.Conn(ctx, s.orm).Model(&Membership{}).Scopes(db.ScopeOrg(orgID)).Select(memberCols).
		Where("status IN ?", statusStrings(f.Statuses))
	if f.Role != "" {
		q = q.Where("role = ?", string(f.Role))
	}
	if f.Sort == SortMemberJoined {
		if after != nil {
			q = q.Where("("+sinceExpr+", id) > ( ?, ?)", after.Since, after.ID)
		}
		q = q.Order(sinceExpr + ", id")
	} else {
		if after != nil {
			q = q.Where("id < ?", after.ID)
		}
		q = q.Order("id DESC")
	}
	var out []Membership
	if err := q.Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list members", err)
	}
	return out, nil
}

// ActiveMembershipsOfUser implements Store.
//
// cross-org: the Actor carries a user's memberships in every org (03 §2.1).
func (s gormStore) ActiveMembershipsOfUser(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	var out []Membership
	err := db.Conn(ctx, s.orm).Select(memberCols).Where("user_id = ? AND status = 'active'", userID).Order("id").Find(&out).Error
	if err != nil {
		return nil, wrap("active memberships of user", err)
	}
	return out, nil
}

// EndedMemberships implements Store.
//
// cross-org: the IDs come from talent's ledger (reconciliation of a closed
// period, BE-090); only the IDs of the ended rows leave.
func (s gormStore) EndedMemberships(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	if len(ids) == 0 {
		return out, nil
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT id FROM org_members WHERE id = ANY( ?) AND status = 'ended' ORDER BY id`,
		db.UUIDArray(ids)).Scan(&out).Error
	if err != nil {
		return nil, wrap("ended memberships", err)
	}
	return out, nil
}

// ListUserMemberships implements Store.
//
// cross-org: listMyMemberships returns the caller's own rows in every org.
func (s gormStore) ListUserMemberships(ctx context.Context, userID uuid.UUID, statuses []MemberStatus, after *uuid.UUID, limit int) ([]Membership, error) {
	q := db.Conn(ctx, s.orm).Select(memberCols).Where("user_id = ? AND status IN ?", userID, statusStrings(statuses))
	if after != nil {
		q = q.Where("id < ?", *after)
	}
	var out []Membership
	if err := q.Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list user memberships", err)
	}
	return out, nil
}

// ActiveMemberIDs implements Store.
func (s gormStore) ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := db.Conn(ctx, s.orm).Model(&Membership{}).Scopes(db.ScopeOrg(orgID)).Where("status = 'active'").
		Order("user_id").Pluck("user_id", &out).Error
	if err != nil {
		return nil, wrap("active member ids", err)
	}
	return out, nil
}

// ActiveByRole implements Store.
//
// cross-org: the admin sweep (01 §7 orgs.admin_sweep) checks every
// club_admin of every club.
func (s gormStore) ActiveByRole(ctx context.Context, role authz.MemberRole, after uuid.UUID, limit int) ([]Membership, error) {
	var out []Membership
	err := db.Conn(ctx, s.orm).Select(memberCols).Where("status = 'active' AND role = ? AND id > ?", string(role), after).
		Order("id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, wrap("active by role", err)
	}
	return out, nil
}

// InsertInvitation implements Store: org_id and org_kind come from the
// parent organizations row (06 H-38).
func (s gormStore) InsertInvitation(ctx context.Context, inv Invitation) (Invitation, error) {
	var rows []Invitation
	err := db.Conn(ctx, s.orm).Raw(`INSERT INTO org_invitations
		(id, org_id, org_kind, email, email_normalized, role, key_id, token_hash, invited_by, expires_at)
		SELECT @id, p.org_id, p.kind, @email, @email_normalized, @role, @key_id, @token_hash, @invited_by, @expires_at
		FROM (SELECT id AS org_id, kind FROM organizations) p WHERE p.org_id = @org_id
		RETURNING `+invitationCols, map[string]any{
		"id": inv.ID, "org_id": inv.OrgID, "email": inv.Email, "email_normalized": inv.EmailNormalized,
		"role": string(inv.Role), "key_id": inv.KeyID, "token_hash": inv.TokenHash, "invited_by": inv.InvitedBy,
		"expires_at": inv.ExpiresAt,
	}).Scan(&rows).Error
	if err != nil {
		return Invitation{}, wrap("insert invitation", err)
	}
	if len(rows) == 0 {
		return Invitation{}, ErrNotFound
	}
	return rows[0], nil
}

func takeInvitation(op string, q *gorm.DB) (Invitation, error) {
	var i Invitation
	err := q.Select(invitationCols).Take(&i).Error
	switch {
	case db.IsNotFound(err):
		return Invitation{}, ErrNotFound
	case err != nil:
		return Invitation{}, wrap(op, err)
	}
	return i, nil
}

// GetInvitation implements Store.
func (s gormStore) GetInvitation(ctx context.Context, orgID, id uuid.UUID) (Invitation, error) {
	return takeInvitation("get invitation", db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("id = ?", id))
}

// LockInvitationByTokenHash implements Store.
//
// cross-org: the token is the credential of acceptInvitation; the org
// follows from the row it finds.
func (s gormStore) LockInvitationByTokenHash(ctx context.Context, hash []byte) (Invitation, error) {
	return takeInvitation("lock invitation by token", locked(db.Conn(ctx, s.orm)).Where("token_hash = ?", hash))
}

// LockOpenInvitationTo implements Store.
func (s gormStore) LockOpenInvitationTo(ctx context.Context, orgID uuid.UUID, emailNormalized string) (Invitation, error) {
	q := locked(db.Conn(ctx, s.orm)).Scopes(db.ScopeOrg(orgID)).
		Where("email_normalized = ? AND accepted_at IS NULL AND revoked_at IS NULL", emailNormalized)
	return takeInvitation("lock open invitation", q)
}

// updateInvitation updates one open invitation; where is the caller's
// org-scoped key condition.
func updateInvitation(conn *gorm.DB, op, set, where string, args map[string]any) (Invitation, error) {
	var rows []Invitation
	err := conn.Raw(`UPDATE org_invitations SET `+set+`
		WHERE `+where+` AND accepted_at IS NULL AND revoked_at IS NULL
		RETURNING `+invitationCols, args).Scan(&rows).Error
	if err != nil {
		return Invitation{}, wrap(op, err)
	}
	if len(rows) == 0 {
		return Invitation{}, ErrNotFound
	}
	return rows[0], nil
}

// CloseInvitation implements Store.
func (s gormStore) CloseInvitation(ctx context.Context, orgID, id uuid.UUID, at time.Time) (Invitation, error) {
	return updateInvitation(db.Conn(ctx, s.orm), "close invitation", "revoked_at = @at", "id = @id AND org_id = @org_id",
		map[string]any{"at": at, "id": id, "org_id": orgID})
}

// AcceptInvitation implements Store.
func (s gormStore) AcceptInvitation(ctx context.Context, orgID, id, userID uuid.UUID, now time.Time) (Invitation, error) {
	return updateInvitation(db.Conn(ctx, s.orm), "accept invitation", "accepted_at = @now, accepted_user_id = @user",
		"id = @id AND org_id = @org_id", map[string]any{"now": now, "user": userID, "id": id, "org_id": orgID})
}

// ListOpenInvitations implements Store.
func (s gormStore) ListOpenInvitations(ctx context.Context, orgID uuid.UUID, now time.Time, after *uuid.UUID, limit int) ([]Invitation, error) {
	q := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Select(invitationCols).
		Where("accepted_at IS NULL AND revoked_at IS NULL AND expires_at > ?", now)
	if after != nil {
		q = q.Where("id < ?", *after)
	}
	var out []Invitation
	if err := q.Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list invitations", err)
	}
	return out, nil
}

// CloseExpiredInvitations implements Store.
//
// cross-org: orgs.invitations_expire closes the expired invitations of
// every org.
func (s gormStore) CloseExpiredInvitations(ctx context.Context, now time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE org_invitations SET revoked_at = expires_at
		WHERE id IN (SELECT id FROM org_invitations
			WHERE accepted_at IS NULL AND revoked_at IS NULL AND expires_at <= @now
			ORDER BY id LIMIT @limit FOR UPDATE SKIP LOCKED)`, map[string]any{"now": now, "limit": limit})
	if res.Error != nil {
		return 0, wrap("close expired invitations", res.Error)
	}
	return int(res.RowsAffected), nil
}

// purgeSQL holds, per retention policy, the count of rows whose retention
// ended and the delete of one batch of them, oldest first (BE-057).
var purgeSQL = map[string]struct{ count, del string }{
	PolicyMembershipEnded: {
		`SELECT count(*) FROM org_members WHERE status = 'ended' AND ended_at < @cutoff`,
		`DELETE FROM org_members WHERE id IN (SELECT id FROM org_members
			WHERE status = 'ended' AND ended_at < @cutoff ORDER BY ended_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`,
	},
	PolicyOrgInvitation: {
		`SELECT count(*) FROM org_invitations WHERE COALESCE(accepted_at, revoked_at, expires_at) < @cutoff`,
		`DELETE FROM org_invitations WHERE id IN (SELECT id FROM org_invitations
			WHERE COALESCE(accepted_at, revoked_at, expires_at) < @cutoff
			ORDER BY COALESCE(accepted_at, revoked_at, expires_at), id LIMIT @limit FOR UPDATE SKIP LOCKED)`,
	},
}

// PurgeExpired implements Store.
//
// cross-org: retention applies to the rows of every org.
func (s gormStore) PurgeExpired(ctx context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error) {
	q, ok := purgeSQL[policy]
	if !ok {
		return 0, fmt.Errorf("orgs: no retention purge for policy %q", policy)
	}
	args := map[string]any{"cutoff": cutoff, "limit": limit}
	if dryRun {
		var n int64
		if err := db.Conn(ctx, s.orm).Raw(q.count, args).Scan(&n).Error; err != nil {
			return 0, wrap("count "+policy, err)
		}
		return int(n), nil
	}
	res := db.Conn(ctx, s.orm).Exec(q.del, args)
	if res.Error != nil {
		return 0, wrap("purge "+policy, res.Error)
	}
	return int(res.RowsAffected), nil
}

func invitationsOfUser(q *gorm.DB, userID uuid.UUID, emailNormalized string) *gorm.DB {
	if emailNormalized == "" {
		return q.Where("accepted_user_id = ?", userID)
	}
	return q.Where("accepted_user_id = ? OR email_normalized = ?", userID, emailNormalized)
}

// InvitationsOfUser implements Store.
//
// cross-org: the KVKK export of one data subject spans every org.
func (s gormStore) InvitationsOfUser(ctx context.Context, userID uuid.UUID, emailNormalized string) ([]Invitation, error) {
	var out []Invitation
	q := invitationsOfUser(db.Conn(ctx, s.orm).Select(invitationCols), userID, emailNormalized)
	if err := q.Order("id").Find(&out).Error; err != nil {
		return nil, wrap("invitations of user", err)
	}
	return out, nil
}

// DeleteInvitationsOfUser implements Store.
//
// cross-org: the KVKK erasure of one data subject spans every org.
func (s gormStore) DeleteInvitationsOfUser(ctx context.Context, userID uuid.UUID, emailNormalized string) (int, error) {
	res := invitationsOfUser(db.Conn(ctx, s.orm), userID, emailNormalized).Delete(&Invitation{})
	if res.Error != nil {
		return 0, wrap("delete invitations of user", res.Error)
	}
	return int(res.RowsAffected), nil
}
