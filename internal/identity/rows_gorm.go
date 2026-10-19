package identity

import (
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/civil"
)

// GORM row types of the identity tables (store_gorm.go scans into them).
// The created_at and updated_at of the [ts] tables (users, mfa_totp,
// edu_verifications) are read only: the column defaults and the touch
// triggers stamp them with the database clock alone (02 §0.1, BE-069).

// userRow is one users row.
type userRow struct {
	ID               uuid.UUID   `gorm:"column:id;primaryKey"`
	Status           string      `gorm:"column:status"`
	Email            *string     `gorm:"column:email"`
	EmailNormalized  *string     `gorm:"column:email_normalized"`
	EmailVerifiedAt  *time.Time  `gorm:"column:email_verified_at"`
	PasswordHash     *string     `gorm:"column:password_hash"`
	FirstName        *string     `gorm:"column:first_name"`
	LastName         *string     `gorm:"column:last_name"`
	BirthDate        *civil.Date `gorm:"column:birth_date"`
	SignupPersona    string      `gorm:"column:signup_persona"`
	Locale           string      `gorm:"column:locale"`
	MFAEnabled       bool        `gorm:"column:mfa_enabled"`
	LastLoginAt      *time.Time  `gorm:"column:last_login_at"`
	SuspendedAt      *time.Time  `gorm:"column:suspended_at"`
	SuspensionReason *string     `gorm:"column:suspension_reason"`
	ErasedAt         *time.Time  `gorm:"column:erased_at"`
	FailedLoginCount int16       `gorm:"column:failed_login_count"`
	LockedUntil      *time.Time  `gorm:"column:locked_until"`
	CreatedAt        time.Time   `gorm:"column:created_at;->"`
	UpdatedAt        time.Time   `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (userRow) TableName() string { return "users" }

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func (r userRow) user() User {
	return User{
		ID: r.ID, Status: Status(r.Status), Email: str(r.Email), EmailNormalized: str(r.EmailNormalized),
		EmailVerifiedAt: utcPtr(r.EmailVerifiedAt), PasswordHash: str(r.PasswordHash), FirstName: str(r.FirstName),
		LastName: str(r.LastName), BirthDate: r.BirthDate, Persona: Persona(r.SignupPersona), Locale: r.Locale,
		MFAEnabled: r.MFAEnabled, LastLoginAt: utcPtr(r.LastLoginAt), SuspendedAt: utcPtr(r.SuspendedAt),
		SuspensionReason: str(r.SuspensionReason), ErasedAt: utcPtr(r.ErasedAt), FailedLogins: int(r.FailedLoginCount),
		LockedUntil: utcPtr(r.LockedUntil), CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
}

func rowOf(u User) userRow {
	return userRow{
		ID: u.ID, Status: string(u.Status), Email: nullable(u.Email), EmailNormalized: nullable(u.EmailNormalized),
		EmailVerifiedAt: u.EmailVerifiedAt, PasswordHash: nullable(u.PasswordHash), FirstName: nullable(u.FirstName),
		LastName: nullable(u.LastName), BirthDate: u.BirthDate, SignupPersona: string(u.Persona), Locale: u.Locale,
		MFAEnabled: u.MFAEnabled, LastLoginAt: u.LastLoginAt, SuspendedAt: u.SuspendedAt,
		SuspensionReason: nullable(u.SuspensionReason), ErasedAt: u.ErasedAt, FailedLoginCount: int16(min(u.FailedLogins, maxFailedLogins)), //nolint:gosec // bounded by maxFailedLogins
		LockedUntil: u.LockedUntil, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

type sessionRow struct {
	ID                uuid.UUID  `gorm:"column:id"`
	UserID            uuid.UUID  `gorm:"column:user_id"`
	TokenHash         []byte     `gorm:"column:token_hash"`
	ClientKind        string     `gorm:"column:client_kind"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	LastSeenAt        time.Time  `gorm:"column:last_seen_at"`
	IdleExpiresAt     time.Time  `gorm:"column:idle_expires_at"`
	AbsoluteExpiresAt time.Time  `gorm:"column:absolute_expires_at"`
	MFAVerifiedAt     *time.Time `gorm:"column:mfa_verified_at"`
	RevokedAt         *time.Time `gorm:"column:revoked_at"`
	RevokeReason      *string    `gorm:"column:revoke_reason"`
	IP                *string    `gorm:"column:ip"`
	UserAgent         *string    `gorm:"column:user_agent"`
	DeviceLabel       *string    `gorm:"column:device_label"`
}

