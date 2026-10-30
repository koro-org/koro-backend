package identity

import (
	"context"
	"time"
	"uuid"
)

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
