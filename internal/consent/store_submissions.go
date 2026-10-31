package consent

import (
	"context"
	"time"
	"uuid"
)

// submissionStore is iys_submissions, the İYS submission of a consent
// (consent.submit_iys,
// consent.iys_reconcile).
type submissionStore interface {
	// InsertSubmission writes a pending İYS submission; a second one for
	// the same consent is a conflict (iys_submissions_consent_id_uq). The
	// store stamps created_at and updated_at (s's are ignored, BE-069).
	InsertSubmission(ctx context.Context, s Submission) error
	// SubmissionOf returns the submission of a consent or ErrNotFound.
	SubmissionOf(ctx context.Context, consentID uuid.UUID) (Submission, error)
	// LockSubmission returns a submission locked for the transaction.
	LockSubmission(ctx context.Context, id uuid.UUID) (Submission, error)
	// UpdateSubmission writes status, iys_transaction_id, attempts,
	// last_error and sent_at (updated_at is set by the trigger).
	UpdateSubmission(ctx context.Context, s Submission) error
	// DueSubmissions returns up to limit pending or failed submissions last
	// updated before the cutoff, with an ID greater than after, in ID order
	// (consent.iys_reconcile).
	DueSubmissions(ctx context.Context, before time.Time, after uuid.UUID, limit int) ([]Submission, error)
}
