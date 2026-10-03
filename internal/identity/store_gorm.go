package identity

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

// NewGormStore returns the PostgreSQL store over users, sessions,
// email_tokens and platform_role_assignments (02 §5).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// constraints maps identity constraints to module errors.
var constraints = db.Constraints{
	"users_email_uq":                    ErrEmailTaken,
	"platform_role_assignments_open_uq": ErrBootstrap,
}

// InsertUser implements Store.
//
// cross-org: accounts belong to no org.
func (s gormStore) InsertUser(ctx context.Context, u User) error {
	r := rowOf(u)
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return fmt.Errorf("identity: insert user: %w", constraints.Translate(err))
	}
	return nil
}

// User implements Store.
//
// cross-org: accounts belong to no org.
func (s gormStore) User(ctx context.Context, id uuid.UUID) (User, error) {
	return s.user(ctx, id, false)
}

// LockUser implements Store.
//
// cross-org: accounts belong to no org.
func (s gormStore) LockUser(ctx context.Context, id uuid.UUID) (User, error) {
	return s.user(ctx, id, true)
}

// cross-org: accounts belong to no org.
func (s gormStore) user(ctx context.Context, id uuid.UUID, lock bool) (User, error) {
	q := db.Conn(ctx, s.orm)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	var r userRow
	if err := q.Where("id = ?", id).Take(&r).Error; err != nil {
		if db.IsNotFound(err) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("identity: user: %w", err)
	}
	return r.user(), nil
}

// UserByEmail implements Store (index users_email_uq).
//
// cross-org: accounts belong to no org.
func (s gormStore) UserByEmail(ctx context.Context, normalized string) (User, bool, error) {
	if normalized == "" {
		return User{}, false, nil
	}
	var rows []userRow
	if err := db.Conn(ctx, s.orm).Where("email_normalized = ?", normalized).Limit(1).Find(&rows).Error; err != nil {
		return User{}, false, fmt.Errorf("identity: user by email: %w", err)
	}
	if len(rows) == 0 {
		return User{}, false, nil
	}
	return rows[0].user(), true, nil
}

// Users implements Store.
//
// cross-org: accounts belong to no org.
func (s gormStore) Users(ctx context.Context, ids []uuid.UUID) ([]User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []userRow
	if err := db.Conn(ctx, s.orm).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: users: %w", err)
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = r.user()
	}
	return out, nil
}

