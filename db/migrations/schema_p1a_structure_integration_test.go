//go:build integration

package migrations_test

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"koro/db/migrations"
	"koro/internal/platform/migratekit"
	"koro/internal/platform/obs"
	"koro/internal/platform/testutil"
)

// p1aGrants lists the tables of migrations 000010–000043 with the privileges
// app_rw holds on them (S select, I insert, U update, D delete). The rule
// (02 §0.5, 05 §7.2): the ledger of consents is S+I only (append-only);
// tables whose meta says "soft delete" or "anonymise" (reference data,
// orgs, users, notice versions, consent purposes) have no D; tables with
// hard-delete retention keep D.
var p1aGrants = map[string]string{
	// consent
	"consent_purposes": "SIU",
	"notice_versions":  "SIU",
	"consents":         "SI",
	"iys_submissions":  "SIUD",
	// identity
	"users":                      "SIU",
	"sessions":                   "SIUD",
	"email_tokens":               "SIUD",
	"edu_verifications":          "SIUD",
	"mfa_totp":                   "SIUD",
	"mfa_recovery_codes":         "SIUD",
	"platform_role_assignments":  "SIUD",
	"privileged_account_actions": "SIU", // 06 H-47 (BE-016): proof of the four-eyes decision
	// notify
	"notification_deliveries":  "SIUD",
	"email_suppressions":       "SIUD",
	"push_tokens":              "SIUD",
	"in_app_notifications":     "SIUD",
	"notification_preferences": "SIUD",
	// orgs
	"organizations":   "SIU",
	"companies":       "SIU",
	"org_members":     "SIUD",
	"org_invitations": "SIUD",
	// directory
	"universities":             "SIU",
	"university_email_domains": "SIU",
	"faculties":                "SIU",
	"departments":              "SIU",
	"clubs":                    "SIU",
	"club_claims":              "SIUD",
	"skills":                   "SIU",
}

