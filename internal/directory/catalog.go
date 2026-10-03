package directory

import (
	"context"
	"errors"
	"slices"
	"uuid"
)

// GetClub implements Catalog.
func (s *service) GetClub(ctx context.Context, orgID uuid.UUID) (Club, error) {
	if orgID == uuid.Nil() {
		return Club{}, ErrNotFound
	}
	return s.d.Store.GetClub(ctx, orgID)
}

// SensitiveOrgIDs implements Catalog.
func (s *service) SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	ids, err := s.d.Store.SensitiveAmong(ctx, orgIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// GetUniversity implements Catalog.
func (s *service) GetUniversity(ctx context.Context, id uuid.UUID) (University, error) {
	return s.d.Store.GetUniversity(ctx, id)
}

// UniversityByEmailDomain implements Catalog. The domain is a machine
// identifier: it is lower-cased without Turkish rules (05 §9.5).
func (s *service) UniversityByEmailDomain(ctx context.Context, domain string) (uuid.UUID, bool, error) {
	d := normalizeDomain(domain)
	if d == "" || len(d) > 253 {
		return uuid.Nil(), false, nil
	}
	m, err := s.d.Store.MatchDomain(ctx, domainCandidates(d))
	switch {
	case errors.Is(err, ErrNotFound):
		return uuid.Nil(), false, nil
	case err != nil:
		return uuid.Nil(), false, err
	}
	return m.UniversityID, true, nil
}

// GetDepartment implements Catalog.
func (s *service) GetDepartment(ctx context.Context, id uuid.UUID) (Department, error) {
	return s.d.Store.GetDepartment(ctx, id)
}

// SkillsExist implements Catalog.
func (s *service) SkillsExist(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	uniq := slices.Clone(ids)
	slices.SortFunc(uniq, func(a, b uuid.UUID) int { return compareUUID(a, b) })
	uniq = slices.Compact(uniq)
	n, err := s.d.Store.CountActiveSkills(ctx, uniq)
	if err != nil {
		return err
	}
	if n != len(uniq) {
		return ErrUnknownSkill
	}
	return nil
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// UniversityOrgs implements Service: the university's own org and the
// orgs of its clubs (sensitive ones included: a club's existence is
// public, 03 §3.6). An unknown university has none.
func (s *service) UniversityOrgs(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error) {
	u, err := s.d.Store.GetUniversity(ctx, universityID)
	switch {
	case errors.Is(err, ErrNotFound):
		return []uuid.UUID{}, nil
	case err != nil:
		return nil, err
	}
	clubs, err := s.d.Store.ClubOrgsOf(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return append([]uuid.UUID{u.OrgID}, clubs...), nil
}
