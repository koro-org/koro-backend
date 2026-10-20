package students

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/civil"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// NewGormStore returns the PostgreSQL store over the students tables
// (02 §9.1–9.6).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// Column lists.
const (
	profileCols = `user_id, university_id, department_id, degree_level, class_year, expected_graduation, status,
		graduated_at, headline, about, city, avatar_file_id, completed_sections, audience_keys, audience_eligible,
		audience_refreshed_at, created_at, updated_at`
	privacyCols    = `user_id, discoverable, show_contact_on_grant, show_clubs_on_grant, activity_ranking_choice, created_at, updated_at`
	experienceCols = `id, user_id, kind, title, organization_name, start_month, end_month, description, created_at, updated_at`
)

// constraints maps the named constraints of 02 §9 onto module errors.
var constraints = db.Constraints{
	"student_profiles_department_id_fkey":   ErrUnknownDepartment,
	"student_profiles_university_id_fkey":   problem.ErrNotFound,
	"student_profiles_user_id_fkey":         problem.ErrNotFound,
	"student_profiles_avatar_file_id_fkey":  problem.ErrNotFound,
	"student_privacy_settings_user_id_fkey": problem.ErrNotFound,
	"student_skills_skill_id_fkey":          problem.ErrValidation,
	"student_skills_user_id_fkey":           problem.ErrNotFound,
	"student_experiences_user_id_fkey":      problem.ErrNotFound,
	"student_languages_user_id_fkey":        problem.ErrNotFound,
	"student_cvs_user_id_fkey":              problem.ErrNotFound,
	"student_cvs_file_id_fkey":              problem.ErrNotFound,
	"student_cvs_file_id_uq":                problem.ErrConflict,
	"student_cvs_current_uq":                problem.ErrConflict,

	"profile_access_grants_student_user_id_fkey":         problem.ErrNotFound,
	"profile_access_grants_company_org_id_org_kind_fkey": problem.ErrNotFound, // unknown org or not a company
	"profile_access_grants_source_uq":                    problem.ErrConflict,
	"profile_access_grants_revoke_reason_chk":            problem.ErrValidation,
	"profile_access_grants_revoked_chk":                  problem.ErrValidation,
	"profile_access_grants_source_chk":                   problem.ErrValidation,
	"profile_access_grants_revoked_by_kind_chk":          problem.ErrValidation,
}

func wrap(op string, err error) error {
	return fmt.Errorf("students: %s: %w", op, constraints.Translate(err))
}

func takeProfile(op string, q *gorm.DB) (Profile, error) {
	var p Profile
	err := q.Select(profileCols).Take(&p).Error
	switch {
	case db.IsNotFound(err):
		return Profile{}, ErrNotFound
	case err != nil:
		return Profile{}, wrap(op, err)
	}
	return p, nil
}

// GetProfile implements Store.
//
// cross-org: a student profile belongs to no org (02 §9.1).
func (s gormStore) GetProfile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return takeProfile("get profile", db.Conn(ctx, s.orm).Where("user_id = ?", userID))
}

// LockProfile implements Store.
//
// cross-org: a student profile belongs to no org (02 §9.1).
func (s gormStore) LockProfile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	q := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).Where("user_id = ?", userID)
	return takeProfile("lock profile", q)
}

func profileArgs(p Profile) map[string]any {
	keys, sections := p.AudienceKeys, p.CompletedSections
	if keys == nil {
		keys = db.TextArray{}
	}
	if sections == nil {
		sections = db.TextArray{}
	}
	return map[string]any{
		"user_id": p.UserID, "university_id": p.UniversityID, "department_id": p.DepartmentID,
		"degree_level": string(p.DegreeLevel), "class_year": p.ClassYear, "expected_graduation": p.ExpectedGraduation,
		"status": string(p.Status), "graduated_at": p.GraduatedAt, "headline": p.Headline, "about": p.About,
		"city": p.City, "avatar_file_id": p.AvatarFileID, "completed_sections": sections, "audience_keys": keys,
		"audience_eligible": p.AudienceEligible, "audience_refreshed_at": p.AudienceRefreshedAt,
	}
}

