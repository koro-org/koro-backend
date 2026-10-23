package students

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// ShareGrantContact implements Store.
//
// cross-org: the grant is addressed by its own ID (its company_org_id is
// the row's scope; the caller found it by student and company).
func (s gormStore) ShareGrantContact(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE profile_access_grants SET contact_shared_at = @at
		WHERE id = @id AND revoked_at IS NULL AND contact_shared_at IS NULL`, map[string]any{"id": id, "at": at})
	if res.Error != nil {
		return false, wrap("share grant contact", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// UnshareGrantContact implements Store.
//
// cross-org: the student's own grant, addressed by its student (IDOR).
func (s gormStore) UnshareGrantContact(ctx context.Context, studentID, id uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE profile_access_grants SET contact_shared_at = NULL
		WHERE id = @id AND student_user_id = @student AND contact_shared_at IS NOT NULL`,
		map[string]any{"id": id, "student": studentID})
	if res.Error != nil {
		return false, wrap("unshare grant contact", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// ClearGrantContacts implements Store. The rows are returned as they were
// (the old contact_shared_at), for the audit.
//
// cross-org: every grant of one student, whatever the company.
func (s gormStore) ClearGrantContacts(ctx context.Context, studentID uuid.UUID, sharedBefore *time.Time) ([]AccessGrant, error) {
	args := map[string]any{"student": studentID}
	cond := ""
	if sharedBefore != nil {
		args["before"], cond = *sharedBefore, " AND g.contact_shared_at <= @before"
	}
	var rows []AccessGrant
	err := db.Conn(ctx, s.orm).Raw(`UPDATE profile_access_grants AS g SET contact_shared_at = NULL
		FROM profile_access_grants AS old
		WHERE g.id = old.id AND g.student_user_id = @student AND g.contact_shared_at IS NOT NULL`+cond+`
		RETURNING old.id, old.student_user_id, old.company_org_id, old.source, old.source_id, old.granted_at,
			old.suspended_at, old.resumed_at, old.revoked_at, old.revoked_by_kind, old.revoke_reason,
			old.contact_shared_at, old.created_at, old.updated_at`, args).Scan(&rows).Error
	if err != nil {
		return nil, wrap("clear grant contacts", err)
	}
	return rows, nil
}
