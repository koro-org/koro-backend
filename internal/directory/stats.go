package directory

import (
	"context"
	"fmt"
	"uuid"
)

// Statistics is the club catalog and claim read of the panels (stats,
// BE-163; SR §8 "doğrulama bekleyen kulüpler"): IDs, flags and counts
// only, no claimant leaves. It is a system read without authorisation: the
// caller authorises its own read (stats.university.read,
// stats.platform.read).
type Statistics interface {
	// UniversityClubStats returns the clubs of the university whose org is
	// universityOrgID and the size of its representatives' claim queue;
	// ErrNotFound when no university has that org.
	UniversityClubStats(ctx context.Context, universityOrgID uuid.UUID) (UniversityClubs, error)
	// PendingClaims counts every pending claim (the platform's queue,
	// sensitive clubs and escalated claims included).
	PendingClaims(ctx context.Context) (int, error)
}

// ClubBrief is one club of a university's catalog.
type ClubBrief struct {
	OrgID     uuid.UUID
	Sensitive bool // clubs.is_sensitive (03 Y2)
	Claimed   bool // claim_status claimed
}

// UniversityClubs are a university's clubs and its claim queue.
// PendingClaims is what its university_rep decides (adminListClubClaims'
// rep scope, 06 H-58): pending claims of its non-sensitive clubs that have
// not been escalated to platform staff (younger than ClaimEscalateAfter).
type UniversityClubs struct {
	UniversityID  uuid.UUID
	Clubs         []ClubBrief // by org ID
	PendingClaims int
}

// StatsStore is the persistence of Statistics, part of Store.
type StatsStore interface {
	// UniversityByOrg returns the university of an org or ErrNotFound.
	UniversityByOrg(ctx context.Context, orgID uuid.UUID) (University, error)
	// ClubsOf returns the clubs of a university by org ID.
	ClubsOf(ctx context.Context, universityID uuid.UUID) ([]Club, error)
	// CountClaims counts the claims of f (ListClaims' filter, unpaged).
	CountClaims(ctx context.Context, f ClaimFilter) (int, error)
}

var _ Statistics = (*service)(nil)

// UniversityClubStats implements Statistics.
func (s *service) UniversityClubStats(ctx context.Context, universityOrgID uuid.UUID) (UniversityClubs, error) {
	u, err := s.d.Store.UniversityByOrg(ctx, universityOrgID)
	if err != nil {
		return UniversityClubs{}, err
	}
	clubs, err := s.d.Store.ClubsOf(ctx, u.ID)
	if err != nil {
		return UniversityClubs{}, fmt.Errorf("directory: university clubs: %w", err)
	}
	since := s.now().Add(-ClaimEscalateAfter)
	pending, err := s.d.Store.CountClaims(ctx, ClaimFilter{Status: ClaimPending, RepUniversityOrgs: []uuid.UUID{universityOrgID},
		RepPendingSince: &since})
	if err != nil {
		return UniversityClubs{}, fmt.Errorf("directory: university claims: %w", err)
	}
	out := UniversityClubs{UniversityID: u.ID, Clubs: make([]ClubBrief, len(clubs)), PendingClaims: pending}
	for i, c := range clubs {
		out.Clubs[i] = ClubBrief{OrgID: c.OrgID, Sensitive: c.IsSensitive, Claimed: c.ClaimStatus == ClaimClaimed}
	}
	return out, nil
}

// PendingClaims implements Statistics.
func (s *service) PendingClaims(ctx context.Context) (int, error) {
	n, err := s.d.Store.CountClaims(ctx, ClaimFilter{Status: ClaimPending})
	if err != nil {
		return 0, fmt.Errorf("directory: pending claims: %w", err)
	}
	return n, nil
}
