package identity

import (
	"context"
	"time"
	"uuid"
)

// sessionStore is the sessions rows (02 §5.2): login, authentication and
// its sliding idle window, step-up, logout and every revocation, the
// caller's session list and the cleanup job (jobs.go).
type sessionStore interface {
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
	// DeleteEndedSessions deletes up to limit sessions that ended (revoked,
	// or a window closed) before cutoff, and returns how many.
	DeleteEndedSessions(ctx context.Context, cutoff time.Time, limit int) (int, error)
}
