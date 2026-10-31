package orgs

import (
	"time"

	"koro/internal/platform/taxid"
)

// CompanyPublicDTO is the public company profile (04 §2.2 OrgPublicDTO.company).
type CompanyPublicDTO struct {
	SectorCode string `json:"sector_code,omitempty"`
	SizeBand   string `json:"size_band,omitempty"`
	HQCity     string `json:"hq_city,omitempty"`
	// The edit form's fields (BE-127): only for a reader who may update
	// the org (its company admins and owner, platform admins).
	EmailDomain      string     `json:"email_domain,omitempty" doc:"Only for the company's editors (updateOrg)"`
	DomainVerifiedAt *time.Time `json:"domain_verified_at,omitempty" doc:"Only for the company's editors"`
	// The tax identity and review facts (BE-165): only for the company's
	// editors (owner, company admins, platform admins).
	*CompanyTaxDTO
}

// CompanyTaxDTO is the tax identity and the review facts of a company
// (BE-165) for its editors and platform staff. A TCKN is never returned
// (06 H-60): tax_id_kind tckn only says a valid one was given.
type CompanyTaxDTO struct {
	TaxIDKind      string `json:"tax_id_kind,omitempty" enum:"vkn,tckn" doc:"Which tax identifier the company gave"`
	TaxNo          string `json:"tax_no,omitempty" doc:"The VKN; masked to its last 4 digits unless tax_no_masked is false (platform admins)"`
	TaxNoMasked    bool   `json:"tax_no_masked,omitempty" doc:"tax_no shows only its last 4 digits"`
	TaxOffice      string `json:"tax_office,omitempty"`
	TaxIDValid     bool   `json:"tax_id_valid" doc:"A VKN or TCKN that passes its check digits was given"`
	DomainVerified bool   `json:"domain_verified" doc:"email_domain is verified: the founder's or an editor's verified address is at it"`
}

// OrgPublicDTO is the public profile of an org (04 §2.2 getOrg). club and
// university details are added by directory's endpoints (BE-020);
// follower_count is null until feed counts followers (k ≥ 10).
type OrgPublicDTO struct {
	OrgID         string            `json:"org_id"`
	Kind          string            `json:"kind" enum:"platform,university,club,company,ngo"`
	Name          string            `json:"name"`
	Slug          string            `json:"slug"`
	Status        string            `json:"status" enum:"pending_review,active,suspended,closed"`
	Description   string            `json:"description,omitempty"`
	LogoURL       string            `json:"logo_url,omitempty" doc:"Short-lived download URL (at most 5 minutes)"`
	LogoPublicURL string            `json:"logo_public_url,omitempty" doc:"Permanent, cacheable URL of the logo (content addressed: a new logo has a new URL; served with an immutable Cache-Control). Only for an active org that is not a sensitive club, once the virus scan cleared the image; absent otherwise (use logo_url)."`
	WebsiteURL    string            `json:"website_url,omitempty"`
	City          string            `json:"city,omitempty"`
	Verified      bool              `json:"verified"`
	FollowerCount *int              `json:"follower_count" doc:"null below 10 followers (k-anonymity)"`
	Company       *CompanyPublicDTO `json:"company,omitempty"`
}

// CompanyDTO is the company profile as its admins and platform staff see it.
type CompanyDTO struct {
	SectorCode       string     `json:"sector_code,omitempty"`
	SizeBand         string     `json:"size_band,omitempty"`
	EmailDomain      string     `json:"email_domain,omitempty"`
	DomainVerifiedAt *time.Time `json:"domain_verified_at,omitempty"`
	HQCity           string     `json:"hq_city,omitempty"`
	CompanyTaxDTO
}

// OrgDTO is an org as its admins and platform staff see it (createCompany,
// updateOrg and the state transitions).
type OrgDTO struct {
	OrgID       string      `json:"org_id"`
	Kind        string      `json:"kind" enum:"platform,university,club,company,ngo"`
	Name        string      `json:"name"`
	Slug        string      `json:"slug"`
	Status      string      `json:"status" enum:"pending_review,active,suspended,closed"`
	Description string      `json:"description,omitempty"`
	LogoURL     string      `json:"logo_url,omitempty"`
	LogoFileID  string      `json:"logo_file_id,omitempty"`
	WebsiteURL  string      `json:"website_url,omitempty"`
	City        string      `json:"city,omitempty"`
	Verified    bool        `json:"verified"`
	VerifiedAt  *time.Time  `json:"verified_at,omitempty"`
	ClosedAt    *time.Time  `json:"closed_at,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	Company     *CompanyDTO `json:"company,omitempty"`
}

// OrgCardDTO is an org in a search result (04 §2.2 searchOrgs,
// listCompanies).
type OrgCardDTO struct {
	OrgID         string `json:"org_id"`
	Kind          string `json:"kind" enum:"platform,university,club,company,ngo"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	LogoURL       string `json:"logo_url,omitempty" doc:"Short-lived download URL (at most 5 minutes)"`
	LogoPublicURL string `json:"logo_public_url,omitempty" doc:"Permanent, cacheable URL of the logo; only for an active org that is not a sensitive club, once the scan cleared it"`
	City          string `json:"city,omitempty"`
	Verified      bool   `json:"verified"`
}

