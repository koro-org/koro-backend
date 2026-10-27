package students

import (
	"context"
	"uuid"
)

// privacyStore is the student_privacy_settings rows: discoverable,
// show_contact_on_grant, show_clubs_on_grant and the activity ranking
// choice of a student (a missing row is the defaults).
type privacyStore interface {
	// GetPrivacy returns the settings row of userID or ErrNotFound.
	GetPrivacy(ctx context.Context, userID uuid.UUID) (Privacy, error)
	// LockPrivacy returns the settings row of userID locked for the rest of
	// the transaction, inserting the default row first when there is none.
	LockPrivacy(ctx context.Context, userID uuid.UUID) (Privacy, error)
	// UpdatePrivacy writes the four settings of p and returns the stored
	// row, or ErrNotFound.
	UpdatePrivacy(ctx context.Context, p Privacy) (Privacy, error)
	// PrivacyFlagged returns up to limit settings rows with discoverable or
	// show_contact_on_grant on, with user_id > after, in user_id order.
	PrivacyFlagged(ctx context.Context, after uuid.UUID, limit int) ([]Privacy, error)
	// DeletePrivacy deletes the settings row of userID; false when none.
	DeletePrivacy(ctx context.Context, userID uuid.UUID) (bool, error)
}
