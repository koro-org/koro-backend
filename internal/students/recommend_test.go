package students_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/students"
)

// recommendations is the recommendations' read of the students service
// (BE-174), asserted like the other system reads.
func (e *env) recommendations(t *testing.T) students.Recommendations {
	t.Helper()
	r, ok := e.svc.(students.Recommendations)
	if !ok {
		t.Fatal("the students service does not implement Recommendations")
	}
	return r
}

// TestRecommendationChoice_StudentTurnsOffAndOn (BE-174): the choice starts
// at default (no settings row), the student turns the recommendations off
// and on through updateMyPrivacy without a consent text (a minor too: the
// recommendations show the student's own data to the student only); the
// change is audited, writes no event, and an unknown value is 422 before
// anything is written.
func TestRecommendationChoice_StudentTurnsOffAndOn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	if c, err := e.recommendations(t).RecommendationChoice(ctx, a.UserID); err != nil || c != students.RankingDefault {
		t.Fatalf("RecommendationChoice before = %q, %v", c, err)
	}
	if p, err := e.svc.GetMyPrivacy(ctx, a); err != nil || p.RecommendationChoice != students.RankingDefault {
		t.Fatalf("GetMyPrivacy defaults = %+v, %v", p, err)
	}
	got, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{RecommendationChoice: ptr(students.RankingDisabled)})
	if err != nil || got.RecommendationChoice != students.RankingDisabled || got.ActivityRankingChoice != students.RankingDefault {
		t.Fatalf("UpdateMyPrivacy = %+v, %v", got, err)
	}
	if c, err := e.recommendations(t).RecommendationChoice(ctx, a.UserID); err != nil || c != students.RankingDisabled {
		t.Fatalf("RecommendationChoice after = %q, %v", c, err)
	}
	rows := e.audit.Of(authz.StudentsPrivacyUpdateOwn)
	if len(rows) != 1 || rows[0].Metadata["recommendation_choice"] != "disabled" {
		t.Fatalf("audit = %+v", rows)
	}
	if len(e.out.Events()) != 0 || len(e.consent.records()) != 0 {
		t.Fatalf("the recommendation choice wrote an event (%v) or a consent", e.topics())
	}
	_, err = e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{RecommendationChoice: ptr(students.RecommendationChoice("always"))})
	wantCode(t, err, "validation_failed")
	if c, _ := e.recommendations(t).RecommendationChoice(ctx, a.UserID); c != students.RankingDisabled {
		t.Fatalf("a refused value changed the choice to %q", c)
	}

	m := e.student(t, e.uniA, true)
	if got, err := e.svc.UpdateMyPrivacy(ctx, m, students.PrivacyPatch{RecommendationChoice: ptr(students.RankingEnabled)}); err != nil ||
		got.RecommendationChoice != students.RankingEnabled {
		t.Fatalf("a minor turning recommendations on = %+v, %v", got, err)
	}
}

// TestPopularClubs_DepartmentAndUniversityOnly (BE-174): the clubs popular
// around a student are counted over the eligible profiles of the student's
// department and university (10 or more members each); a student without a
// profile gets none, one without a department only the university's.
func TestPopularClubs_DepartmentAndUniversityOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.profiled(t) // department deptA of uniA
	deptClub, uniClub, small := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	peer := func(dept uuid.UUID, clubs ...uuid.UUID) {
		t.Helper()
		p := students.Profile{UserID: uuid.NewV7(), UniversityID: e.uniA, DepartmentID: &dept, DegreeLevel: students.DegreeBachelor,
			Status: students.ProfileActive}
		if _, ok, err := e.store.InsertProfile(ctx, p); err != nil || !ok {
			t.Fatalf("InsertProfile: %v %v", ok, err)
		}
		if err := e.store.SetAudience(ctx, p.UserID, keysOf(e, e.uniA, dept, "2", clubs...), true, t0); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 10 {
		peer(e.deptA, deptClub)
		peer(e.deptB, uniClub)
		if i < 9 {
			peer(e.deptA, small)
		}
	}
	got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 10)
	if err != nil || !slices.Equal(got.Department, []uuid.UUID{deptClub}) || len(got.University) != 2 ||
		!slices.Contains(got.University, deptClub) || !slices.Contains(got.University, uniClub) {
		t.Fatalf("PopularClubs = %+v, %v", got, err)
	}
	if slices.Contains(got.University, small) {
		t.Fatal("a club of 9 members is popular")
	}
	if got, err := e.recommendations(t).PopularClubs(ctx, uuid.NewV7(), 10); err != nil || len(got.Department) != 0 || len(got.University) != 0 {
		t.Fatalf("no profile = %+v, %v", got, err)
	}
	if got, err := e.recommendations(t).PopularClubs(ctx, a.UserID, 0); err != nil || len(got.Department) != 0 || len(got.University) != 0 {
		t.Fatalf("limit 0 = %+v, %v", got, err)
	}
	noDept := e.student(t, e.uniA, false)
	if _, ok, err := e.store.InsertProfile(ctx, students.Profile{UserID: noDept.UserID, UniversityID: e.uniA,
		DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}); err != nil || !ok {
		t.Fatalf("InsertProfile: %v %v", ok, err)
	}
	if got, err := e.recommendations(t).PopularClubs(ctx, noDept.UserID, 10); err != nil || len(got.Department) != 0 || len(got.University) != 2 {
		t.Fatalf("without a department = %+v, %v", got, err)
	}
}
