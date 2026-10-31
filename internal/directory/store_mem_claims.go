package directory

import (
	"cmp"
	"context"
	"math"
	"slices"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/problem"
)

// checkClaim applies the CHECK constraints of club_claims (02 §8.6).
func checkClaim(c ClubClaim) error {
	switch {
	case !c.Status.Valid():
		return errCheck
	case utf8.RuneCountInString(c.RoleInClub) < 2 || utf8.RuneCountInString(c.RoleInClub) > 100:
		return errCheck
	case c.EvidenceText != nil && utf8.RuneCountInString(*c.EvidenceText) > 2000:
		return errCheck
	case c.DecisionNote != nil && utf8.RuneCountInString(*c.DecisionNote) > 1000:
		return errCheck
	case c.Status.Decided() != (c.ReviewedBy != nil), c.Status.Decided() != (c.ReviewedAt != nil): // club_claims_review_chk
		return errCheck
	case c.ReviewedBy != nil && *c.ReviewedBy == c.ClaimantUserID: // club_claims_reviewer_chk (03 Y6)
		return problem.ErrForbidden
	}
	return nil
}

// pendingTaken reports whether another pending claim of the claimant for
// the club exists (club_claims_pending_uq).
func (s *MemStore) pendingTaken(c ClubClaim) bool {
	if c.Status != ClaimPending {
		return false
	}
	for _, o := range s.claims {
		if o.ID != c.ID && o.Status == ClaimPending && o.ClubOrgID == c.ClubOrgID && o.ClaimantUserID == c.ClaimantUserID {
			return true
		}
	}
	return false
}

// InsertClaim implements Store.
func (s *MemStore) InsertClaim(_ context.Context, c ClubClaim) (ClubClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Status == "" {
		c.Status = ClaimPending
	}
	if err := checkClaim(c); err != nil {
		return ClubClaim{}, err
	}
	if _, ok := s.clubs[c.ClubOrgID]; !ok {
		return ClubClaim{}, ErrNotFound // the club (parent row, 06 H-38)
	}
	if _, dup := s.claims[c.ID]; dup {
		return ClubClaim{}, problem.ErrConflict
	}
	if s.pendingTaken(c) {
		return ClubClaim{}, ErrClaimPending
	}
	s.created(&c.CreatedAt, &c.UpdatedAt)
	s.claims[c.ID] = c
	return c, nil
}

// GetClaim implements Store.
func (s *MemStore) GetClaim(_ context.Context, id uuid.UUID) (ClubClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.claims[id]
	if !ok {
		return ClubClaim{}, ErrNotFound
	}
	return c, nil
}

// LockClaim implements Store.
func (s *MemStore) LockClaim(ctx context.Context, id uuid.UUID) (ClubClaim, error) {
	return s.GetClaim(ctx, id)
}

