package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// NewGormStore returns the PostgreSQL store over the directory tables
// (02 §8.1–8.5, §8.7).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// Column lists (search_vector is never read).
const (
	uniCols = `id, org_id, org_kind, yok_code, name, short_name, city, type, website_url, is_active,
		created_at, updated_at`
	domainCols = `domain, university_id, domain_kind, is_active, verified_by, created_at, updated_at`
	facCols    = `id, university_id, name, yok_code, is_active, created_at, updated_at`
	deptCols   = `id, university_id, faculty_id, name, yok_program_code, degree_level, duration_years, is_active,
		created_at, updated_at`
	clubCols = `org_id, org_kind, university_id, category, is_sensitive, sensitive_reason, sensitivity_set_by,
		sensitivity_set_at, join_policy, claim_status, founded_year, social_links, created_at, updated_at`
	clubColsC = `c.org_id, c.org_kind, c.university_id, c.category, c.is_sensitive, c.sensitive_reason,
		c.sensitivity_set_by, c.sensitivity_set_at, c.join_policy, c.claim_status, c.founded_year, c.social_links,
		c.created_at, c.updated_at`
	skillCols = `id, slug, name_tr, name_en, category, is_active, created_at, updated_at`
)

// nameMatch is the Turkish free-text match on a name column (02 §0.3
// search pattern, 05 §9.5): a substring of the normalised name
// (koro_norm: tr-TR lower case + unaccent, so İSTANBUL, Istanbul and
// istanbul are one word) or a trigram word similarity of the query within
// the name (pg_trgm %>: typos such as "istanbl"). Both use the
// koro_norm(<col>) gin_trgm_ops index. Two placeholders: the LIKE pattern
// (EscapeLike), the query.
func nameMatch(col string) string {
	return `public.koro_norm(` + col + `) LIKE '%' || public.koro_norm( ?) || '%'
		OR public.koro_norm(` + col + `) OPERATOR(public.%>) public.koro_norm( ?)`
}

// constraints maps the named constraints of 02 §8 onto module errors.
var constraints = db.Constraints{
	"universities_yok_code_uq":                    ErrYokCodeTaken,
	"universities_org_id_uq":                      problem.ErrConflict,
	"universities_org_id_org_kind_fkey":           problem.ErrConflict,
	"university_email_domains_pkey":               ErrDomainTaken,
	"university_email_domains_university_id_fkey": problem.ErrNotFound,
	"faculties_university_name_uq":                problem.ErrConflict,
	"departments_yok_uq":                          ErrDepartmentExists,
	"clubs_sensitive_category_chk":                ErrSensitivityRequired,
	"clubs_pkey":                                  problem.ErrConflict,
	"clubs_org_id_org_kind_fkey":                  problem.ErrConflict,
	"skills_slug_uq":                              ErrSkillSlugTaken,
	"university_email_domains_verified_by_fkey":   problem.ErrNotFound,
	"clubs_sensitivity_set_by_fkey":               problem.ErrNotFound,
	"departments_faculty_id_fkey":                 problem.ErrNotFound,
	"clubs_university_id_fkey":                    problem.ErrNotFound,
	"faculties_university_id_fkey":                problem.ErrNotFound,
	"departments_university_id_fkey":              problem.ErrNotFound,
	"university_email_domains_domain_kind_chk":    problem.ErrValidation,
	"university_email_domains_domain_chk":         problem.ErrValidation,
	"clubs_sensitive_reason_set_chk":              problem.ErrValidation,
	"clubs_sensitivity_stamp_chk":                 problem.ErrValidation,
	"universities_website_url_chk":                problem.ErrValidation,
	"departments_duration_years_chk":              problem.ErrValidation,
	"clubs_founded_year_chk":                      problem.ErrValidation,
	"skills_slug_chk":                             problem.ErrValidation,
	"departments_degree_level_chk":                problem.ErrValidation,
	"universities_type_chk":                       problem.ErrValidation,
	"clubs_category_chk":                          problem.ErrValidation,
	"skills_category_chk":                         problem.ErrValidation,
	"clubs_join_policy_chk":                       problem.ErrValidation,
	"clubs_claim_status_chk":                      problem.ErrValidation,
	"clubs_sensitive_reason_chk":                  problem.ErrValidation,
	"universities_org_kind_chk":                   problem.ErrValidation,
	"clubs_org_kind_chk":                          problem.ErrValidation,
	"club_claims_pending_uq":                      ErrClaimPending,
	"club_claims_reviewer_chk":                    problem.ErrForbidden, // 03 Y6: nobody decides their own claim
	"club_claims_review_chk":                      problem.ErrValidation,
	"club_claims_status_chk":                      problem.ErrValidation,
	"club_claims_role_in_club_chk":                problem.ErrValidation,
	"club_claims_evidence_text_chk":               problem.ErrValidation,
	"club_claims_decision_note_chk":               problem.ErrValidation,
	"club_claims_club_org_id_fkey":                problem.ErrNotFound,
	"club_claims_claimant_user_id_fkey":           problem.ErrNotFound,
	"club_claims_evidence_file_id_fkey":           problem.ErrNotFound,
	"club_claims_reviewed_by_fkey":                problem.ErrNotFound,
}

