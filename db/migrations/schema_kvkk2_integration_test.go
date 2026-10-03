//go:build integration

package migrations_test

import (
	"context"
	"io/fs"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/db/migrations"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// Tests of 000121 (BE-049, kvkk-2): the SECURITY DEFINER purge functions of
// the append-only tables (06 H-34, H-41; T-120, T-153), the retention policy
// alignment (T-121, 06 H-48) and the analytics views (01 §1.4, k ≥ 10).

// migrationVersions lists the migration numbers that exist, ascending.
// Numbers are pre-allocated per ticket (BE-001 R2), so before the pilot the
// list may have gaps (000121 can merge before 000094–000120).
func migrationVersions(t *testing.T) []uint {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]uint, 0, len(names))
	for _, n := range names {
		v, err := strconv.ParseUint(n[:6], 10, 32)
		if err != nil {
			t.Fatalf("migration file %q: %v", n, err)
		}
		out = append(out, uint(v))
	}
	slices.Sort(out)
	if len(out) < 2 {
		t.Fatalf("only %d migrations", len(out))
	}
	return out
}

// approve marks policies as enabled and legally approved (a real purge
// needs both).
func (w *p1aWorld) approve(codes ...string) {
	w.t.Helper()
	w.exec(`UPDATE retention_policies SET legal_approved = true, approved_ref = 'Hukuk görüşü 2026/21', enabled = true WHERE code = ANY ($1)`,
		db.TextArray(codes))
}

// purge calls a purge function as app_rw and returns its result or SQLSTATE.
func (w *p1aWorld) purge(fn string, limit int, dryRun bool) (int64, string) {
	w.t.Helper()
	var n int64
	err := w.d.Pool.QueryRow(w.t.Context(), `SELECT `+fn+`(now(), $1, $2)`, limit, dryRun).Scan(&n)
	return n, db.SQLState(err)
}

// talentPurge calls a talent purge function as app_talent.
func (w *p1aWorld) talentPurge(sql string, args ...any) (int64, string) {
	w.t.Helper()
	var n int64
	err := w.asTalent(func(tx pgx.Tx) error { return tx.QueryRow(w.t.Context(), sql, args...).Scan(&n) })
	return n, db.SQLState(err)
}

// consent inserts one consent row of user for purpose at occurredAgo
// (an interval text) and returns its id.
func (w *p1aWorld) consent(user uuid.UUID, purpose, action, occurredAgo string, notice uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	if action == "granted" {
		w.exec(`INSERT INTO consents (id, user_id, purpose_code, action, notice_version_id, text_sha256, channel, occurred_at)
			VALUES ($1, $2, $3, 'granted', $4, $5, 'web', now() - $6::interval)`, id, user, purpose, notice, sha("Metin"), occurredAgo)
		return id
	}
	w.exec(`INSERT INTO consents (id, user_id, purpose_code, action, channel, occurred_at)
		VALUES ($1, $2, $3, 'withdrawn', 'web', now() - $4::interval)`, id, user, purpose, occurredAgo)
	return id
}

