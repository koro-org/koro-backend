package identity

import (
	"bytes"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/civil"
)

// Status is users.status (02 §5.1).
type Status string

// Account statuses.
const (
	StatusPendingEmail Status = "pending_email" // registered, e-mail not verified: cannot sign in
	StatusActive       Status = "active"
	StatusSuspended    Status = "suspended" // every session closed (revoke_reason suspended)
	StatusErased       Status = "erased"    // tombstone: personal columns NULL (KVKK erasure)
)

// Persona is users.signup_persona: what the user signed up as.
type Persona string

// Signup personas. PersonaPlatform is only created by `koro seed
// bootstrap-admin`; public registration accepts the other four.
const (
	PersonaStudent    Persona = "student"
	PersonaCompany    Persona = "company"
	PersonaUniversity Persona = "university"
	PersonaClub       Persona = "club"
	PersonaPlatform   Persona = "platform"
)

// Locales of users.locale.
const (
	LocaleTR = "tr-TR"
	LocaleEN = "en-US"
)

// User is one users row. Personal columns are empty on a tombstone.
type User struct {
	ID               uuid.UUID
	Status           Status
	Email            string // as entered (trimmed); "" on a tombstone
	EmailNormalized  string // textx.NormalizeEmail(Email): the unique key
	EmailVerifiedAt  *time.Time
	PasswordHash     string // argon2id PHC string; "" = no password (bootstrap admin, erased)
	FirstName        string
	LastName         string
	BirthDate        *civil.Date // mandatory for students (age gate)
	Persona          Persona
	Locale           string
	MFAEnabled       bool
	LastLoginAt      *time.Time
	SuspendedAt      *time.Time
	SuspensionReason string
	ErasedAt         *time.Time
	FailedLogins     int        // consecutive failed logins and re-authentications
	LockedUntil      *time.Time // account lockout (05 §14.1)
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// IsMinor reports whether the user is under 18 at now (02 §5.1: computed on
// every read, never stored). Without a birth date (non-student personas)
// the answer is false.
func (u User) IsMinor(now time.Time) bool { return u.BirthDate != nil && u.BirthDate.IsMinor(now) }

// locked reports whether the account is locked at now.
func (u User) locked(now time.Time) bool { return u.LockedUntil != nil && now.Before(*u.LockedUntil) }

func (u User) clone() User {
	c := u
	c.EmailVerifiedAt, c.LastLoginAt = cloneTime(u.EmailVerifiedAt), cloneTime(u.LastLoginAt)
	c.SuspendedAt, c.ErasedAt, c.LockedUntil = cloneTime(u.SuspendedAt), cloneTime(u.ErasedAt), cloneTime(u.LockedUntil)
	if u.BirthDate != nil {
		d := *u.BirthDate
		c.BirthDate = &d
	}
	return c
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// ClientKind is sessions.client_kind: it fixes the credential transport
// (web → cookie, mobile → bearer; 03 §2.2-1).
type ClientKind string

// Client kinds.
const (
	ClientWeb    ClientKind = "web"
	ClientMobile ClientKind = "mobile"
)

// RevokeReason is sessions.revoke_reason.
type RevokeReason string

// Revoke reasons (02 §5.2).
const (
	RevokeLogout         RevokeReason = "logout"
	RevokePasswordChange RevokeReason = "password_change"
	RevokeAdmin          RevokeReason = "admin"
	RevokeErasure        RevokeReason = "erasure"
	RevokeMFAReset       RevokeReason = "mfa_reset"
	RevokeSuspended      RevokeReason = "suspended"
	RevokeRoleChange     RevokeReason = "role_change"
)

func (r RevokeReason) valid() bool {
	switch r {
	case RevokeLogout, RevokePasswordChange, RevokeAdmin, RevokeErasure, RevokeMFAReset, RevokeSuspended, RevokeRoleChange:
		return true
	}
	return false
}

// Session is one sessions row. Only the SHA-256 of the opaque token is
// stored (05 §14.2); the raw token exists once, in the login response.
type Session struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	TokenHash         []byte
	ClientKind        ClientKind
	CreatedAt         time.Time
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
	MFAVerifiedAt     *time.Time
	RevokedAt         *time.Time
	RevokeReason      RevokeReason
	IP                string // canonical text, "" when unknown
	UserAgent         string
	DeviceLabel       string
}

// live reports whether the session is usable at now (not revoked, inside
// both windows).
func (s Session) live(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.IdleExpiresAt) && now.Before(s.AbsoluteExpiresAt)
}

func (s Session) clone() Session {
	c := s
	c.TokenHash = bytes.Clone(s.TokenHash)
	c.MFAVerifiedAt, c.RevokedAt = cloneTime(s.MFAVerifiedAt), cloneTime(s.RevokedAt)
	return c
}

// TokenPurpose is email_tokens.purpose.
type TokenPurpose string

// E-mail token purposes. edu_email belongs to BE-018.
const (
	PurposeAccountEmail  TokenPurpose = "account_email"
	PurposeEmailChange   TokenPurpose = "email_change"
	PurposePasswordReset TokenPurpose = "password_reset"
	PurposeEduEmail      TokenPurpose = "edu_email"
)

// EmailToken is one email_tokens row. The token itself is derived
// (DerivedToken("identity.email_token", key_id, id)) and never stored: the
// worker re-derives it for the e-mail, the database keeps its SHA-256.
type EmailToken struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Purpose     TokenPurpose
	TargetEmail string // email_change target ("" otherwise)
	KeyID       string
	TokenHash   []byte
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
	Attempts    int
	CreatedAt   time.Time
}

func (t EmailToken) clone() EmailToken {
	c := t
	c.TokenHash = bytes.Clone(t.TokenHash)
	c.ConsumedAt = cloneTime(t.ConsumedAt)
	return c
}

// RoleStatus is platform_role_assignments.status.
type RoleStatus string

// Assignment statuses (02 §5.7). A role is IN EFFECT only while the row is
// active or revoke_pending (06 H-33): a pending request grants nothing.
const (
	RolePending       RoleStatus = "pending"
	RoleActive        RoleStatus = "active"
	RoleRevokePending RoleStatus = "revoke_pending"
	RoleRevoked       RoleStatus = "revoked"
	RoleRejected      RoleStatus = "rejected"
)

// InEffect reports whether an assignment in status s grants its role
// (06 H-33). Every reader of a user's roles uses it.
func (s RoleStatus) InEffect() bool { return s == RoleActive || s == RoleRevokePending }

// RoleAssignment is one platform_role_assignments row (the columns this
// ticket reads and writes; the request/approval flow is BE-016).
type RoleAssignment struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	Role              authz.PlatformRole
	Reason            string
	Status            RoleStatus
	RequestedBy       *uuid.UUID
	RequestedAt       time.Time
	ApprovedBy        *uuid.UUID
	ApprovedAt        *time.Time
	GrantedAt         *time.Time
	RejectedBy        *uuid.UUID
	RejectedAt        *time.Time
	RevokeReason      string
	RevokeRequestedBy *uuid.UUID
	RevokeRequestedAt *time.Time
	RevokedBy         *uuid.UUID
	RevokedAt         *time.Time
}

// bootstrapReason is platform_role_assignments.reason of the rows opened by
// `koro seed bootstrap-admin` (the only privileged rows without a second
// approver, 02 §5.7).
const bootstrapReason = "bootstrap"
