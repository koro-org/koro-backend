package parties

import (
	"context"
	"regexp"
	"slices"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// vknQueryRe recognises a VKN typed into the admin search box.
var vknQueryRe = regexp.MustCompile(`^[0-9]{10}$`)

// Get implements Parties.
func (s *service) Get(ctx context.Context, partyID uuid.UUID) (LegalParty, error) {
	p, err := s.d.Store.GetParty(ctx, partyID)
	if err != nil {
		return LegalParty{}, err
	}
	return p.withoutSecrets(), nil
}

// Platform implements Parties.
func (s *service) Platform(ctx context.Context) (LegalParty, error) {
	p, err := s.d.Store.GetPlatform(ctx)
	if err != nil {
		return LegalParty{}, err
	}
	return p.withoutSecrets(), nil
}

// BuyerPartyOf implements Parties: the newest verified buyer party.
func (s *service) BuyerPartyOf(ctx context.Context, companyOrgID uuid.UUID) (LegalParty, bool, error) {
	ps, err := s.d.Store.BuyerParties(ctx, companyOrgID)
	if err != nil {
		return LegalParty{}, false, err
	}
	for _, p := range ps { // newest first
		if p.Status == StatusVerified {
			return p.withoutSecrets(), true, nil
		}
	}
	return LegalParty{}, false, nil
}

// ListOrgParties implements Service. Order: the org (404) → authz on the
// org's collection (404 outside the org, 403 without a role) → every party
// is authorised again with its own scope (03 k21: a club admin sees its
// payees' parties, never the org's own ones).
func (s *service) ListOrgParties(ctx context.Context, a authz.Actor, orgID uuid.UUID) ([]PartyView, error) {
	if _, err := s.org(ctx, orgID); err != nil {
		return nil, err
	}
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyRead, orgListResource(orgID)); err != nil {
		return nil, err
	}
	ops, err := s.d.Store.OrgParties(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var visible []LegalParty
	for _, op := range ops {
		if authz.Can(ctx, a, authz.PartiesLegalPartyRead, partyResource(orgID, op)) == nil {
			visible = append(visible, op.Party)
		}
	}
	return s.views(ctx, visible, fullTaxIDs(a))
}

// GetOrgParty implements Service. Order: the org (404) → authz on the
// org's collection (404 outside the org, 403 without a role: no ID
// probing) → the party within the org (404) → authz on the party (k21).
func (s *service) GetOrgParty(ctx context.Context, a authz.Actor, orgID, partyID uuid.UUID) (PartyView, error) {
	if _, err := s.org(ctx, orgID); err != nil {
		return PartyView{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyRead, orgListResource(orgID)); err != nil {
		return PartyView{}, err
	}
	op, err := s.d.Store.OrgParty(ctx, orgID, partyID, false)
	if err != nil {
		return PartyView{}, err
	}
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyRead, partyResource(orgID, op)); err != nil {
		return PartyView{}, err
	}
	return s.view(ctx, op.Party, fullTaxIDs(a))
}

// AdminListParties implements Service: every party, newest first (03
// §5.13: finance and platform admins).
func (s *service) AdminListParties(ctx context.Context, a authz.Actor, q AdminQuery) (PartyPage, error) {
	if err := authz.Can(ctx, a, authz.PartiesLegalPartyRead, adminSurface); err != nil {
		return PartyPage{}, err
	}
	f := ListFilter{Kind: q.Kind, Status: q.Status}
	if q.Kind != "" && !q.Kind.Valid() {
		return PartyPage{}, problem.Field("query.kind", "must be platform, buyer_company or payee")
	}
	if q.Status != "" && !q.Status.Valid() {
		return PartyPage{}, problem.Field("query.status", "is not a legal party status")
	}
	var err error
	if f.Q, err = searchText(q.Q); err != nil {
		return PartyPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListParties(ctx, f, q.After, limit+1)
	if err != nil {
		return PartyPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	views, err := s.views(ctx, rows, true)
	if err != nil {
		return PartyPage{}, err
	}
	out := PartyPage{Items: views}
	if more {
		next := rows[len(rows)-1].ID
		out.Next = &next
	}
	return out, nil
}

// view builds the view of one party with its documents.
func (s *service) view(ctx context.Context, p LegalParty, full bool) (PartyView, error) {
	vs, err := s.views(ctx, []LegalParty{p}, full)
	if err != nil {
		return PartyView{}, err
	}
	return vs[0], nil
}

// views builds the views of ps (one document query for all of them).
func (s *service) views(ctx context.Context, ps []LegalParty, full bool) ([]PartyView, error) {
	out := make([]PartyView, len(ps))
	if len(ps) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(ps))
	for i, p := range ps {
		ids[i] = p.ID
	}
	docs, err := s.d.Store.PartyDocuments(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, p := range ps {
		out[i] = PartyView{
			Party: p.withoutSecrets(), ETag: httpx.ETag(p.UpdatedAt), FullTaxIDs: full, TCKNOnFile: p.HasTCKN(),
			Documents: slices.DeleteFunc(slices.Clone(docs), func(d Document) bool { return d.LegalPartyID != p.ID }),
		}
	}
	return out, nil
}

// reload reads a party after a write and builds its view.
func (s *service) reload(ctx context.Context, id uuid.UUID, full bool) (PartyView, error) {
	p, err := s.d.Store.GetParty(ctx, id)
	if err != nil {
		return PartyView{}, err
	}
	return s.view(ctx, p, full)
}

// searchText normalises the q of a legal party search (body.q: 2–100
// characters, no control or bidi character; "" stays empty).
func searchText(q string) (string, error) {
	if q == "" {
		return "", nil
	}
	n, err := textx.NormalizeText(q)
	if err != nil {
		return "", problem.Field("body.q", "contains control or bidi characters")
	}
	if c := utf8.RuneCountInString(n); c < 2 || c > 100 {
		return "", problem.Field("body.q", "must be 2-100 characters")
	}
	return n, nil
}

// isVKNQuery reports a 10-digit query (matched against the VKN exactly).
func isVKNQuery(q string) bool { return vknQueryRe.MatchString(q) }
