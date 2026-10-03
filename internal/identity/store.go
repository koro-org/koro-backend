package identity

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
)

// Store is the persistence boundary of identity (05 §2.2). No identity
// table is org-owned (02 §5: "Org kapsamı: yok"): every method is cross-org
// and addresses rows by user, token hash or ID. The GORM store
// (store_gorm.go) and the in-memory store (store_mem.go) pass the same
// contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any. Raw tokens
// never reach the store: sessions and e-mail tokens are found by the
// SHA-256 of the token.
type Store interface {
	// InsertUser writes a new account. A taken email_normalized is
	// ErrEmailTaken (users_email_uq).
	InsertUser(ctx context.Context, u User) error
	// User returns an account or ErrNotFound.
	User(ctx context.Context, id uuid.UUID) (User, error)
	// LockUser returns an account locked for the transaction (SELECT … FOR
	// UPDATE), or ErrNotFound.
	LockUser(ctx context.Context, id uuid.UUID) (User, error)
	// UserByEmail returns the account of a normalized address; found=false
	// when none (tombstones have no address).
	UserByEmail(ctx context.Context, normalized string) (u User, found bool, err error)
	// Users returns the accounts of ids that exist, in no particular order.
	Users(ctx context.Context, ids []uuid.UUID) ([]User, error)
	// UpdateUser writes the mutable columns of u: status, email,
	// email_normalized, email_verified_at, password_hash, first_name,
	// last_name, locale, last_login_at, failed_login_count, locked_until.
	// A taken email_normalized is ErrEmailTaken; a missing row ErrNotFound.
	UpdateUser(ctx context.Context, u User) error
	// EraseUser turns the account into a tombstone (status erased, personal
	// columns NULL, erased_at = at). found=false when it already is one.
	EraseUser(ctx context.Context, id uuid.UUID, at time.Time) (erased bool, err error)

	// InsertSession writes a session.
	InsertSession(ctx context.Context, s Session) error
	// SessionByTokenHash returns the session of a token hash; found=false
	// when none.
	SessionByTokenHash(ctx context.Context, hash []byte) (s Session, found bool, err error)
	// Session returns a session or ErrNotFound.
	Session(ctx context.Context, id uuid.UUID) (Session, error)
	// TouchSession slides an unrevoked session: last_seen_at and
	// idle_expires_at (never past absolute_expires_at).
	TouchSession(ctx context.Context, id uuid.UUID, lastSeen, idleExpires time.Time) error
	// RevokeSession revokes an unrevoked session of userID; revoked=false
	// when there is none (already revoked, or another user's).
	RevokeSession(ctx context.Context, userID, id uuid.UUID, at time.Time, reason RevokeReason) (revoked bool, err error)
	// RevokeUserSessions revokes every unrevoked session of userID except
	// keep (nil: all) and returns how many it revoked.
	RevokeUserSessions(ctx context.Context, userID uuid.UUID, keep *uuid.UUID, at time.Time, reason RevokeReason) (int, error)
	// ActiveSessions returns up to limit live sessions (unrevoked, inside
	// both windows at now) of userID, newest first (id DESC), after the
	// session ID after (nil: from the start).
	ActiveSessions(ctx context.Context, userID uuid.UUID, now time.Time, after *uuid.UUID, limit int) ([]Session, error)
	// SessionsOf returns every session row of userID, oldest first (KVKK export).
	SessionsOf(ctx context.Context, userID uuid.UUID) ([]Session, error)
	// DeleteEndedSessions deletes up to limit sessions that ended (revoked,
	// or a window closed) before cutoff, and returns how many.
	DeleteEndedSessions(ctx context.Context, cutoff time.Time, limit int) (int, error)
	// DeleteUserSessions deletes every session of userID (erasure).
	DeleteUserSessions(ctx context.Context, userID uuid.UUID) (int, error)

	// InsertEmailToken writes a token row.
	InsertEmailToken(ctx context.Context, t EmailToken) error
	// CloseOpenTokens consumes every open token of (userID, purpose) at at:
	// a new token replaces the previous ones (02 §5.3).
	CloseOpenTokens(ctx context.Context, userID uuid.UUID, purpose TokenPurpose, at time.Time) error
	// EmailToken returns a token row or ErrNotFound.
	EmailToken(ctx context.Context, id uuid.UUID) (EmailToken, error)
	// EmailTokenByHash returns the token of a hash; found=false when none.
	EmailTokenByHash(ctx context.Context, hash []byte) (t EmailToken, found bool, err error)
	// LockEmailToken returns a token row locked for the transaction, or
	// ErrNotFound.
	LockEmailToken(ctx context.Context, id uuid.UUID) (EmailToken, error)
	// ConsumeEmailToken sets consumed_at of an open token; consumed=false
	// when it already was.
	ConsumeEmailToken(ctx context.Context, id uuid.UUID, at time.Time) (consumed bool, err error)
	// DeleteEndedEmailTokens deletes up to limit tokens consumed or expired
	// before cutoff, and returns how many.
	DeleteEndedEmailTokens(ctx context.Context, cutoff time.Time, limit int) (int, error)
	// DeleteUserEmailTokens deletes every token of userID (erasure).
	DeleteUserEmailTokens(ctx context.Context, userID uuid.UUID) (int, error)

	// PlatformRoles returns the roles IN EFFECT of userID, sorted: rows with
	// status active or revoke_pending only (06 H-33). A pending request, a
	// rejected one and a revoked role grant nothing.
	PlatformRoles(ctx context.Context, userID uuid.UUID) ([]authz.PlatformRole, error)
	// RoleAssignmentsOf returns every assignment row of userID, oldest
	// first (KVKK export, bootstrap).
	RoleAssignmentsOf(ctx context.Context, userID uuid.UUID) ([]RoleAssignment, error)
	// UsersWithRole returns the users holding role in effect, sorted.
	UsersWithRole(ctx context.Context, role authz.PlatformRole) ([]uuid.UUID, error)
	// InsertRoleAssignment writes an assignment row (bootstrap; the grant
	// flow is BE-016). The 02 §5.7 CHECKs and the open-row unique index
	// apply: a second open row of (user, role) is ErrBootstrap.
	InsertRoleAssignment(ctx context.Context, a RoleAssignment) error
}
