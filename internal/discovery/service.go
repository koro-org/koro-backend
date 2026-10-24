package discovery

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/orgs"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// ErrNotFound is the answer for everything a company may not tell apart:
// another company's pseudonym, a malformed one, a student who is not (or no
// longer) discoverable, an org that is not an active company.
var ErrNotFound = problem.ErrNotFound

const (
	// sortRank is the cursor's sort key: the position in the ranking.
	sortRank = "discovery_rank"
	// resolveBatch is how many level 2 candidates one step of a pseudonym
	// resolution reads.
	resolveBatch = 1000
	// attrCompanyOrgID is the anon_profile Attrs key (03 §3.5).
	attrCompanyOrgID = "company_org_id"
)

// authorize is the decision order of both operations (03 §3.2, §5.10
// k20): authz.Can on the company's surface, then the company itself
// (active, a company), then its discovery_access right.
func (s *service) authorize(ctx context.Context, a authz.Actor, act authz.Action, companyOrgID uuid.UUID) error {
	org := companyOrgID
	r := authz.Resource{Type: authz.TypeAnonProfile, OrgID: &org, Attrs: map[string]string{attrCompanyOrgID: org.String()}}
	if err := authz.Can(ctx, a, act, r); err != nil {
		return err
	}
	o, err := s.d.Orgs.Get(ctx, companyOrgID)
	switch {
	case errors.Is(err, orgs.ErrNotFound):
		return ErrNotFound
	case err != nil:
		return err
	case o.Kind != orgs.KindCompany:
		return ErrNotFound
	}
	switch o.Status {
	case orgs.StatusActive:
	case orgs.StatusSuspended:
		return orgs.ErrOrgSuspended
	case orgs.StatusPendingReview:
		return orgs.ErrOrgNotVerified
	default:
		return ErrNotFound
	}
	av, err := s.d.Entitlements.Available(ctx, companyOrgID, entitlements.FeatureDiscoveryAccess)
	if err != nil {
		return err
	}
	if !av.Entitled() {
		return entitlements.ErrNoEntitlement
	}
	return nil
}

// sessionOf is the session the ranking seed belongs to.
func sessionOf(a authz.Actor) uuid.UUID {
	if a.SessionID != uuid.Nil() {
		return a.SessionID
	}
	return a.UserID
}

// filters is the canonical form of a query's filters: each list sorted
// and without duplicates (the order of repeated parameters does not change
// the result, the cursor or the relevance).
type filters struct {
	q                         students.DiscoveryQuery
	universities, departments []string
	years                     []string
	skills, languages         []string
}

func canonical(q Query) filters {
	ids := func(in []uuid.UUID) ([]uuid.UUID, []string) {
		out := slices.Clone(in)
		slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		out = slices.DeleteFunc(slices.Compact(out), func(id uuid.UUID) bool { return id == uuid.Nil() })
		strs := make([]string, len(out))
		for i, id := range out {
			strs[i] = id.String()
		}
		return out, strs
	}
	f := filters{q: students.DiscoveryQuery{DegreeLevel: q.DegreeLevel}}
	f.q.UniversityIDs, f.universities = ids(q.UniversityIDs)
	f.q.DepartmentIDs, f.departments = ids(q.DepartmentIDs)
	f.q.SkillIDs, f.skills = ids(q.SkillIDs)
	f.q.ClassYears = slices.Compact(slices.Sorted(slices.Values(q.ClassYears)))
	for _, y := range f.q.ClassYears {
		f.years = append(f.years, strconv.Itoa(int(y)))
	}
	f.q.LanguageCodes = slices.Compact(slices.Sorted(slices.Values(q.LanguageCodes)))
	f.languages = f.q.LanguageCodes
	return f
}

// fingerprint binds a cursor to the company, the session seed and the
// filters (page.Fingerprint hashes the parts; the seed never leaves).
func (f filters) fingerprint(companyOrgID uuid.UUID, seed []byte) string {
	return page.Fingerprint("searchDiscovery", companyOrgID.String(), hex.EncodeToString(seed),
		strings.Join(f.universities, ","), strings.Join(f.departments, ","), strings.Join(f.years, ","),
		string(f.q.DegreeLevel), strings.Join(f.skills, ","), strings.Join(f.languages, ","))
}

// metadata is the search's audit record: the filters, never a total.
func (f filters) metadata(shown int, below, firstPage bool) map[string]any {
	m := map[string]any{"shown": shown, "below_threshold": below, "first_page": firstPage}
	for k, v := range map[string][]string{
		"university_ids": f.universities, "department_ids": f.departments, "class_years": f.years,
		"skill_ids": f.skills, "language_codes": f.languages,
	} {
		if len(v) > 0 {
			m[k] = v
		}
	}
	if f.q.DegreeLevel != "" {
		m["degree_level"] = string(f.q.DegreeLevel)
	}
	return m
}

// position decodes a search cursor: the position of the page's first
// item in the ranking (0 for the first page).
func position(raw, fp string) (int, error) {
	c, err := page.Decode(raw, sortRank, fp, 1)
	if err != nil || c == nil {
		return 0, err
	}
	n, err := strconv.Atoi(c.Vals[0])
	if err != nil || n < 1 || n > students.MaxDiscoveryMatches || strconv.Itoa(n) != c.Vals[0] {
		return 0, page.ErrInvalidCursor
	}
	return n, nil
}

