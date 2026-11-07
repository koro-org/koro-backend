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
	// UniversityStudentStats returns the figures of q.UniversityID's
	// active student profiles (of q.Departments when set); NewInRange
	// counts the profiles created in [q.From, q.To).
	UniversityStudentStats(ctx context.Context, q StudentStatsQuery) (UniversityStudents, error)
}

// StudentStatsQuery is what a university panel asks (BE-190: its faculty
// and department filters, its faculty breakdown). The caller resolves the
// university and its departments for its own panel (the directory's
// reference data: students does not read faculties).
type StudentStatsQuery struct {
	UniversityID uuid.UUID
	From, To     time.Time
	// Departments keeps only the students of these departments when not
	// empty (a department or faculty filter); every figure, the total
	// included, then covers them only, groups below KThreshold still
	// suppressed. A filter that names no department is the caller's to
	// answer (nothing to count).
	Departments []uuid.UUID
	// FacultyOf maps a department to its faculty; ByFaculty sums the
	// departments by it before the suppression (a faculty is published
	// when it reaches KThreshold, whatever its departments' sizes).
	// Students of a department without a faculty are in no faculty group.
	FacultyOf map[uuid.UUID]uuid.UUID
}

// StudentGroup is one group of a breakdown with at least KThreshold
// students: Key is the degree level, the class year ("1"–"8"), the
// department ID or the faculty ID.
type StudentGroup struct {
	Key   string
	Count int
}

// UniversityStudents are a university's student figures. Total and
// NewInRange are nil below KThreshold; a breakdown lists only its groups
// of at least KThreshold students (largest first, then by key), and
// students without a class year, department or faculty are in no group of
// that breakdown. One breakdown at a time over the students asked: the
// breakdowns are never crossed with each other, which would single out
// small intersections. (A department filter narrows the students first:
// its breakdowns are again groups of at least KThreshold students.)
type UniversityStudents struct {
	Total         *int
	NewInRange    *int
	ByDegreeLevel []StudentGroup
	ByClassYear   []StudentGroup
	ByDepartment  []StudentGroup
	ByFaculty     []StudentGroup
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
	// ProfileCounts counts universityID's active student profiles, only
	// those of departments when it is not empty.
	ProfileCounts(ctx context.Context, universityID uuid.UUID, departments []uuid.UUID, from, to time.Time) (ProfileCounts, error)
}

var _ Statistics = (*service)(nil)

// UniversityStudentStats implements Statistics.
func (s *service) UniversityStudentStats(ctx context.Context, q StudentStatsQuery) (UniversityStudents, error) {
	c, err := s.stats().ProfileCounts(ctx, q.UniversityID, q.Departments, q.From, q.To)
	if err != nil {
		return UniversityStudents{}, fmt.Errorf("students: university stats: %w", err)
	}
	return UniversityStudents{Total: atLeastK(c.Total), NewInRange: atLeastK(c.NewInRange),
		ByDegreeLevel: groups(c.ByDegreeLevel), ByClassYear: groups(c.ByClassYear), ByDepartment: groups(c.ByDepartment),
		ByFaculty: groups(byFaculty(c.ByDepartment, q.FacultyOf))}, nil
}

// byFaculty sums the exact department counts by faculty (before any
// suppression); departments without a faculty in facultyOf are left out.
func byFaculty(depts map[string]int, facultyOf map[uuid.UUID]uuid.UUID) map[string]int {
	out := map[string]int{}
	for k, n := range depts {
		id, err := uuid.Parse(k)
		if err != nil {
			continue
		}
		if f, ok := facultyOf[id]; ok {
			out[f.String()] += n
		}
	}
	return out
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