// OrgCardList is a page of OrgCardDTO.
type OrgCardList struct {
	Items      []OrgCardDTO `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// AdminOrgDTO is an org in the admin list (04 §2.2 adminListOrgs).
type AdminOrgDTO struct {
	OrgID      string     `json:"org_id"`
	Kind       string     `json:"kind" enum:"platform,university,club,company,ngo"`
	Name       string     `json:"name"`
	Slug       string     `json:"slug"`
	Status     string     `json:"status" enum:"pending_review,active,suspended,closed"`
	City       string     `json:"city,omitempty"`
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
	CreatedBy  string     `json:"created_by,omitempty" doc:"User who registered the org"`
	CreatedAt  time.Time  `json:"created_at"`
	// Company is the review facts of a company (BE-165): the approval list
	// warns about a missing or invalid tax identifier and an unverified
	// e-mail domain.
	Company *AdminCompanyReviewDTO `json:"company,omitempty"`
}

// AdminCompanyReviewDTO is what the admin list shows of a company's
// application (BE-165); the numbers are on getOrg.
type AdminCompanyReviewDTO struct {
	TaxIDKind      string `json:"tax_id_kind,omitempty" enum:"vkn,tckn"`
	TaxIDValid     bool   `json:"tax_id_valid" doc:"A VKN or TCKN that passes its check digits was given"`
	EmailDomain    string `json:"email_domain,omitempty"`
	DomainVerified bool   `json:"domain_verified" doc:"false: no e-mail domain, or no applicant address at it (warn the reviewer)"`
}

// AdminOrgList is a page of AdminOrgDTO.
type AdminOrgList struct {
	Items      []AdminOrgDTO `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func toPublicDTO(v OrgView) OrgPublicDTO {
	o := v.Org
	d := OrgPublicDTO{
		OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug, Status: string(o.Status),
		Description: str(o.Description), LogoURL: v.LogoURL, LogoPublicURL: v.LogoPublicURL, WebsiteURL: str(o.WebsiteURL),
		City: str(o.City), Verified: o.Verified(),
	}
	if c := v.Company; c != nil {
		d.Company = &CompanyPublicDTO{SectorCode: str(c.SectorCode), SizeBand: str(c.SizeBand), HQCity: str(c.HQCity)}
		if v.Manage {
			d.Company.EmailDomain, d.Company.DomainVerifiedAt = str(c.EmailDomain), utc(c.DomainVerifiedAt)
			t := toTaxDTO(*c, v.FullTaxNo)
			d.Company.CompanyTaxDTO = &t
		}
	}
	return d
}

func toOrgDTO(v OrgView) OrgDTO {
	o := v.Org
	d := OrgDTO{
		OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug, Status: string(o.Status),
		Description: str(o.Description), LogoURL: v.LogoURL, WebsiteURL: str(o.WebsiteURL), City: str(o.City),
		Verified: o.Verified(), VerifiedAt: utc(o.VerifiedAt), ClosedAt: utc(o.ClosedAt),
		CreatedAt: o.CreatedAt.UTC(), UpdatedAt: o.UpdatedAt.UTC(),
	}
	if o.LogoFileID != nil {
		d.LogoFileID = o.LogoFileID.String()
	}
	if c := v.Company; c != nil {
		d.Company = &CompanyDTO{
			SectorCode: str(c.SectorCode), SizeBand: str(c.SizeBand), EmailDomain: str(c.EmailDomain),
			DomainVerifiedAt: utc(c.DomainVerifiedAt), HQCity: str(c.HQCity), CompanyTaxDTO: toTaxDTO(*c, v.FullTaxNo),
		}
	}
	return d
}

func toCardDTO(v OrgView) OrgCardDTO {
	o := v.Org
	return OrgCardDTO{
		OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug, LogoURL: v.LogoURL,
		LogoPublicURL: v.LogoPublicURL, City: str(o.City), Verified: o.Verified(),
	}
}

func toAdminDTO(v OrgView) AdminOrgDTO {
	o := v.Org
	d := AdminOrgDTO{
		OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug, Status: string(o.Status),
		City: str(o.City), Verified: o.Verified(), VerifiedAt: utc(o.VerifiedAt), ClosedAt: utc(o.ClosedAt),
		CreatedAt: o.CreatedAt.UTC(),
	}
	if o.CreatedBy != nil {
		d.CreatedBy = o.CreatedBy.String()
	}
	if c := v.Company; c != nil {
		d.Company = &AdminCompanyReviewDTO{
			TaxIDKind: kindStr(c.TaxIDKind), TaxIDValid: c.TaxIDValid(), EmailDomain: str(c.EmailDomain),
			DomainVerified: c.DomainVerified(),
		}
	}
	return d
}

// toTaxDTO maps the tax identity of c: the VKN masked unless full.
func toTaxDTO(c Company, full bool) CompanyTaxDTO {
	d := CompanyTaxDTO{
		TaxIDKind: kindStr(c.TaxIDKind), TaxNo: str(c.TaxNo), TaxOffice: str(c.TaxOffice), TaxIDValid: c.TaxIDValid(),
		DomainVerified: c.DomainVerified(),
	}
	if d.TaxNo != "" && !full {
		d.TaxNo, d.TaxNoMasked = taxid.Mask(d.TaxNo, taxid.MaskedDigits), true
	}
	return d
}

func kindStr(k *TaxIDKind) string {
	if k == nil {
		return ""
	}
	return string(*k)
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// MemberUserDTO is the member's public identity in a member list (04 §2.2;
// no e-mail: 04 §V.4). avatar_url is the student profile's (BE-023).
type MemberUserDTO struct {
	UserID    string `json:"user_id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// MemberDTO is a membership as the org's admins see it (04 §2.2).
type MemberDTO struct {
	MembershipID string        `json:"membership_id"`
	User         MemberUserDTO `json:"user"`
	Role         string        `json:"role" enum:"member,club_admin,university_rep,career_staff,company_owner,company_admin,recruiter,staff,ngo_admin,ngo_member"`
	Status       string        `json:"status" enum:"pending,active,ended"`
	RequestedAt  *time.Time    `json:"requested_at,omitempty"`
	JoinedAt     *time.Time    `json:"joined_at,omitempty"`
}

// MemberList is a page of MemberDTO.
type MemberList struct {
	Items      []MemberDTO `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// MembershipOrgDTO is the org of one of my memberships.
type MembershipOrgDTO struct {
	OrgID   string `json:"org_id"`
	Kind    string `json:"kind" enum:"platform,university,club,company,ngo"`
	Name    string `json:"name"`
	LogoURL string `json:"logo_url,omitempty"`
}

// MyMembershipDTO is one of the caller's memberships (04 §2.2
// listMyMemberships; sensitive clubs included, only for the caller).
type MyMembershipDTO struct {
	MembershipID string           `json:"membership_id"`
	Org          MembershipOrgDTO `json:"org"`
	Role         string           `json:"role"`
	Status       string           `json:"status" enum:"pending,active,ended"`
	JoinedAt     *time.Time       `json:"joined_at,omitempty"`
}

// MyMembershipList is a page of MyMembershipDTO.
type MyMembershipList struct {
	Items      []MyMembershipDTO `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// InvitationDTO is an invitation as the org's admins see it (04 §2.2).
type InvitationDTO struct {
	InvitationID string    `json:"invitation_id"`
	Email        string    `json:"email"`
	Role         string    `json:"role"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// InvitationList is a page of InvitationDTO.
type InvitationList struct {
	Items      []InvitationDTO `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// AcceptedInvitationDTO is the answer of acceptOrgInvitation.
type AcceptedInvitationDTO struct {
	MembershipID string `json:"membership_id"`
	OrgID        string `json:"org_id"`
	Role         string `json:"role"`
}

// JoinRequestDTO is the answer of joinClub.
type JoinRequestDTO struct {
	MembershipID string `json:"membership_id"`
	Status       string `json:"status" enum:"pending,active"`
}

func toMemberDTO(v MemberView) MemberDTO {
	m := v.Membership
	return MemberDTO{
		MembershipID: m.ID.String(), Role: string(m.Role), Status: string(m.Status),
		User:        MemberUserDTO{UserID: m.UserID.String(), FirstName: v.FirstName, LastName: v.LastName, AvatarURL: v.AvatarURL},
		RequestedAt: utc(m.RequestedAt), JoinedAt: utc(m.JoinedAt),
	}
}

func toInvitationDTO(i Invitation) InvitationDTO {
	return InvitationDTO{InvitationID: i.ID.String(), Email: i.Email, Role: string(i.Role), ExpiresAt: i.ExpiresAt.UTC()}
}
