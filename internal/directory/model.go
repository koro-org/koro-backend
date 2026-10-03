package directory

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
	"uuid"
)

// UniversityType is universities.type (02 §8.1).
type UniversityType string

// University types.
const (
	UniversityState      UniversityType = "state"
	UniversityFoundation UniversityType = "foundation"
	UniversityOther      UniversityType = "other"
)

// Valid reports whether t is a value of universities_type_chk.
func (t UniversityType) Valid() bool {
	return t == UniversityState || t == UniversityFoundation || t == UniversityOther
}

// University is one universities row (02 §8.1): 1:1 with an organizations
// row of kind university (created together). The generated search_vector
// column is not mapped. No personal data.
type University struct {
	ID         uuid.UUID      `gorm:"type:uuid;column:id;primaryKey"`
	OrgID      uuid.UUID      `gorm:"type:uuid;column:org_id"`
	OrgKind    string         `gorm:"column:org_kind"`
	YokCode    string         `gorm:"column:yok_code"`
	Name       string         `gorm:"column:name"`
	ShortName  *string        `gorm:"column:short_name"`
	City       string         `gorm:"column:city"`
	Type       UniversityType `gorm:"column:type"`
	WebsiteURL *string        `gorm:"column:website_url"`
	IsActive   bool           `gorm:"column:is_active"`
	CreatedAt  time.Time      `gorm:"column:created_at"`
	UpdatedAt  time.Time      `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (University) TableName() string { return "universities" }

// DomainKind is university_email_domains.domain_kind (02 §8.2).
type DomainKind string

// Domain kinds.
const (
	DomainStudent DomainKind = "student"
	DomainStaff   DomainKind = "staff"
	DomainMixed   DomainKind = "mixed"
)

// Valid reports whether k is a value of university_email_domains_domain_kind_chk.
func (k DomainKind) Valid() bool { return k == DomainStudent || k == DomainStaff || k == DomainMixed }

// EmailDomain is one university_email_domains row (02 §8.2). VerifiedBy
// references the platform admin who registered it.
type EmailDomain struct {
	Domain       string     `gorm:"column:domain;primaryKey"`
	UniversityID uuid.UUID  `gorm:"type:uuid;column:university_id"`
	DomainKind   DomainKind `gorm:"column:domain_kind"`
	IsActive     bool       `gorm:"column:is_active"`
	VerifiedBy   *uuid.UUID `gorm:"type:uuid;column:verified_by"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (EmailDomain) TableName() string { return "university_email_domains" }

// Faculty is one faculties row (02 §8.3).
type Faculty struct {
	ID           uuid.UUID `gorm:"type:uuid;column:id;primaryKey"`
	UniversityID uuid.UUID `gorm:"type:uuid;column:university_id"`
	Name         string    `gorm:"column:name"`
	YokCode      *string   `gorm:"column:yok_code"`
	IsActive     bool      `gorm:"column:is_active"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Faculty) TableName() string { return "faculties" }

// DegreeLevel is departments.degree_level (02 §8.4).
type DegreeLevel string

// Degree levels.
const (
	DegreeAssociate DegreeLevel = "associate"
	DegreeBachelor  DegreeLevel = "bachelor"
	DegreeMaster    DegreeLevel = "master"
	DegreeDoctorate DegreeLevel = "doctorate"
)

// Valid reports whether d is a value of departments_degree_level_chk.
func (d DegreeLevel) Valid() bool {
	return d == DegreeAssociate || d == DegreeBachelor || d == DegreeMaster || d == DegreeDoctorate
}

// Department is one departments row (02 §8.4).
type Department struct {
	ID             uuid.UUID   `gorm:"type:uuid;column:id;primaryKey"`
	UniversityID   uuid.UUID   `gorm:"type:uuid;column:university_id"`
	FacultyID      *uuid.UUID  `gorm:"type:uuid;column:faculty_id"`
	Name           string      `gorm:"column:name"`
	YokProgramCode *string     `gorm:"column:yok_program_code"`
	DegreeLevel    DegreeLevel `gorm:"column:degree_level"`
	DurationYears  *int16      `gorm:"column:duration_years"`
	IsActive       bool        `gorm:"column:is_active"`
	CreatedAt      time.Time   `gorm:"column:created_at"`
	UpdatedAt      time.Time   `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Department) TableName() string { return "departments" }

// Category is clubs.category (02 §8.5).
type Category string

// Club categories. religious, political and identity clubs are always
// sensitive (clubs_sensitive_category_chk; 03 k7).
const (
	CategoryAcademic             Category = "academic"
	CategoryTechnology           Category = "technology"
	CategoryCareer               Category = "career"
	CategoryEntrepreneurship     Category = "entrepreneurship"
	CategoryArts                 Category = "arts"
	CategoryCulture              Category = "culture"
	CategorySports               Category = "sports"
	CategorySocialResponsibility Category = "social_responsibility"
	CategoryHobby                Category = "hobby"
	CategoryReligious            Category = "religious"
	CategoryPolitical            Category = "political"
	CategoryIdentity             Category = "identity"
	CategoryOther                Category = "other"
)

// Categories lists clubs_category_chk in 02 order.
var Categories = []Category{
	CategoryAcademic, CategoryTechnology, CategoryCareer, CategoryEntrepreneurship, CategoryArts, CategoryCulture,
	CategorySports, CategorySocialResponsibility, CategoryHobby, CategoryReligious, CategoryPolitical,
	CategoryIdentity, CategoryOther,
}

// Valid reports whether c is a value of clubs_category_chk.
func (c Category) Valid() bool { return slices.Contains(Categories, c) }

// Sensitive reports whether the category requires the sensitive flag.
func (c Category) Sensitive() bool {
	return c == CategoryReligious || c == CategoryPolitical || c == CategoryIdentity
}

// SensitiveReason is clubs.sensitive_reason (02 §8.5): why a club's
// membership is special-category data (KVKK md. 6).
type SensitiveReason string

// Sensitive reasons.
var SensitiveReasons = []SensitiveReason{
	"religion", "political_opinion", "philosophical_belief", "lgbtiq", "ethnic_origin", "health", "union", "other",
}

// Valid reports whether r is a value of clubs_sensitive_reason_chk.
func (r SensitiveReason) Valid() bool { return slices.Contains(SensitiveReasons, r) }

// JoinPolicy is clubs.join_policy.
type JoinPolicy string

// Join policies.
const (
	JoinOpen     JoinPolicy = "open"
	JoinApproval JoinPolicy = "approval"
)

// Valid reports whether p is a value of clubs_join_policy_chk.
func (p JoinPolicy) Valid() bool { return p == JoinOpen || p == JoinApproval }

// ClaimStatus is clubs.claim_status.
type ClaimStatus string

// Claim statuses (claims themselves: BE-022).
const (
	ClaimUnclaimed ClaimStatus = "unclaimed"
	ClaimClaimed   ClaimStatus = "claimed"
)

// Valid reports whether s is a value of clubs_claim_status_chk.
func (s ClaimStatus) Valid() bool { return s == ClaimUnclaimed || s == ClaimClaimed }

// SocialLinks is clubs.social_links: network name → https URL (02 §8.5:
// https only). It is stored as a JSON object.
type SocialLinks map[string]string

// Value implements driver.Valuer (jsonb).
func (l SocialLinks) Value() (driver.Value, error) {
	if l == nil {
		return "{}", nil
	}
	b, err := json.Marshal(map[string]string(l))
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner (jsonb arrives as text or bytes).
func (l *SocialLinks) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case nil:
		*l = SocialLinks{}
		return nil
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("directory: social_links: unexpected %T", src)
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return errors.Join(errors.New("directory: social_links is not an object of strings"), err)
	}
	*l = m
	return nil
}

