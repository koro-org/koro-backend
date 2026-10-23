package students

import (
	"context"
	"slices"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/problem"
)

// checkGrant applies the CHECK constraints of profile_access_grants.
func checkGrant(g AccessGrant) error {
	switch {
	case !g.Source.Valid():
		return errCheck
	case (g.RevokedAt == nil) != (g.RevokedByKind == nil): // profile_access_grants_revoked_chk
		return errCheck
	case g.RevokedByKind != nil && *g.RevokedByKind != RevokedByStudent && *g.RevokedByKind != RevokedByCompany &&
		*g.RevokedByKind != RevokedBySystem:
		return errCheck
	case g.RevokeReason != nil && utf8.RuneCountInString(*g.RevokeReason) > 300:
		return errCheck
	case g.SuspendedAt != nil && g.SuspendedAt.Before(g.GrantedAt): // profile_access_grants_suspended_chk
		return errCheck
	case g.ResumedAt != nil && (g.ResumedAt.Before(g.GrantedAt) || // profile_access_grants_resumed_chk
		g.SuspendedAt != nil && g.SuspendedAt.Before(*g.ResumedAt)):
		return errCheck
	case g.ContactSharedAt != nil && g.ContactSharedAt.Before(g.GrantedAt): // profile_access_grants_contact_shared_chk
		return errCheck
	}
	return nil
}

// newestFirst orders grants by granted_at DESC, id DESC.
func newestFirst(a, b AccessGrant) int {
	if c := b.GrantedAt.Compare(a.GrantedAt); c != 0 {
		return c
	}
	return compareUUID(b.ID, a.ID)
}

// LockGrants implements Store (one mutex serialises every MemStore call).
func (s *MemStore) LockGrants(context.Context, uuid.UUID) error { return nil }

// InsertGrant implements Store (the primary key and
// profile_access_grants_source_uq; the stamps are the store's).
func (s *MemStore) InsertGrant(_ context.Context, g AccessGrant) (AccessGrant, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g.SuspendedAt, g.ResumedAt, g.RevokedAt, g.RevokedByKind, g.RevokeReason, g.ContactSharedAt = nil, nil, nil, nil, nil, nil
	if err := checkGrant(g); err != nil {
		return AccessGrant{}, false, err
	}
	if slices.ContainsFunc(s.grants, func(x AccessGrant) bool { return x.Source == g.Source && x.SourceID == g.SourceID }) {
		return AccessGrant{}, false, nil
	}
	if slices.ContainsFunc(s.grants, func(x AccessGrant) bool { return x.ID == g.ID }) {
		return AccessGrant{}, false, problem.ErrConflict
	}
	now := s.stamp()
	g.CreatedAt, g.UpdatedAt = now, now
	s.grants = append(s.grants, g)
	return g, true, nil
}

