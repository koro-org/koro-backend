package orgs

import (
	"context"
	"fmt"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// CompanyInput is the body of createCompany (04 §2.2).
type CompanyInput struct {
	Name        string
	Slug        string // optional: derived from Name
	SectorCode  string
	SizeBand    string
	WebsiteURL  string // optional
	EmailDomain string // optional
	HQCity      string // optional
}

// slugAttempts bounds the numbered slug candidates ("acme", "acme-2", …)
// before a random suffix is used.
const slugAttempts = 20

// insertOrg writes o with the requested slug (taken → ErrSlugTaken) or,
// when none is requested, with the first free slug derived from the name.
func (s *service) insertOrg(ctx context.Context, o Org, requested string) (Org, error) {
	if requested != "" {
		o.Slug = requested
		stored, ok, err := s.d.Store.InsertOrg(ctx, o)
		if err != nil {
			return Org{}, err
		}
		if !ok {
			return Org{}, ErrSlugTaken
		}
		return stored, nil
	}
	base := textx.Slug(o.Name, maxSlug)
	if base == "" {
		base = "org"
	}
	var stored Org
	ok, err := textx.FreeSlug(base, maxSlug, slugAttempts, func(slug string) (inserted bool, err error) {
		o.Slug = slug
		stored, inserted, err = s.d.Store.InsertOrg(ctx, o)
		return inserted, err
	})
	switch {
	case err != nil:
		return Org{}, err
	case !ok:
		return Org{}, ErrSlugTaken
	}
	return stored, nil
}

// companyFields validates the company profile fields of a request.
type companyFields struct {
	sector, size, domain, hq *string
}

func cleanCompany(sector, size, domain, hq string, requireProfile bool) (companyFields, error) {
	var f companyFields
	var err error
	if requireProfile && sector == "" {
		return f, problem.Field("body.sector_code", "is required")
	}
	if requireProfile && size == "" {
		return f, problem.Field("body.size_band", "is required")
	}
	if f.sector, err = cleanSector("body.sector_code", sector); err != nil {
		return f, err
	}
	if f.size, err = cleanSizeBand("body.size_band", size); err != nil {
		return f, err
	}
	if f.domain, err = cleanEmailDomain("body.email_domain", domain); err != nil {
		return f, err
	}
	if f.hq, err = cleanLine("body.hq_city", hq, maxCity); err != nil {
		return f, err
	}
	return f, nil
}

// CreateCompany implements Service. The org, its company profile, the
// founder's company_owner membership, the audit record and
// org.review_requested (BE-144) share one transaction (with the
// Idempotency-Key when one is sent, 06 H-5);
// membership.started and the session revocation follow the commit.
func (s *service) CreateCompany(ctx context.Context, a authz.Actor, in CompanyInput) (OrgView, error) {
	if err := authz.Can(ctx, a, authz.OrgsCompanyCreate, newCompanyResource); err != nil {
		return OrgView{}, err
	}
	name, err := cleanName("body.name", in.Name)
	if err != nil {
		return OrgView{}, err
	}
	var slug string
	if in.Slug != "" {
		if slug, err = cleanSlug("body.slug", in.Slug); err != nil {
			return OrgView{}, err
		}
	}
	site, err := cleanWebsite("body.website_url", in.WebsiteURL)
	if err != nil {
		return OrgView{}, err
	}
	cf, err := cleanCompany(in.SectorCode, in.SizeBand, in.EmailDomain, in.HQCity, true)
	if err != nil {
		return OrgView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Name, Slug                     string
		Sector, Size, Domain, HQ, Site *string
	}{name, slug, cf.sector, cf.size, cf.domain, cf.hq, site})
	if err != nil {
		return OrgView{}, err
	}
	founder := a.UserID
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		now := s.now()
		o, err := s.insertOrg(ctx, Org{
			ID: uuid.NewV7(), Kind: KindCompany, Name: name, Status: StatusPendingReview, WebsiteURL: site,
			City: cf.hq, CreatedBy: &founder,
		}, slug)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Store.InsertCompany(ctx, Company{
			OrgID: o.ID, SectorCode: cf.sector, SizeBand: cf.size, EmailDomain: cf.domain, HQCity: cf.hq,
		}); err != nil {
			return uuid.Nil(), err
		}
		m, err := s.d.Store.InsertMember(ctx, Membership{
			ID: uuid.NewV7(), OrgID: o.ID, UserID: founder, Role: ownerRole, Status: MemberActive, Source: SourceAdmin,
			JoinedAt: &now,
		})
		if err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{"org_kind": string(KindCompany), "slug": o.Slug, "membership_id": m.ID.String()}
		if err := s.d.Audit.Log(ctx, auditEvent(a, authz.OrgsCompanyCreate, o.ID, "", meta)); err != nil {
			return uuid.Nil(), err
		}
		// Q-33: anyone may register a company; a platform admin verifies it.
		if err := s.emitReviewRequested(ctx, o); err != nil {
			return uuid.Nil(), err
		}
		s.emitStarted(ctx, m)
		// 03 §2.2-6 / 04 §2.2: a company role is an MFA role; the founder
		// signs in again so the new session policy applies at once.
		s.revokeAfterCommit(ctx, founder)
		return o.ID, nil
	})
	if err != nil {
		return OrgView{}, err
	}
	o, err := s.d.Store.GetOrg(ctx, id)
	if err != nil {
		return OrgView{}, err
	}
	return s.view(ctx, o)
}

// creatableKinds are the kinds OrgAdmin.Create opens (platform is seeded,
// ngo is phase 2).
var creatableKinds = map[Kind]bool{KindClub: true, KindUniversity: true, KindCompany: true}

// Create implements OrgAdmin. It joins the transaction carried by ctx
// (db.TxManager.WithTx nests without a savepoint), so the caller's
// rollback removes the org as well.
func (s *service) Create(ctx context.Context, in CreateOrg) (uuid.UUID, error) {
	if !creatableKinds[in.Kind] {
		return uuid.Nil(), fmt.Errorf("orgs: create: kind %q cannot be created here: %w", in.Kind, ErrInvalidState)
	}
	status := in.Status
	switch status {
	case "":
		status = StatusActive
	case StatusActive, StatusPendingReview:
	default:
		return uuid.Nil(), fmt.Errorf("orgs: create: initial status %q: %w", status, ErrInvalidState)
	}
	name, err := cleanName("name", in.Name)
	if err != nil {
		return uuid.Nil(), err
	}
	var slug string
	if in.Slug != "" {
		if slug, err = cleanSlug("slug", in.Slug); err != nil {
			return uuid.Nil(), err
		}
	}
	city, err := cleanLine("city", in.City, maxCity)
	if err != nil {
		return uuid.Nil(), err
	}
	desc, err := cleanDescription("description", in.Description)
	if err != nil {
		return uuid.Nil(), err
	}
	site, err := cleanWebsite("website_url", in.WebsiteURL)
	if err != nil {
		return uuid.Nil(), err
	}
	var id uuid.UUID
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		o, err := s.insertOrg(ctx, Org{
			ID: uuid.NewV7(), Kind: in.Kind, Name: name, Status: status, City: city, Description: desc,
			WebsiteURL: site, CreatedBy: in.CreatedBy,
		}, slug)
		if err != nil {
			return err
		}
		if in.Kind == KindCompany {
			if err := s.d.Store.InsertCompany(ctx, Company{OrgID: o.ID}); err != nil {
				return err
			}
		}
		id = o.ID
		return nil
	})
	if err != nil {
		return uuid.Nil(), err
	}
	return id, nil
}
