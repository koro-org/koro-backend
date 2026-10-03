package orgs

import (
	"cmp"
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// errCheck mirrors a CHECK violation of 02 §7 (the GORM store gets 422 from
// db.Translate).
var errCheck = problem.ErrValidation.Wrap(errors.New("orgs: check constraint violated"))

var (
	slugRe        = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	sectorRe      = regexp.MustCompile(`^[a-z_]{2,40}$`)
	emailDomainRe = regexp.MustCompile(`^[a-z0-9.-]+\.[a-z]{2,}$`)
)

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It enforces the CHECK and unique constraints of 02 §7 that a
// test could otherwise meet only in PostgreSQL, and takes org_kind from the
// parent org like the GORM store (06 H-38). Every method is atomic under
// one mutex; it has no transactions (db.NopTx).
type MemStore struct {
	mu        sync.Mutex
	orgs      map[uuid.UUID]Org
	companies map[uuid.UUID]Company
	members   []Membership // insertion order
	// now stamps created_at/updated_at like the column defaults and the
	// touch trigger; updated_at always moves forward (two writes at the
	// same fake instant still change the ETag, as two transactions do).
	now func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory Store; now stamps the rows.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{orgs: map[uuid.UUID]Org{}, companies: map[uuid.UUID]Company{}, now: now}
}

func (s *MemStore) stamp(prev time.Time) time.Time {
	t := s.now().UTC().Truncate(time.Microsecond)
	if !t.After(prev) {
		t = prev.Add(time.Microsecond)
	}
	return t
}

func runes(p *string) int {
	if p == nil {
		return 0
	}
	return utf8.RuneCountInString(*p)
}

// checkOrg applies the CHECK constraints of organizations.
func checkOrg(o Org) error {
	n := utf8.RuneCountInString(o.Name)
	switch {
	case !validKind(o.Kind), !o.Status.Valid(), n < 2 || n > 200:
		return errCheck
	case !slugRe.MatchString(o.Slug), len(o.Slug) > 80:
		return errCheck
	case runes(o.Description) > 5000, runes(o.City) > 60:
		return errCheck
	case o.WebsiteURL != nil && (!strings.HasPrefix(*o.WebsiteURL, "https://") || len(*o.WebsiteURL) > 2048):
		return errCheck
	case (o.Status == StatusClosed) != (o.ClosedAt != nil): // organizations_closed_chk
		return errCheck
	}
	return nil
}

// InsertOrg implements Store.
func (s *MemStore) InsertOrg(_ context.Context, o Org) (Org, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkOrg(o); err != nil {
		return Org{}, false, err
	}
	if _, dup := s.orgs[o.ID]; dup {
		return Org{}, false, problem.ErrConflict
	}
	for _, x := range s.orgs {
		if x.Slug == o.Slug {
			return Org{}, false, nil
		}
		if o.Kind == KindPlatform && x.Kind == KindPlatform { // organizations_single_platform_uq
			return Org{}, false, problem.ErrConflict
		}
	}
	now := s.stamp(time.Time{})
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = o.CreatedAt
	}
	s.orgs[o.ID] = o
	return o, true, nil
}

// GetOrg implements Store.
func (s *MemStore) GetOrg(_ context.Context, id uuid.UUID) (Org, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orgs[id]
	if !ok {
		return Org{}, ErrNotFound
	}
	return o, nil
}

// GetOrgBySlug implements Store.
func (s *MemStore) GetOrgBySlug(_ context.Context, slug string) (Org, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.orgs {
		if o.Slug == slug {
			return o, nil
		}
	}
	return Org{}, ErrNotFound
}

// GetOrgs implements Store.
func (s *MemStore) GetOrgs(_ context.Context, ids []uuid.UUID) ([]Org, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Org
	for _, id := range ids {
		if o, ok := s.orgs[id]; ok && !slices.ContainsFunc(out, func(x Org) bool { return x.ID == id }) {
			out = append(out, o)
		}
	}
	return out, nil
}

// LockOrg implements Store (a plain read: the mutex serialises writes).
func (s *MemStore) LockOrg(ctx context.Context, id uuid.UUID) (Org, error) { return s.GetOrg(ctx, id) }

