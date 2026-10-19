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
	"koro/internal/platform/textx"
)

// NewGormStore returns the PostgreSQL store over users, sessions,
// email_tokens and platform_role_assignments (02 §5).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// constraints maps identity constraints to module errors.
var constraints = db.Constraints{
	"users_email_uq":                                 ErrEmailTaken,
	"platform_role_assignments_open_uq":              ErrRoleAssignmentOpen,
	"platform_role_assignments_four_eyes_chk":        ErrFourEyes,
	"platform_role_assignments_revoke_four_eyes_chk": ErrFourEyes,
	"platform_role_assignments_self_chk":             ErrForbidden,
	"privileged_account_actions_open_uq":             ErrAccountActionOpen,
	"privileged_account_actions_four_eyes_chk":       ErrFourEyes,
	"privileged_account_actions_self_chk":            ErrForbidden,
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
		"mfa_enabled": r.MFAEnabled, "last_login_at": r.LastLoginAt, "suspended_at": r.SuspendedAt,
		"suspension_reason": r.SuspensionReason, "failed_login_count": r.FailedLoginCount, "locked_until": r.LockedUntil,
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
	return assignments(rows), nil
}

func assignments(rows []roleRow) []RoleAssignment {
	out := make([]RoleAssignment, len(rows))
	for i, r := range rows {
		out[i] = r.assignment()
	}
	return out
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
	r := roleRowOf(a)
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return fmt.Errorf("identity: insert role assignment: %w", constraints.Translate(err))
	}
	return nil
}

// RoleAssignment implements Store.
//
// cross-org: platform roles belong to no org.
func (s gormStore) RoleAssignment(ctx context.Context, id uuid.UUID) (RoleAssignment, error) {
	return s.roleAssignment(ctx, id, false)
}

// LockRoleAssignment implements Store.
//
// cross-org: platform roles belong to no org.
func (s gormStore) LockRoleAssignment(ctx context.Context, id uuid.UUID) (RoleAssignment, error) {
	return s.roleAssignment(ctx, id, true)
}

// cross-org: platform roles belong to no org.
func (s gormStore) roleAssignment(ctx context.Context, id uuid.UUID, lock bool) (RoleAssignment, error) {
	q := db.Conn(ctx, s.orm)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	var r roleRow
	if err := q.Where("id = ?", id).Take(&r).Error; err != nil {
		if db.IsNotFound(err) {
			return RoleAssignment{}, ErrNotFound
		}
		return RoleAssignment{}, fmt.Errorf("identity: role assignment: %w", err)
	}
	return r.assignment(), nil
}

