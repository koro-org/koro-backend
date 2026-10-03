package orgs

import (
	"time"
)

// CompanyPublicDTO is the public company profile (04 §2.2 OrgPublicDTO.company).
type CompanyPublicDTO struct {
	SectorCode string `json:"sector_code,omitempty"`
	SizeBand   string `json:"size_band,omitempty"`
	HQCity     string `json:"hq_city,omitempty"`
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

// OrgCardDTO is an org in a search result (04 §2.2 searchOrgs).
type OrgCardDTO struct {
	OrgID    string `json:"org_id"`
	Kind     string `json:"kind" enum:"platform,university,club,company,ngo"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	LogoURL  string `json:"logo_url,omitempty"`
	City     string `json:"city,omitempty"`
	Verified bool   `json:"verified"`
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
		Description: str(o.Description), LogoURL: v.LogoURL, WebsiteURL: str(o.WebsiteURL), City: str(o.City),
		Verified: o.Verified(),
	}
	if c := v.Company; c != nil {
		d.Company = &CompanyPublicDTO{SectorCode: str(c.SectorCode), SizeBand: str(c.SizeBand), HQCity: str(c.HQCity)}
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
			DomainVerifiedAt: utc(c.DomainVerifiedAt), HQCity: str(c.HQCity),
		}
	}
	return d
}

func toCardDTO(v OrgView) OrgCardDTO {
	o := v.Org
	return OrgCardDTO{
		OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug, LogoURL: v.LogoURL,
		City: str(o.City), Verified: o.Verified(),
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
	return d
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
