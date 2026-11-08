//go:build integration

package migrations_test

import (
	"testing"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_StudentInterests (BE-204, migration 000184): a profile starts
// without interests, holds at most five values of the fixed list (the
// non-sensitive club categories) and never a sensitive club category, an
// unknown value or NULL. The down migration drops the column; the up one
// brings the rule back.
func TestSchema_StudentInterests(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uni, _ := w.university()
	check := func(t *testing.T) {
		t.Helper()
		u := w.user()
		w.profile(u, uni, false)
		if got := queryString(t, w.d, `SELECT interests::text FROM student_profiles WHERE user_id = $1`, u); got != "{}" {
			t.Errorf("interests default = %q, want {}", got)
		}
		set := `UPDATE student_profiles SET interests = $2::text[] WHERE user_id = $1`
		expectOK(t, w.d.Pool, []p1aCase{
			{"two", set, []any{u, db.TextArray{"technology", "sports"}}, "", ""},
			{"five", set, []any{u, db.TextArray{"academic", "career", "entrepreneurship", "arts", "social_responsibility"}}, "", ""},
			{"none", set, []any{u, db.TextArray{}}, "", ""},
		})
		expectFail(t, w.d.Pool, []p1aCase{
			{"a sensitive club category", set, []any{u, db.TextArray{"religious"}}, db.CodeCheckViolation, "student_profiles_interests_chk"},
			{"political", set, []any{u, db.TextArray{"arts", "political"}}, db.CodeCheckViolation, "student_profiles_interests_chk"},
			{"an unknown value", set, []any{u, db.TextArray{"health"}}, db.CodeCheckViolation, "student_profiles_interests_chk"},
			{"six", set, []any{u, db.TextArray{"academic", "technology", "career", "arts", "sports", "hobby"}},
				db.CodeCheckViolation, "student_profiles_interests_chk"},
			{"a NULL element", `UPDATE student_profiles SET interests = ARRAY['arts', NULL]::text[] WHERE user_id = $1`, []any{u},
				db.CodeCheckViolation, "student_profiles_interests_chk"},
			{"NULL", `UPDATE student_profiles SET interests = NULL WHERE user_id = $1`, []any{u}, "23502", ""}, // not_null_violation
		})
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000184_student_interests.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'student_profiles'
		AND column_name = 'interests'`); n != 0 {
		t.Fatal("down kept interests")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000184_student_interests.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