// TestSchema_ConsentsPurge (T-120, H-34): consent chains go 3 years after
// the withdrawal or the erasure, together with their İYS submissions;
// a granted chain and a recent withdrawal stay. Without the legal approval
// only the dry run works.
func TestSchema_ConsentsPurge(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	notice := w.notice("commercial_email", "2026.10.03")
	old, recent, live, erased := w.user(), w.user(), w.user(), w.user()
	oldGrant := w.consent(old, "commercial_email", "granted", "5 years", notice)
	w.exec(`INSERT INTO iys_submissions (consent_id, channel) VALUES ($1, 'email')`, oldGrant)
	w.consent(old, "commercial_email", "withdrawn", "4 years", uuid.Nil())
	w.consent(recent, "commercial_email", "granted", "5 years", notice)
	w.consent(recent, "commercial_email", "withdrawn", "1 year", uuid.Nil())
	w.consent(live, "commercial_email", "withdrawn", "5 years", uuid.Nil())
	w.consent(live, "commercial_email", "granted", "4 years", notice) // granted again: in force
	w.consent(erased, "commercial_email", "granted", "5 years", notice)
	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), `UPDATE users SET status = 'erased', email = NULL, email_normalized = NULL, password_hash = NULL,
		first_name = NULL, last_name = NULL, birth_date = NULL, erased_at = now() - interval '4 years' WHERE id = $1`, erased); err != nil {
		t.Fatal(err)
	}

	// Not approved yet (T-35): count only.
	if n, state := w.purge("consents_purge", 1000, true); state != "" || n != 3 {
		t.Fatalf("dry run = %d, %q; want 3 (old chain 2 + erased 1)", n, state)
	}
	if _, state := w.purge("consents_purge", 1000, false); state != "55000" {
		t.Fatalf("purge before the legal approval: SQLSTATE %q, want 55000", state)
	}
	if n := w.count(`SELECT count(*) FROM consents`); n != 7 {
		t.Fatalf("consents after the refused purge = %d, want 7", n)
	}
	for _, bad := range []int{0, 1001} {
		if _, state := w.purge("consents_purge", bad, true); state != "22023" {
			t.Errorf("batch size %d: SQLSTATE %q, want 22023", bad, state)
		}
	}

	w.approve("consent_proof")
	if n, state := w.purge("consents_purge", 1, false); state != "" || n != 1 {
		t.Fatalf("first batch = %d, %q; want 1", n, state)
	}
	// Oldest first: the old chain keeps its newest row, still withdrawn.
	if got := queryString(t, w.d, `SELECT action FROM consent_current WHERE user_id = $1`, old); got != "withdrawn" {
		t.Fatalf("state of a chain cut by the batch = %s, want withdrawn", got)
	}
	if n, state := w.purge("consents_purge", 1000, false); state != "" || n != 2 {
		t.Fatalf("second batch = %d, %q; want 2", n, state)
	}
	if n, _ := w.purge("consents_purge", 1000, false); n != 0 {
		t.Fatalf("a re-run deleted %d rows", n)
	}
	for user, want := range map[uuid.UUID]int{old: 0, erased: 0, recent: 2, live: 2} {
		if n := w.count(`SELECT count(*) FROM consents WHERE user_id = $1`, user); n != want {
			t.Errorf("consents of a user after the purge = %d, want %d", n, want)
		}
	}
	if n := w.count(`SELECT count(*) FROM iys_submissions`); n != 0 {
		t.Errorf("İYS submissions of purged consents = %d, want 0", n)
	}
	// The guard is still closed outside the function, for everyone.
	expectFail(t, w.d.Pool, []p1aCase{{"app_rw delete", `DELETE FROM consents WHERE user_id = $1`, []any{live}, db.CodeInsufficientPriv, ""}})
	expectFail(t, owner, []p1aCase{
		{"owner delete", `DELETE FROM consents WHERE user_id = $1`, []any{live}, db.CodeRestrictViolation, ""},
		{"owner delete with another table's flag", `DO $$ BEGIN PERFORM set_config('koro.retention_purge', 'public.moderation_actions', true);
			DELETE FROM consents; END $$`, nil, db.CodeRestrictViolation, ""},
		{"owner update with the flag", `DO $$ BEGIN PERFORM set_config('koro.retention_purge', 'public.consents', true);
			UPDATE consents SET channel = 'web'; END $$`, nil, db.CodeRestrictViolation, ""},
		{"owner truncate with the flag", `DO $$ BEGIN PERFORM set_config('koro.retention_purge', 'public.consents', true);
			TRUNCATE iys_submissions, consents; END $$`, nil, db.CodeRestrictViolation, ""},
	})
	inTx(t, w.d, func(tx pgx.Tx) {
		var n int64
		if err := tx.QueryRow(t.Context(), `SELECT consents_purge(now(), 10, false)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		var flag string
		if err := tx.QueryRow(t.Context(), `SELECT coalesce(current_setting('koro.retention_purge', true), '')`).Scan(&flag); err != nil || flag != "" {
			t.Fatalf("purge flag leaked out of the function: %q %v", flag, err)
		}
	})
}

// TestSchema_ModerationAndDestructionLogPurge (T-153, H-41): the rows older
// than the policy go in batches; younger ones and the caller's later
// cut-off never widen the window.
func TestSchema_ModerationAndDestructionLogPurge(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	mod := w.user()
	for _, ago := range []string{"3 years", "3 years", "1 year"} {
		w.exec(`INSERT INTO moderation_actions (target_type, target_id, action, reason_code, actor_user_id, created_at)
			VALUES ('post', gen_random_uuid(), 'remove', 'spam', $1, now() - $2::interval)`, mod, ago)
		w.exec(`INSERT INTO destruction_logs (policy_code, run_id, module, target, method, record_count, period_cutoff, started_at, finished_at, created_at)
			VALUES ('notification_delivery', gen_random_uuid(), 'notify', 'notification_deliveries', 'hard_delete', 1, now(), now(), now(), now() - $1::interval)`,
			strings.Replace(ago, "3 years", "4 years", 1))
	}
	for fn, code := range map[string]string{"moderation_actions_purge": "moderation_action", "destruction_logs_purge": "destruction_log"} {
		if _, state := w.purge(fn, 100, false); state != "55000" {
			t.Errorf("%s before the approval: SQLSTATE %q, want 55000", fn, state)
		}
		if n, _ := w.purge(fn, 100, true); n != 2 {
			t.Errorf("%s dry run = %d, want 2", fn, n)
		}
		w.approve(code)
		if n, state := w.purge(fn, 1, false); n != 1 || state != "" {
			t.Errorf("%s batch of 1 = %d, %q", fn, n, state)
		}
		if n, _ := w.purge(fn, 100, false); n != 1 {
			t.Errorf("%s second batch = %d, want 1", fn, n)
		}
		// A cut-off in the future is clamped to now() - retention.
		var n int64
		if err := w.d.Pool.QueryRow(t.Context(), `SELECT `+fn+`(now() + interval '10 years', 100, false)`).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s with a future cut-off = %d, %v; want 0", fn, n, err)
		}
	}
	if n := w.count(`SELECT count(*) FROM moderation_actions`); n != 1 {
		t.Errorf("moderation actions left = %d, want 1", n)
	}
	if n := w.count(`SELECT count(*) FROM destruction_logs`); n != 1 {
		t.Errorf("destruction logs left = %d, want 1", n)
	}
	owner := testutil.OwnerConn(t, w.d)
	for _, fn := range []string{"public.consents_purge(timestamptz, integer, boolean)", "public.moderation_actions_purge(timestamptz, integer, boolean)",
		"public.destruction_logs_purge(timestamptz, integer, boolean)", "talent.point_events_purge(timestamptz, integer, boolean)",
		"talent.point_events_erase_user(uuid)", "talent.config_versions_purge(timestamptz, integer, boolean)"} {
		// Resolved as the owner: app_rw may not even look into the talent schema.
		one := func(sql string, args ...any) string {
			t.Helper()
			var s string
			if err := owner.QueryRow(t.Context(), sql, args...).Scan(&s); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			return s
		}
		if got := one(`SELECT prosecdef::text || ' ' || proowner::regrole::text FROM pg_proc WHERE oid = $1::regprocedure`, fn); got != "true app_owner" {
			t.Errorf("%s: SECURITY DEFINER and owner = %q, want true app_owner", fn, got)
		}
		if got := one(`SELECT count(*)::text FROM pg_proc p, aclexplode(p.proacl) a WHERE p.oid = $1::regprocedure AND a.grantee = 0`, fn); got != "0" {
			t.Errorf("PUBLIC may execute %s", fn)
		}
		talent := strings.HasPrefix(fn, "talent.")
		for role, want := range map[string]bool{"app_rw": !talent, "app_talent": talent, "app_ro": false} {
			if got := one(`SELECT has_function_privilege($1, $2, 'EXECUTE')::text`, role, fn); got != strconv.FormatBool(want) {
				t.Errorf("EXECUTE on %s for %s = %s, want %v", fn, role, got, want)
			}
		}
	}
}

// TestSchema_TalentPurge (T-153): old reversals go before their originals,
// an original with a young reversal stays, an erased account's ledger goes
// only after its erasure request was completed, and a superseded config
// version goes once nothing refers to it.
func TestSchema_TalentPurge(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	admin, student, other := w.user(), w.student(), w.student()
	oldCfg, newCfg := uuid.NewV7(), uuid.NewV7()
	w.talentExec(`INSERT INTO talent.config_versions (id, effective_from, params, created_by) VALUES
		($1, now() - interval '5 years', '{}', $3), ($2, now() - interval '3 years', '{}', $3)`, oldCfg, newCfg, admin)
	point := func(user, cfg uuid.UUID, ago string, reversalOf *uuid.UUID) uuid.UUID {
		id, src := uuid.NewV7(), uuid.NewV7()
		over := map[string]string{"created_at": "now() - " + lit(ago) + "::interval"}
		if reversalOf != nil {
			src = *reversalOf
			over["source_type"], over["reversal_of"], over["raw_points_cp"], over["points_cp"] = "'reversal'", lit(reversalOf.String()), "-500", "-500"
		}
		w.talentExec(pointSQL(over), user, cfg, id, src)
		return id
	}
	a := point(student, oldCfg, "4 years", nil)
	point(student, oldCfg, "4 years", &a) // old reversal of an old row
	b := point(student, newCfg, "4 years", nil)
	point(student, newCfg, "1 year", &b) // young reversal keeps b
	point(other, newCfg, "1 year", nil)

	purge := `SELECT talent.point_events_purge(now(), $1, $2)`
	if _, state := w.talentPurge(purge, 100, false); state != "55000" {
		t.Fatalf("purge before the approval: SQLSTATE %q, want 55000", state)
	}
	if n, _ := w.talentPurge(purge, 100, true); n != 2 {
		t.Fatalf("dry run = %d, want 2 (a and its reversal)", n)
	}
	if got := execState(t, w.d.Pool, `SELECT talent.point_events_purge(now(), 100, true)`); got != db.CodeInsufficientPriv {
		t.Errorf("app_rw calls the talent purge: SQLSTATE %q", got)
	}
	w.approve("talent_ledger")
	if n, state := w.talentPurge(purge, 100, false); n != 1 || state != "" {
		t.Fatalf("first batch = %d, %q; want 1 (the old reversal first)", n, state)
	}
	if n, _ := w.talentPurge(purge, 100, false); n != 1 {
		t.Fatalf("second batch = %d, want 1 (a)", n)
	}
	if n, _ := w.talentPurge(purge, 100, false); n != 0 {
		t.Fatalf("third batch = %d, want 0 (b has a young reversal)", n)
	}

	// The superseded old config version is no longer referenced: it goes;
	// the current one stays.
	cfgPurge := `SELECT talent.config_versions_purge(now(), $1, $2)`
	if n, _ := w.talentPurge(cfgPurge, 100, true); n != 1 {
		t.Fatalf("config dry run = %d, want 1", n)
	}
	if n, state := w.talentPurge(cfgPurge, 100, false); n != 1 || state != "" {
		t.Fatalf("config purge = %d, %q; want 1", n, state)
	}

	// Erasure: refused until the erasure request is completed.
	erase := `SELECT talent.point_events_erase_user($1)`
	if _, state := w.talentPurge(erase, student); state != "55000" {
		t.Fatalf("erase without a completed erasure request: SQLSTATE %q, want 55000", state)
	}
	w.exec(insertSQL("data_subject_requests", dsrDefaults, map[string]string{"type": "'erasure'", "status": "'completed'",
		"completed_at": "now()", "identity_verified_at": "now()"}), student)
	if n, state := w.talentPurge(erase, student); n != 2 || state != "" {
		t.Fatalf("erase = %d, %q; want 2 (b and its reversal together)", n, state)
	}
	var left int
	if err := w.asTalent(func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT count(*) FROM talent.point_events`).Scan(&left)
	}); err != nil || left != 1 {
		t.Fatalf("ledger rows left = %d, %v; want 1 (the other student's)", left, err)
	}
}

