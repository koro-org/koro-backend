package students

import (
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/db"
)

// DegreeLevel is student_profiles.degree_level (02 §9.1).
type DegreeLevel string

// Degree levels.
const (
	DegreeAssociate DegreeLevel = "associate"
	DegreeBachelor  DegreeLevel = "bachelor"
	DegreeMaster    DegreeLevel = "master"
	DegreeDoctorate DegreeLevel = "doctorate"
)

// Valid reports whether d is a level of the CHECK.
func (d DegreeLevel) Valid() bool {
	switch d {
	case DegreeAssociate, DegreeBachelor, DegreeMaster, DegreeDoctorate:
		return true
	}
	return false
}

// ProfileStatus is student_profiles.status.
type ProfileStatus string

// Profile statuses (02 §9.1). graduated is set by students.graduation_check
// only (T-56: the graduate concept is phase 2; v1 has no way back).
const (
	ProfileActive    ProfileStatus = "active"
	ProfileGraduated ProfileStatus = "graduated"
	ProfileInactive  ProfileStatus = "inactive"
)

// Section is an entry of student_profiles.completed_sections.
type Section string

// Profile sections (02 §9.1). cv is maintained by BE-025.
const (
	SectionBasics     Section = "basics"
	SectionSkills     Section = "skills"
	SectionExperience Section = "experience"
	SectionLanguages  Section = "languages"
	SectionCV         Section = "cv"
)

// sectionOrder is the CHECK's order; completed_sections is kept sorted by it.
var sectionOrder = []Section{SectionBasics, SectionSkills, SectionExperience, SectionLanguages, SectionCV}

// RankingChoice is student_privacy_settings.activity_ranking_choice
// (T-32: "default" follows the platform setting talent.default_enabled,
// which starts false).
type RankingChoice string

// Ranking choices.
const (
	RankingDefault  RankingChoice = "default"
	RankingEnabled  RankingChoice = "enabled"
	RankingDisabled RankingChoice = "disabled"
)

// Valid reports whether c is a choice of the CHECK.
func (c RankingChoice) Valid() bool {
	return c == RankingDefault || c == RankingEnabled || c == RankingDisabled
}

// ExperienceKind is student_experiences.kind (02 §9.4).
type ExperienceKind string

// Experience kinds. club_role may name a sensitive club: it never reaches a
// level 2 view and a level 3 view only with show_clubs_on_grant (04 §V.2,
// §V.3).
const (
	KindInternship ExperienceKind = "internship"
	KindJob        ExperienceKind = "job"
	KindVolunteer  ExperienceKind = "volunteer"
	KindProject    ExperienceKind = "project"
	KindClubRole   ExperienceKind = "club_role"
)

// Valid reports whether k is a kind of the CHECK.
func (k ExperienceKind) Valid() bool {
	switch k {
	case KindInternship, KindJob, KindVolunteer, KindProject, KindClubRole:
		return true
	}
	return false
}

// LanguageLevel is student_languages.level (CEFR or native).
type LanguageLevel string

// Valid reports whether l is a level of the CHECK.
func (l LanguageLevel) Valid() bool {
	switch l {
	case "A1", "A2", "B1", "B2", "C1", "C2", "native":
		return true
	}
	return false
}

