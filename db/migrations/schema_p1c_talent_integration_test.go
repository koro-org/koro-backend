//go:build integration

package migrations_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// talentTables lists the tables of the talent schema (000079–000087) with
// the privileges app_talent holds on them: the ledger and the config
// versions are append-only (S+I), the signal catalogue is retired, not
// deleted (S+I+U), the rest are hard-delete tables.
var talentTables = map[string]string{
	"config_versions": "SI",
	"signal_defs":     "SIU",
	"point_events":    "SI",
	"daily_counters":  "SIUD",
	"cohorts":         "SIUD",
	"scores":          "SIUD",
	"score_runs":      "SIUD",
	"neutralizations": "SIUD",
}

// TestRoleBoundaries_AppRwCannotReadTalent (06 H-3, 02 §0.3, 05 §15.4): the
// application role reaches no talent table, not even with the table name
// in hand: every statement fails with permission denied (42501) on the
// schema. The check runs over every table that exists in the schema, so a
// later talent table is covered without editing the test.
func TestRoleBoundaries_AppRwCannotReadTalent(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	rows, err := owner.Query(t.Context(), `SELECT tablename FROM pg_tables WHERE schemaname = 'talent' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(talentTables))
	for name := range talentTables {
		want = append(want, name)
	}
	slices.Sort(want)
	if !slices.Equal(tables, want) {
		t.Fatalf("talent tables = %v, want %v", tables, want)
	}

	const denied = db.CodeInsufficientPriv
	user := w.user()
	for _, table := range tables {
		q := "talent." + table
		for _, sql := range []string{
			`SELECT * FROM ` + q,
			`SELECT count(*) FROM ` + q,
			`DELETE FROM ` + q,
			`TRUNCATE ` + q,
			`UPDATE ` + q + ` SET user_id = user_id`,
			`INSERT INTO ` + q + ` DEFAULT VALUES`,
			`LOCK TABLE ` + q + ` IN ACCESS SHARE MODE`,
		} {
			inTx(t, w.d, func(tx pgx.Tx) {
				if got := execState(t, tx, sql); got != denied {
					t.Errorf("app_rw %q: SQLSTATE %q, want %s", sql, got, denied)
				}
			})
		}
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
			for _, role := range []string{"app_rw", "app_ro", "audit_writer", "audit_chainer"} {
				if got := talentPrivilege(t, w, role, table, priv); got != "false" {
					t.Errorf("%s has %s on %s", role, priv, q)
				}
			}
		}
		if got := queryString(t, w.d, `SELECT count(*)::text FROM pg_class c, aclexplode(c.relacl) a
			WHERE c.relnamespace = 'talent'::regnamespace AND c.relname = $1 AND a.grantee = 0`, table); got != "0" {
			t.Errorf("PUBLIC holds privileges on %s", q)
		}
	}
	// Neither the schema nor the catalogue lets app_rw in: no USAGE, no
	// CREATE, and the NOINHERIT membership gives nothing without SET ROLE.
	for sql, want := range map[string]string{
		`SELECT has_schema_privilege('app_rw', 'talent', 'USAGE')::text`:  "false",
		`SELECT has_schema_privilege('app_rw', 'talent', 'CREATE')::text`: "false",
		`SELECT has_schema_privilege('app_ro', 'talent', 'USAGE')::text`:  "false",
		`SELECT has_schema_privilege('public', 'talent', 'USAGE')::text`:  "false",
		`SELECT pg_has_role('app_rw', 'app_talent', 'USAGE')::text`:       "false",
	} {
		if got := queryString(t, w.d, sql); got != want {
			t.Errorf("%s = %s, want %s", sql, got, want)
		}
	}
	// Reading through a public object does not help either: a view owned by
	// app_rw cannot be created (no CREATE on public), and the planner of
	// app_rw cannot even resolve a talent column.
	if got := execState(t, w.d.Pool, `CREATE VIEW public.leak AS SELECT * FROM talent.scores`); got != denied {
		t.Errorf("app_rw view over talent: SQLSTATE %q, want %s", got, denied)
	}
	if got := execState(t, w.d.Pool, `SELECT band FROM talent.scores WHERE user_id = $1`, user); got != denied {
		t.Errorf("app_rw score lookup: SQLSTATE %q, want %s", got, denied)
	}
	// The ledger is empty: the flag talent.default_enabled is false and no
	// code writes it (T-32); the migrations write only the signal catalogue.
	for _, table := range tables {
		var n int
		if err := owner.QueryRow(t.Context(), `SELECT count(*) FROM talent.`+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if table == "signal_defs" {
			if n != 10 {
				t.Errorf("talent.signal_defs rows = %d, want the 10 seeded definitions", n)
			}
		} else if n != 0 {
			t.Errorf("talent.%s has %d rows after the migrations, want 0 (nothing is written while talent.default_enabled = false)", table, n)
		}
	}
}

// TestRoleBoundaries_AppTalentCanReadTalent (05 §15.4): inside
// SET LOCAL ROLE app_talent the talent store reads and writes its tables
// with exactly the privileges of talentTables; the role ends with the
// transaction, and app_talent sees nothing of the public tables.
func TestRoleBoundaries_AppTalentCanReadTalent(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	user := w.student()
	cfg := w.configVersion(w.user())
	pointID := uuid.NewV7()
	w.talentExec(pointSQL(nil), user, cfg, pointID, uuid.NewV7())
	w.talentExec(`INSERT INTO talent.cohorts (user_id, cohort_key) VALUES ($1, 'global')`, user)
	w.talentExec(`INSERT INTO talent.scores (user_id, raw_cp, pct_final, band, n_signals, cohort_key, config_version_id, run_id, calculated_at)
		VALUES ($1, 500, 0.5000, 1, 1, 'global', $2, gen_random_uuid(), now())`, user, cfg)

	if err := w.asTalent(func(tx pgx.Tx) error {
		var who string
		var points, band int
		if err := tx.QueryRow(t.Context(), `SELECT current_user`).Scan(&who); err != nil {
			return err
		}
		if who != "app_talent" {
			t.Errorf("current_user = %s, want app_talent", who)
		}
		if err := tx.QueryRow(t.Context(), `SELECT points_cp FROM talent.point_events WHERE id = $1`, pointID).Scan(&points); err != nil {
			return err
		}
		if err := tx.QueryRow(t.Context(), `SELECT band FROM talent.scores WHERE user_id = $1`, user).Scan(&band); err != nil {
			return err
		}
		if points != 500 || band != 1 {
			t.Errorf("read back points %d band %d", points, band)
		}
		var defs int
		if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM talent.signal_defs WHERE enabled`).Scan(&defs); err != nil {
			return err
		}
		if defs != 9 {
			t.Errorf("enabled signal definitions = %d, want 9", defs)
		}
		return nil
	}); err != nil {
		t.Fatalf("app_talent read: %v", err)
	}

	// The privileges inside the role, table by table.
	privs := map[byte]string{'S': "SELECT", 'I': "INSERT", 'U': "UPDATE", 'D': "DELETE"}
	for table, want := range talentTables {
		for letter, priv := range privs {
			got := talentPrivilege(t, w, "app_talent", table, priv)
			if wantIt := strings.IndexByte(want, letter) >= 0; got != strconv.FormatBool(wantIt) {
				t.Errorf("app_talent %s on talent.%s = %s, want %v", priv, table, got, wantIt)
			}
		}
		for _, priv := range []string{"TRUNCATE", "REFERENCES", "TRIGGER"} {
			if got := talentPrivilege(t, w, "app_talent", table, priv); got != "false" {
				t.Errorf("app_talent has %s on talent.%s", priv, table)
			}
		}
		if got := queryString(t, w.d, `SELECT tableowner FROM pg_tables WHERE schemaname = 'talent' AND tablename = $1`, table); got != "app_owner" {
			t.Errorf("talent.%s is owned by %q, want app_owner", table, got)
		}
	}
	// app_talent is a talent-only role: no public table is readable inside it.
	const denied = db.CodeInsufficientPriv
	for _, sql := range []string{`SELECT * FROM users`, `SELECT * FROM student_privacy_settings`, `SELECT * FROM posts`, `SELECT * FROM audit_events`} {
		state, _ := w.talentState(sql)
		if state != denied {
			t.Errorf("app_talent %q: SQLSTATE %q, want %s", sql, state, denied)
		}
	}
	// SET LOCAL ends with the transaction: the next statement of the pool is
	// app_rw again and denied.
	if got := execState(t, w.d.Pool, `SELECT * FROM talent.point_events`); got != denied {
		t.Errorf("app_rw after a talent transaction: SQLSTATE %q, want %s", got, denied)
	}
	// app_ro (Metabase) cannot switch into app_talent.
	if got := queryString(t, w.d, `SELECT pg_has_role('app_ro', 'app_talent', 'MEMBER')::text`); got != "false" {
		t.Errorf("app_ro is a member of app_talent")
	}
}

