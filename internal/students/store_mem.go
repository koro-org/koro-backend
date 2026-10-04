package students

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"slices"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/problem"
)

// errCheck mirrors a CHECK violation of 02 §9 (the GORM store gets 422
// from db.Translate).
var errCheck = problem.ErrValidation.Wrap(errors.New("students: check constraint violated"))

var languageCodeRe = regexp.MustCompile(`^[a-z]{2,3}$`)

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It enforces the CHECK and unique constraints of 02 §9 that a
// test could otherwise meet only in PostgreSQL. Every method is atomic
// under one mutex; it has no transactions (db.NopTx).
type MemStore struct {
	mu          sync.Mutex
	profiles    map[uuid.UUID]Profile
	privacy     map[uuid.UUID]Privacy
	skills      map[uuid.UUID][]Skill
	languages   map[uuid.UUID][]Language
	experiences []Experience // insertion order
	cvs         []CV         // insertion order
	// now stamps created_at/updated_at like the column defaults and the
	// touch trigger.
	now func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory Store; now stamps the rows.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{
		profiles: map[uuid.UUID]Profile{}, privacy: map[uuid.UUID]Privacy{},
		skills: map[uuid.UUID][]Skill{}, languages: map[uuid.UUID][]Language{}, now: now,
	}
}

func (s *MemStore) stamp() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func runes(p *string) int {
	if p == nil {
		return 0
	}
	return utf8.RuneCountInString(*p)
}

func firstOfMonth(d *civil.Date) bool { return d == nil || d.Day == 1 }

// checkProfile applies the CHECK constraints of student_profiles.
func checkProfile(p Profile) error {
	switch {
	case !p.DegreeLevel.Valid(), p.Status != ProfileActive && p.Status != ProfileGraduated && p.Status != ProfileInactive:
		return errCheck
	case p.ClassYear != nil && (*p.ClassYear < 1 || *p.ClassYear > 8):
		return errCheck
	case !firstOfMonth(p.ExpectedGraduation):
		return errCheck
	case runes(p.Headline) > 160, runes(p.About) > 2000, runes(p.City) > 60:
		return errCheck
	case (p.Status == ProfileGraduated) != (p.GraduatedAt != nil): // student_profiles_graduated_chk
		return errCheck
	}
	for _, sec := range p.CompletedSections {
		if !slices.Contains(sectionOrder, Section(sec)) {
			return errCheck
		}
	}
	return nil
}

// GetProfile implements Store.
func (s *MemStore) GetProfile(_ context.Context, userID uuid.UUID) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[userID]
	if !ok {
		return Profile{}, ErrNotFound
	}
	return cloneProfile(p), nil
}

// LockProfile implements Store.
func (s *MemStore) LockProfile(ctx context.Context, userID uuid.UUID) (Profile, error) {
	return s.GetProfile(ctx, userID)
}

// InsertProfile implements Store.
func (s *MemStore) InsertProfile(_ context.Context, p Profile) (Profile, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Status == "" {
		p.Status = ProfileActive
	}
	if err := checkProfile(p); err != nil {
		return Profile{}, false, err
	}
	if _, dup := s.profiles[p.UserID]; dup {
		return Profile{}, false, nil
	}
	now := s.stamp()
	p.CreatedAt, p.UpdatedAt = now, now
	p = cloneProfile(p)
	s.profiles[p.UserID] = p
	return cloneProfile(p), true, nil
}

// UpdateProfile implements Store.
func (s *MemStore) UpdateProfile(_ context.Context, p Profile) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.profiles[p.UserID]
	if !ok {
		return Profile{}, ErrNotFound
	}
	if err := checkProfile(p); err != nil {
		return Profile{}, err
	}
	p.CreatedAt, p.UpdatedAt = cur.CreatedAt, s.stamp()
	p = cloneProfile(p)
	s.profiles[p.UserID] = p
	return cloneProfile(p), nil
}

// SetAudience implements Store.
func (s *MemStore) SetAudience(_ context.Context, userID uuid.UUID, keys []string, eligible bool, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.profiles[userID]
	if !ok {
		return nil
	}
	p.AudienceKeys, p.AudienceEligible, p.AudienceRefreshedAt = slices.Clone(keys), eligible, &at
	p.UpdatedAt = s.stamp()
	s.profiles[userID] = p
	return nil
}

// RemoveAudienceKey implements Store.
func (s *MemStore) RemoveAudienceKey(_ context.Context, key string, at time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range s.sortedIDs() {
		if n == limit {
			break
		}
		p := s.profiles[id]
		if !slices.Contains(p.AudienceKeys, key) {
			continue
		}
		p.AudienceKeys = slices.DeleteFunc(slices.Clone(p.AudienceKeys), func(k string) bool { return k == key })
		p.AudienceRefreshedAt, p.UpdatedAt = &at, s.stamp()
		s.profiles[id] = p
		n++
	}
	return n, nil
}

