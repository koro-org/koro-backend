package directory

import (
	"time"

	"koro/internal/orgs"
	"koro/internal/platform/db"
)

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
	LogoPublicURL     string `json:"logo_public_url,omitempty" doc:"Permanent, cacheable URL of the club's logo (content addressed; immutable Cache-Control), the org's logo_public_url: only for an active club that is not sensitive, once the virus scan cleared the image; absent otherwise (BE-126)."`
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
	SocialLinks       socialLinksMap    `json:"social_links" doc:"Network name → https URL"`
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
		MembershipPrivate: r.IsSensitive, MemberCount: c.MemberCount, LogoPublicURL: c.LogoPublicURL,
	}
}

// toOrgPublic is the public org profile (04 §2.2 OrgPublicDTO) of a club's
// org; follower_count stays null until feed counts followers.
func toOrgPublic(v orgs.OrgView) orgs.OrgPublicDTO {
	o := v.Org
	return orgs.OrgPublicDTO{
		OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug, Status: string(o.Status),
		Description: str(o.Description), LogoURL: v.LogoURL, LogoPublicURL: v.LogoPublicURL, WebsiteURL: str(o.WebsiteURL),
		City: str(o.City), Verified: o.Verified(),
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
		MembershipPrivate: c.IsSensitive, MemberCount: v.MemberCount, SocialLinks: socialLinksMap(c.SocialLinks.Clone()),
		FoundedYear: c.FoundedYear,
	}
}

// ClubClaimDTO is a club claim as its claimant sees it (04 §2.3
// ClubClaimDTO; the decision note is for the reviewers only).
type ClubClaimDTO struct {
	ClaimID         string     `json:"claim_id"`
	ClubID          string     `json:"club_id" doc:"The club's org_id"`
	ClubName        string     `json:"club_name"`
	Status          string     `json:"status" enum:"pending,approved,rejected,withdrawn"`
	RoleInClub      string     `json:"role_in_club"`
	HasEvidenceFile bool       `json:"has_evidence_file"`
	CreatedAt       time.Time  `json:"created_at"`
	DecidedAt       *time.Time `json:"decided_at,omitempty"`
}

// ClubClaimList is a page of ClubClaimDTO.
type ClubClaimList struct {
	Items      []ClubClaimDTO `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// ClaimClubDTO is the club of an admin claim view.
type ClaimClubDTO struct {
	ClubID            string `json:"club_id"`
	Name              string `json:"name"`
	UniversityID      string `json:"university_id"`
	ClaimStatus       string `json:"claim_status" enum:"unclaimed,claimed"`
	MembershipPrivate bool   `json:"membership_private" doc:"true for a sensitive club: the claim is special category data"`
}

// ClaimantDTO is the claimant of an admin claim view (null names when the
// account is gone).
type ClaimantDTO struct {
	UserID    string `json:"user_id"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// EvidenceDownloadDTO is the short-lived download of a claim's evidence
// file (04 §1.14).
type EvidenceDownloadDTO struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AdminClubClaimDTO is a claim in the review of platform staff and
// university representatives (04 §2.3 AdminClubClaimDTO).
type AdminClubClaimDTO struct {
	ClaimID        string               `json:"claim_id"`
	Status         string               `json:"status" enum:"pending,approved,rejected,withdrawn"`
	Club           ClaimClubDTO         `json:"club"`
	Claimant       ClaimantDTO          `json:"claimant"`
	RoleInClub     string               `json:"role_in_club"`
	EvidenceText   string               `json:"evidence_text,omitempty"`
	EvidenceFileID string               `json:"evidence_file_id,omitempty"`
	EvidenceState  string               `json:"evidence_state" enum:"none,available,not_clean" doc:"available: evidence holds a 5-minute download (adminGetClubClaim only); not_clean: the virus scan has not cleared the file"`
	Evidence       *EvidenceDownloadDTO `json:"evidence,omitempty"`
	Approver       string               `json:"approver,omitempty" enum:"university_rep,platform_admin" doc:"Who decides the claim (detail and decision only): the university's representative when it has one and the club is not sensitive, a platform admin otherwise"`
	CreatedAt      time.Time            `json:"created_at"`
	ReviewedBy     string               `json:"reviewed_by,omitempty"`
	ReviewedAt     *time.Time           `json:"reviewed_at,omitempty"`
	DecisionNote   string               `json:"decision_note,omitempty"`
}

// AdminClubClaimList is a page of AdminClubClaimDTO.
type AdminClubClaimList struct {
	Items      []AdminClubClaimDTO `json:"items"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

func toClubClaimDTO(v ClaimView) ClubClaimDTO {
	c := v.Claim
	return ClubClaimDTO{
		ClaimID: c.ID.String(), ClubID: c.ClubOrgID.String(), ClubName: v.ClubName, Status: string(c.Status),
		RoleInClub: c.RoleInClub, HasEvidenceFile: c.EvidenceFileID != nil, CreatedAt: c.CreatedAt.UTC(),
		DecidedAt: db.UTCPtr(c.ReviewedAt),
	}
}

func toAdminClaimDTO(v ClaimView) AdminClubClaimDTO {
	c := v.Claim
	d := AdminClubClaimDTO{
		ClaimID: c.ID.String(), Status: string(c.Status),
		Club: ClaimClubDTO{
			ClubID: c.ClubOrgID.String(), Name: v.ClubName, UniversityID: v.Club.UniversityID.String(),
			ClaimStatus: string(v.Club.ClaimStatus), MembershipPrivate: v.Club.IsSensitive,
		},
		Claimant:   ClaimantDTO{UserID: c.ClaimantUserID.String()},
		RoleInClub: c.RoleInClub, EvidenceText: str(c.EvidenceText), EvidenceState: v.EvidenceState, Approver: v.Approver,
		CreatedAt: c.CreatedAt.UTC(), ReviewedAt: db.UTCPtr(c.ReviewedAt), DecisionNote: str(c.DecisionNote),
	}
	if d.EvidenceState == "" {
		d.EvidenceState = EvidenceNone
	}
	if v.Claimant != nil {
		d.Claimant.FirstName, d.Claimant.LastName = v.Claimant.FirstName, v.Claimant.LastName
	}
	if c.EvidenceFileID != nil {
		d.EvidenceFileID = c.EvidenceFileID.String()
	}
	if c.ReviewedBy != nil {
		d.ReviewedBy = c.ReviewedBy.String()
	}
	if v.Evidence != nil {
		d.Evidence = &EvidenceDownloadDTO{URL: v.Evidence.URL, ExpiresAt: v.Evidence.ExpiresAt.UTC()}
	}
	return d
}
