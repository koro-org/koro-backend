//go:build integration

package migrations_test

import (
	"testing"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_RecommendationChoice (BE-174, migration 000176): a student's
// recommendation choice starts at 'default' (the product setting decides)
// and takes only the three values of activity_ranking_choice. The down
// migration drops the column; the up one brings the rule back.
func TestSchema_RecommendationChoice(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		u := w.user()
		w.exec(`INSERT INTO student_privacy_settings (user_id) VALUES ($1)`, u)
		if got := queryString(t, w.d, `SELECT recommendation_choice FROM student_privacy_settings WHERE user_id = $1`, u); got != "default" {
			t.Errorf("recommendation_choice default = %q, want 'default'", got)
		}
		expectOK(t, w.d.Pool, []p1aCase{
			{"turned off", `UPDATE student_privacy_settings SET recommendation_choice = 'disabled' WHERE user_id = $1`, []any{u}, "", ""},
			{"turned on", `UPDATE student_privacy_settings SET recommendation_choice = 'enabled' WHERE user_id = $1`, []any{u}, "", ""},
		})
		expectFail(t, w.d.Pool, []p1aCase{
			{"an unknown choice", `UPDATE student_privacy_settings SET recommendation_choice = 'always' WHERE user_id = $1`, []any{u},
				db.CodeCheckViolation, "student_privacy_settings_recommendation_choice_chk"},
			{"no choice", `UPDATE student_privacy_settings SET recommendation_choice = NULL WHERE user_id = $1`, []any{u},
				"23502", ""}, // not_null_violation
		})
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000176_student_recommendation_choice.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'student_privacy_settings'
		AND column_name = 'recommendation_choice'`); n != 0 {
		t.Fatal("down kept recommendation_choice")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000176_student_recommendation_choice.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