func (s *MemStore) sortedIDs() []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(s.profiles))
	for id := range s.profiles {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, compareUUID)
	return ids
}

func compareUUID(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }

// Profiles implements Store.
func (s *MemStore) Profiles(_ context.Context, ids []uuid.UUID) ([]Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Profile
	for _, id := range ids {
		if p, ok := s.profiles[id]; ok && !slices.ContainsFunc(out, func(x Profile) bool { return x.UserID == id }) {
			out = append(out, cloneProfile(p))
		}
	}
	return out, nil
}

// ProfilesAfter implements Store.
func (s *MemStore) ProfilesAfter(_ context.Context, after uuid.UUID, limit int) ([]Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Profile
	for _, id := range s.sortedIDs() {
		if compareUUID(id, after) > 0 && len(out) < limit {
			out = append(out, cloneProfile(s.profiles[id]))
		}
	}
	return out, nil
}

// GraduationCandidates implements Store.
func (s *MemStore) GraduationCandidates(_ context.Context, today civil.Date, after uuid.UUID, limit int) ([]Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Profile
	for _, id := range s.sortedIDs() {
		p := s.profiles[id]
		if compareUUID(id, after) > 0 && len(out) < limit && p.Status == ProfileActive &&
			p.ExpectedGraduation != nil && p.ExpectedGraduation.Before(today) {
			out = append(out, cloneProfile(p))
		}
	}
	return out, nil
}

// GraduatedAmong implements Store.
func (s *MemStore) GraduatedAmong(_ context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uuid.UUID
	for _, id := range ids {
		if p, ok := s.profiles[id]; ok && p.Status == ProfileGraduated && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, compareUUID)
	return out, nil
}

// AvatarIDs implements Store.
func (s *MemStore) AvatarIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]uuid.UUID{}
	for _, id := range ids {
		if p, ok := s.profiles[id]; ok && p.AvatarFileID != nil {
			out[id] = *p.AvatarFileID
		}
	}
	return out, nil
}

// DeleteProfile implements Store.
func (s *MemStore) DeleteProfile(_ context.Context, userID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.profiles[userID]
	delete(s.profiles, userID)
	return ok, nil
}

// GetPrivacy implements Store.
func (s *MemStore) GetPrivacy(_ context.Context, userID uuid.UUID) (Privacy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.privacy[userID]
	if !ok {
		return Privacy{}, ErrNotFound
	}
	return p, nil
}

// LockPrivacy implements Store.
func (s *MemStore) LockPrivacy(_ context.Context, userID uuid.UUID) (Privacy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.privacy[userID]
	if !ok {
		now := s.stamp()
		p = defaultPrivacy(userID)
		p.CreatedAt, p.UpdatedAt = now, now
		s.privacy[userID] = p
	}
	return p, nil
}

// UpdatePrivacy implements Store.
func (s *MemStore) UpdatePrivacy(_ context.Context, p Privacy) (Privacy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.privacy[p.UserID]
	if !ok {
		return Privacy{}, ErrNotFound
	}
	if !p.ActivityRankingChoice.Valid() {
		return Privacy{}, errCheck
	}
	p.CreatedAt, p.UpdatedAt = cur.CreatedAt, s.stamp()
	s.privacy[p.UserID] = p
	return p, nil
}

// PrivacyFlagged implements Store.
func (s *MemStore) PrivacyFlagged(_ context.Context, after uuid.UUID, limit int) ([]Privacy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]uuid.UUID, 0, len(s.privacy))
	for id, p := range s.privacy {
		if (p.Discoverable || p.ShowContactOnGrant) && compareUUID(id, after) > 0 {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, compareUUID)
	out := make([]Privacy, 0, min(limit, len(ids)))
	for _, id := range ids[:min(limit, len(ids))] {
		out = append(out, s.privacy[id])
	}
	return out, nil
}

// DeletePrivacy implements Store.
func (s *MemStore) DeletePrivacy(_ context.Context, userID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.privacy[userID]
	delete(s.privacy, userID)
	return ok, nil
}

// Skills implements Store.
func (s *MemStore) Skills(_ context.Context, userID uuid.UUID) ([]Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.skills[userID]), nil
}

