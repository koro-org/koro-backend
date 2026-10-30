package identity

import (
	"context"
	"time"
	"uuid"
)

// emailTokenStore is the email_tokens rows (02 §5.3): verification,
// password reset and e-mail change tokens, their single use and the
// cleanup job (jobs.go).
type emailTokenStore interface {
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
}
