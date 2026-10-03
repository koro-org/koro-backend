package directory

import (
	"context"
	"uuid"
)

// Store is the persistence boundary of directory (05 §2.2) over
// universities, university_email_domains, faculties, departments, clubs
// and skills (02 §8.1–8.5, §8.7). The GORM store (store_gorm.go) and the
// in-memory store (store_mem.go) pass the same contract test
// (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any.
//
// Org scope: universities and clubs are 1:1 with their org (org_id is the
// org root's id, returned by orgs.OrgAdmin.Create; the composite FK
// (org_id, org_kind) binds it to an org of the right kind). The reference
// tables (domains, faculties, departments, skills) have no org scope.
//
// ListClubs reads the name, slug and status of the club's org: the GORM
// store joins organizations read-only (01 §1.4 row added by BE-020: club
// search by name needs the org's search_vector and koro_norm(name) index).
type Store interface {
	// InsertUniversity writes a university and returns it as stored;
	// ErrYokCodeTaken when the YÖK code is taken.
	InsertUniversity(ctx context.Context, u University) (University, error)
	// GetUniversity returns a university (any is_active) or ErrNotFound.
	GetUniversity(ctx context.Context, id uuid.UUID) (University, error)
	// UniversityByYokCode returns the university of a YÖK code or ErrNotFound.
	UniversityByYokCode(ctx context.Context, code string) (University, error)
	// UniversityByName returns the university of an exact name (the oldest
	// when several share it) or ErrNotFound.
	UniversityByName(ctx context.Context, name string) (University, error)
	// LockUniversity returns the university locked for the transaction.
	LockUniversity(ctx context.Context, id uuid.UUID) (University, error)
	// UpdateUniversity writes the mutable columns (YÖK code, name, short
	// name, city, type, website, is_active) and returns the stored row.
	UpdateUniversity(ctx context.Context, u University) (University, error)
	// ListUniversities returns up to limit active universities matching f
	// after the position after, in f.Sort order.
	ListUniversities(ctx context.Context, f UniversityFilter, after *After, limit int) ([]University, error)

	// GetDomain returns the row of a domain (lower case) or ErrNotFound.
	GetDomain(ctx context.Context, domain string) (EmailDomain, error)
	// InsertDomain writes a domain row; 409 when it exists.
	InsertDomain(ctx context.Context, d EmailDomain) (EmailDomain, error)
	// UpdateDomain writes domain_kind, is_active and verified_by of a domain.
	UpdateDomain(ctx context.Context, d EmailDomain) (EmailDomain, error)
	// MatchDomain returns the longest active domain among candidates whose
	// university is active, or ErrNotFound.
	MatchDomain(ctx context.Context, candidates []string) (EmailDomain, error)

	// InsertFaculty writes a faculty; 409 when the university has one of
	// that name.
	InsertFaculty(ctx context.Context, f Faculty) (Faculty, error)
	// UpdateFaculty writes yok_code and is_active of a faculty.
	UpdateFaculty(ctx context.Context, f Faculty) (Faculty, error)
	// GetFaculty returns a faculty or ErrNotFound.
	GetFaculty(ctx context.Context, id uuid.UUID) (Faculty, error)
	// FacultyByName returns the faculty of a university by exact name.
	FacultyByName(ctx context.Context, universityID uuid.UUID, name string) (Faculty, error)
	// FacultiesOf returns the active faculties of a university by name.
	FacultiesOf(ctx context.Context, universityID uuid.UUID) ([]Faculty, error)

	// InsertDepartment writes a department; ErrDepartmentExists when the
	// university has its YÖK program code.
	InsertDepartment(ctx context.Context, d Department) (Department, error)
	// UpdateDepartment writes faculty, name, degree, duration, program code
	// and is_active of a department.
	UpdateDepartment(ctx context.Context, d Department) (Department, error)
	// GetDepartment returns a department or ErrNotFound.
	GetDepartment(ctx context.Context, id uuid.UUID) (Department, error)
	// FindDepartment returns the department matching k (see DepartmentKey).
	FindDepartment(ctx context.Context, k DepartmentKey) (Department, error)
	// ListDepartments returns up to limit active departments of
	// f.UniversityID matching f, newest first.
	ListDepartments(ctx context.Context, f DepartmentFilter, after *After, limit int) ([]Department, error)

	// InsertClub writes a club row and returns it as stored. 422 for a
	// CHECK (sensitive category without the flag:
	// ErrSensitivityRequired), 409 for a missing org or university.
	InsertClub(ctx context.Context, c Club) (Club, error)
	// GetClub returns the club of an org or ErrNotFound.
	GetClub(ctx context.Context, orgID uuid.UUID) (Club, error)
	// LockClub returns the club locked for the transaction.
	LockClub(ctx context.Context, orgID uuid.UUID) (Club, error)
	// UpdateClub writes category, sensitivity (flag, reason, set_by,
	// set_at), join policy, claim status, founded year and social links;
	// updated_at changes on every call (ETag, 04 §1.13).
	UpdateClub(ctx context.Context, c Club) (Club, error)
	// ListClubs returns up to limit clubs whose org is active or suspended,
	// matching f, after the position after, in f.Sort order.
	ListClubs(ctx context.Context, f ClubFilter, after *After, limit int) ([]ClubRow, error)
	// SensitiveAmong returns the sensitive clubs among orgIDs.
	SensitiveAmong(ctx context.Context, orgIDs []uuid.UUID) ([]uuid.UUID, error)
	// ClubOrgsOf returns the org IDs of a university's clubs.
	ClubOrgsOf(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error)

	// InsertSkill writes a skill; ErrSkillSlugTaken when the slug is taken.
	InsertSkill(ctx context.Context, s Skill) (Skill, error)
	// UpdateSkill writes name_tr, name_en, category and is_active.
	UpdateSkill(ctx context.Context, s Skill) (Skill, error)
	// GetSkill returns a skill or ErrNotFound.
	GetSkill(ctx context.Context, id uuid.UUID) (Skill, error)
	// SkillBySlug returns the skill of a slug or ErrNotFound.
	SkillBySlug(ctx context.Context, slug string) (Skill, error)
	// ListSkills returns up to limit active skills matching f, newest first.
	ListSkills(ctx context.Context, f SkillFilter, after *After, limit int) ([]Skill, error)
	// CountActiveSkills returns how many of ids are active skills.
	CountActiveSkills(ctx context.Context, ids []uuid.UUID) (int, error)
}