// TestSchema_TalentLedgerAppendOnly (02 §15.3, §0.5): the ledger takes
// inserts only (no UPDATE/DELETE for app_talent; koro_forbid_mutation for
// everyone); one row per (user, signal, source); at most one reversal per
// row; the reversal and cap shapes; the config versions are immutable.
func TestSchema_TalentLedgerAppendOnly(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	user, admin := w.student(), w.user()
	cfg := w.configVersion(admin)
	id, source := uuid.NewV7(), uuid.NewV7()
	w.talentExec(pointSQL(nil), user, cfg, id, source)
	reversal := uuid.NewV7()
	w.talentExec(pointSQL(map[string]string{"source_type": "'reversal'", "reversal_of": "$4", "source_id": "$4", "points_cp": "-500", "raw_points_cp": "-500", "reason_code": "'registration_cancelled'"}),
		user, cfg, reversal, id)

	// Layer 1: no UPDATE/DELETE/TRUNCATE for app_talent.
	denied := db.CodeInsufficientPriv
	expectTalentFail(t, w, []p1aCase{
		{"app_talent update", `UPDATE talent.point_events SET points_cp = 0 WHERE id = $1`, []any{id}, denied, ""},
		{"app_talent delete", `DELETE FROM talent.point_events WHERE id = $1`, []any{id}, denied, ""},
		{"app_talent truncate", `TRUNCATE talent.point_events`, nil, denied, ""},
		{"app_talent edits a config version", `UPDATE talent.config_versions SET notes = 'x' WHERE id = $1`, []any{cfg}, denied, ""},
		{"app_talent deletes a signal", `DELETE FROM talent.signal_defs WHERE code = 'feed_like'`, nil, denied, ""},
	})
	// Layer 2: the triggers hold against the owner.
	owner := testutil.OwnerConn(t, w.d)
	expectFail(t, owner, []p1aCase{
		{"owner update", `UPDATE talent.point_events SET points_cp = 0 WHERE id = $1`, []any{id}, db.CodeRestrictViolation, ""},
		{"owner delete", `DELETE FROM talent.point_events WHERE id = $1`, []any{reversal}, db.CodeRestrictViolation, ""},
		{"owner truncate", `TRUNCATE talent.point_events`, nil, db.CodeRestrictViolation, ""},
		{"owner edits a config version", `UPDATE talent.config_versions SET notes = 'x' WHERE id = $1`, []any{cfg}, db.CodeRestrictViolation, ""},
		{"owner deletes a config version", `DELETE FROM talent.config_versions WHERE id = $1`, []any{cfg}, db.CodeRestrictViolation, ""},
		// point_events, scores and score_runs refer to the versions; naming them all reaches the trigger.
		{"owner truncates the config versions", `TRUNCATE talent.config_versions, talent.point_events, talent.scores, talent.score_runs`, nil, db.CodeRestrictViolation, ""},
	})

	chk := db.CodeCheckViolation
	expectTalentFail(t, w, []p1aCase{
		{"the same source twice", pointSQL(nil), []any{user, cfg, uuid.NewV7(), source}, db.CodeUniqueViolation, "point_events_dedupe_uq"},
		{"a second reversal", pointSQL(map[string]string{"source_type": "'reversal'", "reversal_of": "$4", "source_id": "gen_random_uuid()", "points_cp": "-500"}), []any{user, cfg, uuid.NewV7(), id}, db.CodeUniqueViolation, "point_events_reversal_uq"},
		{"a reversal type without the original", pointSQL(map[string]string{"source_type": "'reversal'"}), []any{user, cfg, uuid.NewV7(), uuid.NewV7()}, chk, "point_events_reversal_chk"},
		{"a reversal link on a normal row", pointSQL(map[string]string{"reversal_of": lit(id.String())}), []any{user, cfg, uuid.NewV7(), uuid.NewV7()}, chk, "point_events_reversal_chk"},
		{"capped without a reason", pointSQL(map[string]string{"capped": "true"}), []any{user, cfg, uuid.NewV7(), uuid.NewV7()}, chk, "point_events_capped_chk"},
		{"a cap reason on an uncapped row", pointSQL(map[string]string{"cap_reason": "'daily'"}), []any{user, cfg, uuid.NewV7(), uuid.NewV7()}, chk, "point_events_capped_chk"},
		{"unknown cap reason", pointSQL(map[string]string{"capped": "true", "cap_reason": "'monthly'"}), []any{user, cfg, uuid.NewV7(), uuid.NewV7()}, chk, "point_events_cap_reason_chk"},
		{"source type with a capital", pointSQL(map[string]string{"source_type": "'Checkin'"}), []any{user, cfg, uuid.NewV7(), uuid.NewV7()}, chk, "point_events_source_type_chk"},
		{"reason code of 2 characters", pointSQL(map[string]string{"reason_code": "'ab'"}), []any{user, cfg, uuid.NewV7(), uuid.NewV7()}, chk, "point_events_reason_code_chk"},
		{"unknown signal", pointSQL(map[string]string{"signal_code": "'mystery_signal'"}), []any{user, cfg, uuid.NewV7(), uuid.NewV7()}, db.CodeForeignKeyViolation, "point_events_signal_code_fkey"},
		{"unknown config version", pointSQL(nil), []any{user, uuid.NewV7(), uuid.NewV7(), uuid.NewV7()}, db.CodeForeignKeyViolation, "point_events_config_version_id_fkey"},
		{"unknown user", pointSQL(nil), []any{uuid.NewV7(), cfg, uuid.NewV7(), uuid.NewV7()}, db.CodeForeignKeyViolation, "point_events_user_id_fkey"},
		{"reversal of an unknown row", pointSQL(map[string]string{"source_type": "'reversal'", "reversal_of": "$4"}), []any{user, cfg, uuid.NewV7(), uuid.NewV7()}, db.CodeForeignKeyViolation, "point_events_reversal_of_fkey"},
		{"two config versions at the same instant", `INSERT INTO talent.config_versions (effective_from, params, created_by) SELECT effective_from, '{}', created_by FROM talent.config_versions WHERE id = $1`, []any{cfg}, db.CodeUniqueViolation, "config_versions_effective_from_uq"},
	})
	if got := queryStrings(t, w, `SELECT c.relname || '.' || g.tgname FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid
		WHERE c.relnamespace = 'talent'::regnamespace AND c.relname IN ('point_events', 'config_versions') AND NOT g.tgisinternal ORDER BY 1`); !slices.Equal(got, []string{
		"config_versions.config_versions_immutable", "config_versions.config_versions_no_truncate",
		"point_events.point_events_immutable", "point_events.point_events_no_truncate",
	}) {
		t.Errorf("immutability triggers = %v", got)
	}
}

