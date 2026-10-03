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
	// last_name, locale, mfa_enabled, last_login_at, suspended_at,
	// suspension_reason, failed_login_count, locked_until. A taken
	// email_normalized is ErrEmailTaken; a missing row ErrNotFound.
	UpdateUser(ctx context.Context, u User) error
	// SearchUsers returns up to q.Limit accounts newest first (id DESC)
	// after q.After: by exact normalized e-mail, by a fragment of the name
	// (koro_norm, users_name_trgm_idx; tombstones excluded) or all of them,
	// filtered by q.Status when set (admin search, 04 §2.1).
	SearchUsers(ctx context.Context, q UserSearch) ([]User, error)
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
	// MarkSessionMFA sets mfa_verified_at of an unrevoked session of userID
	// (step-up, 03 §6.4-2); marked=false when there is none.
	MarkSessionMFA(ctx context.Context, userID, id uuid.UUID, at time.Time) (marked bool, err error)
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
	// InsertRoleAssignment writes an assignment row (bootstrap, grant
	// requests and direct grants). The 02 §5.7 CHECKs and the open-row
	// unique index apply: a second open row of (user, role) is
	// ErrRoleAssignmentOpen, approved_by = requested_by ErrFourEyes, a
	// self-assignment ErrForbidden.
	InsertRoleAssignment(ctx context.Context, a RoleAssignment) error
	// RoleAssignment returns an assignment row or ErrNotFound.
	RoleAssignment(ctx context.Context, id uuid.UUID) (RoleAssignment, error)
	// LockRoleAssignment returns an assignment row locked for the
	// transaction, or ErrNotFound.
	LockRoleAssignment(ctx context.Context, id uuid.UUID) (RoleAssignment, error)
	// UpdateRoleAssignment writes the state columns of a: status,
	// approved_by/at, granted_at, rejected_by/at, revoke_reason,
	// revoke_requested_by/at, revoked_by/at. The CHECKs map as on insert
	// (the DB is the second layer of the four-eyes rule, 03 §6.1).
	UpdateRoleAssignment(ctx context.Context, a RoleAssignment) error
	// RoleAssignments returns up to q.Limit assignments newest first (id
	// DESC) after q.After; open ones only unless q.IncludeEnded.
	RoleAssignments(ctx context.Context, q RoleQuery) ([]RoleAssignment, error)
	// CountRoleAssignments returns how many users hold role in effect and
	// how many requests of it are pending (k3 talent_auditor limit, last
	// platform_admin; 06 H-33 condition for "in effect").
	CountRoleAssignments(ctx context.Context, role authz.PlatformRole) (inEffect, pending, revokePending int, err error)

	// TOTP returns the TOTP row of userID; found=false when none.
	TOTP(ctx context.Context, userID uuid.UUID) (t TOTP, found bool, err error)
	// PutTOTP writes a new, unconfirmed enrollment, replacing an earlier
	// unconfirmed one. A confirmed row stays: ErrMFAAlreadyEnabled.
	PutTOTP(ctx context.Context, t TOTP) error
	// ConfirmTOTP confirms the open enrollment of userID at at and records
	// step as used; confirmed=false when there is no open enrollment.
	ConfirmTOTP(ctx context.Context, userID uuid.UUID, step int64, at time.Time) (confirmed bool, err error)
	// UseTOTPStep is the replay guard (05 §14.1): it records step as the
	// last used step of a confirmed TOTP only if no step ≥ step was used
	// before, atomically. used=false: replay (or no confirmed TOTP).
	UseTOTPStep(ctx context.Context, userID uuid.UUID, step int64) (used bool, err error)
	// DeleteTOTP deletes the TOTP row of userID (disable, admin reset,
	// erasure) and returns how many rows it deleted.
	DeleteTOTP(ctx context.Context, userID uuid.UUID) (int, error)

	// ReplaceRecoveryCodes deletes every recovery code of userID and
	// stores hashes (SHA-256, 32 bytes each) as the new set.
	ReplaceRecoveryCodes(ctx context.Context, userID uuid.UUID, hashes [][]byte, at time.Time) error
	// UseRecoveryCode marks the unused code of userID with hash as used;
	// used=false when there is none (unknown or already used: single use).
	UseRecoveryCode(ctx context.Context, userID uuid.UUID, hash []byte, at time.Time) (used bool, err error)
	// RecoveryCodesLeft returns how many unused recovery codes userID has.
	RecoveryCodesLeft(ctx context.Context, userID uuid.UUID) (int, error)
	// DeleteRecoveryCodes deletes every recovery code of userID.
	DeleteRecoveryCodes(ctx context.Context, userID uuid.UUID) (int, error)

	// InsertAccountAction writes a four-eyes account action request (06
	// H-47). A second pending request of (target, action) is
	// ErrAccountActionOpen; a self request ErrForbidden.
	InsertAccountAction(ctx context.Context, r AccountActionRequest) error
	// LockOpenAccountAction returns the pending request of (target,
	// action) locked for the transaction; found=false when none.
	LockOpenAccountAction(ctx context.Context, target uuid.UUID, action AccountActionKind) (r AccountActionRequest, found bool, err error)
	// UpdateAccountAction writes status, approved_by and approved_at of a
	// request; approved_by = requested_by is ErrFourEyes (DB CHECK).
	UpdateAccountAction(ctx context.Context, r AccountActionRequest) error
	// AccountActionsOf returns every request against userID, oldest first
	// (KVKK export).
	AccountActionsOf(ctx context.Context, userID uuid.UUID) ([]AccountActionRequest, error)

	EduStore
}

