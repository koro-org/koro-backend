package students

import (
	"time"

	"koro/internal/authz"
	"koro/internal/files"
)

// DTOs of BE-025 (04 §2.4, §V.1, §V.4, §V.5). Like dto.go, every type is
// an allow-list: no domain model is embedded, no map widens it, no talent
// value, sensitive club or CV file appears in a view of another person.

// ---- CVs (self)

// StudentCVDTO is one CV of the student (listMyCvs; 04 §V.1 cvs[]).
type StudentCVDTO struct {
	CVID      string    `json:"cv_id" format:"uuid"`
	FileName  string    `json:"file_name" doc:"The uploaded file's name (sanitised; empty when unknown)"`
	IsCurrent bool      `json:"is_current" doc:"The CV a company with level 3 access may download"`
	CreatedAt time.Time `json:"created_at"`
}

// StudentCVList is listMyCvs (at most 3).
type StudentCVList struct {
	Items []StudentCVDTO `json:"items"`
}

// StudentCVDownloadDTO is a short-lived download URL (04 §1.14).
type StudentCVDownloadDTO struct {
	URL       string    `json:"url" doc:"Signed URL (Content-Disposition: attachment), valid for 5 minutes"`
	ExpiresAt time.Time `json:"expires_at"`
}

func toCVDTO(v CVView) StudentCVDTO {
	return StudentCVDTO{CVID: v.CV.ID.String(), FileName: v.FileName, IsCurrent: v.CV.IsCurrent, CreatedAt: v.CV.CreatedAt.UTC()}
}

func cvDTOs(vs []CVView) []StudentCVDTO {
	out := make([]StudentCVDTO, len(vs))
	for i, v := range vs {
		out[i] = toCVDTO(v)
	}
	return out
}

func toDownloadDTO(d files.Download) StudentCVDownloadDTO {
	return StudentCVDownloadDTO{URL: d.URL, ExpiresAt: d.ExpiresAt.UTC()}
}

// ---- member card (04 §V.4)

// MemberCardMembershipDTO is the membership of the card.
type MemberCardMembershipDTO struct {
	MembershipID string     `json:"membership_id" format:"uuid"`
	Role         string     `json:"role" enum:"member,club_admin"`
	Status       string     `json:"status"`
	JoinedAt     *time.Time `json:"joined_at,omitempty"`
	Source       string     `json:"source"`
}

// MemberCardDTO is what a club_admin sees of an active member of the club
// (04 §V.4): never the e-mail (T-69), CV, skills, experience, other club
// memberships, privacy settings, birth date or a talent value.
type MemberCardDTO struct {
	UserID                      string                  `json:"user_id" format:"uuid"`
	FirstName                   string                  `json:"first_name"`
	LastName                    string                  `json:"last_name"`
	AvatarURL                   string                  `json:"avatar_url,omitempty" doc:"Signed URL, valid for 5 minutes"`
	University                  *StudentNameRefDTO      `json:"university,omitempty"`
	Department                  *StudentNameRefDTO      `json:"department,omitempty"`
	ClassYear                   *int                    `json:"class_year,omitempty"`
	Membership                  MemberCardMembershipDTO `json:"membership"`
	ClubEventRegistrationsCount *int                    `json:"club_event_registrations_count" doc:"Registrations for this club's events; null while the events module does not provide it"`
}

func toMemberCardDTO(c MemberCard) MemberCardDTO {
	m := c.Member.Membership
	d := MemberCardDTO{
		UserID: m.UserID.String(), FirstName: c.Member.FirstName, LastName: c.Member.LastName, AvatarURL: c.Member.AvatarURL,
		Membership: MemberCardMembershipDTO{MembershipID: m.ID.String(), Role: string(m.Role), Status: string(m.Status),
			JoinedAt: utcPtr(m.JoinedAt), Source: string(m.Source)},
		ClubEventRegistrationsCount: c.RegistrationsCount,
	}
	if p := c.Profile; p != nil {
		d.University = &StudentNameRefDTO{Name: c.University}
		if p.DepartmentID != nil {
			d.Department = &StudentNameRefDTO{Name: c.Department}
		}
		d.ClassYear = intPtr(p.ClassYear)
	}
	return d
}

// ---- admin view (04 §V.5)

// AdminStudentVerificationDTO is the student verification of the account.
type AdminStudentVerificationDTO struct {
	Status     string                `json:"status" enum:"none,verified,lapsed"`
	University *StudentUniversityDTO `json:"university,omitempty"`
	VerifiedAt *time.Time            `json:"verified_at,omitempty"`
	ExpiresAt  *time.Time            `json:"expires_at,omitempty"`
}

// AdminStudentMembershipDTO is a non-sensitive org membership.
type AdminStudentMembershipDTO struct {
	OrgID   string `json:"org_id" format:"uuid"`
	OrgKind string `json:"org_kind"`
	Role    string `json:"role"`
}