// ReplaceSkills implements Store.
func (s *MemStore) ReplaceSkills(_ context.Context, userID uuid.UUID, items []Skill) ([]Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.skills[userID]
	var out []Skill
	now := s.stamp()
	for _, it := range items {
		if it.Level != nil && (*it.Level < 1 || *it.Level > 5) {
			return nil, errCheck
		}
		if slices.ContainsFunc(out, func(x Skill) bool { return x.SkillID == it.SkillID }) {
			return nil, problem.ErrConflict // primary key
		}
		row := Skill{UserID: userID, SkillID: it.SkillID, Level: it.Level, CreatedAt: now}
		if i := slices.IndexFunc(cur, func(x Skill) bool { return x.SkillID == it.SkillID }); i >= 0 {
			row.CreatedAt = cur[i].CreatedAt
		}
		out = append(out, row)
	}
	slices.SortStableFunc(out, func(a, b Skill) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return compareUUID(a.SkillID, b.SkillID)
	})
	s.skills[userID] = out
	return slices.Clone(out), nil
}

// Languages implements Store.
func (s *MemStore) Languages(_ context.Context, userID uuid.UUID) ([]Language, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.languages[userID]), nil
}

// ReplaceLanguages implements Store.
func (s *MemStore) ReplaceLanguages(_ context.Context, userID uuid.UUID, items []Language) ([]Language, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.languages[userID]
	var out []Language
	now := s.stamp()
	for _, it := range items {
		if !languageCodeRe.MatchString(it.LanguageCode) || !it.Level.Valid() {
			return nil, errCheck
		}
		if slices.ContainsFunc(out, func(x Language) bool { return x.LanguageCode == it.LanguageCode }) {
			return nil, problem.ErrConflict
		}
		row := Language{UserID: userID, LanguageCode: it.LanguageCode, Level: it.Level, CreatedAt: now}
		if i := slices.IndexFunc(cur, func(x Language) bool { return x.LanguageCode == it.LanguageCode }); i >= 0 {
			row.CreatedAt = cur[i].CreatedAt
		}
		out = append(out, row)
	}
	slices.SortStableFunc(out, func(a, b Language) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		if a.LanguageCode < b.LanguageCode {
			return -1
		}
		if a.LanguageCode > b.LanguageCode {
			return 1
		}
		return 0
	})
	s.languages[userID] = out
	return slices.Clone(out), nil
}

// checkExperience applies the CHECK constraints of student_experiences.
func checkExperience(e Experience) error {
	n := utf8.RuneCountInString(e.Title)
	switch {
	case !e.Kind.Valid(), n == 0, n > 120, runes(e.OrganizationName) > 160, runes(e.Description) > 2000:
		return errCheck
	case e.StartMonth.Day != 1, !firstOfMonth(e.EndMonth):
		return errCheck
	case e.EndMonth != nil && e.EndMonth.Before(e.StartMonth):
		return errCheck
	}
	return nil
}

// Experiences implements Store.
func (s *MemStore) Experiences(_ context.Context, userID uuid.UUID) ([]Experience, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Experience
	for _, e := range s.experiences {
		if e.UserID == userID {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b Experience) int {
		if c := b.StartMonth.Compare(a.StartMonth); c != 0 {
			return c
		}
		return compareUUID(b.ID, a.ID)
	})
	return out, nil
}

// GetExperience implements Store.
func (s *MemStore) GetExperience(_ context.Context, userID, id uuid.UUID) (Experience, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.experiences {
		if e.ID == id && e.UserID == userID {
			return e, nil
		}
	}
	return Experience{}, ErrNotFound
}

// InsertExperience implements Store.
func (s *MemStore) InsertExperience(_ context.Context, e Experience) (Experience, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkExperience(e); err != nil {
		return Experience{}, err
	}
	if slices.ContainsFunc(s.experiences, func(x Experience) bool { return x.ID == e.ID }) {
		return Experience{}, problem.ErrConflict
	}
	now := s.stamp()
	e.CreatedAt, e.UpdatedAt = now, now
	s.experiences = append(s.experiences, e)
	return e, nil
}

// UpdateExperience implements Store.
func (s *MemStore) UpdateExperience(_ context.Context, e Experience) (Experience, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.experiences, func(x Experience) bool { return x.ID == e.ID && x.UserID == e.UserID })
	if i < 0 {
		return Experience{}, ErrNotFound
	}
	if err := checkExperience(e); err != nil {
		return Experience{}, err
	}
	e.CreatedAt, e.UpdatedAt = s.experiences[i].CreatedAt, s.stamp()
	s.experiences[i] = e
	return e, nil
}

// DeleteExperience implements Store.
func (s *MemStore) DeleteExperience(_ context.Context, userID, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.experiences)
	s.experiences = slices.DeleteFunc(s.experiences, func(x Experience) bool { return x.ID == id && x.UserID == userID })
	return len(s.experiences) < n, nil
}

// DeleteSections implements Store.
func (s *MemStore) DeleteSections(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.skills[userID]) + len(s.languages[userID])
	delete(s.skills, userID)
	delete(s.languages, userID)
	before := len(s.experiences)
	s.experiences = slices.DeleteFunc(s.experiences, func(x Experience) bool { return x.UserID == userID })
	return n + before - len(s.experiences), nil
}
