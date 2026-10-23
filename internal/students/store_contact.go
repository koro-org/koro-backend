package students

import (
	"context"
	"time"
	"uuid"
)

// grantContacts is the per-company contact sharing of the level 3 grants
// (profile_access_grants.contact_shared_at, 000136; BE-089, T-345): the
// student shares the e-mail with one company at a time, never with every
// grant at once. Writes run in the caller's transaction, under LockGrants.
type grantContacts interface {
	// ShareGrantContact marks the open grant id as sharing the e-mail
	// since at; false when it is not open or already shares it.
	ShareGrantContact(ctx context.Context, id uuid.UUID, at time.Time) (bool, error)
	// UnshareGrantContact stops studentID's grant id sharing the e-mail;
	// false when it did not share it (or is not the student's).
	UnshareGrantContact(ctx context.Context, studentID, id uuid.UUID) (bool, error)
	// ClearGrantContacts stops every grant of studentID sharing the
	// e-mail, or only those shared at or before sharedBefore when it is
	// set, and returns the grants it changed (as they were).
	ClearGrantContacts(ctx context.Context, studentID uuid.UUID, sharedBefore *time.Time) ([]AccessGrant, error)
}