// AdminStudentProfileDTO is the profile content of the admin view.
type AdminStudentProfileDTO struct {
	Headline    string                 `json:"headline,omitempty"`
	About       string                 `json:"about,omitempty"`
	Department  *StudentDepartmentDTO  `json:"department,omitempty"`
	ClassYear   *int                   `json:"class_year,omitempty"`
	Skills      []StudentSkillDTO      `json:"skills"`
	Experiences []StudentExperienceDTO `json:"experiences"`
	Languages   []StudentLanguageDTO   `json:"languages"`
}

// AdminStudentRegistrationDTO is one event registration without answers.
type AdminStudentRegistrationDTO struct {
	EventID      string    `json:"event_id" format:"uuid"`
	EventTitle   string    `json:"event_title"`
	Status       string    `json:"status"`
	RegisteredAt time.Time `json:"registered_at"`
}

// AdminStudentViewDTO is the platform staff view of a student (04 §V.5):
// support and platform_admin by break-glass, the dpo for the subject of an
// open request (birth_date only there). Never: CV files, sensitive club
// memberships, form answers, talent values.
type AdminStudentViewDTO struct {
	UserID        string                        `json:"user_id" format:"uuid"`
	Email         string                        `json:"email"`
	FirstName     string                        `json:"first_name"`
	LastName      string                        `json:"last_name"`
	Status        string                        `json:"status"`
	CreatedAt     time.Time                     `json:"created_at"`
	LastLoginAt   *time.Time                    `json:"last_login_at,omitempty"`
	MFAEnabled    bool                          `json:"mfa_enabled"`
	Locale        string                        `json:"locale"`
	BirthDate     string                        `json:"birth_date,omitempty" doc:"YYYY-MM-DD; dpo only (data subject request)"`
	Student       AdminStudentVerificationDTO   `json:"student"`
	PlatformRoles []string                      `json:"platform_roles"`
	Memberships   []AdminStudentMembershipDTO   `json:"memberships" doc:"Non-sensitive memberships only"`
	Profile       AdminStudentProfileDTO        `json:"profile"`
	Privacy       StudentPrivacyDTO             `json:"privacy"`
	Registrations []AdminStudentRegistrationDTO `json:"registrations,omitempty" doc:"Event registrations without answers; absent while the events module does not provide them"`
}

func toAdminStudentViewDTO(v AdminView) AdminStudentViewDTO {
	u, p := v.Account.User, v.View.Profile
	d := AdminStudentViewDTO{
		UserID: u.ID.String(), Email: u.Email, FirstName: u.FirstName, LastName: u.LastName, Status: string(u.Status),
		CreatedAt: u.CreatedAt.UTC(), LastLoginAt: utcPtr(u.LastLoginAt), MFAEnabled: u.MFAEnabled, Locale: u.Locale,
		Student:       AdminStudentVerificationDTO{Status: string(v.Account.StudentStatus)},
		PlatformRoles: []string{}, Memberships: []AdminStudentMembershipDTO{},
		Profile: AdminStudentProfileDTO{
			Headline: str(p.Headline), About: str(p.About), Department: departmentRef(v.View), ClassYear: intPtr(p.ClassYear),
			Skills: skillDTOs(v.View.Skills), Languages: languageDTOs(v.View.Languages),
			Experiences: make([]StudentExperienceDTO, len(v.View.Experiences)),
		},
		Privacy: toPrivacyDTO(v.View.Privacy),
	}
	if d.Student.Status == "" {
		d.Student.Status = string(authz.StudentNone)
	}
	if s := v.Account.Student; s != nil {
		at, exp := s.VerifiedAt.UTC(), s.ExpiresAt.UTC()
		d.Student.University = &StudentUniversityDTO{UniversityID: s.UniversityID.String(), Name: v.UniversityName}
		d.Student.VerifiedAt, d.Student.ExpiresAt = &at, &exp
	}
	if v.ShowBirthDate && u.BirthDate != nil {
		d.BirthDate = u.BirthDate.String()
	}
	for _, r := range v.Account.PlatformRoles {
		d.PlatformRoles = append(d.PlatformRoles, string(r))
	}
	for _, m := range v.Account.Memberships {
		if m.OrgKind == authz.OrgClub {
			continue // only the clubs students cleared, below (identity hides every club anyway)
		}
		d.Memberships = append(d.Memberships, adminMembershipDTO(m))
	}
	for _, m := range v.Clubs {
		d.Memberships = append(d.Memberships, adminMembershipDTO(m))
	}
	for i, e := range v.View.Experiences {
		d.Profile.Experiences[i] = toExperienceDTO(e)
	}
	if v.Registrations != nil {
		d.Registrations = make([]AdminStudentRegistrationDTO, len(v.Registrations))
		for i, r := range v.Registrations {
			d.Registrations[i] = AdminStudentRegistrationDTO{EventID: r.EventID.String(), EventTitle: r.EventTitle,
				Status: r.Status, RegisteredAt: r.RegisteredAt.UTC()}
		}
	}
	return d
}

func adminMembershipDTO(m authz.Membership) AdminStudentMembershipDTO {
	return AdminStudentMembershipDTO{OrgID: m.OrgID.String(), OrgKind: string(m.OrgKind), Role: string(m.Role)}
}
