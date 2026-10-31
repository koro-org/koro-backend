package students

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"
)

// Statistics is the student read of a university's panel (stats, BE-163;
// SR §8 "kendi öğrencilerinin toplu istatistikleri"; 10 kişinin altındaki
// gruplar gösterilmez): counts of the university's active student
// profiles, every group below KThreshold suppressed here, so a count of
// fewer than 10 people never leaves the module (the caller rounds the
// rest to buckets). It is a system read without authorisation: the caller
// authorises its own read (stats.university.read).
type Statistics interface {
	// UniversityStudentStats returns the figures of universityID's active
	// student profiles; NewInRange counts the profiles created in
	// [from, to).
	UniversityStudentStats(ctx context.Context, universityID uuid.UUID, from, to time.Time) (UniversityStudents, error)
}

// StudentGroup is one group of a breakdown with at least KThreshold
// students: Key is the degree level, the class year ("1"–"8") or the
// department ID.
type StudentGroup struct {
	Key   string
	Count int
}

// UniversityStudents are a university's student figures. Total and
// NewInRange are nil below KThreshold; a breakdown lists only its groups
// of at least KThreshold students (largest first, then by key), and
// students without a class year or department are in no group of that
// breakdown. One breakdown at a time: no cross-tabulation, which would
// single out small intersections.
type UniversityStudents struct {
	Total         *int
	NewInRange    *int
	ByDegreeLevel []StudentGroup
	ByClassYear   []StudentGroup
	ByDepartment  []StudentGroup
}

// ProfileCounts are the exact counts behind UniversityStudents; they stay
// in the package (Statistics suppresses the groups below KThreshold).
type ProfileCounts struct {
	Total, NewInRange int
	ByDegreeLevel     map[string]int
	ByClassYear       map[string]int
	ByDepartment      map[string]int
}

// statsStore is the persistence of Statistics, part of Store, keyed by the
// university the caller resolved for its own panel.
type statsStore interface {
	// ProfileCounts counts universityID's active student profiles.
	ProfileCounts(ctx context.Context, universityID uuid.UUID, from, to time.Time) (ProfileCounts, error)
}

var _ Statistics = (*service)(nil)

// UniversityStudentStats implements Statistics.
func (s *service) UniversityStudentStats(ctx context.Context, universityID uuid.UUID, from, to time.Time) (UniversityStudents, error) {
	c, err := s.stats().ProfileCounts(ctx, universityID, from, to)
	if err != nil {
		return UniversityStudents{}, fmt.Errorf("students: university stats: %w", err)
	}
	return UniversityStudents{Total: atLeastK(c.Total), NewInRange: atLeastK(c.NewInRange),
		ByDegreeLevel: groups(c.ByDegreeLevel), ByClassYear: groups(c.ByClassYear), ByDepartment: groups(c.ByDepartment)}, nil
}

// atLeastK returns n when it reaches KThreshold, nil otherwise.
func atLeastK(n int) *int {
	if n < KThreshold {
		return nil
	}
	return &n
}

// groups returns the groups of at least KThreshold, largest first.
func groups(counts map[string]int) []StudentGroup {
	out := []StudentGroup{}
	for k, n := range counts {
		if n >= KThreshold {
			out = append(out, StudentGroup{Key: k, Count: n})
		}
	}
	slices.SortFunc(out, func(a, b StudentGroup) int { return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Key, b.Key)) })
	return out
}

// newProfileCounts returns counts with empty maps.
func newProfileCounts() ProfileCounts {
	return ProfileCounts{ByDegreeLevel: map[string]int{}, ByClassYear: map[string]int{}, ByDepartment: map[string]int{}}
}
