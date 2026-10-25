package catalog

import (
	"context"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// browseCatalog is the authz view of the marketplace.
var browseCatalog = authz.Resource{Type: authz.TypeOfferingCatalog}

// GetOffering implements Service: a published offering for every signed-in
// user; an unpublished one for its seller, the platform and its university
// while it decides (404 for anyone else, 03 §3.6).
func (s *service) GetOffering(ctx context.Context, a authz.Actor, id uuid.UUID) (OfferingView, error) {
	o, err := s.authorized(ctx, a, authz.CatalogOfferingRead, id)
	if err != nil {
		return OfferingView{}, err
	}
	return s.view(ctx, a, o)
}

// BrowseOfferings implements Service (04 §2.14): active offerings whose
// sales have not ended, filtered by text, seller, event, university,
// service category and maximum price.
func (s *service) BrowseOfferings(ctx context.Context, a authz.Actor, q BrowseQuery) (OfferingPage, error) {
	if err := authz.Can(ctx, a, authz.CatalogOfferingBrowse, browseCatalog); err != nil {
		return OfferingPage{}, err
	}
	f := BrowseFilter{Now: s.now(), OrgID: q.OrgID, EventID: q.EventID, Category: q.Category, PriceMax: q.PriceMax, Sort: q.Sort}
	switch f.Sort {
	case "":
		f.Sort = SortNewest
	case SortNewest, SortPrice, SortSalesEnd:
	default:
		return OfferingPage{}, problem.Field("query.sort", "must be -created_at, unit_price_amount or sales_end_at")
	}
	if q.Q != "" {
		n, err := normalized("query.q", q.Q)
		if err != nil {
			return OfferingPage{}, err
		}
		if c := utf8.RuneCountInString(n); c < 2 || c > maxQueryLen {
			return OfferingPage{}, problem.Field("query.q", "must be 2-100 characters")
		}
		f.Q = n
	}
	if q.Category != "" && !q.Category.Valid() {
		return OfferingPage{}, problem.Field("query.service_category", "is not a service category")
	}
	if q.UniversityID != nil {
		f.OrgIDs = []uuid.UUID{}
		if s.d.Directory != nil {
			ids, err := s.d.Directory.UniversityOrgs(ctx, *q.UniversityID)
			if err != nil {
				return OfferingPage{}, err
			}
			f.OrgIDs = ids
		}
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.BrowseOfferings(ctx, f, q.After, limit+1)
	if err != nil {
		return OfferingPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	items, err := s.views(ctx, a, rows)
	if err != nil {
		return OfferingPage{}, err
	}
	out := OfferingPage{Items: items, Sort: f.Sort}
	if more {
		last := rows[len(rows)-1]
		k := BrowseKey{ID: last.ID, Price: last.UnitPriceAmount, At: last.CreatedAt}
		if f.Sort == SortSalesEnd {
			k.At = farFuture
			if last.SalesEndAt != nil {
				k.At = *last.SalesEndAt
			}
		}
		out.NextBrowse = &k
	}
	return out, nil
}

// ListOrgOfferings implements Service: every offering of the org, drafts
// included (catalog.offering.list_org).
func (s *service) ListOrgOfferings(ctx context.Context, a authz.Actor, orgID uuid.UUID, q OrgQuery) (OfferingPage, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return OfferingPage{}, err
	}
	if err := authz.Can(ctx, a, authz.CatalogOfferingListOrg, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return OfferingPage{}, err
	}
	if q.Status != "" && !q.Status.Valid() {
		return OfferingPage{}, problem.Field("query.status", "is not an offering status")
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListOrgOfferings(ctx, o.ID, OrgOfferingFilter{Status: q.Status}, q.After, limit+1)
	if err != nil {
		return OfferingPage{}, err
	}
	return s.timePage(ctx, a, rows, limit, func(o Offering) TimeKey { return TimeKey{At: o.CreatedAt, ID: o.ID} })
}

// AdminReviewQueue implements Service: the offerings waiting for the
// platform decision, oldest submission first (catalog.offering.review_queue).
func (s *service) AdminReviewQueue(ctx context.Context, a authz.Actor, q QueueQuery) (OfferingPage, error) {
	if err := authz.Can(ctx, a, authz.CatalogOfferingReviewQueue, adminSurface); err != nil {
		return OfferingPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ReviewQueue(ctx, q.After, limit+1)
	if err != nil {
		return OfferingPage{}, err
	}
	return s.timePage(ctx, a, rows, limit, submittedKey)
}

// universityScanBatch is how many pending university decisions one store
// call reads while a university's queue is filled (the store keeps every
// university's: directory has no university-by-org lookup, see the ticket).
const universityScanBatch = 200

// ListUniversityApprovals implements Service: the offerings of the
// university's clubs that wait for its decision (T-21), oldest submission
// first. org_id must be a university (404 otherwise).
func (s *service) ListUniversityApprovals(ctx context.Context, a authz.Actor, orgID uuid.UUID, q QueueQuery) (OfferingPage, error) {
	uni, err := s.org(ctx, orgID)
	if err != nil {
		return OfferingPage{}, err
	}
	if err := authz.Can(ctx, a, authz.CatalogOfferingReviewQueue, authz.OrgResource(uni.ID, string(uni.Kind), string(uni.Status))); err != nil {
		return OfferingPage{}, err
	}
	if uni.Kind != orgs.KindUniversity {
		return OfferingPage{}, ErrNotFound
	}
	limit := page.ClampLimit(q.Limit)
	var out []Offering
	after := q.After
	seen := map[uuid.UUID]uuid.UUID{} // club org → its university's org
	for len(out) <= limit {
		batch, err := s.d.Store.UniversityQueue(ctx, after, universityScanBatch)
		if err != nil {
			return OfferingPage{}, err
		}
		for _, o := range batch {
			u, ok := seen[o.OrgID]
			if !ok {
				f, err := s.universityOf(ctx, o.OrgID)
				if err != nil {
					return OfferingPage{}, err
				}
				u, seen[o.OrgID] = f.universityOrgID, f.universityOrgID
			}
			if u == uni.ID {
				out = append(out, o)
			}
		}
		if len(batch) < universityScanBatch {
			break
		}
		k := submittedKey(batch[len(batch)-1])
		after = &k
	}
	return s.timePage(ctx, a, out, limit, submittedKey)
}

func submittedKey(o Offering) TimeKey {
	k := TimeKey{ID: o.ID}
	if o.SubmittedAt != nil {
		k.At = *o.SubmittedAt
	}
	return k
}

// timePage cuts rows fetched with limit+1 and builds the page.
func (s *service) timePage(ctx context.Context, a authz.Actor, rows []Offering, limit int, key func(Offering) TimeKey) (OfferingPage, error) {
	rows, more := page.Cut(rows, limit)
	items, err := s.views(ctx, a, rows)
	if err != nil {
		return OfferingPage{}, err
	}
	out := OfferingPage{Items: items}
	if more {
		k := key(rows[len(rows)-1])
		out.NextTime = &k
	}
	return out, nil
}
