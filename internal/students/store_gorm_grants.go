package students

import (
	"context"
	"strconv"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

const grantCols = `id, student_user_id, company_org_id, source, source_id, granted_at, revoked_at, revoked_by_kind,
	revoke_reason, created_at, updated_at`

func takeGrant(op string, q *gorm.DB) (AccessGrant, error) {
	var g AccessGrant
	err := q.Select(grantCols).Take(&g).Error
	switch {
	case db.IsNotFound(err):
		return AccessGrant{}, ErrNotFound
	case err != nil:
		return AccessGrant{}, wrap(op, err)
	}
	return g, nil
}

// grantKeyArgs adds the keyset position of a newest-first grant list.
func grantKeyArgs(args map[string]any, after *GrantKey) string {
	if after == nil {
		return ""
	}
	args["after_at"], args["after_id"] = after.GrantedAt, after.ID
	return ` AND (granted_at < @after_at OR (granted_at = @after_at AND id < @after_id))`
}

// LockGrants implements Store: a transaction-scoped advisory lock keyed by
// the student, so two concurrent grants of one company (two contact
// requests accepted at once) do not both pass the "already granted" check.
//
// cross-org: the lock is the student's, whatever the company.
func (s gormStore) LockGrants(ctx context.Context, studentID uuid.UUID) error {
	err := db.Conn(ctx, s.orm).Exec(`SELECT pg_advisory_xact_lock(hashtextextended( ?, 0))`,
		"students.access_grants:"+studentID.String()).Error
	if err != nil {
		return wrap("lock grants", err)
	}
	return nil
}

// InsertGrant implements Store. created_at and updated_at come from the
// column defaults (BE-069); granted_at is the service's business time.
//
// cross-org: company_org_id is the row's own scope (the composite FK binds
// it to a company org).
func (s gormStore) InsertGrant(ctx context.Context, g AccessGrant) (AccessGrant, bool, error) {
	var rows []AccessGrant
	err := db.Conn(ctx, s.orm).Raw(`INSERT INTO profile_access_grants
		(id, student_user_id, company_org_id, source, source_id, granted_at)
		VALUES ( @id, @student, @company, @source, @source_id, @granted_at)
		ON CONFLICT (source, source_id) DO NOTHING
		RETURNING `+grantCols, map[string]any{
		"id": g.ID, "student": g.StudentUserID, "company": g.CompanyOrgID, "source": string(g.Source),
		"source_id": g.SourceID, "granted_at": g.GrantedAt,
	}).Scan(&rows).Error
	if err != nil {
		return AccessGrant{}, false, wrap("insert grant", err)
	}
	if len(rows) == 0 {
		return AccessGrant{}, false, nil
	}
	return rows[0], true, nil
}

// GrantBySource implements Store.
//
// cross-org: a source request names one grant, whatever the company.
func (s gormStore) GrantBySource(ctx context.Context, source GrantSource, sourceID uuid.UUID) (AccessGrant, error) {
	return takeGrant("grant by source", db.Conn(ctx, s.orm).Where("source = ? AND source_id = ?", string(source), sourceID))
}

// ActiveGrant implements Store.
func (s gormStore) ActiveGrant(ctx context.Context, studentID, companyOrgID uuid.UUID) (AccessGrant, error) {
	var rows []AccessGrant
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+grantCols+` FROM profile_access_grants
		WHERE company_org_id = @org_id AND student_user_id = @student AND revoked_at IS NULL
		ORDER BY granted_at, id LIMIT 1`, map[string]any{"org_id": companyOrgID, "student": studentID}).Scan(&rows).Error
	if err != nil {
		return AccessGrant{}, wrap("active grant", err)
	}
	if len(rows) == 0 {
		return AccessGrant{}, ErrNotFound
	}
	return rows[0], nil
}

// GetGrant implements Store.
//
// cross-org: the student's own grant, addressed by its student (IDOR).
func (s gormStore) GetGrant(ctx context.Context, studentID, id uuid.UUID) (AccessGrant, error) {
	return takeGrant("get grant", db.Conn(ctx, s.orm).Where("id = ? AND student_user_id = ?", id, studentID))
}

// RevokeGrants implements Store.
func (s gormStore) RevokeGrants(ctx context.Context, studentID, companyOrgID uuid.UUID, by RevokedBy, reason *string,
	at time.Time) ([]AccessGrant, error) {
	var rows []AccessGrant
	err := db.Conn(ctx, s.orm).Raw(`UPDATE profile_access_grants
		SET revoked_at = @at, revoked_by_kind = @by, revoke_reason = @reason
		WHERE company_org_id = @org_id AND student_user_id = @student AND revoked_at IS NULL
		RETURNING `+grantCols, map[string]any{
		"at": at, "by": string(by), "reason": reason, "org_id": companyOrgID, "student": studentID,
	}).Scan(&rows).Error
	if err != nil {
		return nil, wrap("revoke grants", err)
	}
	return rows, nil
}

