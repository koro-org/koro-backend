package directory

import (
	"context"
	"fmt"
	"slices"
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
	// UniversityStructure returns the departments of universityID with
	// their faculty and its faculties (public reference data; BE-190: the
	// faculty breakdown and the faculty and department filters of the
	// university panel). An unknown university has none.
	UniversityStructure(ctx context.Context, universityID uuid.UUID) (UniversityStructure, error)
}

// DepartmentBrief is one department of a university and its faculty (nil:
// a department outside any faculty).
type DepartmentBrief struct {
	ID        uuid.UUID
	FacultyID *uuid.UUID
}

// UniversityStructure is a university's faculties and departments by ID.
// Departments holds every department, active or not (a student profile
// may still name a department that closed); Faculties the active
// faculties and every faculty a department names.
type UniversityStructure struct {
	Faculties   []uuid.UUID
	Departments []DepartmentBrief
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
	// DepartmentsOf returns every department of a university, active or
	// not, by ID.
	DepartmentsOf(ctx context.Context, universityID uuid.UUID) ([]Department, error)
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

// UniversityStructure implements Statistics.
func (s *service) UniversityStructure(ctx context.Context, universityID uuid.UUID) (UniversityStructure, error) {
	depts, err := s.d.Store.DepartmentsOf(ctx, universityID)
	if err != nil {
		return UniversityStructure{}, fmt.Errorf("directory: university departments: %w", err)
	}
	facs, err := s.d.Store.FacultiesOf(ctx, universityID)
	if err != nil {
		return UniversityStructure{}, fmt.Errorf("directory: university faculties: %w", err)
	}
	out := UniversityStructure{Faculties: make([]uuid.UUID, 0, len(facs)), Departments: make([]DepartmentBrief, len(depts))}
	for _, f := range facs {
		out.Faculties = append(out.Faculties, f.ID)
	}
	for i, d := range depts {
		out.Departments[i] = DepartmentBrief{ID: d.ID, FacultyID: d.FacultyID}
		if d.FacultyID != nil {
			out.Faculties = append(out.Faculties, *d.FacultyID)
		}
	}
	slices.SortFunc(out.Faculties, uuid.UUID.Compare)
	out.Faculties = slices.Compact(out.Faculties)
	return out, nil
}
