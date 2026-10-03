//go:build integration

package migrations_test

import (
	"context"
	"net/url"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// segment is the result of count_segment: nil is NULL (fewer than ten).
func segment(t *testing.T, w *p1aWorld, dims int, keys ...string) *int {
	t.Helper()
	var n *int
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT count_segment($1::text[], $2::smallint)`, db.TextArray(keys), dims).Scan(&n); err != nil {
		t.Fatalf("count_segment(%v, %d): %v", keys, dims, err)
	}
	return n
}

// wantSegment fails the test unless count_segment returned want (-1 = NULL).
func wantSegment(t *testing.T, w *p1aWorld, want int, dims int, keys ...string) {
	t.Helper()
	got := segment(t, w, dims, keys...)
	switch {
	case want < 0 && got != nil:
		t.Errorf("count_segment(%v, %d) = %d, want NULL", keys, dims, *got)
	case want >= 0 && (got == nil || *got != want):
		if got == nil {
			t.Errorf("count_segment(%v, %d) = NULL, want %d", keys, dims, want)
		} else {
			t.Errorf("count_segment(%v, %d) = %d, want %d", keys, dims, *got, want)
		}
	}
}

// addStudents creates n students with the same targeting keys.
func addStudents(w *p1aWorld, n int, university uuid.UUID, eligible bool, keys ...string) {
	w.t.Helper()
	for range n {
		w.profile(w.student(), university, eligible, keys...)
	}
}

// TestCountSegment_BelowTenReturnsNull (01 §1.4, 02 §9.1): the k >= 10
// threshold is enforced in the database. A group of nine is NULL, never the
// number; ten is the first real count. The count is not NULL-able by the
// caller: zero matches, an empty key list and NULL arguments are NULL too.
func TestCountSegment_BelowTenReturnsNull(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uni, _ := w.university()
	key := "uni:" + uni.String()

	wantSegment(t, w, -1, 1, key) // nobody yet
	addStudents(w, 9, uni, true, key, "year:2")
	wantSegment(t, w, -1, 1, key) // nine
	wantSegment(t, w, -1, 1, "year:2")
	addStudents(w, 1, uni, true, key, "year:2")
	wantSegment(t, w, 10, 1, key) // ten: the first number shown
	wantSegment(t, w, 10, 1, "year:2")
	addStudents(w, 3, uni, true, key, "year:3")
	wantSegment(t, w, 13, 1, key)
	wantSegment(t, w, -1, 1, "year:3") // three of them are year 3: below ten

	// Degenerate input never produces a number.
	wantSegment(t, w, -1, 1, "uni:"+uuid.NewV7().String()) // unknown university
	wantSegment(t, w, -1, 1)                               // no keys
	var nullArgs *int
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT count_segment(NULL, 1::smallint)`).Scan(&nullArgs); err != nil || nullArgs != nil {
		t.Errorf("count_segment(NULL, 1) = %v, %v; want NULL", nullArgs, err)
	}
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT count_segment(ARRAY['all'], NULL)`).Scan(&nullArgs); err != nil || nullArgs != nil {
		t.Errorf("count_segment(keys, NULL) = %v, %v; want NULL", nullArgs, err)
	}
}

// TestCountSegment_ExcludesIneligible (02 §9.1): only audience_eligible
// profiles are counted, and the targeting is OR inside a dimension and AND
// across dimensions ('all' is a dimension of its own).
func TestCountSegment_ExcludesIneligible(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uniA, _ := w.university()
	uniB, _ := w.university()
	a, b := "uni:"+uniA.String(), "uni:"+uniB.String()

	// Twelve eligible students and eight who are not (unverified, minors,
	// inactive ...: the flag is the only thing count_segment looks at).
	addStudents(w, 12, uniA, true, a, "year:1")
	addStudents(w, 8, uniA, false, a, "year:1")
	wantSegment(t, w, 12, 1, a)
	wantSegment(t, w, 12, 1, "year:1")

	// Dropping eligibility of three students pulls the group below ten.
	w.exec(`UPDATE student_profiles SET audience_eligible = false WHERE user_id IN
		(SELECT user_id FROM student_profiles WHERE audience_eligible AND university_id = $1 LIMIT 3)`, uniA)
	wantSegment(t, w, -1, 1, a)
	// … and they count again once eligible.
	w.exec(`UPDATE student_profiles SET audience_eligible = true WHERE university_id = $1`, uniA)
	wantSegment(t, w, 20, 1, a)

	// Dimensions: groups of ten with different key combinations.
	addStudents(w, 10, uniB, true, b, "year:1") // B, year 1
	addStudents(w, 10, uniA, true, a, "year:2") // A, year 2
	addStudents(w, 10, uniB, true, b, "year:3", "all")
	wantSegment(t, w, 30, 1, a)                     // A: 20 in year 1 + 10 in year 2
	wantSegment(t, w, 50, 1, a, b)                  // OR inside the 'uni' dimension
	wantSegment(t, w, 20, 2, a, "year:1")           // AND: A and year 1
	wantSegment(t, w, 30, 2, a, "year:1", "year:2") // A and (year 1 or year 2)
	wantSegment(t, w, 30, 2, a, b, "year:1")        // (A or B) and year 1
	wantSegment(t, w, 10, 2, b, "year:3")           // B and year 3
	wantSegment(t, w, 10, 1, "all")                 // only the last group carries 'all'
	wantSegment(t, w, -1, 2, a, "year:3")           // nobody is in A and year 3
	wantSegment(t, w, 10, 3, b, "year:3", "all")
}

// TestSchema_CountSegmentPrivileges: count_segment is not callable by PUBLIC;
// app_rw and app_ro hold EXECUTE (01 §1.4). It is SECURITY INVOKER, so the
// grant to app_ro widens nothing: app_ro has no privilege on student_profiles
// and the call fails on the table (T-132).
func TestSchema_CountSegmentPrivileges(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	const fn = "public.count_segment(text[], smallint)"
	for role, want := range map[string]string{"app_rw": "true", "app_ro": "true", "audit_writer": "false", "audit_chainer": "false", "app_talent": "false"} {
		if got := queryString(t, w.d, `SELECT has_function_privilege($1, $2, 'EXECUTE')::text`, role, fn); got != want {
			t.Errorf("EXECUTE on count_segment for %s = %s, want %s", role, got, want)
		}
	}
	if got := queryString(t, w.d, `SELECT count(*)::text FROM pg_proc p, aclexplode(p.proacl) a WHERE p.oid = $1::regprocedure AND a.grantee = 0`, fn); got != "0" {
		t.Errorf("PUBLIC holds EXECUTE on count_segment")
	}
	if got := queryString(t, w.d, `SELECT prosecdef::text || provolatile::text FROM pg_proc WHERE oid = $1::regprocedure`, fn); got != "falses" {
		t.Errorf("count_segment must be SECURITY INVOKER and STABLE, got %q", got)
	}

	// app_ro: executes, but cannot read the table behind it.
	u, err := url.Parse(testutil.OwnerURL(t, w.d))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("app_ro", testutil.RolePassword)
	ro, err := pgx.Connect(t.Context(), u.String())
	if err != nil {
		t.Fatalf("connect as app_ro: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close(context.Background()) })
	_, err = ro.Exec(t.Context(), `SELECT count_segment(ARRAY['all'], 1::smallint)`)
	if got := db.SQLState(err); got != db.CodeInsufficientPriv {
		t.Errorf("app_ro count_segment: SQLSTATE %q, want %s (%v)", got, db.CodeInsufficientPriv, err)
	}
	if _, err := ro.Exec(t.Context(), `SELECT 1 FROM student_profiles`); db.SQLState(err) != db.CodeInsufficientPriv {
		t.Errorf("app_ro reads student_profiles: %v", err)
	}
}

// TestSchema_StudentProfileConstraints (02 §9.1).
func TestSchema_StudentProfileConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uni, _ := w.university()
	u := w.student()
	file := uuid.NewV7() // no such file
	base := map[string]string{"user_id": "$1", "university_id": "$2", "degree_level": "'bachelor'"}
	c := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("student_profiles", base, over), args, state, constraint}
	}
	ok := []any{u, uni}
	expectFail(t, w.d.Pool, []p1aCase{
		c("unknown degree level", map[string]string{"degree_level": lit("phd")}, ok, db.CodeCheckViolation, "student_profiles_degree_level_chk"),
		c("class year 0", map[string]string{"class_year": "0"}, ok, db.CodeCheckViolation, "student_profiles_class_year_chk"),
		c("class year 9", map[string]string{"class_year": "9"}, ok, db.CodeCheckViolation, "student_profiles_class_year_chk"),
		c("graduation not on the first of a month", map[string]string{"expected_graduation": "DATE '2028-06-15'"}, ok, db.CodeCheckViolation, "student_profiles_expected_graduation_chk"),
		c("unknown status", map[string]string{"status": lit("deleted")}, ok, db.CodeCheckViolation, "student_profiles_status_chk"),
		c("graduated without a date", map[string]string{"status": lit("graduated")}, ok, db.CodeCheckViolation, "student_profiles_graduated_chk"),
		c("graduation date of an active student", map[string]string{"graduated_at": "now()"}, ok, db.CodeCheckViolation, "student_profiles_graduated_chk"),
		c("headline of 161 characters", map[string]string{"headline": lit(repeat("a", 161))}, ok, db.CodeCheckViolation, "student_profiles_headline_chk"),
		c("about of 2001 characters", map[string]string{"about": lit(repeat("a", 2001))}, ok, db.CodeCheckViolation, "student_profiles_about_chk"),
		c("city of 61 characters", map[string]string{"city": lit(repeat("a", 61))}, ok, db.CodeCheckViolation, "student_profiles_city_chk"),
		c("unknown section", map[string]string{"completed_sections": "ARRAY['basics','hobbies']"}, ok, db.CodeCheckViolation, "student_profiles_completed_sections_chk"),
		c("unknown user", nil, []any{uuid.NewV7(), uni}, db.CodeForeignKeyViolation, "student_profiles_user_id_fkey"),
		c("unknown university", nil, []any{u, uuid.NewV7()}, db.CodeForeignKeyViolation, "student_profiles_university_id_fkey"),
		c("unknown department", map[string]string{"department_id": "'" + uuid.NewV7().String() + "'"}, ok, db.CodeForeignKeyViolation, "student_profiles_department_id_fkey"),
		c("unknown avatar file", map[string]string{"avatar_file_id": "'" + file.String() + "'"}, ok, db.CodeForeignKeyViolation, "student_profiles_avatar_file_id_fkey"),
	})

	expectOK(t, w.d.Pool, []p1aCase{
		c("valid profile", map[string]string{"class_year": "3", "expected_graduation": "DATE '2028-06-01'", "city": lit("İzmir"), "completed_sections": "ARRAY['basics','cv']"}, ok, "", ""),
	})
	expectFail(t, w.d.Pool, []p1aCase{
		c("second profile of the same user", nil, ok, db.CodeUniqueViolation, "student_profiles_pkey"),
	})
	var eligible bool
	var keys db.TextArray
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT audience_eligible, audience_keys FROM student_profiles WHERE user_id = $1`, u).Scan(&eligible, &keys); err != nil {
		t.Fatal(err)
	}
	if eligible || len(keys) != 0 {
		t.Errorf("a new profile must start not eligible with no keys, got %v %v", eligible, keys)
	}
	// Hard delete with the account (02 §9.1): the profile goes, the user stays.
	w.exec(`DELETE FROM student_profiles WHERE user_id = $1`, u)
	if n := w.count(`SELECT count(*) FROM users WHERE id = $1`, u); n != 1 {
		t.Errorf("users rows = %d, want 1", n)
	}
}