func wrap(op string, err error) error {
	return fmt.Errorf("directory: %s: %w", op, constraints.Translate(err))
}

// take reads one row of q into T or ErrNotFound.
func take[T any](op, cols string, q *gorm.DB) (T, error) {
	var v T
	err := q.Select(cols).Take(&v).Error
	switch {
	case db.IsNotFound(err):
		return v, ErrNotFound
	case err != nil:
		return v, wrap(op, err)
	}
	return v, nil
}

// returning runs a raw INSERT/UPDATE … RETURNING and returns its row or
// ErrNotFound when nothing was written.
func returning[T any](ctx context.Context, orm *gorm.DB, op, sql string, args map[string]any) (T, error) {
	var rows []T
	if err := db.Conn(ctx, orm).Raw(sql, args).Scan(&rows).Error; err != nil {
		var zero T
		return zero, wrap(op, err)
	}
	if len(rows) == 0 {
		var zero T
		return zero, ErrNotFound
	}
	return rows[0], nil
}

func locked(q *gorm.DB) *gorm.DB {
	return q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
}

// keyset applies the sort order and the position of a page.
func keyset(q *gorm.DB, sort, nameCol, idCol string, after *After) *gorm.DB {
	if sort == SortName {
		if after != nil {
			q = q.Where("("+nameCol+", "+idCol+") > ( ?, ?)", after.Name, after.ID)
		}
		return q.Order(nameCol + ", " + idCol)
	}
	if after != nil {
		q = q.Where(idCol+" < ?", after.ID)
	}
	return q.Order(idCol + " DESC")
}

// ---- universities

// InsertUniversity implements Store.
//
// cross-org: the row is the university's own org root (org_id from
// orgs.OrgAdmin.Create; the composite FK checks the kind).
func (s gormStore) InsertUniversity(ctx context.Context, u University) (University, error) {
	return returning[University](ctx, s.orm, "insert university", `INSERT INTO universities
		(id, org_id, yok_code, name, short_name, city, type, website_url, is_active)
		VALUES ( @id, @org_id, @yok_code, @name, @short_name, @city, @type, @website, @active)
		RETURNING `+uniCols, map[string]any{
		"id": u.ID, "org_id": u.OrgID, "yok_code": u.YokCode, "name": u.Name, "short_name": u.ShortName,
		"city": u.City, "type": string(u.Type), "website": u.WebsiteURL, "active": u.IsActive,
	})
}