// TestSchema_RetentionPolicyAlignment (T-121, 06 H-48): role_history is
// anchored on revoked_at / rejected_at, the export file lives 7 days.
func TestSchema_RetentionPolicyAlignment(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	for code, want := range map[string]string{
		"role_history":    "3 years revoked_or_rejected",
		"dsr_export_file": "7 days created_at",
	} {
		if got := queryString(t, w.d, `SELECT retention::text || ' ' || anchor FROM retention_policies WHERE code = $1`, code); got != want {
			t.Errorf("%s = %q, want %q", code, got, want)
		}
	}
	expectFail(t, w.d.Pool, []p1aCase{{"unknown anchor", `UPDATE retention_policies SET anchor = 'deleted_at' WHERE code = 'role_history'`,
		nil, db.CodeCheckViolation, "retention_policies_anchor_chk"}})
}

// appRO connects as app_ro (Metabase).
func appRO(t *testing.T, w *p1aWorld) *pgx.Conn {
	t.Helper()
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
	return ro
}

// roRows returns the rows of sql read as app_ro, each as its columns joined
// by "|" (NULL as "-").
func roRows(t *testing.T, ro *pgx.Conn, sql string) []string {
	t.Helper()
	rows, err := ro.Query(t.Context(), sql)
	if err != nil {
		t.Fatalf("app_ro %s: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				parts[i] = "-"
			case string:
				parts[i] = x
			case int32:
				parts[i] = strconv.Itoa(int(x))
			case int16:
				parts[i] = strconv.Itoa(int(x))
			default:
				parts[i] = "?"
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAnalyticsViews_SuppressBelow10 (01 §1.4, R-003: k ≥ 10): a group of 9
// people is left out, a group of 10 is shown; sensitive clubs and events
// never appear; a check-in count under 10 is NULL; neutralised users are
// not in the talent distribution.
func TestAnalyticsViews_SuppressBelow10(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	uniA, _ := w.university()
	uniB, _ := w.university()
	w.exec(`UPDATE universities SET name = 'A Üniversitesi' WHERE id = $1`, uniA)
	w.exec(`UPDATE universities SET name = 'B Üniversitesi' WHERE id = $1`, uniB)
	creator := w.user()
	club := w.club(uniB)
	sensitiveClub := w.club(uniB)
	w.exec(`UPDATE clubs SET category = 'religious', is_sensitive = true, sensitive_reason = 'religion', sensitivity_set_at = now() WHERE org_id = $1`, sensitiveClub)
	event := w.event(club, creator, nil)
	small := w.event(club, creator, map[string]string{"category": "'talk'"})
	sensitive := w.event(sensitiveClub, creator, nil)
	cfg := uuid.NewV7()
	w.talentExec(`INSERT INTO talent.config_versions (id, effective_from, params, created_by) VALUES ($1, now() - interval '1 day', '{}', $2)`, cfg, creator)
	score := func(user uuid.UUID, band int, neutralized bool) {
		w.talentExec(`INSERT INTO talent.scores (user_id, raw_cp, pct_final, band, n_signals, cohort_key, config_version_id, run_id, calculated_at, neutralized)
			VALUES ($1, 100, 0.5, $2, 3, 'k', $3, gen_random_uuid(), now(), $4)`, user, band, cfg, neutralized)
	}
	for i := range 10 {
		s := w.student()
		w.profile(s, uniB, true, "all")
		over := map[string]string(nil)
		if i < 9 {
			over = map[string]string{"status": "'checked_in'", "checked_in_at": "now()"}
		}
		w.registration(event, club, s, over)
		w.registration(sensitive, sensitiveClub, s, nil)
		score(s, 1, false)
		if i < 9 {
			w.registration(small, club, s, nil)
			a := w.student()
			w.profile(a, uniA, true, "all")
			score(a, 1, false)
		}
	}
	for range 10 {
		s := w.student()
		w.profile(s, uniB, true, "all")
		score(s, 0, true) // neutralised (band 0): left out
	}
	ro := appRO(t, w)

	if got := roRows(t, ro, `SELECT university, degree_level, students FROM analytics.students_by_university ORDER BY university`); !slices.Equal(got, []string{"B Üniversitesi|bachelor|20"}) {
		t.Errorf("students_by_university = %v, want only B with 20 (A has 9)", got)
	}
	if got := roRows(t, ro, `SELECT org_kind, category, registrations, checked_in FROM analytics.event_participation_monthly`); !slices.Equal(got, []string{"club|workshop|10|-"}) {
		t.Errorf("event_participation_monthly = %v, want one club workshop row of 10 with checked_in NULL (9)", got)
	}
	if got := roRows(t, ro, `SELECT university, department, band, students FROM analytics.talent_distribution`); !slices.Equal(got, []string{"B Üniversitesi|-|1|10"}) {
		t.Errorf("talent_distribution = %v, want B band 1 = 10 (A has 9, neutralised users left out)", got)
	}
	got := roRows(t, ro, `SELECT persona, accounts FROM analytics.signups_monthly ORDER BY persona`)
	if !slices.Equal(got, []string{"student|29"}) {
		t.Errorf("signups_monthly = %v, want student|29 (the 1 company account is left out)", got)
	}
}

// TestAnalyticsViews_AppRwNoAccess_AppRoOnly (01 §1.4): only app_ro reads
// the analytics views; app_rw (the application) cannot; app_ro still reads
// no table; the views carry no personal column.
func TestAnalyticsViews_AppRwNoAccess_AppRoOnly(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	views := queryStrings(t, w, `SELECT table_name FROM information_schema.views WHERE table_schema = 'analytics' ORDER BY 1`)
	if len(views) != 0 {
		t.Fatalf("app_rw can list the analytics views: %v", views)
	}
	owner := testutil.OwnerConn(t, w.d)
	rows, err := owner.Query(t.Context(), `SELECT c.relname, c.relkind::text, pg_get_userbyid(c.relowner), coalesce(array_to_string(c.reloptions, ','), '')
		FROM pg_class c WHERE c.relnamespace = 'analytics'::regnamespace ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name, kind, own, opts string
		if err := rows.Scan(&name, &kind, &own, &opts); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		if kind != "v" || own != "app_owner" || !strings.Contains(opts, "security_barrier=true") {
			t.Errorf("analytics.%s: kind %s owner %s options %q; want a security_barrier view of app_owner", name, kind, own, opts)
		}
	}
	rows.Close()
	want := []string{"event_participation_monthly", "signups_monthly", "students_by_university", "talent_distribution"}
	if !slices.Equal(names, want) {
		t.Fatalf("analytics relations = %v, want %v", names, want)
	}
	ro := appRO(t, w)
	for _, v := range names {
		q := `SELECT * FROM analytics.` + v
		if got := execState(t, w.d.Pool, q); got != db.CodeInsufficientPriv {
			t.Errorf("app_rw %s: SQLSTATE %q, want %s", q, got, db.CodeInsufficientPriv)
		}
		if _, err := ro.Exec(t.Context(), q); err != nil {
			t.Errorf("app_ro %s: %v", q, err)
		}
		for role, want := range map[string]string{"app_ro": "true", "app_rw": "false", "app_talent": "false", "audit_writer": "false"} {
			var got string
			if err := owner.QueryRow(t.Context(), `SELECT has_table_privilege($1, $2, 'SELECT')::text`, role, "analytics."+v).Scan(&got); err != nil || got != want {
				t.Errorf("SELECT on analytics.%s for %s = %s (%v), want %s", v, role, got, err, want)
			}
		}
		var public int
		if err := owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_class c, aclexplode(c.relacl) a WHERE c.oid = $1::regclass AND a.grantee = 0`,
			"analytics."+v).Scan(&public); err != nil || public != 0 {
			t.Errorf("PUBLIC holds privileges on analytics.%s (%v)", v, err)
		}
	}
	// No personal column: no user ID, name, e-mail, birth date or score.
	cols, err := owner.Query(t.Context(), `SELECT table_name || '.' || column_name FROM information_schema.columns WHERE table_schema = 'analytics'
		AND (column_name ~ '(user|email|first_name|last_name|birth|phone|score|pct|raw_cp|ip)' ) ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for cols.Next() {
		var c string
		if err := cols.Scan(&c); err != nil {
			t.Fatal(err)
		}
		bad = append(bad, c)
	}
	cols.Close()
	if len(bad) > 0 {
		t.Errorf("personal columns in analytics views: %v", bad)
	}
	for _, q := range []string{`SELECT 1 FROM users`, `SELECT 1 FROM student_profiles`, `SELECT 1 FROM talent.scores`, `CREATE VIEW analytics.leak AS SELECT 1`} {
		if _, err := ro.Exec(t.Context(), q); db.SQLState(err) != db.CodeInsufficientPriv {
			t.Errorf("app_ro %q: %v, want permission denied", q, err)
		}
	}
}

// TestSchema_PrivilegedAccountActionsPurge (BE-016 table, with role_history):
// app_rw has no DELETE, so the decided requests older than 3 years go
// through the purge function; a recent or open request stays.
func TestSchema_PrivilegedAccountActionsPurge(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	target, a, b := w.user(), w.user(), w.user()
	ins := `INSERT INTO privileged_account_actions (target_user_id, action, reason, status, requested_by, requested_at, expires_at, approved_by, approved_at)
		VALUES ($1, $2, 'Hesap güvenliği', $3, $4, now() - $5::interval, now() - $5::interval + interval '1 day', $6, now() - $7::interval)`
	w.exec(ins, target, "suspend", "approved", a, "4 years", b, "4 years")
	w.exec(ins, target, "reset_mfa", "expired", a, "5 years", nil, nil)
	w.exec(ins, target, "revoke_sessions", "approved", a, "1 year", b, "1 year")
	w.exec(ins, target, "suspend", "pending", a, "1 hour", nil, nil)
	if got := queryString(t, w.d, `SELECT retention::text || ' ' || anchor || ' ' || owner_module || ' ' || legal_approved::text
		FROM retention_policies WHERE code = 'privileged_action_history'`); got != "3 years decided_at identity false" {
		t.Fatalf("privileged_action_history = %q", got)
	}
	expectFail(t, w.d.Pool, []p1aCase{{"app_rw delete", `DELETE FROM privileged_account_actions`, nil, db.CodeInsufficientPriv, ""}})
	if _, state := w.purge("privileged_account_actions_purge", 100, false); state != "55000" {
		t.Fatalf("purge before the approval: SQLSTATE %q, want 55000", state)
	}
	if n, _ := w.purge("privileged_account_actions_purge", 100, true); n != 2 {
		t.Fatalf("dry run = %d, want 2", n)
	}
	w.approve("privileged_action_history")
	if n, state := w.purge("privileged_account_actions_purge", 100, false); n != 2 || state != "" {
		t.Fatalf("purge = %d, %q; want 2", n, state)
	}
	if n := w.count(`SELECT count(*) FROM privileged_account_actions`); n != 2 {
		t.Fatalf("requests left = %d, want 2", n)
	}
}