// UpdateRoleAssignment implements Store.
//
// cross-org: platform roles belong to no org.
func (s gormStore) UpdateRoleAssignment(ctx context.Context, a RoleAssignment) error {
	r := roleRowOf(a)
	res := db.Conn(ctx, s.orm).Model(&roleRow{}).Where("id = ?", a.ID).Updates(map[string]any{
		"status": r.Status, "approved_by": r.ApprovedBy, "approved_at": r.ApprovedAt, "granted_at": r.GrantedAt,
		"rejected_by": r.RejectedBy, "rejected_at": r.RejectedAt, "revoke_reason": r.RevokeReason,
		"revoke_requested_by": r.RevokeRequestedBy, "revoke_requested_at": r.RevokeRequestedAt,
		"revoked_by": r.RevokedBy, "revoked_at": r.RevokedAt,
	})
	if res.Error != nil {
		return fmt.Errorf("identity: update role assignment: %w", constraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// RoleAssignments implements Store.
//
// cross-org: platform roles belong to no org.
func (s gormStore) RoleAssignments(ctx context.Context, q RoleQuery) ([]RoleAssignment, error) {
	tx := db.Conn(ctx, s.orm).Model(&roleRow{})
	if q.Role != "" {
		tx = tx.Where("role = ?", string(q.Role))
	}
	if !q.IncludeEnded {
		tx = tx.Where("status IN ('pending','active','revoke_pending')")
	}
	if q.After != nil {
		tx = tx.Where("id < ?", *q.After)
	}
	var rows []roleRow
	if err := tx.Order("id DESC").Limit(q.Limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: list role assignments: %w", err)
	}
	return assignments(rows), nil
}

// CountRoleAssignments implements Store (06 H-33).
//
// cross-org: platform roles belong to no org.
func (s gormStore) CountRoleAssignments(ctx context.Context, role authz.PlatformRole) (int, int, int, error) {
	var c struct {
		InEffect      int `gorm:"column:in_effect"`
		Pending       int `gorm:"column:pending"`
		RevokePending int `gorm:"column:revoke_pending"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT
		count(*) FILTER (WHERE `+inEffect+`) AS in_effect,
		count(*) FILTER (WHERE status = 'pending') AS pending,
		count(*) FILTER (WHERE status = 'revoke_pending') AS revoke_pending
		FROM platform_role_assignments WHERE role = ?`, string(role)).Scan(&c).Error
	if err != nil {
		return 0, 0, 0, fmt.Errorf("identity: count role assignments: %w", err)
	}
	return c.InEffect, c.Pending, c.RevokePending, nil
}

// SearchUsers implements Store (users_email_uq; users_name_trgm_idx).
//
// cross-org: accounts belong to no org.
func (s gormStore) SearchUsers(ctx context.Context, q UserSearch) ([]User, error) {
	tx := db.Conn(ctx, s.orm).Model(&userRow{})
	switch {
	case q.Email != "":
		tx = tx.Where("email_normalized = ?", q.Email)
	case q.Name != "":
		tx = tx.Where(`status <> 'erased' AND public.koro_norm(first_name || ' ' || last_name) LIKE '%' || public.koro_norm( ?) || '%' ESCAPE '\'`,
			textx.EscapeLike(q.Name))
	}
	if q.Status != "" {
		tx = tx.Where("status = ?", string(q.Status))
	}
	if q.After != nil {
		tx = tx.Where("id < ?", *q.After)
	}
	var rows []userRow
	if err := tx.Order("id DESC").Limit(q.Limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: search users: %w", err)
	}
	out := make([]User, len(rows))
	for i, r := range rows {
		out[i] = r.user()
	}
	return out, nil
}

// MarkSessionMFA implements Store.
//
// cross-org: sessions belong to a user, not an org.
func (s gormStore) MarkSessionMFA(ctx context.Context, userID, id uuid.UUID, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE sessions SET mfa_verified_at = ?
		WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, at, id, userID)
	if res.Error != nil {
		return false, fmt.Errorf("identity: mark session mfa: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// TOTP implements Store.
//
// cross-org: MFA secrets belong to a user, not an org.
func (s gormStore) TOTP(ctx context.Context, userID uuid.UUID) (TOTP, bool, error) {
	var rows []totpRow
	if err := db.Conn(ctx, s.orm).Where("user_id = ?", userID).Limit(1).Find(&rows).Error; err != nil {
		return TOTP{}, false, fmt.Errorf("identity: totp: %w", err)
	}
	if len(rows) == 0 {
		return TOTP{}, false, nil
	}
	return rows[0].totp(), true, nil
}

// PutTOTP implements Store: a confirmed row is never overwritten.
//
// cross-org: MFA secrets belong to a user, not an org.
func (s gormStore) PutTOTP(ctx context.Context, t TOTP) error {
	// created_at is the column default (database clock, BE-069); a new
	// enrollment over an unconfirmed one takes the new default too.
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO mfa_totp (user_id, secret_enc, confirmed_at, last_used_step)
		VALUES ( ?, ?, NULL, NULL)
		ON CONFLICT (user_id) DO UPDATE SET secret_enc = EXCLUDED.secret_enc, last_used_step = NULL, created_at = EXCLUDED.created_at
		WHERE mfa_totp.confirmed_at IS NULL`, t.UserID, t.SecretEnc)
	if res.Error != nil {
		return fmt.Errorf("identity: put totp: %w", db.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrMFAAlreadyEnabled
	}
	return nil
}

// ConfirmTOTP implements Store.
//
// cross-org: MFA secrets belong to a user, not an org.
func (s gormStore) ConfirmTOTP(ctx context.Context, userID uuid.UUID, step int64, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE mfa_totp SET confirmed_at = ?, last_used_step = ?
		WHERE user_id = ? AND confirmed_at IS NULL`, at, step, userID)
	if res.Error != nil {
		return false, fmt.Errorf("identity: confirm totp: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// UseTOTPStep implements Store: one conditional UPDATE, so two requests
// with the same code cannot both pass (05 §14.1 replay guard).
//
// cross-org: MFA secrets belong to a user, not an org.
func (s gormStore) UseTOTPStep(ctx context.Context, userID uuid.UUID, step int64) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE mfa_totp SET last_used_step = ?
		WHERE user_id = ? AND confirmed_at IS NOT NULL AND (last_used_step IS NULL OR last_used_step < ?)`, step, userID, step)
	if res.Error != nil {
		return false, fmt.Errorf("identity: use totp step: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// DeleteTOTP implements Store.
//
// cross-org: MFA secrets belong to a user, not an org.
func (s gormStore) DeleteTOTP(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM mfa_totp WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: delete totp: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// ReplaceRecoveryCodes implements Store.
//
// cross-org: recovery codes belong to a user, not an org.
func (s gormStore) ReplaceRecoveryCodes(ctx context.Context, userID uuid.UUID, hashes [][]byte, at time.Time) error {
	conn := db.Conn(ctx, s.orm)
	if err := conn.Exec(`DELETE FROM mfa_recovery_codes WHERE user_id = ?`, userID).Error; err != nil {
		return fmt.Errorf("identity: replace recovery codes: %w", err)
	}
	if len(hashes) == 0 {
		return nil
	}
	rows := make([]recoveryRow, len(hashes))
	for i, h := range hashes {
		rows[i] = recoveryRow{ID: uuid.NewV7(), UserID: userID, CodeHash: h, CreatedAt: at}
	}
	if err := conn.Create(&rows).Error; err != nil {
		return fmt.Errorf("identity: insert recovery codes: %w", db.Translate(err))
	}
	return nil
}

// UseRecoveryCode implements Store (mfa_recovery_codes_code_hash_uq).
//
// cross-org: recovery codes belong to a user, not an org.
func (s gormStore) UseRecoveryCode(ctx context.Context, userID uuid.UUID, hash []byte, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE mfa_recovery_codes SET used_at = ?
		WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`, at, userID, hash)
	if res.Error != nil {
		return false, fmt.Errorf("identity: use recovery code: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// RecoveryCodesLeft implements Store (mfa_recovery_codes_user_idx).
//
// cross-org: recovery codes belong to a user, not an org.
func (s gormStore) RecoveryCodesLeft(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Model(&recoveryRow{}).Where("user_id = ? AND used_at IS NULL", userID).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("identity: recovery codes left: %w", err)
	}
	return int(n), nil
}

// DeleteRecoveryCodes implements Store.
//
// cross-org: recovery codes belong to a user, not an org.
func (s gormStore) DeleteRecoveryCodes(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM mfa_recovery_codes WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: delete recovery codes: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// InsertAccountAction implements Store.
//
// cross-org: accounts belong to no org.
func (s gormStore) InsertAccountAction(ctx context.Context, r AccountActionRequest) error {
	row := accountActionRowOf(r)
	if err := db.Conn(ctx, s.orm).Create(&row).Error; err != nil {
		return fmt.Errorf("identity: insert account action: %w", constraints.Translate(err))
	}
	return nil
}

// LockOpenAccountAction implements Store (privileged_account_actions_open_uq).
//
// cross-org: accounts belong to no org.
func (s gormStore) LockOpenAccountAction(ctx context.Context, target uuid.UUID, action AccountActionKind) (AccountActionRequest, bool, error) {
	var rows []accountActionRow
	err := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("target_user_id = ? AND action = ? AND status = 'pending'", target, string(action)).Limit(1).Find(&rows).Error
	if err != nil {
		return AccountActionRequest{}, false, fmt.Errorf("identity: open account action: %w", err)
	}
	if len(rows) == 0 {
		return AccountActionRequest{}, false, nil
	}
	return rows[0].request(), true, nil
}

// UpdateAccountAction implements Store.
//
// cross-org: accounts belong to no org.
func (s gormStore) UpdateAccountAction(ctx context.Context, r AccountActionRequest) error {
	res := db.Conn(ctx, s.orm).Model(&accountActionRow{}).Where("id = ?", r.ID).Updates(map[string]any{
		"status": string(r.Status), "approved_by": r.ApprovedBy, "approved_at": r.ApprovedAt,
	})
	if res.Error != nil {
		return fmt.Errorf("identity: update account action: %w", constraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// AccountActionsOf implements Store.
//
// cross-org: accounts belong to no org.
func (s gormStore) AccountActionsOf(ctx context.Context, userID uuid.UUID) ([]AccountActionRequest, error) {
	var rows []accountActionRow
	if err := db.Conn(ctx, s.orm).Where("target_user_id = ?", userID).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: account actions of: %w", err)
	}
	out := make([]AccountActionRequest, len(rows))
	for i, r := range rows {
		out[i] = r.request()
	}
	return out, nil
}