func (r sessionRow) session() Session {
	return Session{
		ID: r.ID, UserID: r.UserID, TokenHash: r.TokenHash, ClientKind: ClientKind(r.ClientKind), CreatedAt: r.CreatedAt.UTC(),
		LastSeenAt: r.LastSeenAt.UTC(), IdleExpiresAt: r.IdleExpiresAt.UTC(), AbsoluteExpiresAt: r.AbsoluteExpiresAt.UTC(),
		MFAVerifiedAt: utcPtr(r.MFAVerifiedAt), RevokedAt: utcPtr(r.RevokedAt), RevokeReason: RevokeReason(str(r.RevokeReason)),
		IP: canonicalIP(str(r.IP)), UserAgent: str(r.UserAgent), DeviceLabel: str(r.DeviceLabel),
	}
}

type tokenRow struct {
	ID          uuid.UUID  `gorm:"column:id;primaryKey"`
	UserID      uuid.UUID  `gorm:"column:user_id"`
	Purpose     string     `gorm:"column:purpose"`
	TargetEmail *string    `gorm:"column:target_email"`
	KeyID       string     `gorm:"column:key_id"`
	TokenHash   []byte     `gorm:"column:token_hash"`
	ExpiresAt   time.Time  `gorm:"column:expires_at"`
	ConsumedAt  *time.Time `gorm:"column:consumed_at"`
	Attempts    int16      `gorm:"column:attempts"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (tokenRow) TableName() string { return "email_tokens" }

func (r tokenRow) token() EmailToken {
	return EmailToken{
		ID: r.ID, UserID: r.UserID, Purpose: TokenPurpose(r.Purpose), TargetEmail: str(r.TargetEmail), KeyID: r.KeyID,
		TokenHash: r.TokenHash, ExpiresAt: r.ExpiresAt.UTC(), ConsumedAt: utcPtr(r.ConsumedAt), Attempts: int(r.Attempts),
		CreatedAt: r.CreatedAt.UTC(),
	}
}

type roleRow struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	UserID            uuid.UUID  `gorm:"column:user_id"`
	Role              string     `gorm:"column:role"`
	Reason            string     `gorm:"column:reason"`
	Status            string     `gorm:"column:status"`
	RequestedBy       *uuid.UUID `gorm:"column:requested_by"`
	RequestedAt       time.Time  `gorm:"column:requested_at"`
	ApprovedBy        *uuid.UUID `gorm:"column:approved_by"`
	ApprovedAt        *time.Time `gorm:"column:approved_at"`
	GrantedAt         *time.Time `gorm:"column:granted_at"`
	RejectedBy        *uuid.UUID `gorm:"column:rejected_by"`
	RejectedAt        *time.Time `gorm:"column:rejected_at"`
	RevokeReason      *string    `gorm:"column:revoke_reason"`
	RevokeRequestedBy *uuid.UUID `gorm:"column:revoke_requested_by"`
	RevokeRequestedAt *time.Time `gorm:"column:revoke_requested_at"`
	RevokedBy         *uuid.UUID `gorm:"column:revoked_by"`
	RevokedAt         *time.Time `gorm:"column:revoked_at"`
}

// TableName implements gorm's Tabler.
func (roleRow) TableName() string { return "platform_role_assignments" }

func (r roleRow) assignment() RoleAssignment {
	return RoleAssignment{
		ID: r.ID, UserID: r.UserID, Role: authz.PlatformRole(r.Role), Reason: r.Reason, Status: RoleStatus(r.Status),
		RequestedBy: r.RequestedBy, RequestedAt: r.RequestedAt.UTC(), ApprovedBy: r.ApprovedBy, ApprovedAt: utcPtr(r.ApprovedAt),
		GrantedAt: utcPtr(r.GrantedAt), RejectedBy: r.RejectedBy, RejectedAt: utcPtr(r.RejectedAt), RevokeReason: str(r.RevokeReason),
		RevokeRequestedBy: r.RevokeRequestedBy, RevokeRequestedAt: utcPtr(r.RevokeRequestedAt), RevokedBy: r.RevokedBy,
		RevokedAt: utcPtr(r.RevokedAt),
	}
}

func roleRowOf(a RoleAssignment) roleRow {
	return roleRow{
		ID: a.ID, UserID: a.UserID, Role: string(a.Role), Reason: a.Reason, Status: string(a.Status),
		RequestedBy: a.RequestedBy, RequestedAt: a.RequestedAt, ApprovedBy: a.ApprovedBy, ApprovedAt: a.ApprovedAt,
		GrantedAt: a.GrantedAt, RejectedBy: a.RejectedBy, RejectedAt: a.RejectedAt, RevokeReason: nullable(a.RevokeReason),
		RevokeRequestedBy: a.RevokeRequestedBy, RevokeRequestedAt: a.RevokeRequestedAt, RevokedBy: a.RevokedBy, RevokedAt: a.RevokedAt,
	}
}

// totpRow is one mfa_totp row.
type totpRow struct {
	UserID       uuid.UUID  `gorm:"column:user_id;primaryKey"`
	SecretEnc    []byte     `gorm:"column:secret_enc"`
	ConfirmedAt  *time.Time `gorm:"column:confirmed_at"`
	LastUsedStep *int64     `gorm:"column:last_used_step"`
	CreatedAt    time.Time  `gorm:"column:created_at;->"`
}

// TableName implements gorm's Tabler.
func (totpRow) TableName() string { return "mfa_totp" }

func (r totpRow) totp() TOTP {
	return TOTP{UserID: r.UserID, SecretEnc: r.SecretEnc, ConfirmedAt: utcPtr(r.ConfirmedAt), LastUsedStep: r.LastUsedStep, CreatedAt: r.CreatedAt.UTC()}
}

// recoveryRow is one mfa_recovery_codes row.
type recoveryRow struct {
	ID        uuid.UUID  `gorm:"column:id;primaryKey"`
	UserID    uuid.UUID  `gorm:"column:user_id"`
	CodeHash  []byte     `gorm:"column:code_hash"`
	UsedAt    *time.Time `gorm:"column:used_at"`
	CreatedAt time.Time  `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (recoveryRow) TableName() string { return "mfa_recovery_codes" }

// accountActionRow is one privileged_account_actions row (06 H-47).
type accountActionRow struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TargetUserID uuid.UUID  `gorm:"column:target_user_id"`
	Action       string     `gorm:"column:action"`
	Reason       string     `gorm:"column:reason"`
	Status       string     `gorm:"column:status"`
	RequestedBy  uuid.UUID  `gorm:"column:requested_by"`
	RequestedAt  time.Time  `gorm:"column:requested_at"`
	ExpiresAt    time.Time  `gorm:"column:expires_at"`
	ApprovedBy   *uuid.UUID `gorm:"column:approved_by"`
	ApprovedAt   *time.Time `gorm:"column:approved_at"`
}

// TableName implements gorm's Tabler.
func (accountActionRow) TableName() string { return "privileged_account_actions" }

func accountActionRowOf(r AccountActionRequest) accountActionRow {
	return accountActionRow{ID: r.ID, TargetUserID: r.TargetUserID, Action: string(r.Action), Reason: r.Reason, Status: string(r.Status),
		RequestedBy: r.RequestedBy, RequestedAt: r.RequestedAt, ExpiresAt: r.ExpiresAt, ApprovedBy: r.ApprovedBy, ApprovedAt: r.ApprovedAt}
}

func (r accountActionRow) request() AccountActionRequest {
	return AccountActionRequest{ID: r.ID, TargetUserID: r.TargetUserID, Action: AccountActionKind(r.Action), Reason: r.Reason,
		Status: AccountActionStatus(r.Status), RequestedBy: r.RequestedBy, RequestedAt: r.RequestedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(),
		ApprovedBy: r.ApprovedBy, ApprovedAt: utcPtr(r.ApprovedAt)}
}