// Clone returns a copy (nil → empty).
func (l SocialLinks) Clone() SocialLinks {
	if l == nil {
		return SocialLinks{}
	}
	return maps.Clone(l)
}

// Club is one clubs row (02 §8.5): the catalog data of an org of kind
// club. The club's name, slug and status live in its organizations row
// (orgs). The sensitivity flag marks clubs whose membership is
// special-category data; it is never logged (03 §3.5).
type Club struct {
	OrgID            uuid.UUID        `gorm:"type:uuid;column:org_id;primaryKey"`
	OrgKind          string           `gorm:"column:org_kind"`
	UniversityID     uuid.UUID        `gorm:"type:uuid;column:university_id"`
	Category         Category         `gorm:"column:category"`
	IsSensitive      bool             `gorm:"column:is_sensitive"`
	SensitiveReason  *SensitiveReason `gorm:"column:sensitive_reason"`
	SensitivitySetBy *uuid.UUID       `gorm:"type:uuid;column:sensitivity_set_by"`
	SensitivitySetAt *time.Time       `gorm:"column:sensitivity_set_at"`
	JoinPolicy       JoinPolicy       `gorm:"column:join_policy"`
	ClaimStatus      ClaimStatus      `gorm:"column:claim_status"`
	FoundedYear      *int16           `gorm:"column:founded_year"`
	SocialLinks      SocialLinks      `gorm:"column:social_links;type:jsonb"`
	CreatedAt        time.Time        `gorm:"column:created_at"`
	UpdatedAt        time.Time        `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Club) TableName() string { return "clubs" }

// ClubRow is a club with the catalog columns of its org (listClubs).
type ClubRow struct {
	Club
	Name      string
	Slug      string
	OrgStatus string
}

// SkillCategory is skills.category (02 §8.7).
type SkillCategory string

// SkillCategories lists skills_category_chk.
var SkillCategories = []SkillCategory{"technical", "tool", "soft", "domain", "language_tool", "other"}

// Valid reports whether c is a value of skills_category_chk.
func (c SkillCategory) Valid() bool { return slices.Contains(SkillCategories, c) }

// Skill is one skills row (02 §8.7).
type Skill struct {
	ID        uuid.UUID     `gorm:"type:uuid;column:id;primaryKey"`
	Slug      string        `gorm:"column:slug"`
	NameTR    string        `gorm:"column:name_tr"`
	NameEN    *string       `gorm:"column:name_en"`
	Category  SkillCategory `gorm:"column:category"`
	IsActive  bool          `gorm:"column:is_active"`
	CreatedAt time.Time     `gorm:"column:created_at"`
	UpdatedAt time.Time     `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Skill) TableName() string { return "skills" }