// TestSchema_StudentChildTableConstraints: privacy settings, skills,
// experiences, languages and CVs (02 §9.2–§9.6).
func TestSchema_StudentChildTableConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	u := w.student()
	other := uuid.NewV7()
	skill := uuid.NewV7()
	w.exec(`INSERT INTO skills (id, slug, name_tr, category) VALUES ($1, 'go-' || $2::text, 'Go', 'technical')`, skill, w.n())

	// student_privacy_settings: the private side is the default.
	expectFail(t, w.d.Pool, []p1aCase{
		{"unknown activity ranking choice", `INSERT INTO student_privacy_settings (user_id, activity_ranking_choice) VALUES ($1, 'always')`, []any{u}, db.CodeCheckViolation, "student_privacy_settings_activity_ranking_choice_chk"},
		{"settings of an unknown user", `INSERT INTO student_privacy_settings (user_id) VALUES ($1)`, []any{other}, db.CodeForeignKeyViolation, "student_privacy_settings_user_id_fkey"},
	})
	w.exec(`INSERT INTO student_privacy_settings (user_id) VALUES ($1)`, u)
	if got := queryString(t, w.d, `SELECT discoverable::text || show_contact_on_grant::text || show_clubs_on_grant::text || activity_ranking_choice
		FROM student_privacy_settings WHERE user_id = $1`, u); got != "falsefalsefalsedefault" {
		t.Errorf("privacy defaults = %q, want everything off and 'default'", got)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"second settings row", `INSERT INTO student_privacy_settings (user_id) VALUES ($1)`, []any{u}, db.CodeUniqueViolation, "student_privacy_settings_pkey"},
	})

	// student_skills.
	expectFail(t, w.d.Pool, []p1aCase{
		{"skill level 0", `INSERT INTO student_skills (user_id, skill_id, level) VALUES ($1, $2, 0)`, []any{u, skill}, db.CodeCheckViolation, "student_skills_level_chk"},
		{"skill level 6", `INSERT INTO student_skills (user_id, skill_id, level) VALUES ($1, $2, 6)`, []any{u, skill}, db.CodeCheckViolation, "student_skills_level_chk"},
		{"unknown skill", `INSERT INTO student_skills (user_id, skill_id) VALUES ($1, $2)`, []any{u, other}, db.CodeForeignKeyViolation, "student_skills_skill_id_fkey"},
		{"skill of an unknown user", `INSERT INTO student_skills (user_id, skill_id) VALUES ($1, $2)`, []any{other, skill}, db.CodeForeignKeyViolation, "student_skills_user_id_fkey"},
	})
	w.exec(`INSERT INTO student_skills (user_id, skill_id, level) VALUES ($1, $2, 5)`, u, skill)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the same skill twice", `INSERT INTO student_skills (user_id, skill_id) VALUES ($1, $2)`, []any{u, skill}, db.CodeUniqueViolation, "student_skills_pkey"},
	})

	// student_experiences.
	exp := map[string]string{"user_id": "$1", "kind": "'job'", "title": "'Stajyer'", "start_month": "DATE '2025-07-01'"}
	e := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("student_experiences", exp, over), args, state, constraint}
	}
	expectFail(t, w.d.Pool, []p1aCase{
		e("unknown kind", map[string]string{"kind": lit("hobby")}, []any{u}, db.CodeCheckViolation, "student_experiences_kind_chk"),
		e("title of 121 characters", map[string]string{"title": lit(repeat("a", 121))}, []any{u}, db.CodeCheckViolation, "student_experiences_title_chk"),
		e("organisation of 161 characters", map[string]string{"organization_name": lit(repeat("a", 161))}, []any{u}, db.CodeCheckViolation, "student_experiences_organization_name_chk"),
		e("start not on the first of a month", map[string]string{"start_month": "DATE '2025-07-02'"}, []any{u}, db.CodeCheckViolation, "student_experiences_start_month_chk"),
		e("end not on the first of a month", map[string]string{"end_month": "DATE '2025-09-30'"}, []any{u}, db.CodeCheckViolation, "student_experiences_end_month_chk"),
		e("ends before it starts", map[string]string{"end_month": "DATE '2025-06-01'"}, []any{u}, db.CodeCheckViolation, "student_experiences_period_chk"),
		e("description of 2001 characters", map[string]string{"description": lit(repeat("a", 2001))}, []any{u}, db.CodeCheckViolation, "student_experiences_description_chk"),
		e("unknown user", nil, []any{other}, db.CodeForeignKeyViolation, "student_experiences_user_id_fkey"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		e("open-ended experience", nil, []any{u}, "", ""),
		e("one-month experience", map[string]string{"end_month": "DATE '2025-07-01'"}, []any{u}, "", ""),
	})

	// student_languages.
	expectFail(t, w.d.Pool, []p1aCase{
		{"one-letter language code", `INSERT INTO student_languages (user_id, language_code, level) VALUES ($1, 't', 'B2')`, []any{u}, db.CodeCheckViolation, "student_languages_language_code_chk"},
		{"upper-case language code", `INSERT INTO student_languages (user_id, language_code, level) VALUES ($1, 'TR', 'B2')`, []any{u}, db.CodeCheckViolation, "student_languages_language_code_chk"},
		{"four-letter language code", `INSERT INTO student_languages (user_id, language_code, level) VALUES ($1, 'turk', 'B2')`, []any{u}, db.CodeCheckViolation, "student_languages_language_code_chk"},
		{"unknown level", `INSERT INTO student_languages (user_id, language_code, level) VALUES ($1, 'en', 'D1')`, []any{u}, db.CodeCheckViolation, "student_languages_level_chk"},
		{"language of an unknown user", `INSERT INTO student_languages (user_id, language_code, level) VALUES ($1, 'en', 'B2')`, []any{other}, db.CodeForeignKeyViolation, "student_languages_user_id_fkey"},
	})
	w.exec(`INSERT INTO student_languages (user_id, language_code, level) VALUES ($1, 'en', 'native')`, u)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the same language twice", `INSERT INTO student_languages (user_id, language_code, level) VALUES ($1, 'en', 'B1')`, []any{u}, db.CodeUniqueViolation, "student_languages_pkey"},
	})

	// student_cvs: at most one current CV per user.
	f1, f2, f3 := w.file("cv"), w.file("cv"), w.file("cv")
	cv := func(file uuid.UUID, current bool) p1aCase {
		return p1aCase{"cv", `INSERT INTO student_cvs (user_id, file_id, is_current) VALUES ($1, $2, $3)`, []any{u, file, current}, "", ""}
	}
	expectOK(t, w.d.Pool, []p1aCase{cv(f1, true), cv(f2, false)})
	expectFail(t, w.d.Pool, []p1aCase{
		{"second current CV", `INSERT INTO student_cvs (user_id, file_id, is_current) VALUES ($1, $2, true)`, []any{u, f3}, db.CodeUniqueViolation, "student_cvs_current_uq"},
		{"the same file as a second CV", `INSERT INTO student_cvs (user_id, file_id) VALUES ($1, $2)`, []any{u, f2}, db.CodeUniqueViolation, "student_cvs_file_id_uq"},
		{"unknown file", `INSERT INTO student_cvs (user_id, file_id) VALUES ($1, $2)`, []any{u, other}, db.CodeForeignKeyViolation, "student_cvs_file_id_fkey"},
		{"CV of an unknown user", `INSERT INTO student_cvs (user_id, file_id) VALUES ($1, $2)`, []any{other, f3}, db.CodeForeignKeyViolation, "student_cvs_user_id_fkey"},
	})
	// Another user may have a current CV; the current one can be swapped.
	u2 := w.student()
	expectOK(t, w.d.Pool, []p1aCase{{"another user's current CV", `INSERT INTO student_cvs (user_id, file_id, is_current) VALUES ($1, $2, true)`, []any{u2, f3}, "", ""}})
	w.exec(`UPDATE student_cvs SET is_current = false WHERE user_id = $1 AND is_current`, u)
	w.exec(`UPDATE student_cvs SET is_current = true WHERE file_id = $1`, f2)

	// Hard delete with the account: every child table lets app_rw delete.
	for table, key := range map[string]string{"student_privacy_settings": "user_id", "student_skills": "user_id", "student_experiences": "user_id", "student_languages": "user_id", "student_cvs": "user_id"} {
		w.exec(`DELETE FROM `+table+` WHERE `+key+` = $1`, u)
		if n := w.count(`SELECT count(*) FROM `+table+` WHERE `+key+` = $1`, u); n != 0 {
			t.Errorf("%s: %d rows left after the delete", table, n)
		}
	}
}