// UpdateUser implements Store.
//
// cross-org: accounts belong to no org.
func (s gormStore) UpdateUser(ctx context.Context, u User) error {
	r := rowOf(u)
	res := db.Conn(ctx, s.orm).Model(&userRow{}).Where("id = ?", u.ID).Updates(map[string]any{
		"status": r.Status, "email": r.Email, "email_normalized": r.EmailNormalized, "email_verified_at": r.EmailVerifiedAt,
		"password_hash": r.PasswordHash, "first_name": r.FirstName, "last_name": r.LastName, "locale": r.Locale,
		"last_login_at": r.LastLoginAt, "failed_login_count": r.FailedLoginCount, "locked_until": r.LockedUntil,
	})
	if res.Error != nil {
		return fmt.Errorf("identity: update user: %w", constraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// EraseUser implements Store: the tombstone of 02 §5.1 (users_erased_chk).
//
// cross-org: accounts belong to no org.
func (s gormStore) EraseUser(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE users SET status = 'erased', email = NULL, email_normalized = NULL,
		email_verified_at = NULL, password_hash = NULL, first_name = NULL, last_name = NULL, birth_date = NULL,
		mfa_enabled = false, last_login_at = NULL, suspension_reason = NULL, failed_login_count = 0,
		locked_until = NULL, erased_at = ?
		WHERE id = ? AND status <> 'erased'`, at, id)
	if res.Error != nil {
		return false, fmt.Errorf("identity: erase user: %w", db.Translate(res.Error))
	}
	if res.RowsAffected == 1 {
		return true, nil
	}
	if _, err := s.User(ctx, id); err != nil {
		return false, err
	}
	return false, nil
}

// sessionCols are the selected sessions columns; ip is read as host text.
const sessionCols = `id, user_id, token_hash, client_kind, created_at, last_seen_at, idle_expires_at,
	absolute_expires_at, mfa_verified_at, revoked_at, revoke_reason, host(ip) AS ip, user_agent, device_label`

func sessions(rows []sessionRow) []Session {
	out := make([]Session, len(rows))
	for i, r := range rows {
		out[i] = r.session()
	}
	return out
}

// InsertSession implements Store.
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) InsertSession(ctx context.Context, x Session) error {
	var reason *string
	if x.RevokeReason != "" {
		reason = nullable(string(x.RevokeReason))
	}
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO sessions (id, user_id, token_hash, client_kind, created_at, last_seen_at,
		idle_expires_at, absolute_expires_at, mfa_verified_at, revoked_at, revoke_reason, ip, user_agent, device_label)
		VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CAST( ? AS inet), ?, ?)`,
		x.ID, x.UserID, x.TokenHash, string(x.ClientKind), x.CreatedAt, x.LastSeenAt, x.IdleExpiresAt, x.AbsoluteExpiresAt,
		x.MFAVerifiedAt, x.RevokedAt, reason, nullable(x.IP), nullable(x.UserAgent), nullable(x.DeviceLabel)).Error
	if err != nil {
		return fmt.Errorf("identity: insert session: %w", db.Translate(err))
	}
	return nil
}

// SessionByTokenHash implements Store (sessions_token_hash_uq).
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) SessionByTokenHash(ctx context.Context, hash []byte) (Session, bool, error) {
	var rows []sessionRow
	if err := db.Conn(ctx, s.orm).Table("sessions").Select(sessionCols).Where("token_hash = ?", hash).Limit(1).Find(&rows).Error; err != nil {
		return Session{}, false, fmt.Errorf("identity: session by token: %w", err)
	}
	if len(rows) == 0 {
		return Session{}, false, nil
	}
	return rows[0].session(), true, nil
}

// Session implements Store.
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) Session(ctx context.Context, id uuid.UUID) (Session, error) {
	var rows []sessionRow
	if err := db.Conn(ctx, s.orm).Table("sessions").Select(sessionCols).Where("id = ?", id).Limit(1).Find(&rows).Error; err != nil {
		return Session{}, fmt.Errorf("identity: session: %w", err)
	}
	if len(rows) == 0 {
		return Session{}, ErrNotFound
	}
	return rows[0].session(), nil
}

// TouchSession implements Store.
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) TouchSession(ctx context.Context, id uuid.UUID, lastSeen, idle time.Time) error {
	err := db.Conn(ctx, s.orm).Exec(`UPDATE sessions SET last_seen_at = ?, idle_expires_at = LEAST( ?::timestamptz, absolute_expires_at)
		WHERE id = ? AND revoked_at IS NULL`, lastSeen, idle, id).Error
	if err != nil {
		return fmt.Errorf("identity: touch session: %w", db.Translate(err))
	}
	return nil
}

// RevokeSession implements Store.
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) RevokeSession(ctx context.Context, userID, id uuid.UUID, at time.Time, reason RevokeReason) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE sessions SET revoked_at = ?, revoke_reason = ?
		WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, at, string(reason), id, userID)
	if res.Error != nil {
		return false, fmt.Errorf("identity: revoke session: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// RevokeUserSessions implements Store (sessions_user_active_idx).
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) RevokeUserSessions(ctx context.Context, userID uuid.UUID, keep *uuid.UUID, at time.Time, reason RevokeReason) (int, error) {
	q := db.Conn(ctx, s.orm).Model(&sessionRow{}).Table("sessions").Where("user_id = ? AND revoked_at IS NULL", userID)
	if keep != nil {
		q = q.Where("id <> ?", *keep)
	}
	res := q.Updates(map[string]any{"revoked_at": at, "revoke_reason": string(reason)})
	if res.Error != nil {
		return 0, fmt.Errorf("identity: revoke sessions: %w", db.Translate(res.Error))
	}
	return int(res.RowsAffected), nil
}

// ActiveSessions implements Store.
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) ActiveSessions(ctx context.Context, userID uuid.UUID, now time.Time, after *uuid.UUID, limit int) ([]Session, error) {
	q := db.Conn(ctx, s.orm).Table("sessions").Select(sessionCols).
		Where("user_id = ? AND revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?", userID, now, now)
	if after != nil {
		q = q.Where("id < ?", *after)
	}
	var rows []sessionRow
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: active sessions: %w", err)
	}
	return sessions(rows), nil
}