// TestSchema_TalentTableConstraints (02 §15.2, §15.4–§15.8).
func TestSchema_TalentTableConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	user, admin := w.student(), w.user()
	cfg := w.configVersion(admin)
	chk := db.CodeCheckViolation
	signal := `INSERT INTO talent.signal_defs (code, category, description, base_points_cp, daily_cap_cp, weekly_cap_cp, half_life_days) VALUES ($1, $2, 'x', 10, $3, $4, $5)`
	score := `INSERT INTO talent.scores (user_id, raw_cp, pct_final, band, n_signals, cohort_key, config_version_id, run_id, neutralized, calculated_at)
		VALUES ($1, $2, $3, $4, 0, 'global', $5, gen_random_uuid(), $6, now())`
	neutral := `INSERT INTO talent.neutralizations (user_id, reason, dsr_id, created_by) VALUES ($1, $2, $3, $4)`
	expectTalentFail(t, w, []p1aCase{
		{"signal code with a digit", signal, []any{"signal_1", "profile", nil, nil, nil}, chk, "signal_defs_code_chk"},
		{"unknown category", signal, []any{"fame_signal", "fame", nil, nil, nil}, chk, "signal_defs_category_chk"},
		{"a daily cap of 0", signal, []any{"new_signal", "profile", 0, nil, nil}, chk, "signal_defs_daily_cap_cp_chk"},
		{"a weekly cap of 0", signal, []any{"new_signal", "profile", nil, 0, nil}, chk, "signal_defs_weekly_cap_cp_chk"},
		{"a half-life of 0 days", signal, []any{"new_signal", "profile", nil, nil, 0}, chk, "signal_defs_half_life_days_chk"},
		{"an existing code", signal, []any{"feed_like", "engagement", nil, nil, nil}, db.CodeUniqueViolation, "signal_defs_pkey"},
		{"a cohort key that is not a university or global", `INSERT INTO talent.cohorts (user_id, cohort_key) VALUES ($1, 'city:ankara')`, []any{user}, chk, "cohorts_cohort_key_chk"},
		{"a cohort with class year 9", `INSERT INTO talent.cohorts (user_id, cohort_key) VALUES ($1, 'uni:01990000-0000-7000-8000-000000000001:year:9')`, []any{user}, chk, "cohorts_cohort_key_chk"},
		{"negative raw score", score, []any{user, -1, 0.5, 0, cfg, false}, chk, "scores_raw_cp_chk"},
		{"a percentile above 1", score, []any{user, 0, 1.5, 0, cfg, false}, chk, "scores_pct_final_chk"},
		{"band 4", score, []any{user, 0, 0.5, 4, cfg, false}, chk, "scores_band_chk"},
		{"a neutralised user above band 0", score, []any{user, 900, 0.95, 3, cfg, true}, chk, "scores_neutralized_chk"},
		{"an objection without its request", neutral, []any{user, "objection", nil, admin}, chk, "neutralizations_objection_dsr_chk"},
		{"unknown neutralisation reason", neutral, []any{user, "bored", nil, admin}, chk, "neutralizations_reason_chk"},
		{"unknown run status", `INSERT INTO talent.score_runs (config_version_id, status) VALUES ($1, 'paused')`, []any{cfg}, chk, "score_runs_status_chk"},
		{"a counter of an unknown signal", `INSERT INTO talent.daily_counters (user_id, day, signal_code, applied_cp) VALUES ($1, current_date, 'nope_signal', 10)`, []any{user}, db.CodeForeignKeyViolation, "daily_counters_signal_code_fkey"},
	})
	w.talentExec(neutral, user, "opt_out", nil, user)
	expectTalentFail(t, w, []p1aCase{
		{"a second active opt-out", neutral, []any{user, "opt_out", nil, user}, db.CodeUniqueViolation, "neutralizations_active_uq"},
	})
	w.talentExec(`UPDATE talent.neutralizations SET lifted_at = now(), lifted_by = $1 WHERE user_id = $1 AND lifted_at IS NULL`, user)
	w.talentExec(neutral, user, "opt_out", nil, user) // a new opt-out after the lift
	w.talentExec(neutral, user, "objection", uuid.NewV7(), admin)
	w.talentExec(score, user, 900, 0.95, 0, cfg, true)
}

