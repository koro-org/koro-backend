package orgs

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/httpx"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// logoTTL is the lifetime of a logo_url (04 §1.11: signed URLs ≤ 5 min).
const logoTTL = 5 * time.Minute

// SearchQuery narrows searchOrgs (04 §2.2: q, kind, university_id, city,
// sort name).
type SearchQuery struct {
	Q            string
	Kind         Kind
	UniversityID *uuid.UUID
	City         string
	Sort         string // SortNewest (default) or SortName
	Limit        int
	After        *ListAfter
}

// AdminQuery narrows adminListOrgs (04 §2.2: kind, status, q).
type AdminQuery struct {
	Kind   Kind
	Status Status
	Q      string
	Limit  int
	After  *ListAfter
}

// OrgPage is a page of orgs (views without a company profile or ETag).
// Next is the keyset position of the next page (nil: last page).
type OrgPage struct {
	Items []OrgView
	Next  *ListAfter
	Sort  string
}

// Get implements OrgDirectory.
func (s *service) Get(ctx context.Context, orgID uuid.UUID) (OrgSummary, error) {
	o, err := s.d.Store.GetOrg(ctx, orgID)
	if err != nil {
		return OrgSummary{}, err
	}
	return o.Summary(), nil
}

// GetMany implements OrgDirectory.
func (s *service) GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]OrgSummary, error) {
	rows, err := s.d.Store.GetOrgs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]OrgSummary, len(rows))
	for _, o := range rows {
		out[o.ID] = o.Summary()
	}
	return out, nil
}

// GetOrg implements Service.
func (s *service) GetOrg(ctx context.Context, a authz.Actor, id uuid.UUID) (OrgView, error) {
	o, err := s.d.Store.GetOrg(ctx, id)
	if err != nil {
		return OrgView{}, err
	}
	return s.readable(ctx, a, o)
}

// GetOrgBySlug implements Service.
func (s *service) GetOrgBySlug(ctx context.Context, a authz.Actor, slug string) (OrgView, error) {
	o, err := s.d.Store.GetOrgBySlug(ctx, slug)
	if err != nil {
		return OrgView{}, err
	}
	return s.readable(ctx, a, o)
}

// readable authorises the public profile read (03 §3.6: pending_review
// and closed orgs only for members and staff) and builds the view.
func (s *service) readable(ctx context.Context, a authz.Actor, o Org) (OrgView, error) {
	if err := authz.Can(ctx, a, authz.OrgsOrgRead, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return OrgView{}, err
	}
	return s.view(ctx, o)
}

// view loads the company profile and the logo URL of o.
func (s *service) view(ctx context.Context, o Org) (OrgView, error) {
	v := OrgView{Org: o, ETag: httpx.ETag(o.UpdatedAt), LogoURL: s.logoURL(ctx, o)}
	if o.Kind == KindCompany {
		c, err := s.d.Store.GetCompany(ctx, o.ID)
		switch {
		case err == nil:
			v.Company = &c
		case !errors.Is(err, ErrNotFound): // a company org without a profile row shows none
			return OrgView{}, err
		}
	}
	return v, nil
}

// logoURL returns a short-lived URL of the logo, or "" (no logo, files not
// wired, scan not clean): a missing logo never fails a read.
func (s *service) logoURL(ctx context.Context, o Org) string {
	if o.LogoFileID == nil || s.d.Files == nil {
		return ""
	}
	dl, err := s.d.Files.PresignGet(ctx, *o.LogoFileID, logoTTL)
	if err != nil {
		if !errors.Is(err, files.ErrNotClean) && !errors.Is(err, files.ErrNotFound) {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "org logo URL not signed",
				slog.String("org_id", o.ID.String()), slog.String("err", err.Error()))
		}
		return ""
	}
	return dl.URL
}

// cleanQuery normalises a free-text query (04 §1.7: 2–100 characters).
func cleanQuery(loc, q string) (string, error) {
	if q == "" {
		return "", nil
	}
	n, err := textx.NormalizeText(q)
	if err != nil {
		return "", problem.Field(loc, "contains control or bidi characters")
	}
	if c := utf8.RuneCountInString(n); c < 2 || c > 100 {
		return "", problem.Field(loc, "must be 2-100 characters")
	}
	return n, nil
}

// publicKinds are the kinds the public directory lists (ngo is phase 2).
var publicKinds = []Kind{KindPlatform, KindUniversity, KindClub, KindCompany}

// SearchOrgs implements Service. Only orgs whose public profile everyone
// may see are listed (03 §3.6: active and suspended).
func (s *service) SearchOrgs(ctx context.Context, a authz.Actor, q SearchQuery) (OrgPage, error) {
	if err := authz.Can(ctx, a, authz.OrgsOrgSearch, catalogResource); err != nil {
		return OrgPage{}, err
	}
	text, err := cleanQuery("query.q", q.Q)
	if err != nil {
		return OrgPage{}, err
	}
	city, err := cleanLine("query.city", q.City, maxCity)
	if err != nil {
		return OrgPage{}, err
	}
	f := ListFilter{Statuses: []Status{StatusActive, StatusSuspended}, Kinds: publicKinds, Q: text, Sort: sortOf(q.Sort)}
	if q.Kind != "" {
		f.Kinds = []Kind{q.Kind}
	}
	if city != nil {
		f.City = *city
	}
	if q.UniversityID != nil {
		f.OrgIDs = []uuid.UUID{}
		if s.d.UniversityOrgs != nil {
			ids, err := s.d.UniversityOrgs(ctx, *q.UniversityID)
			if err != nil {
				return OrgPage{}, err
			}
			f.OrgIDs = append(f.OrgIDs, ids...)
		}
	}
	return s.list(ctx, f, q.Limit, q.After)
}

// sortOf maps the sort query value (04 §2.2 searchOrgs: "name") to the
// store order; anything else is newest first. T-203: "name" used to fall
// through to newest first.
func sortOf(s string) string {
	if s == "name" || s == SortName {
		return SortName
	}
	return SortNewest
}

// list reads one page (limit+1 rows, 05 §10.3) and builds card views.
func (s *service) list(ctx context.Context, f ListFilter, limit int, after *ListAfter) (OrgPage, error) {
	limit = page.ClampLimit(limit)
	rows, err := s.d.Store.ListOrgs(ctx, f, after, limit+1)
	if err != nil {
		return OrgPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := OrgPage{Items: make([]OrgView, len(rows)), Sort: f.Sort}
	for i, o := range rows {
		out.Items[i] = OrgView{Org: o, LogoURL: s.logoURL(ctx, o)}
	}
	if more {
		last := rows[len(rows)-1]
		out.Next = &ListAfter{Name: last.Name, ID: last.ID}
	}
	return out, nil
}

// AdminListOrgs implements Service: every org, any status (03 §5.2
// orgs.org.list_admin: moderator, support, platform_admin).
func (s *service) AdminListOrgs(ctx context.Context, a authz.Actor, q AdminQuery) (OrgPage, error) {
	if err := authz.Can(ctx, a, authz.OrgsOrgListAdmin, adminSurface); err != nil {
		return OrgPage{}, err
	}
	text, err := cleanQuery("query.q", q.Q)
	if err != nil {
		return OrgPage{}, err
	}
	f := ListFilter{Q: text, Sort: SortNewest}
	if q.Kind != "" {
		f.Kinds = []Kind{q.Kind}
	}
	if q.Status != "" {
		f.Statuses = []Status{q.Status}
	}
	return s.list(ctx, f, q.Limit, q.After)
}