// SessionsOf implements Store.
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) SessionsOf(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	var rows []sessionRow
	if err := db.Conn(ctx, s.orm).Table("sessions").Select(sessionCols).Where("user_id = ?", userID).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: sessions of: %w", err)
	}
	return sessions(rows), nil
}

// DeleteEndedSessions implements Store (retention session_expired).
//
// cross-org: retention sweep over every user's sessions.
func (s gormStore) DeleteEndedSessions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM sessions WHERE id IN (
		SELECT id FROM sessions
		WHERE LEAST(idle_expires_at, absolute_expires_at, COALESCE(revoked_at, 'infinity'::timestamptz)) < ?
		LIMIT ?)`, cutoff, limit)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: delete ended sessions: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DeleteUserSessions implements Store.
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) DeleteUserSessions(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: delete user sessions: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// InsertEmailToken implements Store.
//
// cross-org: tokens belong to a user, not an org.
func (s gormStore) InsertEmailToken(ctx context.Context, t EmailToken) error {
	r := tokenRow{
		ID: t.ID, UserID: t.UserID, Purpose: string(t.Purpose), TargetEmail: nullable(t.TargetEmail), KeyID: t.KeyID,
		TokenHash: t.TokenHash, ExpiresAt: t.ExpiresAt, ConsumedAt: t.ConsumedAt, CreatedAt: t.CreatedAt,
	}
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return fmt.Errorf("identity: insert email token: %w", db.Translate(err))
	}
	return nil
}

// CloseOpenTokens implements Store (email_tokens_open_idx).
//
// cross-org: tokens belong to a user, not an org.
func (s gormStore) CloseOpenTokens(ctx context.Context, userID uuid.UUID, purpose TokenPurpose, at time.Time) error {
	err := db.Conn(ctx, s.orm).Model(&tokenRow{}).Where("user_id = ? AND purpose = ? AND consumed_at IS NULL", userID, string(purpose)).
		Update("consumed_at", at).Error
	if err != nil {
		return fmt.Errorf("identity: close tokens: %w", db.Translate(err))
	}
	return nil
}

// EmailToken implements Store.
//
// cross-org: tokens belong to a user, not an org.
func (s gormStore) EmailToken(ctx context.Context, id uuid.UUID) (EmailToken, error) {
	return s.tokenBy(ctx, "id = ?", id, false)
}

// EmailTokenByHash implements Store (email_tokens_token_hash_uq).
//
// cross-org: tokens belong to a user, not an org.
func (s gormStore) EmailTokenByHash(ctx context.Context, hash []byte) (EmailToken, bool, error) {
	t, err := s.tokenBy(ctx, "token_hash = ?", hash, false)
	if isNotFound(err) {
		return EmailToken{}, false, nil
	}
	return t, err == nil, err
}

// LockEmailToken implements Store.
//
// cross-org: tokens belong to a user, not an org.
func (s gormStore) LockEmailToken(ctx context.Context, id uuid.UUID) (EmailToken, error) {
	return s.tokenBy(ctx, "id = ?", id, true)
}

// cross-org: tokens belong to a user, not an org.
func (s gormStore) tokenBy(ctx context.Context, where string, arg any, lock bool) (EmailToken, error) {
	q := db.Conn(ctx, s.orm)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	var r tokenRow
	if err := q.Where(where, arg).Take(&r).Error; err != nil {
		if db.IsNotFound(err) {
			return EmailToken{}, ErrNotFound
		}
		return EmailToken{}, fmt.Errorf("identity: email token: %w", err)
	}
	return r.token(), nil
}

// ConsumeEmailToken implements Store.
//
// cross-org: tokens belong to a user, not an org.
func (s gormStore) ConsumeEmailToken(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Model(&tokenRow{}).Where("id = ? AND consumed_at IS NULL", id).Update("consumed_at", at)
	if res.Error != nil {
		return false, fmt.Errorf("identity: consume token: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// DeleteEndedEmailTokens implements Store (retention email_token).
//
// cross-org: retention sweep over every user's tokens.
func (s gormStore) DeleteEndedEmailTokens(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM email_tokens WHERE id IN (
		SELECT id FROM email_tokens WHERE expires_at < ? OR consumed_at < ? LIMIT ?)`, cutoff, cutoff, limit)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: delete ended tokens: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DeleteUserEmailTokens implements Store.
