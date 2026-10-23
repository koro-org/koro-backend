package directory

import (
	"cmp"
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// errCheck mirrors a CHECK violation of 02 §8 (the GORM store gets 422 from
// db.Translate).
var errCheck = problem.ErrValidation.Wrap(errors.New("directory: check constraint violated"))

var (
	domainRe    = regexp.MustCompile(`^([a-z0-9-]+\.)+edu\.tr$`)
	skillSlugRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	websiteRe   = regexp.MustCompile(`^https?://`)
)

// OrgRef is what the in-memory ListClubs needs of a club's org (the GORM
// store joins organizations).
type OrgRef struct {
	Name, Slug, Status string
}

// OrgLookup returns the org of a club for the in-memory store (cmd/koro
// binds orgs.OrgDirectory.Get; tests a map).
type OrgLookup func(ctx context.Context, orgID uuid.UUID) (OrgRef, bool)

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It enforces the CHECK, unique and foreign key constraints of
// 02 §8 that a test could otherwise meet only in PostgreSQL. Every method
// is atomic under one mutex; it has no transactions (db.NopTx).
type MemStore struct {
	mu      sync.Mutex
	unis    map[uuid.UUID]University
	domains map[string]EmailDomain
	facs    map[uuid.UUID]Faculty
	depts   map[uuid.UUID]Department
	clubs   map[uuid.UUID]Club
	skills  map[uuid.UUID]Skill
	claims  map[uuid.UUID]ClubClaim
	now     func() time.Time
	orgs    OrgLookup
	coll    *collate.Collator
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory Store; now stamps the rows and
// orgs names the clubs' orgs (nil: no club is listed).
func NewMemStore(now func() time.Time, orgs OrgLookup) *MemStore {
	return &MemStore{
		unis: map[uuid.UUID]University{}, domains: map[string]EmailDomain{}, facs: map[uuid.UUID]Faculty{},
		depts: map[uuid.UUID]Department{}, clubs: map[uuid.UUID]Club{}, skills: map[uuid.UUID]Skill{},
		claims: map[uuid.UUID]ClubClaim{}, now: now, orgs: orgs, coll: collate.New(language.Turkish),
	}
}

func (s *MemStore) stamp(prev time.Time) time.Time {
	t := s.now().UTC().Truncate(time.Microsecond)
	if !t.After(prev) {
		t = prev.Add(time.Microsecond)
	}
	return t
}

// created fills created_at/updated_at of a new row unless preset (matrix
// fixtures date their rows).
func (s *MemStore) created(c, u *time.Time) {
	if c.IsZero() {
		*c = s.stamp(time.Time{})
	}
	if u.IsZero() {
		*u = *c
	}
}

// memNorm approximates koro_norm: Turkish lower case, ASCII
// transliteration, runs of other characters as one hyphen.
func memNorm(s string) string { return textx.Slug(s, 0) }

func memMatch(name, q string) bool {
	n := memNorm(q)
	return n != "" && strings.Contains(memNorm(name), n)
}

// after reports whether (name, id) sorts after the keyset position.
func (s *MemStore) after(sort, name string, id uuid.UUID, a *After) bool {
	if a == nil {
		return true
	}
	if sort == SortName {
		c := s.coll.CompareString(name, a.Name)
		return c > 0 || c == 0 && id.String() > a.ID.String()
	}
	return id.String() < a.ID.String()
}

func sortRows[T any](s *MemStore, rows []T, sort string, key func(T) (string, uuid.UUID)) {
	slices.SortFunc(rows, func(a, b T) int {
		an, ai := key(a)
		bn, bi := key(b)
		if sort == SortName {
			if c := s.coll.CompareString(an, bn); c != 0 {
				return c
			}
			return cmp.Compare(ai.String(), bi.String())
		}
		return cmp.Compare(bi.String(), ai.String())
	})
}

func limitRows[T any](rows []T, limit int) []T {
	if len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// ---- universities

func checkUniversity(u University) error {
	switch {
	case !u.Type.Valid(), u.YokCode == "", u.Name == "", u.City == "":
		return errCheck
	case u.WebsiteURL != nil && !websiteRe.MatchString(*u.WebsiteURL):
		return errCheck
	}
	return nil
}

// InsertUniversity implements Store.
func (s *MemStore) InsertUniversity(_ context.Context, u University) (University, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u.OrgKind = "university"
	if err := checkUniversity(u); err != nil {
		return University{}, err
	}
	for _, x := range s.unis {
		switch {
		case x.ID == u.ID, x.OrgID == u.OrgID:
			return University{}, problem.ErrConflict
		case x.YokCode == u.YokCode:
			return University{}, ErrYokCodeTaken
		}
	}
	s.created(&u.CreatedAt, &u.UpdatedAt)
	s.unis[u.ID] = u
	return u, nil
}

// GetUniversity implements Store.
func (s *MemStore) GetUniversity(_ context.Context, id uuid.UUID) (University, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.unis[id]
	if !ok {
		return University{}, ErrNotFound
	}
	return u, nil
}

// UniversityByYokCode implements Store.
func (s *MemStore) UniversityByYokCode(_ context.Context, code string) (University, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.unis {
		if u.YokCode == code {
			return u, nil
		}
	}
	return University{}, ErrNotFound
}

// UniversityByName implements Store.
func (s *MemStore) UniversityByName(_ context.Context, name string) (University, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []University
	for _, u := range s.unis {
		if u.Name == name {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return University{}, ErrNotFound
	}
	slices.SortFunc(out, func(a, b University) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out[0], nil
}

// LockUniversity implements Store (a plain read: the mutex serialises writes).
func (s *MemStore) LockUniversity(ctx context.Context, id uuid.UUID) (University, error) {
	return s.GetUniversity(ctx, id)
}

// UpdateUniversity implements Store.
func (s *MemStore) UpdateUniversity(_ context.Context, u University) (University, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.unis[u.ID]
	if !ok {
		return University{}, ErrNotFound
	}
	next := cur
	next.YokCode, next.Name, next.ShortName, next.City, next.Type, next.WebsiteURL, next.IsActive =
		u.YokCode, u.Name, u.ShortName, u.City, u.Type, u.WebsiteURL, u.IsActive
	if err := checkUniversity(next); err != nil {
		return University{}, err
	}
	for _, x := range s.unis {
		if x.ID != u.ID && x.YokCode == next.YokCode {
			return University{}, ErrYokCodeTaken
		}
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.unis[u.ID] = next
	return next, nil
}

// ListUniversities implements Store.
func (s *MemStore) ListUniversities(_ context.Context, f UniversityFilter, after *After, limit int) ([]University, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []University
	for _, u := range s.unis {
		short := ""
		if u.ShortName != nil {
			short = *u.ShortName
		}
		switch {
		case !u.IsActive:
		case f.City != "" && memNorm(u.City) != memNorm(f.City):
		case f.Type != "" && u.Type != f.Type:
		case f.Q != "" && !memMatch(u.Name, f.Q) && !memMatch(short, f.Q) && !memMatch(u.City, f.Q):
		case !s.after(f.Sort, u.Name, u.ID, after):
		default:
			out = append(out, u)
		}
	}
	sortRows(s, out, f.Sort, func(u University) (string, uuid.UUID) { return u.Name, u.ID })
	return limitRows(out, limit), nil
}

// ---- e-mail domains

func checkDomain(d EmailDomain) error {
	if !domainRe.MatchString(d.Domain) || !d.DomainKind.Valid() {
		return errCheck
	}
	return nil
}

// GetDomain implements Store.
func (s *MemStore) GetDomain(_ context.Context, domain string) (EmailDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.domains[domain]
	if !ok {
		return EmailDomain{}, ErrNotFound
	}
	return d, nil
}

// InsertDomain implements Store.
func (s *MemStore) InsertDomain(_ context.Context, d EmailDomain) (EmailDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkDomain(d); err != nil {
		return EmailDomain{}, err
	}
	if _, ok := s.unis[d.UniversityID]; !ok {
		return EmailDomain{}, ErrNotFound // university_email_domains_university_id_fkey
	}
	if _, dup := s.domains[d.Domain]; dup {
		return EmailDomain{}, ErrDomainTaken
	}
	s.created(&d.CreatedAt, &d.UpdatedAt)
	s.domains[d.Domain] = d
	return d, nil
}

// UpdateDomain implements Store.
func (s *MemStore) UpdateDomain(_ context.Context, d EmailDomain) (EmailDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.domains[d.Domain]
	if !ok {
		return EmailDomain{}, ErrNotFound
	}
	cur.DomainKind, cur.IsActive, cur.VerifiedBy = d.DomainKind, d.IsActive, d.VerifiedBy
	if err := checkDomain(cur); err != nil {
		return EmailDomain{}, err
	}
	cur.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.domains[d.Domain] = cur
	return cur, nil
}

// MatchDomain implements Store.
func (s *MemStore) MatchDomain(_ context.Context, candidates []string) (EmailDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best EmailDomain
	found := false
	for _, c := range candidates {
		d, ok := s.domains[c]
		if !ok || !d.IsActive || !s.unis[d.UniversityID].IsActive {
			continue
		}
		if !found || len(d.Domain) > len(best.Domain) {
			best, found = d, true
		}
	}
	if !found {
		return EmailDomain{}, ErrNotFound
	}
	return best, nil
}

// ---- faculties

// InsertFaculty implements Store.
func (s *MemStore) InsertFaculty(_ context.Context, f Faculty) (Faculty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.unis[f.UniversityID]; !ok {
		return Faculty{}, ErrNotFound
	}
	for _, x := range s.facs {
		if x.ID == f.ID || x.UniversityID == f.UniversityID && x.Name == f.Name {
			return Faculty{}, problem.ErrConflict
		}
	}
	s.created(&f.CreatedAt, &f.UpdatedAt)
	s.facs[f.ID] = f
	return f, nil
}

// UpdateFaculty implements Store.
func (s *MemStore) UpdateFaculty(_ context.Context, f Faculty) (Faculty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.facs[f.ID]
	if !ok {
		return Faculty{}, ErrNotFound
	}
	cur.YokCode, cur.IsActive = f.YokCode, f.IsActive
	cur.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.facs[f.ID] = cur
	return cur, nil
}

// GetFaculty implements Store.
func (s *MemStore) GetFaculty(_ context.Context, id uuid.UUID) (Faculty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.facs[id]
	if !ok {
		return Faculty{}, ErrNotFound
	}
	return f, nil
}

// FacultyByName implements Store.
func (s *MemStore) FacultyByName(_ context.Context, universityID uuid.UUID, name string) (Faculty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.facs {
		if f.UniversityID == universityID && f.Name == name {
			return f, nil
		}
	}
	return Faculty{}, ErrNotFound
}

// FacultiesOf implements Store.
func (s *MemStore) FacultiesOf(_ context.Context, universityID uuid.UUID) ([]Faculty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Faculty
	for _, f := range s.facs {
		if f.UniversityID == universityID && f.IsActive {
			out = append(out, f)
		}
	}
	sortRows(s, out, SortName, func(f Faculty) (string, uuid.UUID) { return f.Name, f.ID })
	return out, nil
}

// ---- departments

func (s *MemStore) checkDepartment(d Department) error {
	switch {
	case !d.DegreeLevel.Valid(), d.Name == "":
		return errCheck
	case d.DurationYears != nil && (*d.DurationYears < 1 || *d.DurationYears > 7):
		return errCheck
	}
	if _, ok := s.unis[d.UniversityID]; !ok {
		return ErrNotFound
	}
	if d.FacultyID != nil {
		if _, ok := s.facs[*d.FacultyID]; !ok {
			return ErrNotFound
		}
	}
	for _, x := range s.depts {
		if x.ID != d.ID && x.UniversityID == d.UniversityID && x.YokProgramCode != nil && d.YokProgramCode != nil &&
			*x.YokProgramCode == *d.YokProgramCode {
			return ErrDepartmentExists
		}
	}
	return nil
}

// InsertDepartment implements Store.
func (s *MemStore) InsertDepartment(_ context.Context, d Department) (Department, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.depts[d.ID]; dup {
		return Department{}, problem.ErrConflict
	}
	if err := s.checkDepartment(d); err != nil {
		return Department{}, err
	}
	s.created(&d.CreatedAt, &d.UpdatedAt)
	s.depts[d.ID] = d
	return d, nil
}

// UpdateDepartment implements Store.
func (s *MemStore) UpdateDepartment(_ context.Context, d Department) (Department, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.depts[d.ID]
	if !ok {
		return Department{}, ErrNotFound
	}
	next := cur
	next.FacultyID, next.Name, next.YokProgramCode, next.DegreeLevel, next.DurationYears, next.IsActive =
		d.FacultyID, d.Name, d.YokProgramCode, d.DegreeLevel, d.DurationYears, d.IsActive
	if err := s.checkDepartment(next); err != nil {
		return Department{}, err
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.depts[d.ID] = next
	return next, nil
}

// GetDepartment implements Store.
func (s *MemStore) GetDepartment(_ context.Context, id uuid.UUID) (Department, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.depts[id]
	if !ok {
		return Department{}, ErrNotFound
	}
	return d, nil
}

// FindDepartment implements Store.
func (s *MemStore) FindDepartment(_ context.Context, k DepartmentKey) (Department, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Department
	for _, d := range s.depts {
		if d.UniversityID != k.UniversityID {
			continue
		}
		if k.YokProgramCode != "" {
			if d.YokProgramCode != nil && *d.YokProgramCode == k.YokProgramCode {
				out = append(out, d)
			}
			continue
		}
		sameFaculty := k.FacultyID == nil && d.FacultyID == nil ||
			k.FacultyID != nil && d.FacultyID != nil && *k.FacultyID == *d.FacultyID
		if d.YokProgramCode == nil && d.Name == k.Name && d.DegreeLevel == k.DegreeLevel && sameFaculty {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return Department{}, ErrNotFound
	}
	slices.SortFunc(out, func(a, b Department) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out[0], nil
}

// ListDepartments implements Store.
func (s *MemStore) ListDepartments(_ context.Context, f DepartmentFilter, after *After, limit int) ([]Department, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Department
	for _, d := range s.depts {
		switch {
		case d.UniversityID != f.UniversityID || !d.IsActive:
		case f.FacultyID != nil && (d.FacultyID == nil || *d.FacultyID != *f.FacultyID):
		case f.DegreeLevel != "" && d.DegreeLevel != f.DegreeLevel:
		case f.Q != "" && !memMatch(d.Name, f.Q):
		case !s.after(SortNewest, d.Name, d.ID, after):
		default:
			out = append(out, d)
		}
	}
	sortRows(s, out, SortNewest, func(d Department) (string, uuid.UUID) { return d.Name, d.ID })
	return limitRows(out, limit), nil
}

// ---- clubs

// checkClub applies the CHECK constraints of clubs (02 §8.5).
func checkClub(c Club) error {
	switch {
	case !c.Category.Valid(), !c.JoinPolicy.Valid(), !c.ClaimStatus.Valid():
		return errCheck
	case c.Category.Sensitive() && !c.IsSensitive: // clubs_sensitive_category_chk
		return ErrSensitivityRequired
	case c.IsSensitive != (c.SensitiveReason != nil), c.IsSensitive != (c.SensitivitySetAt != nil):
		return errCheck
	case c.SensitiveReason != nil && !c.SensitiveReason.Valid():
		return errCheck
	case c.FoundedYear != nil && (*c.FoundedYear < 1900 || *c.FoundedYear > 2100):
		return errCheck
	}
	return nil
}

func cloneClub(c Club) Club {
	c.SocialLinks = c.SocialLinks.Clone()
	return c
}

// InsertClub implements Store.
func (s *MemStore) InsertClub(_ context.Context, c Club) (Club, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.OrgKind = "club"
	if c.SocialLinks == nil {
		c.SocialLinks = SocialLinks{}
	}
	if err := checkClub(c); err != nil {
		return Club{}, err
	}
	if _, ok := s.unis[c.UniversityID]; !ok {
		return Club{}, ErrNotFound // clubs_university_id_fkey
	}
	if _, dup := s.clubs[c.OrgID]; dup {
		return Club{}, problem.ErrConflict
	}
	s.created(&c.CreatedAt, &c.UpdatedAt)
	c = cloneClub(c)
	s.clubs[c.OrgID] = c
	return cloneClub(c), nil
}

// GetClub implements Store.
func (s *MemStore) GetClub(_ context.Context, orgID uuid.UUID) (Club, error) {
	if orgID == uuid.Nil() {
		return Club{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clubs[orgID]
	if !ok {
		return Club{}, ErrNotFound
	}
	return cloneClub(c), nil
}

// LockClub implements Store.
func (s *MemStore) LockClub(ctx context.Context, orgID uuid.UUID) (Club, error) {
	return s.GetClub(ctx, orgID)
}

// UpdateClub implements Store.
func (s *MemStore) UpdateClub(_ context.Context, c Club) (Club, error) {
	if c.OrgID == uuid.Nil() {
		return Club{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.clubs[c.OrgID]
	if !ok {
		return Club{}, ErrNotFound
	}
	next := cur
	next.Category, next.IsSensitive, next.SensitiveReason = c.Category, c.IsSensitive, c.SensitiveReason
	next.SensitivitySetBy, next.SensitivitySetAt = c.SensitivitySetBy, c.SensitivitySetAt
	next.JoinPolicy, next.ClaimStatus, next.FoundedYear = c.JoinPolicy, c.ClaimStatus, c.FoundedYear
	next.SocialLinks = c.SocialLinks.Clone()
	if err := checkClub(next); err != nil {
		return Club{}, err
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.clubs[c.OrgID] = next
	return cloneClub(next), nil
}

// ListClubs implements Store.
func (s *MemStore) ListClubs(ctx context.Context, f ClubFilter, after *After, limit int) ([]ClubRow, error) {
	s.mu.Lock()
	clubs := make([]Club, 0, len(s.clubs))
	for _, c := range s.clubs {
		clubs = append(clubs, cloneClub(c))
	}
	s.mu.Unlock()
	if s.orgs == nil {
		return nil, nil
	}
	var out []ClubRow
	for _, c := range clubs {
		o, ok := s.orgs(ctx, c.OrgID)
		switch {
		case !ok || o.Status != "active" && o.Status != "suspended":
		case f.UniversityID != nil && c.UniversityID != *f.UniversityID:
		case f.Category != "" && c.Category != f.Category:
		case f.Claimed != nil && (c.ClaimStatus == ClaimClaimed) != *f.Claimed:
		case f.Q != "" && !memMatch(o.Name, f.Q):
		case !s.after(f.Sort, o.Name, c.OrgID, after):
		default:
			out = append(out, ClubRow{Club: c, Name: o.Name, Slug: o.Slug, OrgStatus: o.Status})
		}
	}
	sortRows(s, out, f.Sort, func(r ClubRow) (string, uuid.UUID) { return r.Name, r.OrgID })
	return limitRows(out, limit), nil
}

// SensitiveAmong implements Store.
func (s *MemStore) SensitiveAmong(_ context.Context, orgIDs []uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uuid.UUID
	for _, id := range orgIDs {
		if c, ok := s.clubs[id]; ok && c.IsSensitive && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// ClubOrgsOf implements Store.
func (s *MemStore) ClubOrgsOf(_ context.Context, universityID uuid.UUID) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uuid.UUID
	for _, c := range s.clubs {
		if c.UniversityID == universityID {
			out = append(out, c.OrgID)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return cmp.Compare(a.String(), b.String()) })
	return out, nil
}

// ---- skills

func checkSkill(k Skill) error {
	if !skillSlugRe.MatchString(k.Slug) || !k.Category.Valid() || k.NameTR == "" {
		return errCheck
	}
	return nil
}

// InsertSkill implements Store.
func (s *MemStore) InsertSkill(_ context.Context, k Skill) (Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkSkill(k); err != nil {
		return Skill{}, err
	}
	for _, x := range s.skills {
		switch {
		case x.ID == k.ID:
			return Skill{}, problem.ErrConflict
		case x.Slug == k.Slug:
			return Skill{}, ErrSkillSlugTaken
		}
	}
	s.created(&k.CreatedAt, &k.UpdatedAt)
	s.skills[k.ID] = k
	return k, nil
}

// UpdateSkill implements Store.
func (s *MemStore) UpdateSkill(_ context.Context, k Skill) (Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.skills[k.ID]
	if !ok {
		return Skill{}, ErrNotFound
	}
	next := cur
	next.NameTR, next.NameEN, next.Category, next.IsActive = k.NameTR, k.NameEN, k.Category, k.IsActive
	if err := checkSkill(next); err != nil {
		return Skill{}, err
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.skills[k.ID] = next
	return next, nil
}

// GetSkill implements Store.
func (s *MemStore) GetSkill(_ context.Context, id uuid.UUID) (Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.skills[id]
	if !ok {
		return Skill{}, ErrNotFound
	}
	return k, nil
}

// SkillBySlug implements Store.
func (s *MemStore) SkillBySlug(_ context.Context, slug string) (Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.skills {
		if k.Slug == slug {
			return k, nil
		}
	}
	return Skill{}, ErrNotFound
}

// ListSkills implements Store.
func (s *MemStore) ListSkills(_ context.Context, f SkillFilter, after *After, limit int) ([]Skill, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Skill
	for _, k := range s.skills {
		en := ""
		if k.NameEN != nil {
			en = *k.NameEN
		}
		switch {
		case !k.IsActive:
		case f.Category != "" && k.Category != f.Category:
		case f.Q != "" && !memMatch(k.NameTR, f.Q) && !memMatch(en, f.Q):
		case !s.after(SortNewest, k.NameTR, k.ID, after):
		default:
			out = append(out, k)
		}
	}
	sortRows(s, out, SortNewest, func(k Skill) (string, uuid.UUID) { return k.NameTR, k.ID })
	return limitRows(out, limit), nil
}

// CountActiveSkills implements Store.
func (s *MemStore) CountActiveSkills(_ context.Context, ids []uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if k, ok := s.skills[id]; ok && k.IsActive && !seen[id] {
			n++
		}
		seen[id] = true
	}
	return n, nil
}

// SkillNamesTR implements Store.
func (s *MemStore) SkillNamesTR(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		if k, ok := s.skills[id]; ok && k.IsActive {
			out[id] = k.NameTR
		}
	}
	return out, nil
}
