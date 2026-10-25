package students

import (
	"context"
	"errors"
	"slices"
	"uuid"

	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/problem"
)

// Level 2 discoverability (01 §4.9 Discoverable; 03 §5.10 k20, Y4; 04
// §V.2; T-68, T-237; BE-046).
const (
	// DefaultDiscoveryLimit and MaxDiscoveryLimit bound a search page.
	DefaultDiscoveryLimit = 20
	MaxDiscoveryLimit     = 50
	// discoveryBatch is how many candidates a search reads per store call.
	discoveryBatch = 200
	// maxDiscoveryFilter is the most values one filter takes.
	maxDiscoveryFilter = 50
)

// errAnonProfileJSON stops an AnonProfile (it carries the user ID) from
// ever being serialised: the answer is the AnonProfileDTO of WithAnonID.
var errAnonProfileJSON = errors.New("students: AnonProfile is not an answer (it carries the user ID); use WithAnonID")

// AnonProfile is one level 2 result: the user ID for the caller's
// company-scoped pseudonym (01 §4.16) and the level 2 projection without
// it. It never marshals.
type AnonProfile struct {
	UserID uuid.UUID
	dto    AnonProfileDTO
}

// WithAnonID returns the level 2 answer under anonID (04 §V.2: the
// pseudonym, never the user ID).
func (p AnonProfile) WithAnonID(anonID string) (AnonProfileDTO, error) {
	if anonID == "" || p.UserID == uuid.Nil() {
		return AnonProfileDTO{}, ErrNotFound
	}
	d := p.dto
	d.AnonID = anonID
	d.Skills, d.Languages, d.Experiences = slices.Clone(d.Skills), slices.Clone(d.Languages), slices.Clone(d.Experiences)
	return d, nil
}

// MarshalJSON refuses: an AnonProfile is no answer.
func (AnonProfile) MarshalJSON() ([]byte, error) { return nil, errAnonProfileJSON }

// DiscoveryPage is a page of Search. Next is the position of the next page
// (the last item's user ID) or nil; it is a user ID, so a caller never
// hands it to a company as it is. BelowThreshold: fewer than 10 students
// match the filters in total, the page is empty (T-68).
type DiscoveryPage struct {
	Items          []AnonProfile
	Next           *uuid.UUID
	BelowThreshold bool
}

// MarshalJSON refuses: a page carries user IDs.
func (DiscoveryPage) MarshalJSON() ([]byte, error) { return nil, errAnonProfileJSON }

// cleanDiscovery validates and normalises q (422 validation_failed naming
// the filter).
func cleanDiscovery(q DiscoveryQuery) (DiscoveryQuery, error) {
	for _, f := range []struct {
		loc string
		n   int
	}{
		{"query.university_id", len(q.UniversityIDs)}, {"query.department_id", len(q.DepartmentIDs)},
		{"query.class_year", len(q.ClassYears)}, {"query.skill_id", len(q.SkillIDs)}, {"query.language_code", len(q.LanguageCodes)},
	} {
		if f.n > maxDiscoveryFilter {
			return q, problem.Field(f.loc, "at most 50 values")
		}
	}
	if q.DegreeLevel != "" && !q.DegreeLevel.Valid() {
		return q, problem.Field("query.degree_level", "must be associate, bachelor, master or doctorate")
	}
	for _, y := range q.ClassYears {
		if y < 1 || y > 8 {
			return q, problem.Field("query.class_year", "must be between 1 and 8")
		}
	}
	for _, c := range q.LanguageCodes {
		if !languageCodeRe.MatchString(c) {
			return q, problem.Field("query.language_code", "must be an ISO 639 code")
		}
	}
	uniq := func(ids []uuid.UUID) []uuid.UUID {
		out := slices.Clone(ids)
		slices.SortFunc(out, compareUUID)
		return slices.DeleteFunc(slices.Compact(out), func(id uuid.UUID) bool { return id == uuid.Nil() })
	}
	q.UniversityIDs, q.DepartmentIDs, q.SkillIDs = uniq(q.UniversityIDs), uniq(q.DepartmentIDs), uniq(q.SkillIDs)
	q.ClassYears = slices.Compact(slices.Sorted(slices.Values(q.ClassYears)))
	q.LanguageCodes = slices.Compact(slices.Sorted(slices.Values(q.LanguageCodes)))
	switch {
	case q.Limit <= 0:
		q.Limit = DefaultDiscoveryLimit
	case q.Limit > MaxDiscoveryLimit:
		q.Limit = MaxDiscoveryLimit
	}
	return q, nil
}

// discoverableAmong returns which of ids pass the facts other modules
// hold: a live adult account (identity), an active student verification
// (identity) and the discovery_profile consent in force (consent, read
// now: T-237). A missing port finds nobody (fail closed).
func (s *service) discoverableAmong(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 || s.d.Verifications == nil || s.d.Consents == nil {
		return out, nil
	}
	users, err := s.d.Users.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	ver, err := s.d.Verifications.ActiveMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	consented, err := s.d.Consents.FilterActive(ctx, ids, consent.PurposeDiscoveryProfile)
	if err != nil {
		return nil, err
	}
	for _, id := range consented {
		u, ok := users[id]
		if _, verified := ver[id]; ok && verified && u.Status == identity.StatusActive && !u.IsMinor {
			out[id] = true
		}
	}
	return out, nil
}