// Search implements Service.
func (s *service) Search(ctx context.Context, a authz.Actor, companyOrgID uuid.UUID, q Query) (Page, error) {
	if err := s.authorize(ctx, a, authz.DiscoveryProfileSearch, companyOrgID); err != nil {
		return Page{}, err
	}
	if q.invalid != nil {
		return Page{}, q.invalid
	}
	f := canonical(q)
	seed, err := s.d.Pseudonyms.Seed(sessionOf(a), companyOrgID)
	if err != nil {
		return Page{}, err
	}
	fp := f.fingerprint(companyOrgID, seed)
	from, err := position(q.Cursor, fp)
	if err != nil {
		return Page{}, err
	}
	limit := page.ClampLimit(q.Limit)
	set, err := s.d.Students.DiscoveryMatches(ctx, f.q, students.MaxDiscoveryMatches)
	if err != nil {
		return Page{}, err
	}
	out := Page{Items: []students.AnonProfileDTO{}, BelowThreshold: set.BelowThreshold}
	if !set.BelowThreshold && len(set.Matches) > 0 {
		if set.Truncated {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "discovery ranking truncated: more students match than one ranking reads",
				slog.String("org_id", companyOrgID.String()), slog.Int("read", len(set.Matches)))
		}
		ids := make([]uuid.UUID, len(set.Matches))
		for i, m := range set.Matches {
			ids[i] = m.UserID
		}
		boosts, err := s.boosts(ctx, ids)
		if err != nil {
			return Page{}, err
		}
		order := rank(set.Matches, boosts, len(f.q.SkillIDs), len(f.q.LanguageCodes), seed)
		if from < len(order) {
			end := min(from+limit, len(order))
			if out.Items, err = s.profiles(ctx, companyOrgID, order[from:end]); err != nil {
				return Page{}, err
			}
			if end < len(order) {
				out.NextCursor = page.Next(sortRank, fp, strconv.Itoa(end))
			}
		}
	}
	org := companyOrgID
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.DiscoveryProfileSearch), Outcome: audit.Success, OrgID: &org, ActorOrgID: &org,
			ResourceType: authz.TypeAnonProfile, Metadata: f.metadata(len(out.Items), out.BelowThreshold, from == 0),
		}.By(a))
	})
	if err != nil {
		return Page{}, err
	}
	return out, nil
}

// profiles returns the level 2 answers of ids (ranking order kept) under
// the company's pseudonyms; a student who left level 2 meanwhile is
// skipped.
func (s *service) profiles(ctx context.Context, companyOrgID uuid.UUID, ids []uuid.UUID) ([]students.AnonProfileDTO, error) {
	found, err := s.d.Students.AnonProfiles(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]students.AnonProfileDTO, 0, len(ids))
	for _, id := range ids {
		p, ok := found[id]
		if !ok {
			continue
		}
		anon, err := s.d.Pseudonyms.AnonID(companyOrgID, id)
		if err != nil {
			return nil, err
		}
		d, err := p.WithAnonID(anon)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// Get implements Service.
func (s *service) Get(ctx context.Context, a authz.Actor, companyOrgID uuid.UUID, anonID string) (students.AnonProfileDTO, error) {
	if err := s.authorize(ctx, a, authz.DiscoveryProfileRead, companyOrgID); err != nil {
		return students.AnonProfileDTO{}, err
	}
	userID, ok, err := s.match(ctx, companyOrgID, anonID)
	if err != nil {
		return students.AnonProfileDTO{}, err
	}
	if !ok {
		return students.AnonProfileDTO{}, ErrNotFound
	}
	found, err := s.d.Students.AnonProfiles(ctx, []uuid.UUID{userID})
	if err != nil {
		return students.AnonProfileDTO{}, err
	}
	p, ok := found[userID]
	if !ok {
		return students.AnonProfileDTO{}, ErrNotFound
	}
	d, err := p.WithAnonID(anonID)
	if err != nil {
		return students.AnonProfileDTO{}, err
	}
	org := companyOrgID
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.DiscoveryProfileRead), Outcome: audit.Success, OrgID: &org, ActorOrgID: &org,
			ResourceType: authz.TypeAnonProfile,
			Metadata:     map[string]any{"anon_id": anonID, audit.MetaSubjectUserID: userID.String()},
		}.By(a))
	})
	if err != nil {
		return students.AnonProfileDTO{}, err
	}
	return d, nil
}

// Resolve implements Service.
func (s *service) Resolve(ctx context.Context, companyOrgID uuid.UUID, anonID string) (uuid.UUID, bool, error) {
	userID, ok, err := s.match(ctx, companyOrgID, anonID)
	if err != nil || !ok {
		return uuid.Nil(), false, err
	}
	ok, err = s.d.Students.IsDiscoverable(ctx, userID)
	if err != nil || !ok {
		return uuid.Nil(), false, err
	}
	return userID, true, nil
}

// AnonID implements Service.
func (s *service) AnonID(companyOrgID, userID uuid.UUID) (string, error) {
	return s.d.Pseudonyms.AnonID(companyOrgID, userID)
}

// match finds the student whose pseudonym for companyOrgID is anonID among
// the profiles with level 2 on: the pseudonym is one-way, so the walk
// derives it for every candidate (the read-time checks are the caller's).
func (s *service) match(ctx context.Context, companyOrgID uuid.UUID, anonID string) (uuid.UUID, bool, error) {
	if !ValidAnonID(anonID) || companyOrgID == uuid.Nil() {
		return uuid.Nil(), false, nil
	}
	for after := uuid.Nil(); ; {
		ids, err := s.d.Students.DiscoveryCandidatesAfter(ctx, after, resolveBatch)
		if err != nil {
			return uuid.Nil(), false, err
		}
		for _, id := range ids {
			ok, err := s.d.Pseudonyms.Matches(anonID, companyOrgID, id)
			if err != nil {
				return uuid.Nil(), false, err
			}
			if ok {
				return id, true, nil
			}
		}
		if len(ids) < resolveBatch {
			return uuid.Nil(), false, nil
		}
		after = ids[len(ids)-1]
	}
}