func (s *MemStore) findGrant(match func(AccessGrant) bool) (AccessGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AccessGrant
	for _, g := range s.grants {
		if match(g) {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return AccessGrant{}, ErrNotFound
	}
	slices.SortFunc(out, func(a, b AccessGrant) int { return newestFirst(b, a) }) // oldest first
	return out[0], nil
}

// GrantBySource implements Store.
func (s *MemStore) GrantBySource(_ context.Context, source GrantSource, sourceID uuid.UUID) (AccessGrant, error) {
	return s.findGrant(func(g AccessGrant) bool { return g.Source == source && g.SourceID == sourceID })
}

// ActiveGrant implements Store.
func (s *MemStore) ActiveGrant(_ context.Context, studentID, companyOrgID uuid.UUID) (AccessGrant, error) {
	return s.findGrant(func(g AccessGrant) bool {
		return g.StudentUserID == studentID && g.CompanyOrgID == companyOrgID && g.Active()
	})
}

// GetGrant implements Store.
func (s *MemStore) GetGrant(_ context.Context, studentID, id uuid.UUID) (AccessGrant, error) {
	return s.findGrant(func(g AccessGrant) bool { return g.ID == id && g.StudentUserID == studentID })
}

// RevokeGrants implements Store (active and suspended grants alike).
func (s *MemStore) RevokeGrants(_ context.Context, studentID, companyOrgID uuid.UUID, by RevokedBy, reason *string,
	at time.Time) ([]AccessGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []AccessGrant{}
	next := slices.Clone(s.grants)
	for i, g := range next {
		if g.StudentUserID != studentID || g.CompanyOrgID != companyOrgID || !g.Open() {
			continue
		}
		t, k := at, by
		g.RevokedAt, g.RevokedByKind, g.RevokeReason = &t, &k, reason
		if err := checkGrant(g); err != nil {
			return nil, err
		}
		g.UpdatedAt = s.stamp()
		next[i] = g
		out = append(out, g)
	}
	s.grants = next
	return out, nil
}

// SuspendGrants implements Store.
func (s *MemStore) SuspendGrants(_ context.Context, studentID uuid.UUID, at time.Time) ([]AccessGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []AccessGrant{}
	next := slices.Clone(s.grants)
	for i, g := range next {
		if g.StudentUserID != studentID || !g.Active() || g.GrantedAt.After(at) || g.ResumedAt != nil && g.ResumedAt.After(at) {
			continue
		}
		t := at
		g.SuspendedAt = &t
		if err := checkGrant(g); err != nil {
			return nil, err
		}
		g.UpdatedAt = s.stamp()
		next[i] = g
		out = append(out, g)
	}
	s.grants = next
	return out, nil
}

// ResumeGrant implements Store.
func (s *MemStore) ResumeGrant(_ context.Context, studentID, id uuid.UUID, at time.Time) (AccessGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, g := range s.grants {
		if g.ID != id || g.StudentUserID != studentID || !g.Open() || g.SuspendedAt == nil {
			continue
		}
		t := at
		g.SuspendedAt, g.ResumedAt = nil, &t
		if err := checkGrant(g); err != nil {
			return AccessGrant{}, err
		}
		g.UpdatedAt = s.stamp()
		s.grants[i] = g
		return g, nil
	}
	return AccessGrant{}, ErrNotFound
}

// SuspendedStudentsBefore implements Store.
func (s *MemStore) SuspendedStudentsBefore(_ context.Context, cutoff time.Time, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uuid.UUID
	for _, g := range s.grants {
		if g.Open() && g.SuspendedAt != nil && g.SuspendedAt.Before(cutoff) && compareUUID(g.StudentUserID, after) > 0 &&
			!slices.Contains(out, g.StudentUserID) {
			out = append(out, g.StudentUserID)
		}
	}
	slices.SortFunc(out, compareUUID)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ExpireSuspendedGrants implements Store.
func (s *MemStore) ExpireSuspendedGrants(_ context.Context, studentID uuid.UUID, cutoff, at time.Time) ([]AccessGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []AccessGrant{}
	next := slices.Clone(s.grants)
	for i, g := range next {
		if g.StudentUserID != studentID || !g.Open() || g.SuspendedAt == nil || !g.SuspendedAt.Before(cutoff) {
			continue
		}
		t, k, why := at, RevokedBySystem, ReasonSuspensionExpired
		g.RevokedAt, g.RevokedByKind, g.RevokeReason = &t, &k, &why
		if err := checkGrant(g); err != nil {
			return nil, err
		}
		g.UpdatedAt = s.stamp()
		next[i] = g
		out = append(out, g)
	}
	s.grants = next
	return out, nil
}

// ExpiredRevocations implements Store.
func (s *MemStore) ExpiredRevocations(_ context.Context, since time.Time, after *ExpiryKey, limit int) ([]ExpiryKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ExpiryKey
	for _, g := range s.grants {
		if g.RevokedAt == nil || g.RevokedByKind == nil || *g.RevokedByKind != RevokedBySystem || g.RevokeReason == nil ||
			*g.RevokeReason != ReasonSuspensionExpired || g.RevokedAt.Before(since) {
			continue
		}
		k := ExpiryKey{StudentUserID: g.StudentUserID, RevokedAt: *g.RevokedAt}
		if (after == nil || compareExpiry(k, *after) > 0) && !slices.ContainsFunc(out, func(x ExpiryKey) bool {
			return compareExpiry(x, k) == 0
		}) {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, compareExpiry)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// compareExpiry orders expiries by (student_user_id, revoked_at).
func compareExpiry(a, b ExpiryKey) int {
	if c := compareUUID(a.StudentUserID, b.StudentUserID); c != 0 {
		return c
	}
	return a.RevokedAt.Compare(b.RevokedAt)
}

// CountSuspendedAt implements Store.
func (s *MemStore) CountSuspendedAt(_ context.Context, studentID uuid.UUID, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, g := range s.grants {
		if g.StudentUserID == studentID && g.Open() && g.SuspendedAt != nil && g.SuspendedAt.Equal(at) {
			n++
		}
	}
	return n, nil
}

// grantPage returns up to limit grants matching match after the position,
// newest first.
func (s *MemStore) grantPage(match func(AccessGrant) bool, after *GrantKey, limit int) []AccessGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AccessGrant
	for _, g := range s.grants {
		if match(g) && (after == nil || after.before(g)) {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, newestFirst)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// StudentGrants implements Store (open grants: active and suspended).
func (s *MemStore) StudentGrants(_ context.Context, studentID uuid.UUID, after *GrantKey, limit int) ([]AccessGrant, error) {
	return s.grantPage(func(g AccessGrant) bool { return g.StudentUserID == studentID && g.Open() }, after, limit), nil
}

// CompanyGrants implements Store (active grants only).
func (s *MemStore) CompanyGrants(_ context.Context, companyOrgID uuid.UUID, after *GrantKey, limit int) ([]AccessGrant, error) {
	return s.grantPage(func(g AccessGrant) bool { return g.CompanyOrgID == companyOrgID && g.Active() }, after, limit), nil
}

// AllGrants implements Store.
func (s *MemStore) AllGrants(_ context.Context, studentID uuid.UUID) ([]AccessGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AccessGrant
	for _, g := range s.grants {
		if g.StudentUserID == studentID {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b AccessGrant) int { return newestFirst(b, a) })
	return out, nil
}

// DeleteGrants implements Store.
func (s *MemStore) DeleteGrants(_ context.Context, studentID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.grants)
	s.grants = slices.DeleteFunc(s.grants, func(g AccessGrant) bool { return g.StudentUserID == studentID })
	return n - len(s.grants), nil
}

// CountRevokedGrantsBefore implements Store.
func (s *MemStore) CountRevokedGrantsBefore(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, g := range s.grants {
		if g.RevokedAt != nil && g.RevokedAt.Before(cutoff) {
			n++
		}
	}
	return n, nil
}

// PurgeRevokedGrants implements Store.
func (s *MemStore) PurgeRevokedGrants(_ context.Context, cutoff time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var old []AccessGrant
	for _, g := range s.grants {
		if g.RevokedAt != nil && g.RevokedAt.Before(cutoff) {
			old = append(old, g)
		}
	}
	slices.SortFunc(old, func(a, b AccessGrant) int {
		if c := a.RevokedAt.Compare(*b.RevokedAt); c != 0 {
			return c
		}
		return compareUUID(a.ID, b.ID)
	})
	if len(old) > limit {
		old = old[:limit]
	}
	gone := map[uuid.UUID]bool{}
	for _, g := range old {
		gone[g.ID] = true
	}
	s.grants = slices.DeleteFunc(s.grants, func(g AccessGrant) bool { return gone[g.ID] })
	return len(old), nil
}

// DiscoveryCandidates implements Store.
func (s *MemStore) DiscoveryCandidates(_ context.Context, q DiscoveryQuery, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uuid.UUID
	for id, p := range s.profiles {
		if compareUUID(id, after) <= 0 || p.Status != ProfileActive || !s.privacy[id].Discoverable {
			continue
		}
		if matchesDiscovery(q, p, s.skills[id], s.languages[id]) {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, compareUUID)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// matchesDiscovery is the filter of DiscoveryCandidates: OR inside a
// filter, AND across filters; an empty filter matches.
func matchesDiscovery(q DiscoveryQuery, p Profile, skills []Skill, langs []Language) bool {
	in := func(ids []uuid.UUID, id *uuid.UUID) bool {
		return len(ids) == 0 || id != nil && slices.Contains(ids, *id)
	}
	uni := p.UniversityID
	switch {
	case !in(q.UniversityIDs, &uni), !in(q.DepartmentIDs, p.DepartmentID):
		return false
	case len(q.ClassYears) > 0 && (p.ClassYear == nil || !slices.Contains(q.ClassYears, *p.ClassYear)):
		return false
	case q.DegreeLevel != "" && p.DegreeLevel != q.DegreeLevel:
		return false
	case len(q.SkillIDs) > 0 && !slices.ContainsFunc(skills, func(k Skill) bool { return slices.Contains(q.SkillIDs, k.SkillID) }):
		return false
	case len(q.LanguageCodes) > 0 && !slices.ContainsFunc(langs, func(l Language) bool {
		return slices.Contains(q.LanguageCodes, l.LanguageCode)
	}):
		return false
	}
	return true
}