// Sort orders of the lists.
const (
	SortNewest = "id_desc"  // newest first (UUIDv7)
	SortName   = "name_asc" // by name (Turkish collation), then id
)

// After is the keyset position of a page: the last row's sort values.
type After struct {
	Name string // SortName only
	ID   uuid.UUID
}

// UniversityFilter narrows ListUniversities. Zero fields do not filter.
type UniversityFilter struct {
	// Q is free text (2–100 characters): full-text match on name, short
	// name and city (koro_tr), or a substring of the normalised name
	// (koro_norm, trigram index).
	Q    string
	City string
	Type UniversityType
	Sort string
}

// DepartmentFilter narrows ListDepartments.
type DepartmentFilter struct {
	UniversityID uuid.UUID
	Q            string // substring or trigram match on koro_norm(name)
	FacultyID    *uuid.UUID
	DegreeLevel  DegreeLevel
}

// DepartmentKey identifies a department of the reference data: by YÖK
// program code when there is one, else by faculty, name and degree.
type DepartmentKey struct {
	UniversityID   uuid.UUID
	YokProgramCode string
	FacultyID      *uuid.UUID
	Name           string
	DegreeLevel    DegreeLevel
}

// ClubFilter narrows ListClubs.
type ClubFilter struct {
	// Q is free text (2–100 characters) on the club's org: full-text match
	// on name and description (koro_tr), a substring of koro_norm(name), or
	// a trigram match (typos, missing Turkish letters).
	Q            string
	UniversityID *uuid.UUID
	Category     Category
	Claimed      *bool
	Sort         string
}

// SkillFilter narrows ListSkills.
type SkillFilter struct {
	Q        string // substring or trigram match on koro_norm(name_tr) / name_en
	Category SkillCategory
}