// InsertProfile implements Store.
//
// cross-org: a student profile belongs to no org (02 §9.1).
func (s gormStore) InsertProfile(ctx context.Context, p Profile) (Profile, bool, error) {
	var rows []Profile
	err := db.Conn(ctx, s.orm).Raw(`INSERT INTO student_profiles
		(user_id, university_id, department_id, degree_level, class_year, expected_graduation, status, graduated_at,
		 headline, about, city, avatar_file_id, completed_sections, audience_keys, audience_eligible, audience_refreshed_at)
		VALUES ( @user_id, @university_id, @department_id, @degree_level, @class_year, @expected_graduation, @status,
		 @graduated_at, @headline, @about, @city, @avatar_file_id, @completed_sections, @audience_keys,
		 @audience_eligible, @audience_refreshed_at)
		ON CONFLICT (user_id) DO NOTHING
		RETURNING `+profileCols, profileArgs(p)).Scan(&rows).Error
	if err != nil {
		return Profile{}, false, wrap("insert profile", err)
	}
	if len(rows) == 0 {
		return Profile{}, false, nil
	}
	return rows[0], true, nil
}

// UpdateProfile implements Store.
//
// cross-org: a student profile belongs to no org (02 §9.1).
func (s gormStore) UpdateProfile(ctx context.Context, p Profile) (Profile, error) {
	var rows []Profile
	err := db.Conn(ctx, s.orm).Raw(`UPDATE student_profiles SET
		university_id = @university_id, department_id = @department_id, degree_level = @degree_level,
		class_year = @class_year, expected_graduation = @expected_graduation, status = @status,
		graduated_at = @graduated_at, headline = @headline, about = @about, city = @city,
		avatar_file_id = @avatar_file_id, completed_sections = @completed_sections, audience_keys = @audience_keys,
		audience_eligible = @audience_eligible, audience_refreshed_at = @audience_refreshed_at
		WHERE user_id = @user_id
		RETURNING `+profileCols, profileArgs(p)).Scan(&rows).Error
	if err != nil {
		return Profile{}, wrap("update profile", err)
	}
	if len(rows) == 0 {
		return Profile{}, ErrNotFound
	}
	return rows[0], nil
}

// SetAudience implements Store.
//
// cross-org: a student profile belongs to no org (02 §9.1).
func (s gormStore) SetAudience(ctx context.Context, userID uuid.UUID, keys []string, eligible bool, at time.Time) error {
	err := db.Conn(ctx, s.orm).Exec(`UPDATE student_profiles
		SET audience_keys = @keys, audience_eligible = @eligible, audience_refreshed_at = @at
		WHERE user_id = @user_id`, map[string]any{
		"keys": db.TextArray(keys), "eligible": eligible, "at": at, "user_id": userID,
	}).Error
	if err != nil {
		return wrap("set audience", err)
	}
	return nil
}