// p1aTableNames returns the keys of p1aGrants, sorted.
func p1aTableNames() []string {
	names := make([]string, 0, len(p1aGrants))
	for n := range p1aGrants {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// queryStrings runs a query returning one text column as app_rw.
func queryStrings(t *testing.T, w *p1aWorld, sql string, args ...any) []string {
	t.Helper()
	rows, err := w.d.Pool.Query(t.Context(), sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

// TestSchema_GrantsAndOwnership: every table of 000010–000043 belongs to
// app_owner; app_rw holds exactly the privileges of p1aGrants and nothing
// else (no TRUNCATE, REFERENCES, TRIGGER); no other role and not PUBLIC can
// touch them; consent_current is read-only.
func TestSchema_GrantsAndOwnership(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	privs := map[byte]string{'S': "SELECT", 'I': "INSERT", 'U': "UPDATE", 'D': "DELETE"}
	for _, table := range p1aTableNames() {
		want := p1aGrants[table]
		for letter, priv := range privs {
			got := queryString(t, w.d, `SELECT has_table_privilege('app_rw', $1, $2)::text`, table, priv)
			if wantIt := strings.IndexByte(want, letter) >= 0; got != strconv.FormatBool(wantIt) {
				t.Errorf("app_rw %s on %s = %s, want %v", priv, table, got, wantIt)
			}
		}
		for _, priv := range []string{"TRUNCATE", "REFERENCES", "TRIGGER"} {
			if got := queryString(t, w.d, `SELECT has_table_privilege('app_rw', $1, $2)::text`, table, priv); got != "false" {
				t.Errorf("app_rw has %s on %s", priv, table)
			}
		}
		for _, role := range []string{"app_ro", "audit_writer", "audit_chainer", "app_talent"} {
			if got := queryString(t, w.d, `SELECT (has_table_privilege($1, $2, 'SELECT') OR has_table_privilege($1, $2, 'INSERT')
				OR has_table_privilege($1, $2, 'UPDATE') OR has_table_privilege($1, $2, 'DELETE'))::text`, role, table); got != "false" {
				t.Errorf("%s has a privilege on %s", role, table)
			}
		}
		if got := queryString(t, w.d, `SELECT count(*)::text FROM pg_class c, aclexplode(c.relacl) a WHERE c.oid = $1::regclass AND a.grantee = 0`, table); got != "0" {
			t.Errorf("PUBLIC holds privileges on %s", table)
		}
		if got := queryString(t, w.d, `SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = $1`, table); got != "app_owner" {
			t.Errorf("%s is owned by %q, want app_owner", table, got)
		}
	}
	if got := queryString(t, w.d, `SELECT viewowner FROM pg_views WHERE schemaname = 'public' AND viewname = 'consent_current'`); got != "app_owner" {
		t.Errorf("consent_current is owned by %q", got)
	}
	for priv, want := range map[string]string{"SELECT": "true", "INSERT": "false", "UPDATE": "false", "DELETE": "false"} {
		if got := queryString(t, w.d, `SELECT has_table_privilege('app_rw', 'consent_current', $1)::text`, priv); got != want {
			t.Errorf("app_rw %s on consent_current = %s, want %s", priv, got, want)
		}
	}
	if got := queryString(t, w.d, `SELECT has_table_privilege('app_ro', 'consent_current', 'SELECT')::text`); got != "false" {
		t.Errorf("app_ro can read consent_current")
	}
}

// TestSchema_ExplicitConstraintAndIndexNames (02 §23): every constraint and
// index of the P1 tables has an explicit name of the form
// <table>_<purpose>_{chk,uq,fkey,idx} (the primary key keeps <table>_pkey).
// Generated names (…_check, …_key, …_check1) would break the error mapping of
// the stores, which relies on the names.
func TestSchema_ExplicitConstraintAndIndexNames(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	tables := p1aTableNames()
	bad := queryStrings(t, w, `
		SELECT c.conrelid::regclass::text || ' ' || c.conname || ' (' || c.contype::text || ')'
		  FROM pg_constraint c
		 WHERE c.conrelid IN (SELECT ('public.' || quote_ident(t))::regclass FROM unnest($1::text[]) t)
		   AND c.contype IN ('p', 'u', 'f', 'c')
		   AND NOT (c.conname LIKE (c.conrelid::regclass::text || '\_%') ESCAPE '\'
		        AND ((c.contype = 'p' AND c.conname = c.conrelid::regclass::text || '_pkey')
		          OR (c.contype = 'u' AND c.conname LIKE '%\_uq' ESCAPE '\')
		          OR (c.contype = 'f' AND c.conname LIKE '%\_fkey' ESCAPE '\')
		          OR (c.contype = 'c' AND c.conname LIKE '%\_chk' ESCAPE '\')))
		 ORDER BY 1`, tables)
	if len(bad) > 0 {
		t.Errorf("constraints without an explicit name of the form <table>_<purpose>_{chk,uq,fkey}:\n%s", strings.Join(bad, "\n"))
	}
	badIdx := queryStrings(t, w, `
		SELECT indexname FROM pg_indexes
		 WHERE schemaname = 'public' AND tablename = ANY ($1::text[])
		   AND NOT (indexname LIKE (tablename || '\_%') ESCAPE '\'
		        AND (indexname LIKE '%\_idx' ESCAPE '\' OR indexname LIKE '%\_uq' ESCAPE '\' OR indexname LIKE '%\_pkey' ESCAPE '\'))
		 ORDER BY 1`, tables)
	if len(badIdx) > 0 {
		t.Errorf("indexes without an explicit name of the form <table>_<purpose>_{idx,uq}:\n%s", strings.Join(badIdx, "\n"))
	}
	// A typo in the table list would make both queries pass vacuously.
	if n := queryString(t, w.d, `SELECT count(*)::text FROM pg_constraint WHERE contype = 'c'
		AND conrelid IN (SELECT ('public.' || quote_ident(t))::regclass FROM unnest($1::text[]) t)`, tables); n == "0" {
		t.Errorf("no CHECK constraints found for the P1 tables")
	}
}

// TestSchema_QueryIndexes pins the non-unique indexes of 02 (partial
// predicates, key order, the trigram and full-text indexes): they carry no
// constraint, so only their definition tells whether the query paths of the
// modules (sessions per user, unread notifications, org member lookups,
// searches) are served.
func TestSchema_QueryIndexes(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	want := map[string][]string{
		"consents_current_idx":             {"(user_id, purpose_code, scope_id, occurred_at DESC, id DESC)"},
		"consents_purpose_idx":             {"(purpose_code, occurred_at)"},
		"iys_submissions_pending_idx":      {"(created_at)", "pending", "failed"},
		"users_name_trgm_idx":              {"USING gin", "koro_norm", "gin_trgm_ops", "erased"},
		"sessions_expiry_idx":              {"(absolute_expires_at)"},
		"sessions_user_active_idx":         {"(user_id) WHERE (revoked_at IS NULL)"},
		"email_tokens_open_idx":            {"(user_id, purpose) WHERE (consumed_at IS NULL)"},
		"edu_verifications_expiry_idx":     {"(expires_at)", "verified"},
		"mfa_recovery_codes_user_idx":      {"(user_id) WHERE (used_at IS NULL)"},
		"notification_deliveries_user_idx": {"(user_id, created_at DESC) WHERE (user_id IS NOT NULL)"},
		"push_tokens_user_idx":             {"(user_id) WHERE (revoked_at IS NULL)"},
		"in_app_notifications_user_idx":    {"(user_id, created_at DESC, id DESC)"},
		"in_app_notifications_unread_idx":  {"(user_id) WHERE (read_at IS NULL)"},
		"organizations_fts_idx":            {"USING gin (search_vector)"},
		"organizations_kind_status_idx":    {"(kind, status)"},
		"organizations_trgm_idx":           {"gin_trgm_ops", "koro_norm(name)"},
		"org_members_user_idx":             {"(user_id)", "active"},
		"org_members_org_role_idx":         {"(org_id, role)", "active"},
		"org_members_ended_idx":            {"(ended_at)", "ended"},
		"universities_fts_idx":             {"USING gin (search_vector)"},
		"universities_trgm_idx":            {"gin_trgm_ops", "koro_norm(name)"},
		"university_email_domains_uni_idx": {"(university_id)"},
		"departments_uni_idx":              {"(university_id) WHERE is_active"},
		"departments_trgm_idx":             {"gin_trgm_ops", "koro_norm(name)"},
		"clubs_university_idx":             {"(university_id, category)"},
		"clubs_nonsensitive_idx":           {"(university_id) WHERE (NOT is_sensitive)"},
		"club_claims_queue_idx":            {"(created_at)", "pending"},
		"skills_trgm_idx":                  {"gin_trgm_ops", "koro_norm(name_tr)"},
	}
	for name, parts := range want {
		def := queryString(t, w.d, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name)
		if def == "<NULL>" {
			t.Errorf("index %s does not exist", name)
			continue
		}
		for _, part := range parts {
			if !strings.Contains(def, part) {
				t.Errorf("index %s lacks %q:\n  %s", name, part, def)
			}
		}
		if strings.Contains(name, "_trgm_") && !strings.Contains(def, "public.koro_norm") && !strings.Contains(def, "koro_norm(") {
			t.Errorf("index %s does not use koro_norm: %s", name, def)
		}
	}
}

// TestSchema_ForeignKeyActions (02 §0.1): FKs are NO ACTION unless the DDL
// writes otherwise; the only exception in 000010–000043 is
// push_tokens.session_id (ON DELETE SET NULL). Nothing cascades.
func TestSchema_ForeignKeyActions(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	got := queryStrings(t, w, `
		SELECT conrelid::regclass::text || '.' || conname || ' delete=' || confdeltype::text || ' update=' || confupdtype::text
		  FROM pg_constraint
		 WHERE contype = 'f' AND (confdeltype <> 'a' OR confupdtype <> 'a')
		   AND conrelid IN (SELECT ('public.' || quote_ident(t))::regclass FROM unnest($1::text[]) t)
		 ORDER BY 1`, p1aTableNames())
	want := []string{"push_tokens.push_tokens_session_id_fkey delete=n update=a"}
	if !slices.Equal(got, want) {
		t.Errorf("foreign keys with an action other than NO ACTION:\n got %v\nwant %v", got, want)
	}
	if n := queryString(t, w.d, `SELECT count(*)::text FROM pg_constraint WHERE contype = 'f'
		AND conrelid IN (SELECT ('public.' || quote_ident(t))::regclass FROM unnest($1::text[]) t)`, p1aTableNames()); n == "0" {
		t.Errorf("no foreign keys found")
	}
}

// TestSchema_IDColumnsAndTouchTriggers: every `id uuid` column defaults to
// uuidv7() (02 §0.2, the safety net for hand-written SQL) and every table
// with an updated_at column has the BEFORE UPDATE koro_touch_updated_at row
// trigger named <table>_touch_updated_at (02 §0.1 [ts]).
func TestSchema_IDColumnsAndTouchTriggers(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	tables := p1aTableNames()

	badID := queryStrings(t, w, `
		SELECT table_name || '.id default ' || coalesce(column_default, '<none>') FROM information_schema.columns
		 WHERE table_schema = 'public' AND column_name = 'id' AND table_name = ANY ($1::text[])
		   AND (data_type <> 'uuid' OR column_default IS DISTINCT FROM 'uuidv7()')
		 ORDER BY 1`, tables)
	if len(badID) > 0 {
		t.Errorf("id columns without DEFAULT uuidv7():\n%s", strings.Join(badID, "\n"))
	}
	if n := queryString(t, w.d, `SELECT count(*)::text FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'id' AND table_name = ANY ($1::text[])`, tables); n == "0" {
		t.Errorf("no id columns found")
	}

	missing := queryStrings(t, w, `
		SELECT c.relname FROM pg_class c
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'updated_at' AND NOT a.attisdropped
		 WHERE c.relnamespace = 'public'::regnamespace AND c.relname = ANY ($1::text[])
		   AND NOT EXISTS (SELECT 1 FROM pg_trigger g
		        WHERE g.tgrelid = c.oid AND NOT g.tgisinternal
		          AND g.tgname = c.relname || '_touch_updated_at'
		          AND g.tgfoid = 'public.koro_touch_updated_at'::regproc
		          AND g.tgtype & 1 = 1 AND g.tgtype & 2 = 2 AND g.tgtype & 16 = 16)   -- ROW, BEFORE, UPDATE
		 ORDER BY 1`, tables)
	if len(missing) > 0 {
		t.Errorf("tables with updated_at but without the koro_touch_updated_at trigger: %v", missing)
	}
	if n := queryString(t, w.d, `SELECT count(*)::text FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		WHERE a.attname = 'updated_at' AND NOT a.attisdropped AND c.relnamespace = 'public'::regnamespace AND c.relname = ANY ($1::text[])`, tables); n != "16" {
		t.Errorf("tables with updated_at = %s, want 16", n)
	}
}

// TestSchema_TurkishCollationOnNameColumns (02 §0.4): human names and titles
// are declared COLLATE "tr", so ORDER BY follows Turkish rules even in a
// database whose default collation is something else.
func TestSchema_TurkishCollationOnNameColumns(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	for table, columns := range map[string][]string{
		"users":         {"first_name", "last_name"},
		"organizations": {"name", "city"},
		"companies":     {"hq_city"},
		"universities":  {"name", "short_name", "city"},
		"faculties":     {"name"},
		"departments":   {"name"},
		"skills":        {"name_tr"},
	} {
		for _, column := range columns {
			got := queryString(t, w.d, `SELECT collation_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, column)
			if got != "tr" {
				t.Errorf("%s.%s collation = %q, want tr", table, column, got)
			}
		}
	}
}

// migrationsUpTo returns the embedded migrations numbered <= n.
func migrationsUpTo(t *testing.T, n uint) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		v, err := strconv.ParseUint(e.Name()[:6], 10, 32)
		if err != nil {
			t.Fatalf("migration file %q: %v", e.Name(), err)
		}
		if uint(v) > n {
			continue
		}
		data, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: data}
	}
	return out
}

// TestMigrations_EachStepRestoresThePreviousSchema is the per-file form of
// TestMigrations_UpDownUp (05 §7.5): the migrations are applied one at a
// time and the schema fingerprint after each step is recorded; then they
// are reverted one at a time with the full file set and every down file
// must restore exactly the fingerprint of the step below it. This finds the
// down file that forgets an object (a function, a trigger, a grant, an
// index) even when a later down file happens to drop it.
func TestMigrations_EachStepRestoresThePreviousSchema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ownerURL := testutil.NewEmptyDB(t)
	latest, err := migratekit.Latest(migrations.FS)
	if err != nil || latest == 0 {
		t.Fatalf("Latest() = %d, %v", latest, err)
	}
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	forward := map[uint]string{}
	// Numbers are pre-allocated per ticket and may leave gaps before the
	// pilot (BE-001 R2): walk the versions that exist.
	versions := migrationVersions(t)
	below := func(i int) uint {
		if i == 0 {
			return 0
		}
		return versions[i-1]
	}
	for i, n := range versions {
		m, err := migratekit.New(migrationsUpTo(t, n), ownerURL, obs.Discard())
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			forward[0] = fingerprint(t, conn) // schema_migrations exists once a migrator is open
		}
		if err := m.Up(ctx); err != nil {
			_ = m.Close()
			t.Fatalf("up to %06d: %v", n, err)
		}
		if v, dirty, err := m.Version(); err != nil || dirty || v != n {
			_ = m.Close()
			t.Fatalf("version after up to %06d = %d, dirty %v, %v", n, v, dirty, err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		forward[n] = fingerprint(t, conn)
		// Seeds and data corrections (NNNNNN_data_*, e.g. 000158, BE-135)
		// change rows only.
		seeds, _ := fs.Glob(migrations.FS, fmt.Sprintf("%06d_seed_*.up.sql", n))
		fixes, _ := fs.Glob(migrations.FS, fmt.Sprintf("%06d_data_*.up.sql", n))
		if len(seeds)+len(fixes) == 0 && forward[n] == forward[below(i)] {
			t.Errorf("migration %06d changed nothing in the schema fingerprint", n)
		}
	}
	// The seeds change data only: the fingerprint cannot see them.
	seedRows := map[uint]string{11: `SELECT count(*) FROM consent_purposes`, 31: `SELECT count(*) FROM organizations`, 60: `SELECT count(*) FROM form_lint_rules`,
		81: `SELECT count(*) FROM talent.signal_defs`, 90: `SELECT count(*) FROM retention_policies`, 110: `SELECT count(*) FROM ledger_accounts`}
	rowCount := func(sql string) int {
		t.Helper()
		var n int
		if err := conn.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if latest >= 31 {
		if rowCount(seedRows[11]) != 7 || rowCount(seedRows[31]) != 1 {
			t.Fatalf("seed rows after the last up: purposes %d, orgs %d; want 7 and 1", rowCount(seedRows[11]), rowCount(seedRows[31]))
		}
	}
	if latest >= 60 {
		if n := rowCount(seedRows[60]); n != 19 {
			t.Fatalf("seed rows after the last up: lint rules %d; want 19", n)
		}
	}
	if latest >= 90 {
		wantPolicies := 44 // 000090 (02 §24.2)
		if latest >= 121 {
			wantPolicies = 45 // + privileged_action_history (000121, BE-049)
		}
		if latest >= 134 {
			wantPolicies = 46 // + talent_collection_off (000134, BE-087)
		}
		if latest >= 142 {
			wantPolicies = 47 // + talent_score_dropped (000142, BE-101)
		}
		if latest >= 147 {
			wantPolicies = 48 // + iys_netgsm_event (000147, BE-110)
		}
		if latest >= 148 {
			wantPolicies = 49 // + terms_acceptance (000148, BE-118)
		}
		if defs, policies := rowCount(seedRows[81]), rowCount(seedRows[90]); defs != 10 || policies != wantPolicies {
			t.Fatalf("seed rows after the last up: signal defs %d, retention policies %d; want 10 and %d", defs, policies, wantPolicies)
		}
	}
	if latest >= 110 {
		if n := rowCount(seedRows[110]); n != 13 { // + payee_receivable (06 H-59, BE-043)
			t.Fatalf("seed rows after the last up: ledger accounts %d; want 13", n)
		}
	}

	m, err := migratekit.New(migrations.FS, ownerURL, obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	for i := len(versions) - 1; i >= 0; i-- {
		n, prev := versions[i], below(i)
		if err := m.Down(ctx, 1); err != nil {
			t.Fatalf("down of %06d: %v", n, err)
		}
		if v, dirty, err := m.Version(); err != nil || dirty || v != prev {
			t.Fatalf("version after down of %06d = %d, dirty %v, %v", n, v, dirty, err)
		}
		if got := fingerprint(t, conn); got != forward[prev] {
			t.Errorf("down of %06d does not restore the schema of %06d:\n%s", n, prev, diff(forward[prev], got))
		}
		if q, ok := seedRows[n]; ok && rowCount(q) != 0 {
			t.Errorf("down of %06d left the seed rows behind (%s)", n, q)
		}
	}
}