// Profile is a student_profiles row (02 §9.1). UniversityID comes from the
// active student verification and is never taken from a request.
type Profile struct {
	UserID              uuid.UUID     `gorm:"type:uuid;column:user_id;primaryKey"`
	UniversityID        uuid.UUID     `gorm:"type:uuid;column:university_id"`
	DepartmentID        *uuid.UUID    `gorm:"type:uuid;column:department_id"`
	DegreeLevel         DegreeLevel   `gorm:"column:degree_level"`
	ClassYear           *int16        `gorm:"column:class_year"`
	ExpectedGraduation  *civil.Date   `gorm:"column:expected_graduation"` // the first day of the month
	Status              ProfileStatus `gorm:"column:status"`
	GraduatedAt         *time.Time    `gorm:"column:graduated_at"`
	Headline            *string       `gorm:"column:headline"`
	About               *string       `gorm:"column:about"`
	City                *string       `gorm:"column:city"`
	AvatarFileID        *uuid.UUID    `gorm:"type:uuid;column:avatar_file_id"`
	CompletedSections   db.TextArray  `gorm:"column:completed_sections"`
	AudienceKeys        db.TextArray  `gorm:"column:audience_keys"`
	AudienceEligible    bool          `gorm:"column:audience_eligible"`
	AudienceRefreshedAt *time.Time    `gorm:"column:audience_refreshed_at"`
	CreatedAt           time.Time     `gorm:"column:created_at"`
	UpdatedAt           time.Time     `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Profile) TableName() string { return "student_profiles" }

// cloneProfile returns p with its own copies of the array columns (never
// nil: the columns are NOT NULL).
func cloneProfile(p Profile) Profile {
	p.CompletedSections = slices.Clone(p.CompletedSections)
	p.AudienceKeys = slices.Clone(p.AudienceKeys)
	if p.CompletedSections == nil {
		p.CompletedSections = db.TextArray{}
	}
	if p.AudienceKeys == nil {
		p.AudienceKeys = db.TextArray{}
	}
	return p
}

// Completed reports whether section is in the completed sections.
func (p Profile) Completed(s Section) bool { return slices.Contains(p.CompletedSections, string(s)) }

// withSection returns the completed sections with s set to done, in the
// CHECK's order.
func (p Profile) withSection(s Section, done bool) db.TextArray {
	out := db.TextArray{}
	for _, x := range sectionOrder {
		has := p.Completed(x)
		if x == s {
			has = done
		}
		if has {
			out = append(out, string(x))
		}
	}
	return out
}

// Privacy is a student_privacy_settings row (02 §9.2). Every flag defaults
// to the private side: level 2 (discoverable), contact details and club
// memberships at level 3 are off until the student turns them on.
type Privacy struct {
	UserID                uuid.UUID     `gorm:"type:uuid;column:user_id;primaryKey"`
	Discoverable          bool          `gorm:"column:discoverable"`
	ShowContactOnGrant    bool          `gorm:"column:show_contact_on_grant"`
	ShowClubsOnGrant      bool          `gorm:"column:show_clubs_on_grant"`
	ActivityRankingChoice RankingChoice `gorm:"column:activity_ranking_choice"`
	CreatedAt             time.Time     `gorm:"column:created_at"`
	UpdatedAt             time.Time     `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Privacy) TableName() string { return "student_privacy_settings" }

// defaultPrivacy is the settings of a user without a row (02 §9.2 defaults).
func defaultPrivacy(userID uuid.UUID) Privacy {
	return Privacy{UserID: userID, ActivityRankingChoice: RankingDefault}
}

// Skill is a student_skills row (02 §9.3).
type Skill struct {
	UserID    uuid.UUID `gorm:"type:uuid;column:user_id;primaryKey"`
	SkillID   uuid.UUID `gorm:"type:uuid;column:skill_id;primaryKey"`
	Level     *int16    `gorm:"column:level"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Skill) TableName() string { return "student_skills" }

// Experience is a student_experiences row (02 §9.4).
type Experience struct {
	ID               uuid.UUID      `gorm:"type:uuid;column:id;primaryKey"`
	UserID           uuid.UUID      `gorm:"type:uuid;column:user_id"`
	Kind             ExperienceKind `gorm:"column:kind"`
	Title            string         `gorm:"column:title"`
	OrganizationName *string        `gorm:"column:organization_name"`
	StartMonth       civil.Date     `gorm:"column:start_month"`
	EndMonth         *civil.Date    `gorm:"column:end_month"`
	Description      *string        `gorm:"column:description"`
	CreatedAt        time.Time      `gorm:"column:created_at"`
	UpdatedAt        time.Time      `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Experience) TableName() string { return "student_experiences" }

// Language is a student_languages row (02 §9.5).
type Language struct {
	UserID       uuid.UUID     `gorm:"type:uuid;column:user_id;primaryKey"`
	LanguageCode string        `gorm:"column:language_code;primaryKey"`
	Level        LanguageLevel `gorm:"column:level"`
	CreatedAt    time.Time     `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Language) TableName() string { return "student_languages" }

// ProfileView is a profile with its sections and the display facts other
// modules hold (names of the university and department, the avatar URL).
// Stored is false for the profile of a verified student who has never
// written it (it is created on the first write).
type ProfileView struct {
	Profile        Profile
	Stored         bool
	UniversityName string
	DepartmentName string
	AvatarURL      string
	Skills         []Skill
	Experiences    []Experience
	Languages      []Language
	Privacy        Privacy
}
