package students

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// ProfileCounts implements statsStore: one grouping-sets pass over the
// university's active profiles (student_profiles_filter_idx).
//
// cross-org: keyed by a university (student profiles belong to no org);
// only counts leave, the service suppresses the groups below 10.
func (s gormStore) ProfileCounts(ctx context.Context, universityID uuid.UUID, from, to time.Time) (ProfileCounts, error) {
	var rows []struct {
		Dim   string
		Key   *string
		N     int
		Fresh int
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT CASE WHEN GROUPING(degree_level) = 0 THEN 'degree'
	             WHEN GROUPING(class_year) = 0 THEN 'year'
	             WHEN GROUPING(department_id) = 0 THEN 'dept' ELSE 'total' END AS dim,
	        coalesce(degree_level, class_year::text, department_id::text) AS key,
	        count(*) AS n, count(*) FILTER (WHERE created_at >= ? AND created_at < ?) AS fresh
	   FROM student_profiles
	  WHERE university_id = ? AND status = 'active'
	  GROUP BY GROUPING SETS ((degree_level), (class_year), (department_id), ())`, from, to, universityID).Scan(&rows).Error
	if err != nil {
		return ProfileCounts{}, fmt.Errorf("students: profile counts: %w", err)
	}
	c := newProfileCounts()
	for _, r := range rows {
		switch {
		case r.Dim == "total":
			c.Total, c.NewInRange = r.N, r.Fresh
		case r.Key == nil: // no class year or department
		case r.Dim == "degree":
			c.ByDegreeLevel[*r.Key] = r.N
		case r.Dim == "year":
			c.ByClassYear[*r.Key] = r.N
		case r.Dim == "dept":
			c.ByDepartment[*r.Key] = r.N
		}
	}
	return c, nil
}