// EduStore is the edu_verifications part of Store (02 §5.4; BE-018). The
// partial unique indexes are the arbiters of the concurrent cases: a second
// pending row of a user is ErrEduPending, a second open row of a .edu.tr
// address ErrEduEmailTaken, an unknown university ErrUniversityUnknown.
type EduStore interface {
	// InsertEduVerification writes a new verification.
	InsertEduVerification(ctx context.Context, v EduRecord) error
	// EduVerification returns a verification or ErrNotFound.
	EduVerification(ctx context.Context, id uuid.UUID) (EduRecord, error)
	// LockEduVerification returns a verification locked for the
	// transaction, or ErrNotFound.
	LockEduVerification(ctx context.Context, id uuid.UUID) (EduRecord, error)
	// LockOpenEduVerifications returns the pending and verified rows of
	// userID (at most one of each), locked for the transaction.
	LockOpenEduVerifications(ctx context.Context, userID uuid.UUID) ([]EduRecord, error)
	// UpdateEduVerification writes the state columns of v: status,
	// university_id, document_file_id, verified_at, expires_at,
	// reviewed_by, review_note (updated_at is stamped). The CHECKs and
	// indexes map as on insert; a missing row is ErrNotFound.
	UpdateEduVerification(ctx context.Context, v EduRecord) error
	// EduVerificationsOf returns every verification of userID, newest
	// first (getMyEduVerification, KVKK export).
	EduVerificationsOf(ctx context.Context, userID uuid.UUID) ([]EduRecord, error)
	// LatestEduOutcome returns the newest verified, expired or revoked row
	// of userID (the Actor's StudentStatus: a verified row is always the
	// newest of these); found=false when the user was never verified.
	LatestEduOutcome(ctx context.Context, userID uuid.UUID) (v EduRecord, found bool, err error)
	// ActiveEduVerifications returns the verified rows of userIDs whose
	// expires_at is after now.
	ActiveEduVerifications(ctx context.Context, userIDs []uuid.UUID, now time.Time) ([]EduRecord, error)
	// OpenEduVerificationOfEmail returns the pending or verified row of a
	// normalized .edu.tr address; found=false when the address is free.
	OpenEduVerificationOfEmail(ctx context.Context, normalized string) (v EduRecord, found bool, err error)
	// EduVerificationOfFile returns the verification that references a
	// document file; found=false when none.
	EduVerificationOfFile(ctx context.Context, fileID uuid.UUID) (v EduRecord, found bool, err error)
	// EduQueue returns up to q.Limit document verifications in q.Status
	// after q.After (pending: id ASC; others: id DESC).
	EduQueue(ctx context.Context, q EduQuery) ([]EduRecord, error)
	// ExpireEduVerifications turns up to limit verified rows whose
	// expires_at ≤ now into expired and returns them (identity.edu_expire;
	// concurrent runs skip each other's rows).
	ExpireEduVerifications(ctx context.Context, now time.Time, limit int) ([]EduRecord, error)
	// EduExpiringBetween returns up to limit verified rows with from ≤
	// expires_at < to, by id after after (renewal reminders).
	EduExpiringBetween(ctx context.Context, from, to time.Time, after *uuid.UUID, limit int) ([]EduRecord, error)
	// DeleteEduVerification deletes one verification (found=false: none).
	DeleteEduVerification(ctx context.Context, id uuid.UUID) (found bool, err error)
	// DeleteUserEduVerifications deletes every verification of userID
	// (erasure) and returns the deleted rows.
	DeleteUserEduVerifications(ctx context.Context, userID uuid.UUID) ([]EduRecord, error)
}