//
// cross-org: tokens belong to a user, not an org.
func (s gormStore) DeleteUserEmailTokens(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM email_tokens WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: delete user tokens: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// inEffect is the 06 H-33 condition: the only way to read roles in effect.
const inEffect = "status IN ('active','revoke_pending')"

// PlatformRoles implements Store (06 H-33; index platform_role_assignments_open_uq).
//
// cross-org: platform roles belong to no org.
func (s gormStore) PlatformRoles(ctx context.Context, userID uuid.UUID) ([]authz.PlatformRole, error) {
	var roles []string
	err := db.Conn(ctx, s.orm).Model(&roleRow{}).Distinct("role").Where("user_id = ? AND "+inEffect, userID).
		Order("role").Pluck("role", &roles).Error
	if err != nil {
		return nil, fmt.Errorf("identity: platform roles: %w", err)
	}
	out := make([]authz.PlatformRole, len(roles))
	for i, r := range roles {
		out[i] = authz.PlatformRole(r)
	}
	return out, nil
}

// RoleAssignmentsOf implements Store.
//
// cross-org: platform roles belong to no org.
func (s gormStore) RoleAssignmentsOf(ctx context.Context, userID uuid.UUID) ([]RoleAssignment, error) {
	var rows []roleRow
	if err := db.Conn(ctx, s.orm).Where("user_id = ?", userID).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: role assignments: %w", err)
	}
	out := make([]RoleAssignment, len(rows))
	for i, r := range rows {
		out[i] = RoleAssignment{
			ID: r.ID, UserID: r.UserID, Role: authz.PlatformRole(r.Role), Reason: r.Reason, Status: RoleStatus(r.Status),
			RequestedBy: r.RequestedBy, RequestedAt: r.RequestedAt.UTC(), ApprovedBy: r.ApprovedBy, ApprovedAt: utcPtr(r.ApprovedAt),
			GrantedAt: utcPtr(r.GrantedAt), RejectedBy: r.RejectedBy, RejectedAt: utcPtr(r.RejectedAt), RevokeReason: str(r.RevokeReason),
			RevokeRequestedBy: r.RevokeRequestedBy, RevokeRequestedAt: utcPtr(r.RevokeRequestedAt), RevokedBy: r.RevokedBy,
			RevokedAt: utcPtr(r.RevokedAt),
		}
	}
	return out, nil
}

// UsersWithRole implements Store (06 H-33).
//
// cross-org: platform roles belong to no org.
func (s gormStore) UsersWithRole(ctx context.Context, role authz.PlatformRole) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := db.Conn(ctx, s.orm).Model(&roleRow{}).Distinct("user_id").Where("role = ? AND "+inEffect, string(role)).
		Order("user_id").Pluck("user_id", &ids).Error
	if err != nil {
		return nil, fmt.Errorf("identity: users with role: %w", err)
	}
	return ids, nil
}

// InsertRoleAssignment implements Store.
//
// cross-org: platform roles belong to no org.
func (s gormStore) InsertRoleAssignment(ctx context.Context, a RoleAssignment) error {
	r := roleRow{
		ID: a.ID, UserID: a.UserID, Role: string(a.Role), Reason: a.Reason, Status: string(a.Status),
		RequestedBy: a.RequestedBy, RequestedAt: a.RequestedAt, ApprovedBy: a.ApprovedBy, ApprovedAt: a.ApprovedAt,
		GrantedAt: a.GrantedAt, RejectedBy: a.RejectedBy, RejectedAt: a.RejectedAt, RevokeReason: nullable(a.RevokeReason),
		RevokeRequestedBy: a.RevokeRequestedBy, RevokeRequestedAt: a.RevokeRequestedAt, RevokedBy: a.RevokedBy, RevokedAt: a.RevokedAt,
	}
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return fmt.Errorf("identity: insert role assignment: %w", constraints.Translate(err))
	}
	return nil
}