// IsDiscoverable implements Discoverable.
func (s *service) IsDiscoverable(ctx context.Context, userID uuid.UUID) (bool, error) {
	p, err := s.d.Store.GetProfile(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	if p.Status != ProfileActive {
		return false, nil
	}
	switch pr, err := s.d.Store.GetPrivacy(ctx, userID); {
	case errors.Is(err, ErrNotFound):
		return false, nil // the defaults: level 2 off
	case err != nil:
		return false, err
	case !pr.Discoverable:
		return false, nil
	}
	ok, err := s.discoverableAmong(ctx, []uuid.UUID{userID})
	if err != nil {
		return false, err
	}
	return ok[userID], nil
}

// Search implements Discoverable. The candidates (active profiles with
// level 2 on that match the filters, from the store in user_id order) are
// checked batch by batch against the facts of the other modules; the walk
// stops once 10 students passed (T-68) and the page after q.After is full.
// Fewer than 10 in total: an empty page with BelowThreshold. The page's
// level 2 projections drop every club_role experience (04 §V.2); there is
// no club filter at all (03 §5.10).
func (s *service) Search(ctx context.Context, q DiscoveryQuery) (DiscoveryPage, error) {
	q, err := cleanDiscovery(q)
	if err != nil {
		return DiscoveryPage{}, err
	}
	total := 0
	var page []uuid.UUID
	for after := uuid.Nil(); ; {
		ids, err := s.d.Store.DiscoveryCandidates(ctx, q, after, discoveryBatch)
		if err != nil {
			return DiscoveryPage{}, err
		}
		ok, err := s.discoverableAmong(ctx, ids)
		if err != nil {
			return DiscoveryPage{}, err
		}
		for _, id := range ids {
			if !ok[id] {
				continue
			}
			total++
			if compareUUID(id, q.After) > 0 && len(page) <= q.Limit {
				page = append(page, id)
			}
		}
		if len(ids) < discoveryBatch || (total >= KThreshold && len(page) > q.Limit) {
			break
		}
		after = ids[len(ids)-1]
	}
	if total < KThreshold {
		return DiscoveryPage{Items: []AnonProfile{}, BelowThreshold: true}, nil
	}
	out := DiscoveryPage{Items: make([]AnonProfile, 0, min(len(page), q.Limit))}
	if len(page) > q.Limit {
		page = page[:q.Limit]
		next := page[q.Limit-1]
		out.Next = &next
	}
	var srcs []CompanySource
	for _, id := range page {
		src, ok, err := s.anonSource(ctx, id)
		if err != nil {
			return DiscoveryPage{}, err
		}
		if ok {
			srcs = append(srcs, src)
		}
	}
	if err := s.nameSkills(ctx, srcs); err != nil {
		return DiscoveryPage{}, err
	}
	for _, src := range srcs {
		out.Items = append(out.Items, AnonProfile{UserID: src.View.Profile.UserID, dto: anonDTO(src)})
	}
	return out, nil
}

// nameSkills names the skills of every source with one SkillNamer call
// (BE-084: one directory query per page, not one per profile); the sources
// share the read-only map.
func (s *service) nameSkills(ctx context.Context, srcs []CompanySource) error {
	var all []Skill
	for _, src := range srcs {
		all = append(all, src.View.Skills...)
	}
	names, err := s.skillNames(ctx, all)
	if err != nil {
		return err
	}
	for i := range srcs {
		srcs[i].SkillNames = names
	}
	return nil
}

// anonSource fills the level 2 source of a discoverable student (no
// avatar, CV, name or contact: level 2 never shows them) without the skill
// names (nameSkills names a whole page at once); ok=false when the profile
// went meanwhile.
func (s *service) anonSource(ctx context.Context, userID uuid.UUID) (CompanySource, bool, error) {
	p, err := s.d.Store.GetProfile(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return CompanySource{}, false, nil
	case err != nil:
		return CompanySource{}, false, err
	}
	v := ProfileView{Profile: p, Stored: true}
	if v.UniversityName, err = s.universityName(ctx)(p.UniversityID); err != nil {
		return CompanySource{}, false, err
	}
	if p.DepartmentID != nil {
		if v.DepartmentName, err = s.departmentName(ctx)(*p.DepartmentID); err != nil {
			return CompanySource{}, false, err
		}
	}
	if v.Skills, err = s.d.Store.Skills(ctx, userID); err != nil {
		return CompanySource{}, false, err
	}
	if v.Languages, err = s.d.Store.Languages(ctx, userID); err != nil {
		return CompanySource{}, false, err
	}
	if v.Experiences, err = s.d.Store.Experiences(ctx, userID); err != nil {
		return CompanySource{}, false, err
	}
	return CompanySource{View: v, Verified: true, DiscoveryConsent: true}, true, nil
}