// UpdateOrg implements Store.
func (s *MemStore) UpdateOrg(_ context.Context, o Org) (Org, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.orgs[o.ID]
	if !ok {
		return Org{}, ErrNotFound
	}
	next := cur
	next.Name, next.Status, next.Description, next.LogoFileID = o.Name, o.Status, o.Description, o.LogoFileID
	next.WebsiteURL, next.City, next.VerifiedAt, next.VerifiedBy, next.ClosedAt = o.WebsiteURL, o.City, o.VerifiedAt, o.VerifiedBy, o.ClosedAt
	if err := checkOrg(next); err != nil {
		return Org{}, err
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.orgs[o.ID] = next
	return next, nil
}

// memNorm approximates koro_norm for the in-memory search: Turkish lower
// case, ASCII transliteration, runs of other characters as one hyphen.
func memNorm(s string) string { return textx.Slug(s, 0) }

// ListOrgs implements Store.
func (s *MemStore) ListOrgs(_ context.Context, f ListFilter, after *ListAfter, limit int) ([]Org, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Org
	for _, o := range s.orgs {
		switch {
		case len(f.Kinds) > 0 && !slices.Contains(f.Kinds, o.Kind):
		case len(f.Statuses) > 0 && !slices.Contains(f.Statuses, o.Status):
		case f.City != "" && (o.City == nil || !textx.EqualFoldTR(*o.City, f.City)):
		case f.OrgIDs != nil && !slices.Contains(f.OrgIDs, o.ID):
		case f.Q != "" && !strings.Contains(memNorm(o.Name), memNorm(f.Q)) &&
			(o.Description == nil || !strings.Contains(memNorm(*o.Description), memNorm(f.Q))):
		default:
			out = append(out, o)
		}
	}
	byName := f.Sort == SortName
	slices.SortFunc(out, func(a, b Org) int {
		if byName {
			if c := cmp.Compare(a.Name, b.Name); c != 0 {
				return c
			}
			return cmp.Compare(a.ID.String(), b.ID.String())
		}
		return cmp.Compare(b.ID.String(), a.ID.String())
	})
	if after != nil {
		out = slices.DeleteFunc(out, func(o Org) bool {
			if byName {
				return o.Name < after.Name || o.Name == after.Name && o.ID.String() <= after.ID.String()
			}
			return o.ID.String() >= after.ID.String()
		})
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// InsertCompany implements Store.
func (s *MemStore) InsertCompany(_ context.Context, c Company) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.orgs[c.OrgID]
	if !ok {
		return problem.ErrConflict // companies_org_id_org_kind_fkey
	}
	c.OrgKind = parent.Kind // 06 H-38: from the parent row
	if err := checkCompany(c); err != nil {
		return err
	}
	if _, dup := s.companies[c.OrgID]; dup {
		return problem.ErrConflict
	}
	now := s.stamp(time.Time{})
	c.CreatedAt, c.UpdatedAt = now, now
	s.companies[c.OrgID] = c
	return nil
}

func checkCompany(c Company) error {
	switch {
	case c.OrgKind != KindCompany: // companies_org_kind_chk
		return errCheck
	case c.SectorCode != nil && !sectorRe.MatchString(*c.SectorCode):
		return errCheck
	case c.SizeBand != nil && !slices.Contains(sizeBands, *c.SizeBand):
		return errCheck
	case c.EmailDomain != nil && !emailDomainRe.MatchString(*c.EmailDomain):
		return errCheck
	}
	return nil
}

// GetCompany implements Store.
func (s *MemStore) GetCompany(_ context.Context, orgID uuid.UUID) (Company, error) {
	if orgID == uuid.Nil() {
		return Company{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.companies[orgID]
	if !ok {
		return Company{}, ErrNotFound
	}
	return c, nil
}

// UpdateCompany implements Store.
func (s *MemStore) UpdateCompany(_ context.Context, c Company) (Company, error) {
	if c.OrgID == uuid.Nil() {
		return Company{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.companies[c.OrgID]
	if !ok {
		return Company{}, ErrNotFound
	}
	cur.SectorCode, cur.SizeBand, cur.EmailDomain, cur.HQCity = c.SectorCode, c.SizeBand, c.EmailDomain, c.HQCity
	if err := checkCompany(cur); err != nil {
		return Company{}, err
	}
	cur.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.companies[c.OrgID] = cur
	return cur, nil
}

// checkMember applies the CHECK constraints of org_members.
func checkMember(m Membership) error {
	switch {
	case !roleFitsKind(m.OrgKind, m.Role):
		return errCheck
	case m.Status != MemberPending && m.Status != MemberActive && m.Status != MemberEnded:
		return errCheck
	case m.Status == MemberActive && (m.JoinedAt == nil || m.EndedAt != nil):
		return errCheck
	case m.Status == MemberPending && (m.JoinedAt != nil || m.RequestedAt == nil):
		return errCheck
	case (m.Status == MemberEnded) != (m.EndedAt != nil), (m.Status == MemberEnded) != (m.EndReason != nil):
		return errCheck
	}
	return nil
}

func open(m Membership) bool { return m.Status == MemberPending || m.Status == MemberActive }

// InsertMember implements Store.
func (s *MemStore) InsertMember(_ context.Context, m Membership) (Membership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.orgs[m.OrgID]
	if !ok {
		return Membership{}, ErrNotFound
	}
	m.OrgKind = parent.Kind // 06 H-38
	if err := checkMember(m); err != nil {
		return Membership{}, err
	}
	for _, x := range s.members {
		switch {
		case x.ID == m.ID:
			return Membership{}, problem.ErrConflict
		case open(m) && open(x) && x.OrgID == m.OrgID && x.UserID == m.UserID: // org_members_open_uq
			return Membership{}, problem.ErrConflict
		case m.Status == MemberActive && m.Role == ownerRole && x.Status == MemberActive && x.Role == ownerRole &&
			x.OrgID == m.OrgID: // org_members_one_owner_uq
			return Membership{}, problem.ErrConflict
		}
	}
	now := s.stamp(time.Time{})
	m.CreatedAt, m.UpdatedAt = now, now
	s.members = append(s.members, m)
	return m, nil
}

// LockActiveMember implements Store.
func (s *MemStore) LockActiveMember(_ context.Context, orgID, userID uuid.UUID) (Membership, error) {
	return s.find(orgID, func(m Membership) bool { return m.UserID == userID && m.Status == MemberActive })
}

// LockOwner implements Store.
func (s *MemStore) LockOwner(_ context.Context, orgID uuid.UUID) (Membership, error) {
	return s.find(orgID, func(m Membership) bool { return m.Role == ownerRole && m.Status == MemberActive })
}

func (s *MemStore) find(orgID uuid.UUID, match func(Membership) bool) (Membership, error) {
	if orgID == uuid.Nil() {
		return Membership{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.members {
		if m.OrgID == orgID && match(m) {
			return m, nil
		}
	}
	return Membership{}, ErrNotFound
}

func ended(m Membership, now time.Time, reason EndReason) Membership {
	r := reason
	m.Status, m.EndedAt, m.EndReason = MemberEnded, &now, &r
	return m
}

// EndMember implements Store.
func (s *MemStore) EndMember(_ context.Context, orgID, id uuid.UUID, now time.Time, reason EndReason) (Membership, error) {
	if orgID == uuid.Nil() {
		return Membership{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.members {
		if m.ID == id && m.OrgID == orgID && open(m) {
			e := ended(m, now, reason)
			e.UpdatedAt = s.stamp(m.UpdatedAt)
			s.members[i] = e
			return e, nil
		}
	}
	return Membership{}, ErrNotFound
}

// EndOpenMembers implements Store.
func (s *MemStore) EndOpenMembers(_ context.Context, orgID uuid.UUID, now time.Time, reason EndReason) ([]Membership, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Membership
	for i, m := range s.members {
		if m.OrgID == orgID && open(m) {
			out = append(out, m)
			e := ended(m, now, reason)
			e.UpdatedAt = s.stamp(m.UpdatedAt)
			s.members[i] = e
		}
	}
	return out, nil
}

// MembershipsOfUser implements Store.
func (s *MemStore) MembershipsOfUser(_ context.Context, userID uuid.UUID) ([]Membership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Membership
	for _, m := range s.members {
		if m.UserID == userID {
			out = append(out, m)
		}
	}
	return out, nil
}

// OrgsCreatedBy implements Store.
func (s *MemStore) OrgsCreatedBy(_ context.Context, userID uuid.UUID) ([]Org, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Org
	for _, o := range s.orgs {
		if o.CreatedBy != nil && *o.CreatedBy == userID {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b Org) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}