// GetUniversity implements Store.
//
// cross-org: public reference data addressed by id.
func (s gormStore) GetUniversity(ctx context.Context, id uuid.UUID) (University, error) {
	return take[University]("get university", uniCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// UniversityByYokCode implements Store.
//
// cross-org: public reference data addressed by its YÖK code.
func (s gormStore) UniversityByYokCode(ctx context.Context, code string) (University, error) {
	return take[University]("university by yok code", uniCols, db.Conn(ctx, s.orm).Where("yok_code = ?", code))
}

// UniversityByName implements Store.
//
// cross-org: public reference data addressed by name.
func (s gormStore) UniversityByName(ctx context.Context, name string) (University, error) {
	return take[University]("university by name", uniCols, db.Conn(ctx, s.orm).Where("name = ?", name).Order("id"))
}

// LockUniversity implements Store.
//
// cross-org: public reference data addressed by id.
func (s gormStore) LockUniversity(ctx context.Context, id uuid.UUID) (University, error) {
	return take[University]("lock university", uniCols, locked(db.Conn(ctx, s.orm)).Where("id = ?", id))
}

// UpdateUniversity implements Store.
//
// cross-org: public reference data addressed by id.
func (s gormStore) UpdateUniversity(ctx context.Context, u University) (University, error) {
	return returning[University](ctx, s.orm, "update university", `UPDATE universities SET
		yok_code = @yok_code, name = @name, short_name = @short_name, city = @city, type = @type, website_url = @website,
		is_active = @active
		WHERE id = @id RETURNING `+uniCols, map[string]any{
		"id": u.ID, "yok_code": u.YokCode, "name": u.Name, "short_name": u.ShortName, "city": u.City, "type": string(u.Type),
		"website": u.WebsiteURL, "active": u.IsActive,
	})
}

// ListUniversities implements Store.
//
// cross-org: the public university catalog.
func (s gormStore) ListUniversities(ctx context.Context, f UniversityFilter, after *After, limit int) ([]University, error) {
	q := db.Conn(ctx, s.orm).Model(&University{}).Select(uniCols).Where("is_active")
	if f.City != "" {
		q = q.Where("public.koro_norm(city) = public.koro_norm( ?)", f.City)
	}
	if f.Type != "" {
		q = q.Where("type = ?", string(f.Type))
	}
	if f.Q != "" {
		q = q.Where(`(search_vector @@ plainto_tsquery('public.koro_tr', ?) OR `+nameMatch("name")+`
			OR public.koro_norm(coalesce(short_name, '')) LIKE '%' || public.koro_norm( ?) || '%')`,
			f.Q, textx.EscapeLike(f.Q), f.Q, textx.EscapeLike(f.Q))
	}
	var out []University
	if err := keyset(q, f.Sort, "name", "id", after).Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list universities", err)
	}
	return out, nil
}

// ---- e-mail domains

// GetDomain implements Store.
//
// cross-org: public reference data addressed by domain.
func (s gormStore) GetDomain(ctx context.Context, domain string) (EmailDomain, error) {
	return take[EmailDomain]("get domain", domainCols, locked(db.Conn(ctx, s.orm)).Where("domain = ?", domain))
}

// InsertDomain implements Store.
//
// cross-org: public reference data.
func (s gormStore) InsertDomain(ctx context.Context, d EmailDomain) (EmailDomain, error) {
	return returning[EmailDomain](ctx, s.orm, "insert domain", `INSERT INTO university_email_domains
		(domain, university_id, domain_kind, is_active, verified_by)
		VALUES ( @domain, @university_id, @kind, @active, @verified_by)
		RETURNING `+domainCols, map[string]any{
		"domain": d.Domain, "university_id": d.UniversityID, "kind": string(d.DomainKind), "active": d.IsActive,
		"verified_by": d.VerifiedBy,
	})
}

// UpdateDomain implements Store.
//
// cross-org: public reference data addressed by domain.
func (s gormStore) UpdateDomain(ctx context.Context, d EmailDomain) (EmailDomain, error) {
	return returning[EmailDomain](ctx, s.orm, "update domain", `UPDATE university_email_domains SET
		domain_kind = @kind, is_active = @active, verified_by = @verified_by
		WHERE domain = @domain RETURNING `+domainCols, map[string]any{
		"domain": d.Domain, "kind": string(d.DomainKind), "active": d.IsActive, "verified_by": d.VerifiedBy,
	})
}

// MatchDomain implements Store.
//
// cross-org: public reference data (identity's .edu.tr resolution).
func (s gormStore) MatchDomain(ctx context.Context, candidates []string) (EmailDomain, error) {
	if len(candidates) == 0 {
		return EmailDomain{}, ErrNotFound
	}
	var out []EmailDomain
	err := db.Conn(ctx, s.orm).Raw(`SELECT d.domain, d.university_id, d.domain_kind, d.is_active, d.verified_by,
			d.created_at, d.updated_at
		FROM university_email_domains d JOIN universities u ON u.id = d.university_id
		WHERE d.domain IN ? AND d.is_active AND u.is_active
		ORDER BY char_length(d.domain) DESC LIMIT 1`, candidates).Scan(&out).Error
	if err != nil {
		return EmailDomain{}, wrap("match domain", err)
	}
	if len(out) == 0 {
		return EmailDomain{}, ErrNotFound
	}
	return out[0], nil
}

// ---- faculties

// InsertFaculty implements Store.
//
// cross-org: public reference data.
func (s gormStore) InsertFaculty(ctx context.Context, f Faculty) (Faculty, error) {
	return returning[Faculty](ctx, s.orm, "insert faculty", `INSERT INTO faculties (id, university_id, name, yok_code, is_active)
		VALUES ( @id, @university_id, @name, @yok_code, @active) RETURNING `+facCols, map[string]any{
		"id": f.ID, "university_id": f.UniversityID, "name": f.Name, "yok_code": f.YokCode, "active": f.IsActive,
	})
}

// UpdateFaculty implements Store.
//
// cross-org: public reference data addressed by id.
func (s gormStore) UpdateFaculty(ctx context.Context, f Faculty) (Faculty, error) {
	return returning[Faculty](ctx, s.orm, "update faculty", `UPDATE faculties SET yok_code = @yok_code, is_active = @active
		WHERE id = @id RETURNING `+facCols, map[string]any{"id": f.ID, "yok_code": f.YokCode, "active": f.IsActive})
}

// GetFaculty implements Store.
//
// cross-org: public reference data addressed by id.
func (s gormStore) GetFaculty(ctx context.Context, id uuid.UUID) (Faculty, error) {
	return take[Faculty]("get faculty", facCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// FacultyByName implements Store.
//
// cross-org: public reference data.
func (s gormStore) FacultyByName(ctx context.Context, universityID uuid.UUID, name string) (Faculty, error) {
	return take[Faculty]("faculty by name", facCols, db.Conn(ctx, s.orm).Where("university_id = ? AND name = ?", universityID, name))
}

// FacultiesOf implements Store.
//
// cross-org: public reference data.
func (s gormStore) FacultiesOf(ctx context.Context, universityID uuid.UUID) ([]Faculty, error) {
	var out []Faculty
	err := db.Conn(ctx, s.orm).Select(facCols).Where("university_id = ? AND is_active", universityID).
		Order("name, id").Find(&out).Error
	if err != nil {
		return nil, wrap("faculties of", err)
	}
	return out, nil
}

// ---- departments

// InsertDepartment implements Store.
//
// cross-org: public reference data.
func (s gormStore) InsertDepartment(ctx context.Context, d Department) (Department, error) {
	return returning[Department](ctx, s.orm, "insert department", `INSERT INTO departments
		(id, university_id, faculty_id, name, yok_program_code, degree_level, duration_years, is_active)
		VALUES ( @id, @university_id, @faculty_id, @name, @code, @degree, @duration, @active)
		RETURNING `+deptCols, map[string]any{
		"id": d.ID, "university_id": d.UniversityID, "faculty_id": d.FacultyID, "name": d.Name,
		"code": d.YokProgramCode, "degree": string(d.DegreeLevel), "duration": d.DurationYears, "active": d.IsActive,
	})
}

// UpdateDepartment implements Store.
//
// cross-org: public reference data addressed by id.
func (s gormStore) UpdateDepartment(ctx context.Context, d Department) (Department, error) {
	return returning[Department](ctx, s.orm, "update department", `UPDATE departments SET
		faculty_id = @faculty_id, name = @name, yok_program_code = @code, degree_level = @degree,
		duration_years = @duration, is_active = @active
		WHERE id = @id RETURNING `+deptCols, map[string]any{
		"id": d.ID, "faculty_id": d.FacultyID, "name": d.Name, "code": d.YokProgramCode,
		"degree": string(d.DegreeLevel), "duration": d.DurationYears, "active": d.IsActive,
	})
}

// GetDepartment implements Store.
//
// cross-org: public reference data addressed by id.
func (s gormStore) GetDepartment(ctx context.Context, id uuid.UUID) (Department, error) {
	return take[Department]("get department", deptCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// FindDepartment implements Store.
//
// cross-org: public reference data.
func (s gormStore) FindDepartment(ctx context.Context, k DepartmentKey) (Department, error) {
	q := db.Conn(ctx, s.orm).Where("university_id = ?", k.UniversityID)
	if k.YokProgramCode != "" {
		q = q.Where("yok_program_code = ?", k.YokProgramCode)
	} else {
		q = q.Where("yok_program_code IS NULL AND name = ? AND degree_level = ?", k.Name, string(k.DegreeLevel))
		if k.FacultyID != nil {
			q = q.Where("faculty_id = ?", *k.FacultyID)
		} else {
			q = q.Where("faculty_id IS NULL")
		}
	}
	return take[Department]("find department", deptCols, q.Order("id"))
}

// ListDepartments implements Store.
//
// cross-org: public reference data.
func (s gormStore) ListDepartments(ctx context.Context, f DepartmentFilter, after *After, limit int) ([]Department, error) {
	q := db.Conn(ctx, s.orm).Model(&Department{}).Select(deptCols).Where("university_id = ? AND is_active", f.UniversityID)
	if f.FacultyID != nil {
		q = q.Where("faculty_id = ?", *f.FacultyID)
	}
	if f.DegreeLevel != "" {
		q = q.Where("degree_level = ?", string(f.DegreeLevel))
	}
	if f.Q != "" {
		q = q.Where("("+nameMatch("name")+")", textx.EscapeLike(f.Q), f.Q)
	}
	var out []Department
	if err := keyset(q, SortNewest, "name", "id", after).Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list departments", err)
	}
	return out, nil
}

// ---- clubs

func reasonArg(r *SensitiveReason) *string {
	if r == nil {
		return nil
	}
	v := string(*r)
	return &v
}

func linksArg(l SocialLinks) (string, error) {
	if l == nil {
		return "{}", nil
	}
	b, err := json.Marshal(map[string]string(l))
	return string(b), err
}

// InsertClub implements Store.
//
// cross-org: the row is the club's own org root (org_id from
// orgs.OrgAdmin.Create; the composite FK checks the kind).
func (s gormStore) InsertClub(ctx context.Context, c Club) (Club, error) {
	links, err := linksArg(c.SocialLinks)
	if err != nil {
		return Club{}, err
	}
	return returning[Club](ctx, s.orm, "insert club", `INSERT INTO clubs
		(org_id, university_id, category, is_sensitive, sensitive_reason, sensitivity_set_by, sensitivity_set_at,
		 join_policy, claim_status, founded_year, social_links)
		VALUES ( @org_id, @university_id, @category, @sensitive, @reason, @set_by, @set_at, @join, @claim, @founded,
		 CAST( @links AS jsonb))
		RETURNING `+clubCols, map[string]any{
		"org_id": c.OrgID, "university_id": c.UniversityID, "category": string(c.Category), "sensitive": c.IsSensitive,
		"reason": reasonArg(c.SensitiveReason), "set_by": c.SensitivitySetBy, "set_at": c.SensitivitySetAt,
		"join": string(c.JoinPolicy), "claim": string(c.ClaimStatus), "founded": c.FoundedYear, "links": links,
	})
}

// GetClub implements Store.
func (s gormStore) GetClub(ctx context.Context, orgID uuid.UUID) (Club, error) {
	return take[Club]("get club", clubCols, db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)))
}

// LockClub implements Store.
func (s gormStore) LockClub(ctx context.Context, orgID uuid.UUID) (Club, error) {
	return take[Club]("lock club", clubCols, locked(db.Conn(ctx, s.orm)).Scopes(db.ScopeOrg(orgID)))
}

// UpdateClub implements Store.
func (s gormStore) UpdateClub(ctx context.Context, c Club) (Club, error) {
	links, err := linksArg(c.SocialLinks)
	if err != nil {
		return Club{}, err
	}
	return returning[Club](ctx, s.orm, "update club", `UPDATE clubs SET
		category = @category, is_sensitive = @sensitive, sensitive_reason = @reason, sensitivity_set_by = @set_by,
		sensitivity_set_at = @set_at, join_policy = @join, claim_status = @claim, founded_year = @founded,
		social_links = CAST( @links AS jsonb)
		WHERE org_id = @org_id RETURNING `+clubCols, map[string]any{
		"org_id": c.OrgID, "category": string(c.Category), "sensitive": c.IsSensitive,
		"reason": reasonArg(c.SensitiveReason), "set_by": c.SensitivitySetBy, "set_at": c.SensitivitySetAt,
		"join": string(c.JoinPolicy), "claim": string(c.ClaimStatus), "founded": c.FoundedYear, "links": links,
	})
}

// clubRow is the scan target of ListClubs.
type clubRow struct {
	Club
	Name      string `gorm:"column:name"`
	Slug      string `gorm:"column:slug"`
	OrgStatus string `gorm:"column:org_status"`
}

// ListClubs implements Store. The org's name, slug, status and search
// vector are read from organizations (read-only join, 01 §1.4).
//
// cross-org: the public club catalog spans every club org.
func (s gormStore) ListClubs(ctx context.Context, f ClubFilter, after *After, limit int) ([]ClubRow, error) {
	q := db.Conn(ctx, s.orm).Table("clubs c").Select(clubColsC+", o.name, o.slug, o.status AS org_status").
		Joins("JOIN organizations o ON o.id = c.org_id").
		Where("o.status IN ?", []string{"active", "suspended"})
	if f.UniversityID != nil {
		q = q.Where("c.university_id = ?", *f.UniversityID)
	}
	if f.Category != "" {
		q = q.Where("c.category = ?", string(f.Category))
	}
	if f.Claimed != nil {
		status := ClaimUnclaimed
		if *f.Claimed {
			status = ClaimClaimed
		}
		q = q.Where("c.claim_status = ?", string(status))
	}
	if f.Q != "" {
		q = q.Where(`(o.search_vector @@ plainto_tsquery('public.koro_tr', ?) OR `+nameMatch("o.name")+`)`,
			f.Q, textx.EscapeLike(f.Q), f.Q)
	}
	var rows []clubRow
	if err := keyset(q, f.Sort, "o.name", "c.org_id", after).Limit(limit).Scan(&rows).Error; err != nil {
		return nil, wrap("list clubs", err)
	}
	out := make([]ClubRow, len(rows))
	for i, r := range rows {
		out[i] = ClubRow(r)
	}
	return out, nil
}

// SensitiveAmong implements Store.
//
// cross-org: a lookup over the given club orgs (outreach, students).
func (s gormStore) SensitiveAmong(ctx context.Context, orgIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(orgIDs) == 0 {
		return nil, nil
	}
	var out []uuid.UUID
	err := db.Conn(ctx, s.orm).Model(&Club{}).Where("org_id IN ? AND is_sensitive", orgIDs).Pluck("org_id", &out).Error
	if err != nil {
		return nil, wrap("sensitive among", err)
	}
	return out, nil
}

// ClubOrgsOf implements Store.
//
// cross-org: the club orgs of one university (searchOrgs?university_id).
func (s gormStore) ClubOrgsOf(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := db.Conn(ctx, s.orm).Model(&Club{}).Where("university_id = ?", universityID).Order("org_id").
		Pluck("org_id", &out).Error
	if err != nil {
		return nil, wrap("club orgs of", err)
	}
	return out, nil
}

// ---- skills

// InsertSkill implements Store.
//
// cross-org: public reference data.
func (s gormStore) InsertSkill(ctx context.Context, k Skill) (Skill, error) {
	return returning[Skill](ctx, s.orm, "insert skill", `INSERT INTO skills (id, slug, name_tr, name_en, category, is_active)
		VALUES ( @id, @slug, @name_tr, @name_en, @category, @active) RETURNING `+skillCols, map[string]any{
		"id": k.ID, "slug": k.Slug, "name_tr": k.NameTR, "name_en": k.NameEN, "category": string(k.Category),
		"active": k.IsActive,
	})
}

// UpdateSkill implements Store.
//
// cross-org: public reference data addressed by id.
func (s gormStore) UpdateSkill(ctx context.Context, k Skill) (Skill, error) {
	return returning[Skill](ctx, s.orm, "update skill", `UPDATE skills SET name_tr = @name_tr, name_en = @name_en,
		category = @category, is_active = @active WHERE id = @id RETURNING `+skillCols, map[string]any{
		"id": k.ID, "name_tr": k.NameTR, "name_en": k.NameEN, "category": string(k.Category), "active": k.IsActive,
	})
}

// GetSkill implements Store.
//
// cross-org: public reference data addressed by id.
func (s gormStore) GetSkill(ctx context.Context, id uuid.UUID) (Skill, error) {
	return take[Skill]("get skill", skillCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// SkillBySlug implements Store.
//
// cross-org: public reference data addressed by slug.
func (s gormStore) SkillBySlug(ctx context.Context, slug string) (Skill, error) {
	return take[Skill]("skill by slug", skillCols, db.Conn(ctx, s.orm).Where("slug = ?", slug))
}

// ListSkills implements Store.
//
// cross-org: the public skill taxonomy.
func (s gormStore) ListSkills(ctx context.Context, f SkillFilter, after *After, limit int) ([]Skill, error) {
	q := db.Conn(ctx, s.orm).Model(&Skill{}).Select(skillCols).Where("is_active")
	if f.Category != "" {
		q = q.Where("category = ?", string(f.Category))
	}
	if f.Q != "" {
		q = q.Where("("+nameMatch("name_tr")+` OR public.koro_norm(coalesce(name_en, '')) LIKE '%' || public.koro_norm( ?) || '%')`,
			textx.EscapeLike(f.Q), f.Q, textx.EscapeLike(f.Q))
	}
	var out []Skill
	if err := keyset(q, SortNewest, "name_tr", "id", after).Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list skills", err)
	}
	return out, nil
}

// CountActiveSkills implements Store.
//
// cross-org: public reference data.
func (s gormStore) CountActiveSkills(ctx context.Context, ids []uuid.UUID) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var n int64
	if err := db.Conn(ctx, s.orm).Model(&Skill{}).Where("id IN ? AND is_active", ids).Count(&n).Error; err != nil {
		return 0, wrap("count skills", err)
	}
	return int(n), nil
}

// SkillNamesTR implements Store: one query (id = ANY).
//
// cross-org: public reference data.
func (s gormStore) SkillNamesTR(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		ID     uuid.UUID `gorm:"column:id"`
		NameTR string    `gorm:"column:name_tr"`
	}
	if err := db.Conn(ctx, s.orm).Model(&Skill{}).Select("id, name_tr").Where("id = ANY( ?) AND is_active", db.UUIDArray(ids)).
		Scan(&rows).Error; err != nil {
		return nil, wrap("skill names", err)
	}
	for _, r := range rows {
		out[r.ID] = r.NameTR
	}
	return out, nil
}