// UpdateClaim implements Store.
func (s *MemStore) UpdateClaim(_ context.Context, c ClubClaim) (ClubClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.claims[c.ID]
	if !ok {
		return ClubClaim{}, ErrNotFound
	}
	next := cur
	next.Status, next.EvidenceFileID, next.ReviewedBy, next.ReviewedAt, next.DecisionNote =
		c.Status, c.EvidenceFileID, c.ReviewedBy, c.ReviewedAt, c.DecisionNote
	if err := checkClaim(next); err != nil {
		return ClubClaim{}, err
	}
	if s.pendingTaken(next) {
		return ClubClaim{}, ErrClaimPending
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.claims[c.ID] = next
	return next, nil
}

// claimRows returns the claims matching keep in id order (desc when
// newest), after the keyset position, up to limit.
func (s *MemStore) claimRows(keep func(ClubClaim) bool, after *uuid.UUID, newest bool, limit int) []ClubClaim {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ClubClaim
	for _, c := range s.claims {
		if !keep(c) {
			continue
		}
		if after != nil {
			k := cmp.Compare(c.ID.String(), after.String())
			if newest && k >= 0 || !newest && k <= 0 {
				continue
			}
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b ClubClaim) int {
		if newest {
			return cmp.Compare(b.ID.String(), a.ID.String())
		}
		return cmp.Compare(a.ID.String(), b.ID.String())
	})
	return limitRows(out, limit)
}

// ClaimsOf implements Store.
func (s *MemStore) ClaimsOf(_ context.Context, userID uuid.UUID, after *uuid.UUID, limit int) ([]ClubClaim, error) {
	return s.claimRows(func(c ClubClaim) bool { return c.ClaimantUserID == userID }, after, true, limit), nil
}

// ListClaims implements Store.
func (s *MemStore) ListClaims(_ context.Context, f ClaimFilter, after *uuid.UUID, limit int) ([]ClubClaim, error) {
	var keep map[uuid.UUID]bool // club org IDs of a rep's scope or of one university
	if f.RepUniversityOrgs != nil || f.UniversityOrg != nil {
		s.mu.Lock()
		keep = map[uuid.UUID]bool{}
		for _, c := range s.clubs {
			u, ok := s.unis[c.UniversityID]
			switch {
			case !ok:
			case f.RepUniversityOrgs != nil && (c.IsSensitive || !slices.Contains(f.RepUniversityOrgs, u.OrgID)):
			case f.UniversityOrg != nil && u.OrgID != *f.UniversityOrg:
			default:
				keep[c.OrgID] = true
			}
		}
		s.mu.Unlock()
	}
	return s.claimRows(func(c ClubClaim) bool {
		switch {
		case f.RepUniversityOrgs != nil && f.RepPendingSince != nil && f.Status == ClaimPending && !c.CreatedAt.After(*f.RepPendingSince),
			f.CreatedBefore != nil && c.CreatedAt.After(*f.CreatedBefore):
			return false
		}
		return c.Status == f.Status && (keep == nil || keep[c.ClubOrgID])
	}, after, f.Status != ClaimPending, limit), nil
}

// CountClaims implements Store: ListClaims' filter, unpaged.
func (s *MemStore) CountClaims(ctx context.Context, f ClaimFilter) (int, error) {
	rows, err := s.ListClaims(ctx, f, nil, math.MaxInt)
	return len(rows), err
}

// ClaimOfFile implements Store.
func (s *MemStore) ClaimOfFile(_ context.Context, fileID uuid.UUID) (ClubClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.claims {
		if c.EvidenceFileID != nil && *c.EvidenceFileID == fileID {
			return c, nil
		}
	}
	return ClubClaim{}, ErrNotFound
}

// DeleteClaim implements Store.
func (s *MemStore) DeleteClaim(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claims, id)
	return nil
}

// decidedAt is when a claim was decided (reviewed_at, or the last update
// of a withdrawal); ok=false for a pending claim.
func (c ClubClaim) decidedAt() (time.Time, bool) {
	switch {
	case c.Status == ClaimPending:
		return time.Time{}, false
	case c.ReviewedAt != nil:
		return *c.ReviewedAt, true
	}
	return c.UpdatedAt, true
}

func (s *MemStore) decidedBefore(cutoff time.Time) []ClubClaim {
	var out []ClubClaim
	for _, c := range s.claims {
		if at, ok := c.decidedAt(); ok && at.Before(cutoff) {
			out = append(out, c)
		}
	}
	return out
}

// ClaimsDecidedBefore implements Store.
func (s *MemStore) ClaimsDecidedBefore(_ context.Context, cutoff time.Time, limit int) ([]ClubClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.decidedBefore(cutoff)
	slices.SortFunc(out, func(a, b ClubClaim) int {
		x, _ := a.decidedAt()
		y, _ := b.decidedAt()
		return cmp.Or(x.Compare(y), a.ID.Compare(b.ID))
	})
	return out[:min(len(out), max(limit, 0))], nil
}

// CountClaimsDecidedBefore implements Store.
func (s *MemStore) CountClaimsDecidedBefore(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.decidedBefore(cutoff)), nil
}
