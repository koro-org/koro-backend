package students

import (
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// Visibility DTOs (04 §V). Every level is a separate type whose fields are
// an allow-list (04 §V.0-1): a domain model is never embedded and no map
// widens a DTO. No type carries a talent value, a sensitive club or a
// minor's data at a company level (TestDTO_* pin the field sets).
//
//	level  who      type                                     built by
//	self   student  StudentProfileSelfDTO, StudentPrivacyDTO  toSelfDTO, toPrivacyDTO (BE-023 endpoints)
//	self   student  StudentCVDTO, StudentCVDownloadDTO        toCVDTO, toDownloadDTO (BE-025, dto_views.go)
//	club   cla      MemberCardDTO                            toMemberCardDTO (BE-025, 04 §V.4)
//	staff  sup/pad/dpo AdminStudentViewDTO                   toAdminStudentViewDTO (BE-025, 04 §V.5)
//	0      company  AudienceCountDTO                         NewAudienceCount (outreach; BE-025 Audience.Count)
//	1      company  none: delivery counts are AudienceCountDTO values (outreach)
//	2      company  AnonProfileDTO                           ToAnonProfile, AnonProfile.WithAnonID (Discoverable.Search, BE-046; discovery, BE-051)
//	3      company  StudentProfileL3DTO, StudentL3CardDTO    ToStudentProfileL3, ToStudentL3Card (BE-046)

// ---- shared parts

// StudentUniversityDTO names the student's university.
type StudentUniversityDTO struct {
	UniversityID string `json:"university_id" format:"uuid"`
	Name         string `json:"name"`
}

// StudentDepartmentDTO names the student's department.
type StudentDepartmentDTO struct {
	DepartmentID string `json:"department_id" format:"uuid"`
	Name         string `json:"name"`
}

// StudentNameRefDTO is a name without an ID (level 3 card).
type StudentNameRefDTO struct {
	Name string `json:"name"`
}

// StudentLanguageDTO is one language of a student (02 §9.5).
type StudentLanguageDTO struct {
	LanguageCode string `json:"language_code" pattern:"^[a-z]{2,3}$" doc:"ISO 639"`
	Level        string `json:"level" enum:"A1,A2,B1,B2,C1,C2,native"`
}

func languageDTOs(ls []Language) []StudentLanguageDTO {
	out := make([]StudentLanguageDTO, len(ls))
	for i, l := range ls {
		out[i] = StudentLanguageDTO{LanguageCode: l.LanguageCode, Level: string(l.Level)}
	}
	return out
}

func intPtr(v *int16) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func universityRef(v ProfileView) StudentUniversityDTO {
	return StudentUniversityDTO{UniversityID: v.Profile.UniversityID.String(), Name: v.UniversityName}
}

func departmentRef(v ProfileView) *StudentDepartmentDTO {
	if v.Profile.DepartmentID == nil {
		return nil
	}
	return &StudentDepartmentDTO{DepartmentID: v.Profile.DepartmentID.String(), Name: v.DepartmentName}
}

// ---- self (04 §V.1)

// StudentSkillDTO is one skill of the student's own profile.
type StudentSkillDTO struct {
	SkillID string `json:"skill_id" format:"uuid"`
	Level   *int   `json:"level,omitempty" minimum:"1" maximum:"5"`
}

// StudentSkillList is the body of putMySkills.
type StudentSkillList struct {
	Items []StudentSkillDTO `json:"items"`
}

// StudentLanguageList is the body of putMyLanguages.
type StudentLanguageList struct {
	Items []StudentLanguageDTO `json:"items"`
}

// StudentExperienceDTO is one experience of the student's own profile.
type StudentExperienceDTO struct {
	ExperienceID     string `json:"experience_id" format:"uuid"`
	Kind             string `json:"kind" enum:"internship,job,volunteer,project,club_role"`
	Title            string `json:"title"`
	OrganizationName string `json:"organization_name,omitempty"`
	StartMonth       string `json:"start_month" pattern:"^[0-9]{4}-(0[1-9]|1[0-2])$" doc:"YYYY-MM"`
	EndMonth         string `json:"end_month,omitempty" pattern:"^[0-9]{4}-(0[1-9]|1[0-2])$" doc:"YYYY-MM; absent: ongoing"`
	Description      string `json:"description,omitempty"`
}

// StudentPrivacyDTO is getMyPrivacy / updateMyPrivacy (04 §2.4): the activity
// ranking choice only, never a talent value.
type StudentPrivacyDTO struct {
	Discoverable          bool   `json:"discoverable" doc:"Level 2: companies may find the anonymous profile (default off)"`
	ShowContactOnGrant    bool   `json:"show_contact_on_grant" doc:"Your contact_info_share consent and the default of \"share my e-mail\" when you accept a contact request (default off). The e-mail goes only to the companies you share it with (per grant); turning this off stops every share"`
	ShowClubsOnGrant      bool   `json:"show_clubs_on_grant" doc:"Level 3: non-sensitive club memberships (default off)"`
	ActivityRankingChoice string `json:"activity_ranking_choice" enum:"default,enabled,disabled"`
}

// StudentProfileSelfDTO is the student's own profile (04 §V.1): every
// field of the profile and its sections, the CVs and the privacy
// settings. Only the owner ever receives it.
type StudentProfileSelfDTO struct {
	UserID             string                 `json:"user_id" format:"uuid"`
	University         StudentUniversityDTO   `json:"university" doc:"From the active student verification; not editable"`
	Department         *StudentDepartmentDTO  `json:"department,omitempty"`
	DegreeLevel        string                 `json:"degree_level" enum:"associate,bachelor,master,doctorate"`
	ClassYear          *int                   `json:"class_year,omitempty" minimum:"1" maximum:"8"`
	ExpectedGraduation string                 `json:"expected_graduation,omitempty" pattern:"^[0-9]{4}-(0[1-9]|1[0-2])$" doc:"YYYY-MM"`
	Status             string                 `json:"status" enum:"active,graduated,inactive"`
	Headline           string                 `json:"headline,omitempty"`
	About              string                 `json:"about,omitempty"`
	City               string                 `json:"city,omitempty"`
	AvatarURL          string                 `json:"avatar_url,omitempty" doc:"Signed URL, valid for 5 minutes"`
	CompletedSections  []string               `json:"completed_sections"`
	Skills             []StudentSkillDTO      `json:"skills"`
	Experiences        []StudentExperienceDTO `json:"experiences"`
	Languages          []StudentLanguageDTO   `json:"languages"`
	CVs                []StudentCVDTO         `json:"cvs"`
	Privacy            StudentPrivacyDTO      `json:"privacy"`
}

func toSkillDTO(s Skill) StudentSkillDTO {
	return StudentSkillDTO{SkillID: s.SkillID.String(), Level: intPtr(s.Level)}
}

func skillDTOs(ss []Skill) []StudentSkillDTO {
	out := make([]StudentSkillDTO, len(ss))
	for i, s := range ss {
		out[i] = toSkillDTO(s)
	}
	return out
}

func toExperienceDTO(e Experience) StudentExperienceDTO {
	d := StudentExperienceDTO{
		ExperienceID: e.ID.String(), Kind: string(e.Kind), Title: e.Title, OrganizationName: str(e.OrganizationName),
		StartMonth: monthString(e.StartMonth), Description: str(e.Description),
	}
	if e.EndMonth != nil {
		d.EndMonth = monthString(*e.EndMonth)
	}
	return d
}

func toPrivacyDTO(p Privacy) StudentPrivacyDTO {
	return StudentPrivacyDTO{
		Discoverable: p.Discoverable, ShowContactOnGrant: p.ShowContactOnGrant, ShowClubsOnGrant: p.ShowClubsOnGrant,
		ActivityRankingChoice: string(p.ActivityRankingChoice),
	}
}

func toSelfDTO(v ProfileView) StudentProfileSelfDTO {
	p := v.Profile
	d := StudentProfileSelfDTO{
		UserID: p.UserID.String(), University: universityRef(v), Department: departmentRef(v),
		DegreeLevel: string(p.DegreeLevel), ClassYear: intPtr(p.ClassYear), Status: string(p.Status),
		Headline: str(p.Headline), About: str(p.About), City: str(p.City), AvatarURL: v.AvatarURL,
		CompletedSections: append([]string{}, p.CompletedSections...),
		Skills:            skillDTOs(v.Skills), Languages: languageDTOs(v.Languages),
		Experiences: make([]StudentExperienceDTO, len(v.Experiences)), CVs: cvDTOs(v.CVs), Privacy: toPrivacyDTO(v.Privacy),
	}
	if p.ExpectedGraduation != nil {
		d.ExpectedGraduation = monthString(*p.ExpectedGraduation)
	}
	for i, e := range v.Experiences {
		d.Experiences[i] = toExperienceDTO(e)
	}
	return d
}

// ---- level 0 (04 §V.2): counts only

// KThreshold is the smallest group a company may see a number for
// (04 §V.2; count_segment enforces the same in the database).
const KThreshold = 10

// AudienceCountDTO is level 0: a number or nothing, never a person.
type AudienceCountDTO struct {
	Count          *int `json:"count" doc:"null below 10"`
	BelowThreshold bool `json:"below_threshold"`
}

// NewAudienceCount applies the k ≥ 10 rule to n (a negative n is "no
// number", e.g. count_segment's NULL).
func NewAudienceCount(n int) AudienceCountDTO {
	if n < KThreshold {
		return AudienceCountDTO{BelowThreshold: true}
	}
	return AudienceCountDTO{Count: &n}
}

// ---- company sources

// ClubFact is one active club membership of the student as the company
// projections need it; Sensitive clubs never leave the module.
type ClubFact struct {
	OrgID     uuid.UUID
	Name      string
	Role      string // org_members.role (member | club_admin)
	Sensitive bool
}

// CompanySource is everything a company view may be built from; the
// projections copy only the fields their level allows (04 §V.2, §V.3).
// BE-046 fills it from this module's store and its ports.
type CompanySource struct {
	View      ProfileView
	FirstName string
	LastName  string
	IsMinor   bool
	// Verified: an active student verification (03 §2.1).
	Verified bool
	// DiscoveryConsent / ContactConsent: consents('discovery_profile') and
	// consents('contact_info_share') are in force (the settings mirror
	// them; both are checked).
	DiscoveryConsent bool
	ContactConsent   bool
	// Email is the account's address; it reaches level 3 only with
	// show_contact_on_grant and the consent.
	Email      string
	SkillNames map[uuid.UUID]string
	Clubs      []ClubFact
	// CVUploadedAt is the current CV's upload time (nil: none; BE-025).
	CVUploadedAt *time.Time
}

// Grant is a level 3 access grant of one company (02 §9.7; BE-046).
type Grant struct {
	CompanyOrgID     uuid.UUID
	Active           bool
	GrantedAt        time.Time
	Source           string // contact_request | application
	ContactRequestID *uuid.UUID
	// ContactShared: the student shared the e-mail with this company
	// (contact_shared_at; BE-089).
	ContactShared bool
}

// Discoverable is Discoverable.IsDiscoverable (01 §4.9): level 2 on (and
// its consent in force), an active verification, an adult, an active
// stored profile.
func (c CompanySource) Discoverable() bool {
	return c.View.Stored && c.View.Privacy.Discoverable && c.DiscoveryConsent && c.Verified && !c.IsMinor &&
		c.View.Profile.Status == ProfileActive
}

// grantFor reports whether g gives viewerOrg level 3 access to a student
// a company may see at all (never a minor: 04 §V.0-3).
func (c CompanySource) grantFor(viewerOrg uuid.UUID, g Grant) bool {
	return c.View.Stored && !c.IsMinor && g.Active && g.CompanyOrgID == viewerOrg && viewerOrg != uuid.Nil()
}

// ---- level 2 (04 §V.2)

// AnonSkillDTO is a skill at level 2 and 3.
type AnonSkillDTO struct {
	SkillID string `json:"skill_id" format:"uuid"`
	Name    string `json:"name"`
	Level   *int   `json:"level,omitempty"`
}

// AnonExperienceDTO is an experience at level 2: years only, no
// organisation and no description (free text identifies people).
type AnonExperienceDTO struct {
	Kind      string `json:"kind" enum:"internship,job,volunteer,project"`
	Title     string `json:"title"`
	StartYear int    `json:"start_year"`
	EndYear   *int   `json:"end_year,omitempty"`
}

// AnonProfileDTO is level 2 (04 §V.2): a company-scoped pseudonym, never
// the user ID, the name, the photo, free texts, clubs or contact.
type AnonProfileDTO struct {
	AnonID                 string                `json:"anon_id"`
	University             StudentUniversityDTO  `json:"university"`
	Department             *StudentDepartmentDTO `json:"department,omitempty"`
	DegreeLevel            string                `json:"degree_level" enum:"associate,bachelor,master,doctorate"`
	ClassYear              *int                  `json:"class_year,omitempty"`
	ExpectedGraduationYear *int                  `json:"expected_graduation_year,omitempty"`
	Skills                 []AnonSkillDTO        `json:"skills"`
	Languages              []StudentLanguageDTO  `json:"languages"`
	Experiences            []AnonExperienceDTO   `json:"experiences"`
	ProfileCompleteness    int                   `json:"profile_completeness" doc:"Number of completed profile sections"`
}

func namedSkills(src CompanySource) []AnonSkillDTO {
	out := make([]AnonSkillDTO, len(src.View.Skills))
	for i, s := range src.View.Skills {
		out[i] = AnonSkillDTO{SkillID: s.SkillID.String(), Name: src.SkillNames[s.SkillID], Level: intPtr(s.Level)}
	}
	return out
}

// ToAnonProfile builds level 2 for a company under anonID (discovery maps
// the pseudonym). A student who is not discoverable (level 2 off, consent
// withdrawn, unverified, minor, graduated) is not found.
func ToAnonProfile(src CompanySource, anonID string) (AnonProfileDTO, error) {
	if !src.Discoverable() || anonID == "" {
		return AnonProfileDTO{}, ErrNotFound
	}
	d := anonDTO(src)
	d.AnonID = anonID
	return d, nil
}

// anonDTO is the level 2 projection of src without its pseudonym (the
// caller checked discoverability).
func anonDTO(src CompanySource) AnonProfileDTO {
	v, p := src.View, src.View.Profile
	d := AnonProfileDTO{
		University: universityRef(v), Department: departmentRef(v), DegreeLevel: string(p.DegreeLevel),
		ClassYear: intPtr(p.ClassYear), Skills: namedSkills(src), Languages: languageDTOs(v.Languages),
		Experiences: []AnonExperienceDTO{}, ProfileCompleteness: len(p.CompletedSections),
	}
	if p.ExpectedGraduation != nil {
		y := p.ExpectedGraduation.Year
		d.ExpectedGraduationYear = &y
	}
	for _, e := range v.Experiences {
		if e.Kind == KindClubRole { // may name a sensitive club: never at level 2
			continue
		}
		x := AnonExperienceDTO{Kind: string(e.Kind), Title: e.Title, StartYear: e.StartMonth.Year}
		if e.EndMonth != nil {
			y := e.EndMonth.Year
			x.EndYear = &y
		}
		d.Experiences = append(d.Experiences, x)
	}
	return d
}

// ---- level 3 (04 §V.3)

// L3ExperienceDTO is a full experience at level 3.
type L3ExperienceDTO struct {
	Kind             string `json:"kind" enum:"internship,job,volunteer,project,club_role"`
	Title            string `json:"title"`
	OrganizationName string `json:"organization_name,omitempty"`
	StartMonth       string `json:"start_month"`
	EndMonth         string `json:"end_month,omitempty"`
	Description      string `json:"description,omitempty"`
}

// L3ClubDTO is a non-sensitive club membership at level 3.
type L3ClubDTO struct {
	OrgID string `json:"org_id" format:"uuid"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// L3CVDTO tells whether a current CV exists (the file is a separate,
// audited download).
type L3CVDTO struct {
	Available  bool       `json:"available"`
	UploadedAt *time.Time `json:"uploaded_at,omitempty"`
}

// L3ContactDTO is the contact the student agreed to share.
type L3ContactDTO struct {
	Email string `json:"email" format:"email"`
}

// L3GrantDTO is the grant the company holds.
type L3GrantDTO struct {
	GrantedAt        time.Time `json:"granted_at"`
	Source           string    `json:"source"`
	ContactRequestID string    `json:"contact_request_id,omitempty"`
}

// StudentProfileL3DTO is level 3 (04 §V.3): the full profile of a student
// who accepted this company's contact request. Clubs (non-sensitive only)
// and club_role experiences only with show_clubs_on_grant; the e-mail only
// with show_contact_on_grant and the consent in force. Never: birth date,
// minor flag, edu e-mail, privacy settings, registrations, consents,
// sensitive clubs, talent.
type StudentProfileL3DTO struct {
	UserID             string                `json:"user_id" format:"uuid"`
	FirstName          string                `json:"first_name"`
	LastName           string                `json:"last_name"`
	AvatarURL          string                `json:"avatar_url,omitempty"`
	University         StudentUniversityDTO  `json:"university"`
	Department         *StudentDepartmentDTO `json:"department,omitempty"`
	DegreeLevel        string                `json:"degree_level" enum:"associate,bachelor,master,doctorate"`
	ClassYear          *int                  `json:"class_year,omitempty"`
	ExpectedGraduation string                `json:"expected_graduation,omitempty"`
	Headline           string                `json:"headline,omitempty"`
	About              string                `json:"about,omitempty"`
	City               string                `json:"city,omitempty"`
	Skills             []AnonSkillDTO        `json:"skills"`
	Languages          []StudentLanguageDTO  `json:"languages"`
	Experiences        []L3ExperienceDTO     `json:"experiences"`
	Clubs              []L3ClubDTO           `json:"clubs,omitempty"`
	CV                 L3CVDTO               `json:"cv"`
	Contact            *L3ContactDTO         `json:"contact,omitempty"`
	Grant              L3GrantDTO            `json:"grant"`
}

// StudentL3CardDTO is one row of a company's granted students (04 §V.3).
type StudentL3CardDTO struct {
	UserID     string             `json:"user_id" format:"uuid"`
	FirstName  string             `json:"first_name"`
	LastName   string             `json:"last_name"`
	AvatarURL  string             `json:"avatar_url,omitempty"`
	University StudentNameRefDTO  `json:"university"`
	Department *StudentNameRefDTO `json:"department,omitempty"`
	ClassYear  *int               `json:"class_year,omitempty"`
	GrantedAt  time.Time          `json:"granted_at"`
}

// ToStudentProfileL3 builds level 3 for viewerOrg. Without an active grant
// of viewerOrg itself (another company's grant included: IDOR), or for a
// minor, the student is not found (03 Y5, 404).
func ToStudentProfileL3(src CompanySource, viewerOrg uuid.UUID, g Grant) (StudentProfileL3DTO, error) {
	if !src.grantFor(viewerOrg, g) {
		return StudentProfileL3DTO{}, ErrNotFound
	}
	v, p, pr := src.View, src.View.Profile, src.View.Privacy
	d := StudentProfileL3DTO{
		UserID: p.UserID.String(), FirstName: src.FirstName, LastName: src.LastName, AvatarURL: v.AvatarURL,
		University: universityRef(v), Department: departmentRef(v), DegreeLevel: string(p.DegreeLevel),
		ClassYear: intPtr(p.ClassYear), Headline: str(p.Headline), About: str(p.About), City: str(p.City),
		Skills: namedSkills(src), Languages: languageDTOs(v.Languages), Experiences: []L3ExperienceDTO{},
		CV:    L3CVDTO{Available: src.CVUploadedAt != nil, UploadedAt: db.UTCPtr(src.CVUploadedAt)},
		Grant: L3GrantDTO{GrantedAt: g.GrantedAt.UTC(), Source: g.Source},
	}
	if p.ExpectedGraduation != nil {
		d.ExpectedGraduation = monthString(*p.ExpectedGraduation)
	}
	if g.ContactRequestID != nil {
		d.Grant.ContactRequestID = g.ContactRequestID.String()
	}
	for _, e := range v.Experiences {
		if e.Kind == KindClubRole && !pr.ShowClubsOnGrant {
			continue
		}
		x := L3ExperienceDTO{Kind: string(e.Kind), Title: e.Title, OrganizationName: str(e.OrganizationName),
			StartMonth: monthString(e.StartMonth), Description: str(e.Description)}
		if e.EndMonth != nil {
			x.EndMonth = monthString(*e.EndMonth)
		}
		d.Experiences = append(d.Experiences, x)
	}
	if pr.ShowClubsOnGrant {
		clubs := slices.Clone(src.Clubs)
		slices.SortFunc(clubs, func(a, b ClubFact) int { return compareUUID(a.OrgID, b.OrgID) })
		for _, c := range clubs {
			if c.Sensitive { // 03 Y2: never, whatever the student chose
				continue
			}
			d.Clubs = append(d.Clubs, L3ClubDTO{OrgID: c.OrgID.String(), Name: c.Name, Role: c.Role})
		}
	}
	if g.ContactShared && src.ContactConsent && src.Email != "" {
		d.Contact = &L3ContactDTO{Email: src.Email}
	}
	return d, nil
}

// ToStudentL3Card builds a list row of level 3 (same grant rule).
func ToStudentL3Card(src CompanySource, viewerOrg uuid.UUID, g Grant) (StudentL3CardDTO, error) {
	if !src.grantFor(viewerOrg, g) {
		return StudentL3CardDTO{}, ErrNotFound
	}
	v := src.View
	d := StudentL3CardDTO{
		UserID: v.Profile.UserID.String(), FirstName: src.FirstName, LastName: src.LastName, AvatarURL: v.AvatarURL,
		University: StudentNameRefDTO{Name: v.UniversityName}, ClassYear: intPtr(v.Profile.ClassYear), GrantedAt: g.GrantedAt.UTC(),
	}
	if v.Profile.DepartmentID != nil {
		d.Department = &StudentNameRefDTO{Name: v.DepartmentName}
	}
	return d, nil
}