// TestSchema_TalentDailyCapUnderConcurrency (02 §15.4): the conditional
// upsert of the daily counter never passes the cap, even when many
// consumers add points for one user, day and signal at the same time.
func TestSchema_TalentDailyCapUnderConcurrency(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	user := w.student()
	const add, limit = 10, 50 // feed_like: 10 cp, 50 cp a day
	upsert := `INSERT INTO talent.daily_counters AS c (user_id, day, signal_code, applied_cp) VALUES ($1, DATE '2026-10-03', 'feed_like', $2)
		ON CONFLICT (user_id, day, signal_code) DO UPDATE SET applied_cp = c.applied_cp + $2 WHERE c.applied_cp + $2 <= $3
		RETURNING applied_cp`
	var wg sync.WaitGroup
	applied := make([]bool, 20)
	for i := range applied {
		wg.Go(func() {
			err := w.asTalent(func(tx pgx.Tx) error {
				var v int
				err := tx.QueryRow(t.Context(), upsert, user, add, limit).Scan(&v)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil // over the cap: no row
				}
				if err == nil {
					applied[i] = true
				}
				return err
			})
			if err != nil {
				t.Errorf("concurrent upsert: %v", err)
			}
		})
	}
	wg.Wait()
	if n := len(slices.DeleteFunc(applied, func(b bool) bool { return !b })); n != limit/add {
		t.Errorf("applied additions = %d, want %d", n, limit/add)
	}
	owner := testutil.OwnerConn(t, w.d)
	var total int
	if err := owner.QueryRow(t.Context(), `SELECT applied_cp FROM talent.daily_counters WHERE user_id = $1`, user).Scan(&total); err != nil || total != limit {
		t.Errorf("counter = %d, %v; want exactly the cap %d", total, err, limit)
	}
}

