package identity

import (
	"context"
	"uuid"
)

// userStore is the users rows, the account (02 §5.1): registration, login
// and its lockout columns, the account settings, the e-mail change, the
// admin search and the user directory other modules read.
type userStore interface {
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
}
