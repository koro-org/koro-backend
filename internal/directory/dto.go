package directory

import "koro/internal/orgs"

// UniversityCardDTO is a university in listUniversities (04 §2.3).
type UniversityCardDTO struct {
	UniversityID string `json:"university_id"`
	OrgID        string `json:"org_id"`
	Name         string `json:"name"`
	ShortName    string `json:"short_name,omitempty"`
	City         string `json:"city"`
	Type         string `json:"type" enum:"state,foundation,other"`
}

// UniversityList is a page of UniversityCardDTO.
type UniversityList struct {
	Items      []UniversityCardDTO `json:"items"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

// FacultyDTO is a faculty of getUniversity.
type FacultyDTO struct {
	FacultyID string `json:"faculty_id"`
	Name      string `json:"name"`
}

// UniversityDTO is getUniversity and the admin university operations.
type UniversityDTO struct {
	UniversityID string       `json:"university_id"`
	OrgID        string       `json:"org_id"`
	YokCode      string       `json:"yok_code"`
	Name         string       `json:"name"`
	ShortName    string       `json:"short_name,omitempty"`
	City         string       `json:"city"`
	Type         string       `json:"type" enum:"state,foundation,other"`
	WebsiteURL   string       `json:"website_url,omitempty"`
	IsActive     bool         `json:"is_active"`
	Faculties    []FacultyDTO `json:"faculties"`
}

// EmailDomainDTO is adminPutUniversityEmailDomain.
type EmailDomainDTO struct {
	Domain       string `json:"domain"`
	UniversityID string `json:"university_id"`
	DomainKind   string `json:"domain_kind" enum:"student,staff,mixed"`
	IsActive     bool   `json:"is_active"`
}

// DepartmentDTO is a department (listDepartments, adminCreateDepartment).
type DepartmentDTO struct {
	DepartmentID   string `json:"department_id"`
	Name           string `json:"name"`
	FacultyID      string `json:"faculty_id,omitempty"`
	DegreeLevel    string `json:"degree_level" enum:"associate,bachelor,master,doctorate"`
	DurationYears  *int16 `json:"duration_years,omitempty"`
	YokProgramCode string `json:"yok_program_code,omitempty"`
}

// DepartmentList is a page of DepartmentDTO.
type DepartmentList struct {
	Items      []DepartmentDTO `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// SkillDTO is a skill of listSkills: name in the Accept-Language language.
type SkillDTO struct {
	SkillID  string `json:"skill_id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Category string `json:"category" enum:"technical,tool,soft,domain,language_tool,other"`
}

// SkillList is a page of SkillDTO.
type SkillList struct {
	Items      []SkillDTO `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// AdminSkillDTO is a skill as the admin operations return it.
type AdminSkillDTO struct {
	SkillID  string `json:"skill_id"`
	Slug     string `json:"slug"`
	NameTR   string `json:"name_tr"`
	NameEN   string `json:"name_en,omitempty"`
	Category string `json:"category" enum:"technical,tool,soft,domain,language_tool,other"`
	IsActive bool   `json:"is_active"`
}

// ClubCardDTO is a club in listClubs (04 §2.3).
type ClubCardDTO struct {
	ClubID            string `json:"club_id" doc:"The club's org_id"`
	Name              string `json:"name"`
	Slug              string `json:"slug"`
	UniversityID      string `json:"university_id"`
	Category          string `json:"category"`
	JoinPolicy        string `json:"join_policy" enum:"open,approval"`
	ClaimStatus       string `json:"claim_status" enum:"unclaimed,claimed"`
	MembershipPrivate bool   `json:"membership_private" doc:"true for a sensitive club: its membership is never shown"`
	MemberCount       *int   `json:"member_count" doc:"null below 10 members or for a sensitive club (k-anonymity)"`
}

// ClubCardList is a page of ClubCardDTO.
type ClubCardList struct {
	Items      []ClubCardDTO `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// ClubUniversityDTO is the university of a club.
type ClubUniversityDTO struct {
	UniversityID string `json:"university_id"`
	OrgID        string `json:"org_id"`
	Name         string `json:"name"`
	ShortName    string `json:"short_name,omitempty"`
	City         string `json:"city"`
}

// ClubDTO is getClub, updateClub, adminCreateClub and
// adminSetClubSensitivity (04 §2.3).
type ClubDTO struct {
	ClubID            string            `json:"club_id" doc:"The club's org_id"`
	Org               orgs.OrgPublicDTO `json:"org"`
	University        ClubUniversityDTO `json:"university"`
	Category          string            `json:"category"`
	JoinPolicy        string            `json:"join_policy" enum:"open,approval"`
	ClaimStatus       string            `json:"claim_status" enum:"unclaimed,claimed"`
	MembershipPrivate bool              `json:"membership_private" doc:"true for a sensitive club: its membership is never shown"`
	MemberCount       *int              `json:"member_count" doc:"null below 10 members or for a sensitive club (k-anonymity)"`
	SocialLinks       map[string]string `json:"social_links" doc:"Network name → https URL"`
	FoundedYear       *int16            `json:"founded_year,omitempty"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func toUniversityCard(u University) UniversityCardDTO {
	return UniversityCardDTO{
		UniversityID: u.ID.String(), OrgID: u.OrgID.String(), Name: u.Name, ShortName: str(u.ShortName), City: u.City,
		Type: string(u.Type),
	}
}

func toUniversityDTO(u University, facs []Faculty) UniversityDTO {
	d := UniversityDTO{
		UniversityID: u.ID.String(), OrgID: u.OrgID.String(), YokCode: u.YokCode, Name: u.Name, ShortName: str(u.ShortName),
		City: u.City, Type: string(u.Type), WebsiteURL: str(u.WebsiteURL), IsActive: u.IsActive,
		Faculties: make([]FacultyDTO, len(facs)),
	}
	for i, f := range facs {
		d.Faculties[i] = FacultyDTO{FacultyID: f.ID.String(), Name: f.Name}
	}
	return d
}

func toDomainDTO(d EmailDomain) EmailDomainDTO {
	return EmailDomainDTO{Domain: d.Domain, UniversityID: d.UniversityID.String(), DomainKind: string(d.DomainKind), IsActive: d.IsActive}
}

func toDepartmentDTO(d Department) DepartmentDTO {
	out := DepartmentDTO{
		DepartmentID: d.ID.String(), Name: d.Name, DegreeLevel: string(d.DegreeLevel), DurationYears: d.DurationYears,
		YokProgramCode: str(d.YokProgramCode),
	}
	if d.FacultyID != nil {
		out.FacultyID = d.FacultyID.String()
	}
	return out
}

func toSkillDTO(k Skill, locale string) SkillDTO {
	return SkillDTO{SkillID: k.ID.String(), Slug: k.Slug, Name: k.Name(locale), Category: string(k.Category)}
}

func toAdminSkillDTO(k Skill) AdminSkillDTO {
	return AdminSkillDTO{
		SkillID: k.ID.String(), Slug: k.Slug, NameTR: k.NameTR, NameEN: str(k.NameEN), Category: string(k.Category),
		IsActive: k.IsActive,
	}
}

func toClubCard(c ClubCard) ClubCardDTO {
	r := c.Row
	return ClubCardDTO{
		ClubID: r.OrgID.String(), Name: r.Name, Slug: r.Slug, UniversityID: r.UniversityID.String(),
		Category: string(r.Category), JoinPolicy: string(r.JoinPolicy), ClaimStatus: string(r.ClaimStatus),
		MembershipPrivate: r.IsSensitive, MemberCount: c.MemberCount,
	}
}

// toOrgPublic is the public org profile (04 §2.2 OrgPublicDTO) of a club's
// org; follower_count stays null until feed counts followers.
func toOrgPublic(v orgs.OrgView) orgs.OrgPublicDTO {
	o := v.Org
	return orgs.OrgPublicDTO{
		OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug, Status: string(o.Status),
		Description: str(o.Description), LogoURL: v.LogoURL, WebsiteURL: str(o.WebsiteURL), City: str(o.City),
		Verified: o.Verified(),
	}
}

func toClubDTO(v ClubView) ClubDTO {
	c, u := v.Club, v.University
	return ClubDTO{
		ClubID: c.OrgID.String(), Org: toOrgPublic(v.Org),
		University: ClubUniversityDTO{
			UniversityID: u.ID.String(), OrgID: u.OrgID.String(), Name: u.Name, ShortName: str(u.ShortName), City: u.City,
		},
		Category: string(c.Category), JoinPolicy: string(c.JoinPolicy), ClaimStatus: string(c.ClaimStatus),
		MembershipPrivate: c.IsSensitive, MemberCount: v.MemberCount, SocialLinks: map[string]string(c.SocialLinks.Clone()),
		FoundedYear: c.FoundedYear,
	}
}
