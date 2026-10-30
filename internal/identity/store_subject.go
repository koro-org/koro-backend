package identity

import (
	"context"
	"time"
	"uuid"
)

// subjectStore is what only the KVKK provider (privacy.go) does to the rows
// of a data subject: the session export, the deletion of sessions and
// e-mail tokens and the tombstone of the account (01 §3.3). No service
// file holds it.
type subjectStore interface {
	// EraseUser turns the account into a tombstone (status erased, personal
	// columns NULL, erased_at = at). found=false when it already is one.
	EraseUser(ctx context.Context, id uuid.UUID, at time.Time) (erased bool, err error)
	// SessionsOf returns every session row of userID, oldest first (KVKK export).
	SessionsOf(ctx context.Context, userID uuid.UUID) ([]Session, error)
	// DeleteUserSessions deletes every session of userID (erasure).
	DeleteUserSessions(ctx context.Context, userID uuid.UUID) (int, error)
	// DeleteUserEmailTokens deletes every token of userID (erasure).
	DeleteUserEmailTokens(ctx context.Context, userID uuid.UUID) (int, error)
}