// TestSchema_ProfileAccessGrantConstraints (02 §9.7): the company is bound to
// org kind 'company', a source request has at most one grant, and a
// revocation carries its kind.
func TestSchema_ProfileAccessGrantConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	student := w.student()
	company := w.org("company")
	uni, _ := w.university()
	club := w.club(uni)
	req := uuid.NewV7()
	base := map[string]string{"student_user_id": "$1", "company_org_id": "$2", "source": "'contact_request'", "source_id": "$3"}
	g := func(name string, over map[string]string, args []any, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("profile_access_grants", base, over), args, state, constraint}
	}
	ok := []any{student, company, req}
	expectFail(t, w.d.Pool, []p1aCase{
		g("a club cannot be granted access", nil, []any{student, club, req}, db.CodeForeignKeyViolation, "profile_access_grants_company_org_id_org_kind_fkey"),
		g("an unknown org cannot be granted access", nil, []any{student, uuid.NewV7(), req}, db.CodeForeignKeyViolation, "profile_access_grants_company_org_id_org_kind_fkey"),
		g("a non-company org kind", map[string]string{"org_kind": lit("club")}, []any{student, club, req}, db.CodeCheckViolation, "profile_access_grants_org_kind_chk"),
		g("unknown source", map[string]string{"source": lit("ad")}, ok, db.CodeCheckViolation, "profile_access_grants_source_chk"),
		g("unknown student", nil, []any{uuid.NewV7(), company, req}, db.CodeForeignKeyViolation, "profile_access_grants_student_user_id_fkey"),
		g("revoked without a kind", map[string]string{"revoked_at": "now()"}, ok, db.CodeCheckViolation, "profile_access_grants_revoked_chk"),
		g("a kind without a revocation", map[string]string{"revoked_by_kind": lit("student")}, ok, db.CodeCheckViolation, "profile_access_grants_revoked_chk"),
		g("unknown revoker kind", map[string]string{"revoked_at": "now()", "revoked_by_kind": lit("admin")}, ok, db.CodeCheckViolation, "profile_access_grants_revoked_by_kind_chk"),
		g("revoke reason of 301 characters", map[string]string{"revoked_at": "now()", "revoked_by_kind": lit("student"), "revoke_reason": lit(repeat("a", 301))}, ok, db.CodeCheckViolation, "profile_access_grants_revoke_reason_chk"),
	})
	expectOK(t, w.d.Pool, []p1aCase{g("grant", nil, ok, "", "")})
	expectFail(t, w.d.Pool, []p1aCase{
		g("a second grant for the same request", nil, []any{w.student(), company, req}, db.CodeUniqueViolation, "profile_access_grants_source_uq"),
	})
	// The same id under the other source is another request.
	expectOK(t, w.d.Pool, []p1aCase{
		g("same id, other source", map[string]string{"source": lit("application")}, ok, "", ""),
		g("revoked grant", map[string]string{"revoked_at": "now()", "revoked_by_kind": lit("system"), "revoke_reason": lit("expired")}, []any{student, company, uuid.NewV7()}, "", ""),
	})
	// Retention: a revoked grant is hard-deleted.
	w.exec(`DELETE FROM profile_access_grants WHERE student_user_id = $1 AND revoked_at IS NOT NULL`, student)
	if n := w.count(`SELECT count(*) FROM profile_access_grants WHERE student_user_id = $1`, student); n != 2 {
		t.Errorf("grants left = %d, want 2", n)
	}
}