// StudentGrants implements Store.
//
// cross-org: the student's own grants of every company (listMyAccessGrants).
func (s gormStore) StudentGrants(ctx context.Context, studentID uuid.UUID, after *GrantKey, limit int) ([]AccessGrant, error) {
	args := map[string]any{"student": studentID, "limit": limit}
	var rows []AccessGrant
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+grantCols+` FROM profile_access_grants
		WHERE student_user_id = @student AND revoked_at IS NULL`+grantKeyArgs(args, after)+`
		ORDER BY granted_at DESC, id DESC LIMIT @limit`, args).Scan(&rows).Error
	if err != nil {
		return nil, wrap("student grants", err)
	}
	return rows, nil
}

// CompanyGrants implements Store.
func (s gormStore) CompanyGrants(ctx context.Context, companyOrgID uuid.UUID, after *GrantKey, limit int) ([]AccessGrant, error) {
	args := map[string]any{"org_id": companyOrgID, "limit": limit}
	var rows []AccessGrant
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+grantCols+` FROM profile_access_grants
		WHERE company_org_id = @org_id AND revoked_at IS NULL`+grantKeyArgs(args, after)+`
		ORDER BY granted_at DESC, id DESC LIMIT @limit`, args).Scan(&rows).Error
	if err != nil {
		return nil, wrap("company grants", err)
	}
	return rows, nil
}

// AllGrants implements Store.
//
// cross-org: the data subject's grants of every company (KVKK export).
func (s gormStore) AllGrants(ctx context.Context, studentID uuid.UUID) ([]AccessGrant, error) {
	var rows []AccessGrant
	err := db.Conn(ctx, s.orm).Select(grantCols).Where("student_user_id = ?", studentID).
		Order("granted_at, id").Find(&rows).Error
	if err != nil {
		return nil, wrap("all grants", err)
	}
	return rows, nil
}

// DeleteGrants implements Store.
//
// cross-org: the data subject's grants of every company (account erasure).
func (s gormStore) DeleteGrants(ctx context.Context, studentID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Where("student_user_id = ?", studentID).Delete(&AccessGrant{})
	if res.Error != nil {
		return 0, wrap("delete grants", res.Error)
	}
	return int(res.RowsAffected), nil
}

// CountRevokedGrantsBefore implements Store.
//
// cross-org: retention spans every grant.
func (s gormStore) CountRevokedGrantsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM profile_access_grants WHERE revoked_at < @cutoff`,
		map[string]any{"cutoff": cutoff}).Scan(&n).Error; err != nil {
		return 0, wrap("count revoked grants", err)
	}
	return int(n), nil
}

// PurgeRevokedGrants implements Store.
//
// cross-org: retention spans every grant.
func (s gormStore) PurgeRevokedGrants(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM profile_access_grants WHERE id IN (
		SELECT id FROM profile_access_grants WHERE revoked_at < @cutoff
		ORDER BY revoked_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`, map[string]any{"cutoff": cutoff, "limit": limit})
	if res.Error != nil {
		return 0, wrap("purge revoked grants", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DiscoveryCandidates implements Store. The filters are bound as arrays
// (= ANY): OR inside a filter, AND across filters (02 §9.1 semantics).
//
// cross-org: discovery spans every discoverable student (01 §4.16).
func (s gormStore) DiscoveryCandidates(ctx context.Context, q DiscoveryQuery, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	args := map[string]any{"after": after, "limit": limit}
	where := `ps.discoverable AND sp.status = 'active' AND sp.user_id > @after`
	if len(q.UniversityIDs) > 0 {
		where += ` AND sp.university_id = ANY( @unis)`
		args["unis"] = db.UUIDArray(q.UniversityIDs)
	}
	if len(q.DepartmentIDs) > 0 {
		where += ` AND sp.department_id = ANY( @depts)`
		args["depts"] = db.UUIDArray(q.DepartmentIDs)
	}
	if len(q.ClassYears) > 0 {
		years := make(db.TextArray, len(q.ClassYears))
		for i, y := range q.ClassYears {
			years[i] = strconv.Itoa(int(y))
		}
		where += ` AND sp.class_year = ANY( @years)`
		args["years"] = years
	}
	if q.DegreeLevel != "" {
		where += ` AND sp.degree_level = @degree`
		args["degree"] = string(q.DegreeLevel)
	}
	if len(q.SkillIDs) > 0 {
		where += ` AND EXISTS (SELECT 1 FROM student_skills k WHERE k.user_id = sp.user_id AND k.skill_id = ANY( @skills))`
		args["skills"] = db.UUIDArray(q.SkillIDs)
	}
	if len(q.LanguageCodes) > 0 {
		where += ` AND EXISTS (SELECT 1 FROM student_languages l WHERE l.user_id = sp.user_id AND l.language_code = ANY( @langs))`
		args["langs"] = db.TextArray(q.LanguageCodes)
	}
	var ids []uuid.UUID
	err := db.Conn(ctx, s.orm).Raw(`SELECT sp.user_id FROM student_profiles sp
		JOIN student_privacy_settings ps ON ps.user_id = sp.user_id
		WHERE `+where+` ORDER BY sp.user_id LIMIT @limit`, args).Scan(&ids).Error
	if err != nil {
		return nil, wrap("discovery candidates", err)
	}
	return ids, nil
}