// Name returns the skill's name in locale ("en" → name_en when set).
func (s Skill) Name(locale string) string {
	if locale == "en" && s.NameEN != nil && *s.NameEN != "" {
		return *s.NameEN
	}
	return s.NameTR
}

// ClaimState is club_claims.status (02 §8.6).
type ClaimState string

// Claim states: a claim is pending until its claimant withdraws it or a
// its approver decides it; the three others are final.
const (
	ClaimPending   ClaimState = "pending"
	ClaimApproved  ClaimState = "approved"
	ClaimRejected  ClaimState = "rejected"
	ClaimWithdrawn ClaimState = "withdrawn"
)

// Valid reports whether s is a value of club_claims_status_chk.
func (s ClaimState) Valid() bool {
	return s == ClaimPending || s == ClaimApproved || s == ClaimRejected || s == ClaimWithdrawn
}

// Decided reports whether s carries a review (club_claims_review_chk).
func (s ClaimState) Decided() bool { return s == ClaimApproved || s == ClaimRejected }

// ClubClaim is one club_claims row (02 §8.6): a student's request to run a
// club as its club_admin. It is personal data (the claimant, the role,
// the evidence); DecisionNote is shown to the reviewers only.
type ClubClaim struct {
	ID             uuid.UUID  `gorm:"type:uuid;column:id;primaryKey"`
	ClubOrgID      uuid.UUID  `gorm:"type:uuid;column:club_org_id"`
	ClaimantUserID uuid.UUID  `gorm:"type:uuid;column:claimant_user_id"`
	Status         ClaimState `gorm:"column:status"`
	RoleInClub     string     `gorm:"column:role_in_club"`
	EvidenceText   *string    `gorm:"column:evidence_text"`
	EvidenceFileID *uuid.UUID `gorm:"type:uuid;column:evidence_file_id"`
	ReviewedBy     *uuid.UUID `gorm:"type:uuid;column:reviewed_by"`
	ReviewedAt     *time.Time `gorm:"column:reviewed_at"`
	DecisionNote   *string    `gorm:"column:decision_note"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (ClubClaim) TableName() string { return "club_claims" }

// ClaimFilter narrows Store.ListClaims.
type ClaimFilter struct {
	Status ClaimState
	// RepUniversityOrgs, when not nil, keeps only the claims of the
	// non-sensitive clubs of these universities (by the university's org
	// ID): a university_rep's review queue (E-21).
	RepUniversityOrgs []uuid.UUID
}