// RemoveAudienceKey implements Store.
//
// cross-org: the audience keys of every profile (a club became sensitive,
// T-80).
func (s gormStore) RemoveAudienceKey(ctx context.Context, key string, at time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE student_profiles
		SET audience_keys = array_remove(audience_keys, @key), audience_refreshed_at = @at
		WHERE user_id IN (SELECT user_id FROM student_profiles WHERE audience_keys @> @keys LIMIT @limit)`,
		map[string]any{"key": key, "keys": db.TextArray{key}, "at": at, "limit": limit})
	if res.Error != nil {
		return 0, wrap("remove audience key", res.Error)
	}
	return int(res.RowsAffected), nil
}

// Profiles implements Store.
//
// cross-org: student profiles belong to no org (02 §9.1).
func (s gormStore) Profiles(ctx context.Context, ids []uuid.UUID) ([]Profile, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []Profile
	if err := db.Conn(ctx, s.orm).Select(profileCols).Where("user_id IN ?", ids).Find(&out).Error; err != nil {
		return nil, wrap("profiles", err)
	}
	return out, nil
}

// ProfilesAfter implements Store.
//
// cross-org: the nightly walk over every profile.
func (s gormStore) ProfilesAfter(ctx context.Context, after uuid.UUID, limit int) ([]Profile, error) {
	var out []Profile
	err := db.Conn(ctx, s.orm).Select(profileCols).Where("user_id > ?", after).Order("user_id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, wrap("profiles after", err)
	}
	return out, nil
}

// GraduationCandidates implements Store.
//
// cross-org: the nightly walk over every active profile.
func (s gormStore) GraduationCandidates(ctx context.Context, today civil.Date, after uuid.UUID, limit int) ([]Profile, error) {
	var out []Profile
	err := db.Conn(ctx, s.orm).Select(profileCols).
		Where("status = 'active' AND expected_graduation < ? AND user_id > ?", today, after).
		Order("user_id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, wrap("graduation candidates", err)
	}
	return out, nil
}

// GraduatedAmong implements Store.
//
// cross-org: student profiles belong to no org (02 §9.1).
func (s gormStore) GraduatedAmong(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []uuid.UUID
	err := db.Conn(ctx, s.orm).Model(&Profile{}).Where("status = 'graduated' AND user_id IN ?", ids).
		Order("user_id").Pluck("user_id", &out).Error
	if err != nil {
		return nil, wrap("graduated among", err)
	}
	return out, nil
}

// AvatarIDs implements Store.
//
// cross-org: student profiles belong to no org (02 §9.1).
func (s gormStore) AvatarIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := map[uuid.UUID]uuid.UUID{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		UserID       uuid.UUID `gorm:"type:uuid;column:user_id"`
		AvatarFileID uuid.UUID `gorm:"type:uuid;column:avatar_file_id"`
	}
	err := db.Conn(ctx, s.orm).Model(&Profile{}).Select("user_id, avatar_file_id").
		Where("avatar_file_id IS NOT NULL AND user_id IN ?", ids).Scan(&rows).Error
	if err != nil {
		return nil, wrap("avatar ids", err)
	}
	for _, r := range rows {
		out[r.UserID] = r.AvatarFileID
	}
	return out, nil
}

// DeleteProfile implements Store.
//
// cross-org: a student profile belongs to no org (02 §9.1).
func (s gormStore) DeleteProfile(ctx context.Context, userID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Where("user_id = ?", userID).Delete(&Profile{})
	if res.Error != nil {
		return false, wrap("delete profile", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// GetPrivacy implements Store.
//
// cross-org: privacy settings belong to no org (02 §9.2).
func (s gormStore) GetPrivacy(ctx context.Context, userID uuid.UUID) (Privacy, error) {
	var p Privacy
	err := db.Conn(ctx, s.orm).Select(privacyCols).Where("user_id = ?", userID).Take(&p).Error
	switch {
	case db.IsNotFound(err):
		return Privacy{}, ErrNotFound
	case err != nil:
		return Privacy{}, wrap("get privacy", err)
	}
	return p, nil
}

// LockPrivacy implements Store.
//
// cross-org: privacy settings belong to no org (02 §9.2).
func (s gormStore) LockPrivacy(ctx context.Context, userID uuid.UUID) (Privacy, error) {
	conn := db.Conn(ctx, s.orm)
	if err := conn.Exec(`INSERT INTO student_privacy_settings (user_id) VALUES ( ?) ON CONFLICT (user_id) DO NOTHING`,
		userID).Error; err != nil {
		return Privacy{}, wrap("lock privacy", err)
	}
	var p Privacy
	err := conn.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).Select(privacyCols).
		Where("user_id = ?", userID).Take(&p).Error
	if err != nil {
		return Privacy{}, wrap("lock privacy", err)
	}
	return p, nil
}

// UpdatePrivacy implements Store.
//
// cross-org: privacy settings belong to no org (02 §9.2).
func (s gormStore) UpdatePrivacy(ctx context.Context, p Privacy) (Privacy, error) {
	var rows []Privacy
	err := db.Conn(ctx, s.orm).Raw(`UPDATE student_privacy_settings SET
		discoverable = @discoverable, show_contact_on_grant = @contact, show_clubs_on_grant = @clubs,
		activity_ranking_choice = @ranking
		WHERE user_id = @user_id
		RETURNING `+privacyCols, map[string]any{
		"discoverable": p.Discoverable, "contact": p.ShowContactOnGrant, "clubs": p.ShowClubsOnGrant,
		"ranking": string(p.ActivityRankingChoice), "user_id": p.UserID,
	}).Scan(&rows).Error
	if err != nil {
		return Privacy{}, wrap("update privacy", err)
	}
	if len(rows) == 0 {
		return Privacy{}, ErrNotFound
	}
	return rows[0], nil
}

// PrivacyFlagged implements Store.
//
// cross-org: the nightly reconciliation over every settings row.
func (s gormStore) PrivacyFlagged(ctx context.Context, after uuid.UUID, limit int) ([]Privacy, error) {
	var out []Privacy
	err := db.Conn(ctx, s.orm).Select(privacyCols).
		Where("(discoverable OR show_contact_on_grant) AND user_id > ?", after).Order("user_id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, wrap("privacy flagged", err)
	}
	return out, nil
}

// DeletePrivacy implements Store.
//
// cross-org: privacy settings belong to no org (02 §9.2).
func (s gormStore) DeletePrivacy(ctx context.Context, userID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Where("user_id = ?", userID).Delete(&Privacy{})
	if res.Error != nil {
		return false, wrap("delete privacy", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// Skills implements Store.
//
// cross-org: a student's sections belong to no org (02 §9.3).
func (s gormStore) Skills(ctx context.Context, userID uuid.UUID) ([]Skill, error) {
	var out []Skill
	err := db.Conn(ctx, s.orm).Where("user_id = ?", userID).Order("created_at, skill_id").Find(&out).Error
	if err != nil {
		return nil, wrap("skills", err)
	}
	return out, nil
}

// ReplaceSkills implements Store. The replacement is atomic (a savepoint
// inside the caller's transaction): a refused item changes nothing.
//
// cross-org: a student's sections belong to no org (02 §9.3).
func (s gormStore) ReplaceSkills(ctx context.Context, userID uuid.UUID, items []Skill) ([]Skill, error) {
	ids := make([]uuid.UUID, len(items))
	for i, it := range items {
		ids[i] = it.SkillID
	}
	err := db.Conn(ctx, s.orm).Transaction(func(tx *gorm.DB) error {
		q := tx.Where("user_id = ?", userID)
		if len(ids) > 0 {
			q = q.Where("skill_id NOT IN ?", ids)
		}
		if err := q.Delete(&Skill{}).Error; err != nil {
			return err
		}
		for _, it := range items {
			if err := tx.Exec(`INSERT INTO student_skills (user_id, skill_id, level) VALUES ( ?, ?, ?)
				ON CONFLICT (user_id, skill_id) DO UPDATE SET level = EXCLUDED.level`, userID, it.SkillID, it.Level).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, wrap("replace skills", err)
	}
	return s.Skills(ctx, userID)
}

// Languages implements Store.
//
// cross-org: a student's sections belong to no org (02 §9.5).
func (s gormStore) Languages(ctx context.Context, userID uuid.UUID) ([]Language, error) {
	var out []Language
	err := db.Conn(ctx, s.orm).Where("user_id = ?", userID).Order("created_at, language_code").Find(&out).Error
	if err != nil {
		return nil, wrap("languages", err)
	}
	return out, nil
}

// ReplaceLanguages implements Store (atomic, as ReplaceSkills).
//
// cross-org: a student's sections belong to no org (02 §9.5).
func (s gormStore) ReplaceLanguages(ctx context.Context, userID uuid.UUID, items []Language) ([]Language, error) {
	codes := make([]string, len(items))
	for i, it := range items {
		codes[i] = it.LanguageCode
	}
	err := db.Conn(ctx, s.orm).Transaction(func(tx *gorm.DB) error {
		q := tx.Where("user_id = ?", userID)
		if len(codes) > 0 {
			q = q.Where("language_code NOT IN ?", codes)
		}
		if err := q.Delete(&Language{}).Error; err != nil {
			return err
		}
		for _, it := range items {
			if err := tx.Exec(`INSERT INTO student_languages (user_id, language_code, level) VALUES ( ?, ?, ?)
				ON CONFLICT (user_id, language_code) DO UPDATE SET level = EXCLUDED.level`,
				userID, it.LanguageCode, string(it.Level)).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, wrap("replace languages", err)
	}
	return s.Languages(ctx, userID)
}

// Experiences implements Store.
//
// cross-org: a student's sections belong to no org (02 §9.4).
func (s gormStore) Experiences(ctx context.Context, userID uuid.UUID) ([]Experience, error) {
	var out []Experience
	err := db.Conn(ctx, s.orm).Select(experienceCols).Where("user_id = ?", userID).
		Order("start_month DESC, id DESC").Find(&out).Error
	if err != nil {
		return nil, wrap("experiences", err)
	}
	return out, nil
}

// GetExperience implements Store.
//
// cross-org: a student's sections belong to no org; the row is addressed
// by its owner too (IDOR).
func (s gormStore) GetExperience(ctx context.Context, userID, id uuid.UUID) (Experience, error) {
	var e Experience
	err := db.Conn(ctx, s.orm).Select(experienceCols).Where("id = ? AND user_id = ?", id, userID).Take(&e).Error
	switch {
	case db.IsNotFound(err):
		return Experience{}, ErrNotFound
	case err != nil:
		return Experience{}, wrap("get experience", err)
	}
	return e, nil
}

func experienceArgs(e Experience) map[string]any {
	return map[string]any{
		"id": e.ID, "user_id": e.UserID, "kind": string(e.Kind), "title": e.Title, "org": e.OrganizationName,
		"start": e.StartMonth, "end": e.EndMonth, "description": e.Description,
	}
}

// InsertExperience implements Store.
//
// cross-org: a student's sections belong to no org (02 §9.4).
func (s gormStore) InsertExperience(ctx context.Context, e Experience) (Experience, error) {
	var rows []Experience
	err := db.Conn(ctx, s.orm).Raw(`INSERT INTO student_experiences
		(id, user_id, kind, title, organization_name, start_month, end_month, description)
		VALUES ( @id, @user_id, @kind, @title, @org, @start, @end, @description)
		RETURNING `+experienceCols, experienceArgs(e)).Scan(&rows).Error
	if err != nil {
		return Experience{}, wrap("insert experience", err)
	}
	if len(rows) == 0 {
		return Experience{}, ErrNotFound
	}
	return rows[0], nil
}

// UpdateExperience implements Store.
//
// cross-org: a student's sections belong to no org; the row is addressed
// by its owner too (IDOR).
func (s gormStore) UpdateExperience(ctx context.Context, e Experience) (Experience, error) {
	var rows []Experience
	err := db.Conn(ctx, s.orm).Raw(`UPDATE student_experiences SET
		kind = @kind, title = @title, organization_name = @org, start_month = @start, end_month = @end,
		description = @description
		WHERE id = @id AND user_id = @user_id
		RETURNING `+experienceCols, experienceArgs(e)).Scan(&rows).Error
	if err != nil {
		return Experience{}, wrap("update experience", err)
	}
	if len(rows) == 0 {
		return Experience{}, ErrNotFound
	}
	return rows[0], nil
}

// DeleteExperience implements Store.
//
// cross-org: a student's sections belong to no org; the row is addressed
// by its owner too (IDOR).
func (s gormStore) DeleteExperience(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Where("id = ? AND user_id = ?", id, userID).Delete(&Experience{})
	if res.Error != nil {
		return false, wrap("delete experience", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// DeleteSections implements Store.
//
// cross-org: a student's sections belong to no org (02 §9.3–9.5).
func (s gormStore) DeleteSections(ctx context.Context, userID uuid.UUID) (int, error) {
	conn := db.Conn(ctx, s.orm)
	n := 0
	for _, m := range []any{&Skill{}, &Experience{}, &Language{}} {
		res := conn.Where("user_id = ?", userID).Delete(m)
		if res.Error != nil {
			return n, wrap("delete sections", res.Error)
		}
		n += int(res.RowsAffected)
	}
	return n, nil
}