// TestSchema_SeedTalentSignalDefs (02 §15.2, R-003 §3.3): the starting
// catalogue with its weights, caps and half-lives; opportunity_application
// is off (phase 3); re-running the seed changes nothing.
func TestSchema_SeedTalentSignalDefs(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	rows, err := owner.Query(t.Context(), `SELECT code || ' ' || category || ' ' || base_points_cp || ' ' || coalesce(daily_cap_cp::text, '-') || ' ' ||
		coalesce(weekly_cap_cp::text, '-') || ' ' || coalesce(half_life_days::text, '-') || ' ' || enabled || ' ' || excluded_when_sensitive
		FROM talent.signal_defs ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"club_membership_active community 200 - - - true true",
		"event_checkin participation 500 1000 - 120 true true",
		"event_no_show participation -200 - 400 60 true true",
		"event_registration_confirmed participation 100 300 500 45 true true",
		"feed_like engagement 10 50 - 30 true true",
		"feed_save engagement 10 50 - 30 true true",
		"login_active_day engagement 20 20 100 30 true true",
		"opportunity_application intent 300 900 - 90 false true",
		"profile_refresh profile 200 - - 365 true true",
		"profile_section_complete profile 200 - - - true true",
	}
	if !slices.Equal(got, want) {
		t.Errorf("signal catalogue:\n got %v\nwant %v", got, want)
	}
	// A tuned row survives a re-run of the seed (ON CONFLICT DO NOTHING).
	if _, err := owner.Exec(t.Context(), `UPDATE talent.signal_defs SET base_points_cp = 400 WHERE code = 'event_checkin'`); err != nil {
		t.Fatal(err)
	}
	src := mustRead(t, "000081_seed_talent_signal_defs.up.sql")
	if _, err := owner.Exec(t.Context(), src); err != nil {
		t.Fatalf("re-running the seed: %v", err)
	}
	var points, n int
	if err := owner.QueryRow(t.Context(), `SELECT (SELECT base_points_cp FROM talent.signal_defs WHERE code = 'event_checkin'), (SELECT count(*) FROM talent.signal_defs)`).Scan(&points, &n); err != nil || points != 400 || n != 10 {
		t.Errorf("after a re-run: event_checkin %d, rows %d, %v; want 400 and 10", points, n, err)
	}
}
