package students

import (
	"context"
	"uuid"
)

// cvStore is the student_cvs rows: a student's uploaded CVs, at most one
// of them current, each addressed together with its owner's user_id.
type cvStore interface {
	// CVs returns the CVs of userID, oldest first (created_at, id).
	CVs(ctx context.Context, userID uuid.UUID) ([]CV, error)
	// GetCV returns userID's CV id, or ErrNotFound (also when id belongs
	// to another user: IDOR).
	GetCV(ctx context.Context, userID, id uuid.UUID) (CV, error)
	// CVByFile returns the CV holding fileID or ErrNotFound (file.rejected).
	CVByFile(ctx context.Context, fileID uuid.UUID) (CV, error)
	// InsertCV writes a new CV (not current) and returns it as stored. A
	// file already used by a CV is a conflict (student_cvs_file_id_uq).
	InsertCV(ctx context.Context, cv CV) (CV, error)
	// SetCurrentCV makes userID's CV id the only current one and returns
	// it, or ErrNotFound when id is not userID's.
	SetCurrentCV(ctx context.Context, userID, id uuid.UUID) (CV, error)
	// DeleteCV deletes userID's CV id; false when it is not there (or not
	// userID's).
	DeleteCV(ctx context.Context, userID, id uuid.UUID) (bool, error)
}
