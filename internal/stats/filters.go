package stats

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"
	"uuid"

	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/outreach"
	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
)

// The panel filters (BE-190; T-406, SR §8): what each panel narrows and
// what it never reveals. Every filter selects among figures the panel
// already publishes under the same rules (06 H-61); none adds a number
// about fewer than 10 people:
//
//   - club: event_id, one event of the club (its own registrations: exact).
//   - company: university_id, department_id, class_year select the saved
//     audiences and dispatches whose audience names them; their stored,
//     already published estimates pass through (no new count, no daily
//     budget). Posts, contact requests, package and sponsorship are not
//     about an audience and stay as they are. The club category is not an
//     audience dimension (Q-58).
//   - university: faculty_id or department_id narrows the students (each
//     group again at least 10, rounded down to its bucket: students
//     suppresses, stats rounds); club_id narrows the club activity to one
//     club (a sensitive club's stays empty, like a club without events).
//     The students and the clubs are not crossed: clubs have no
//     department.

// minClassYear and maxClassYear bound class_year (students' 1–8).
const (
	minClassYear = 1
	maxClassYear = 8
)

// clubFigures returns the club's event figures: those of the range, or
// with q.EventID the one event of the club whatever its date (the web
// picks it from the range's items). An event of another org, unknown or
// malformed is 422 query.event_id, the same answer for each.
func (s *service) clubFigures(ctx context.Context, clubID uuid.UUID, q RangeQuery, from, to time.Time) ([]events.EventFigures, error) {
	if q.EventID == "" {
		return s.d.Events.OrgEventFigures(ctx, []uuid.UUID{clubID}, from, to)
	}
	notOurs := problem.Field("query.event_id", "must be an event of the club")
	id, err := uuid.Parse(q.EventID)
	if err != nil {
		return nil, notOurs
	}
	e, err := s.d.EventInfo.Get(ctx, id)
	switch {
	case errors.Is(err, events.ErrNotFound):
		return nil, notOurs
	case err != nil:
		return nil, err
	case e.OrgID != clubID:
		return nil, notOurs
	}
	// The figures of the events that start in the event's own second: a
	// draft has none (OrgEventFigures leaves drafts out).
	figures, err := s.d.Events.OrgEventFigures(ctx, []uuid.UUID{clubID}, e.StartsAt, e.StartsAt.Add(time.Second))
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(figures, func(f events.EventFigures) bool { return f.EventID != id }), nil
}

// audienceFilter turns the company panel's filters into outreach's: a
// university also names its departments (a department implies its
// university). A malformed ID or class year is 422; an unknown ID simply
// matches nothing (reference data is public).
func (s *service) audienceFilter(ctx context.Context, q RangeQuery) (outreach.StatsFilter, error) {
	var f outreach.StatsFilter
	uni, err := httpx.OptID("query.university_id", q.UniversityID)
	if err != nil {
		return f, err
	}
	if f.Dept, err = httpx.OptID("query.department_id", q.DepartmentID); err != nil {
		return f, err
	}
	if q.ClassYear != "" {
		y, err := strconv.ParseInt(q.ClassYear, 10, 16)
		if err != nil || y < minClassYear || y > maxClassYear {
			return f, problem.Field("query.class_year", "must be a class year (1-8)")
		}
		v := int16(y)
		f.Year = &v
	}
	if uni != nil {
		st, err := s.d.Universities.UniversityStructure(ctx, *uni)
		if err != nil {
			return f, err
		}
		f.Uni = uni
		for _, d := range st.Departments {
			f.UniDepts = append(f.UniDepts, d.ID)
		}
	}
	return f, nil
}

// studentScope is the university panel's student filter resolved against
// the university's structure.
type studentScope struct {
	filtered    bool        // a faculty or department filter is present
	departments []uuid.UUID // the departments it keeps (empty: none, nothing to count)
	facultyOf   map[uuid.UUID]uuid.UUID
}

// resolveStudentScope validates the faculty and department filters against the
// university's faculties and departments (422 naming the parameter: one
// of each university's own, never both) and builds the faculty map of the
// breakdown.
func resolveStudentScope(q RangeQuery, st directory.UniversityStructure) (studentScope, error) {
	out := studentScope{facultyOf: map[uuid.UUID]uuid.UUID{}}
	for _, d := range st.Departments {
		if d.FacultyID != nil {
			out.facultyOf[d.ID] = *d.FacultyID
		}
	}
	if q.FacultyID != "" && q.DepartmentID != "" {
		return studentScope{}, problem.Field("query.department_id", "use faculty_id or department_id, not both")
	}
	if q.FacultyID != "" {
		id, err := uuid.Parse(q.FacultyID)
		if err != nil || !slices.Contains(st.Faculties, id) {
			return studentScope{}, problem.Field("query.faculty_id", "must be a faculty of the university")
		}
		out.filtered, out.departments = true, []uuid.UUID{}
		for d, f := range out.facultyOf {
			if f == id {
				out.departments = append(out.departments, d)
			}
		}
		slices.SortFunc(out.departments, uuid.UUID.Compare)
	}
	if q.DepartmentID != "" {
		id, err := uuid.Parse(q.DepartmentID)
		if err != nil || !slices.ContainsFunc(st.Departments, func(d directory.DepartmentBrief) bool { return d.ID == id }) {
			return studentScope{}, problem.Field("query.department_id", "must be a department of the university")
		}
		out.filtered, out.departments = true, []uuid.UUID{id}
	}
	return out, nil
}

// clubScope validates the club filter against the university's clubs
// (422 otherwise) and returns the clubs whose activity is read: the
// non-sensitive ones, or the one club asked when it is not sensitive.
func clubScope(q RangeQuery, clubs []directory.ClubBrief) ([]uuid.UUID, error) {
	var only *uuid.UUID
	if q.ClubID != "" {
		id, err := uuid.Parse(q.ClubID)
		if err != nil || !slices.ContainsFunc(clubs, func(c directory.ClubBrief) bool { return c.OrgID == id }) {
			return nil, problem.Field("query.club_id", "must be a club of the university")
		}
		only = &id
	}
	open := make([]uuid.UUID, 0, len(clubs))
	for _, c := range clubs {
		if !c.Sensitive && (only == nil || c.OrgID == *only) {
			open = append(open, c.OrgID)
		}
	}
	return open, nil
}
