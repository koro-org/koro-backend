//go:build integration

package migrations_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"koro/internal/platform/migratekit"
	"koro/internal/platform/obs"
	"koro/internal/platform/testutil"
)

// p1cGrants lists the public tables of migrations 000065–000093 with the
// privileges app_rw holds on them, by the rule of 02 §0.5: append-only
// tables (moderation_actions, destruction_logs) are S+I; tables whose
// deletion is "soft" or "—" (feed_org_settings, segments,
// retention_policies, subprocessors) are S+I+U; the rest are hard-delete
// tables. The talent tables are not here: app_rw holds nothing on them
// (talentTables, TestRoleBoundaries_AppRwCannotReadTalent).
var p1cGrants = map[string]string{
	// feed
	"feed_org_settings":  "SIU",
	"posts":              "SIUD",
	"post_media":         "SIUD",
	"post_reactions":     "SIUD",
	"post_saves":         "SIUD",
	"post_hides":         "SIUD",
	"org_follows":        "SIUD",
	"org_mutes":          "SIUD",
	"content_reports":    "SIUD",
	"moderation_actions": "SI",
	// outreach
	"segments":            "SIU",
	"outreach_dispatches": "SIUD",
	// messaging
	"contact_requests": "SIUD",
	"contact_blocks":   "SIUD",
	// kvkk
	"data_subject_requests": "SIUD",
	"retention_policies":    "SIU",
	"destruction_logs":      "SI",
	"breach_incidents":      "SIUD",
	"subprocessors":         "SIU",
}

// p1cQualifiedTables returns every table of 000065–000093 as schema.name,
// sorted: the public tables of p1cGrants and the talent tables.
func p1cQualifiedTables() []string {
	var out []string
	for n := range p1cGrants {
		out = append(out, "public."+n)
	}
	for n := range talentTables {
		out = append(out, "talent."+n)
	}
	slices.Sort(out)
	return out
}

// p1cTableOIDs is the SQL that turns $1 (schema.name text[]) into the table
// oids without resolving the names as app_rw (talent.* would be denied).
const p1cTableOIDs = `(SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname || '.' || c.relname = ANY ($1::text[]))`

// TestSchema_P1Schema3GrantsAndOwnership: every public table of 000065–000093
// belongs to app_owner; app_rw holds exactly the privileges of p1cGrants and
// nothing else; no other role and not PUBLIC can touch them (app_talent
// included: the talent role reaches the talent schema only).
func TestSchema_P1Schema3GrantsAndOwnership(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	privs := map[byte]string{'S': "SELECT", 'I': "INSERT", 'U': "UPDATE", 'D': "DELETE"}
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range p1cGrants {
			if !yield(n) {
				return
			}
		}
	})
	for _, table := range names {
		want := p1cGrants[table]
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
	if got := queryString(t, w.d, `SELECT proowner::regrole::text FROM pg_proc WHERE oid = 'public.moderation_actions_forbid_mutation()'::regprocedure`); got != "app_owner" {
		t.Errorf("moderation_actions_forbid_mutation is owned by %q, want app_owner", got)
	}
}

// TestSchema_P1Schema3ExplicitConstraintAndIndexNames (02 §23): every
// constraint and index of the tables (public and talent) has an explicit
// name of the form <table>_<purpose>_{chk,uq,fkey,idx} (the primary key
// keeps <table>_pkey); the table name is used without its schema.
func TestSchema_P1Schema3ExplicitConstraintAndIndexNames(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	tables := p1cQualifiedTables()
	bad := queryStrings(t, w, `
		SELECT r.relname || ' ' || c.conname || ' (' || c.contype::text || ')'
		  FROM pg_constraint c JOIN pg_class r ON r.oid = c.conrelid
		 WHERE c.conrelid IN `+p1cTableOIDs+`
		   AND c.contype IN ('p', 'u', 'f', 'c')
		   AND NOT (c.conname LIKE (r.relname || '\_%') ESCAPE '\'
		        AND ((c.contype = 'p' AND c.conname = r.relname || '_pkey')
		          OR (c.contype = 'u' AND c.conname LIKE '%\_uq' ESCAPE '\')
		          OR (c.contype = 'f' AND c.conname LIKE '%\_fkey' ESCAPE '\')
		          OR (c.contype = 'c' AND c.conname LIKE '%\_chk' ESCAPE '\')))
		 ORDER BY 1`, tables)
	if len(bad) > 0 {
		t.Errorf("constraints without an explicit name of the form <table>_<purpose>_{chk,uq,fkey}:\n%s", strings.Join(bad, "\n"))
	}
	badIdx := queryStrings(t, w, `
		SELECT schemaname || '.' || indexname FROM pg_indexes
		 WHERE schemaname || '.' || tablename = ANY ($1::text[])
		   AND NOT (indexname LIKE (tablename || '\_%') ESCAPE '\'
		        AND (indexname LIKE '%\_idx' ESCAPE '\' OR indexname LIKE '%\_uq' ESCAPE '\' OR indexname LIKE '%\_pkey' ESCAPE '\'))
		 ORDER BY 1`, tables)
	if len(badIdx) > 0 {
		t.Errorf("indexes without an explicit name of the form <table>_<purpose>_{idx,uq}:\n%s", strings.Join(badIdx, "\n"))
	}
	if n := queryString(t, w.d, `SELECT count(DISTINCT conrelid)::text FROM pg_constraint WHERE conrelid IN `+p1cTableOIDs, tables); n != strconv.Itoa(len(tables)) {
		t.Errorf("tables with constraints = %s, want %d (a typo in the table list?)", n, len(tables))
	}
}

// TestSchema_P1Schema3ForeignKeyActions (02 §0.1): FKs are NO ACTION unless
// the DDL writes otherwise. The exceptions of 000065–000093: the children of
// a post go with it (CASCADE) and a moderation action outlives its report
// (SET NULL).
func TestSchema_P1Schema3ForeignKeyActions(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	got := queryStrings(t, w, `
		SELECT r.relname || '.' || c.conname || ' delete=' || c.confdeltype::text || ' update=' || c.confupdtype::text
		  FROM pg_constraint c JOIN pg_class r ON r.oid = c.conrelid
		 WHERE c.contype = 'f' AND (c.confdeltype <> 'a' OR c.confupdtype <> 'a')
		   AND c.conrelid IN `+p1cTableOIDs+`
		 ORDER BY 1`, p1cQualifiedTables())
	want := []string{
		"moderation_actions.moderation_actions_report_id_fkey delete=n update=a",
		"post_hides.post_hides_post_id_fkey delete=c update=a",
		"post_media.post_media_post_id_fkey delete=c update=a",
		"post_reactions.post_reactions_post_id_fkey delete=c update=a",
		"post_saves.post_saves_post_id_fkey delete=c update=a",
	}
	if !slices.Equal(got, want) {
		t.Errorf("foreign keys with an action other than NO ACTION:\n got %v\nwant %v", got, want)
	}
	// 06 H-38: the dispatch's post and segment are bound to its org.
	composite := queryStrings(t, w, `SELECT conname || ' ' || pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'outreach_dispatches'::regclass AND contype = 'f' AND cardinality(conkey) = 2 ORDER BY 1`)
	wantComposite := []string{
		"outreach_dispatches_post_id_org_id_fkey FOREIGN KEY (post_id, org_id) REFERENCES posts(id, org_id)",
		"outreach_dispatches_segment_id_org_id_fkey FOREIGN KEY (segment_id, org_id) REFERENCES segments(id, org_id)",
	}
	if !slices.Equal(composite, wantComposite) {
		t.Errorf("composite FKs of outreach_dispatches:\n got %v\nwant %v", composite, wantComposite)
	}
}

// TestSchema_P1Schema3IDColumnsAndTriggers: every `id uuid` column defaults
// to uuidv7() (02 §0.2); every table with updated_at has the BEFORE UPDATE
// koro_touch_updated_at trigger named <table>_touch_updated_at; the other
// triggers are exactly the immutability guards.
func TestSchema_P1Schema3IDColumnsAndTriggers(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	tables := p1cQualifiedTables()
	badID := queryStrings(t, w, `
		SELECT table_schema || '.' || table_name || '.id default ' || coalesce(column_default, '<none>') FROM information_schema.columns
		 WHERE column_name = 'id' AND table_schema || '.' || table_name = ANY ($1::text[])
		   AND (data_type <> 'uuid' OR column_default IS DISTINCT FROM 'uuidv7()')
		 ORDER BY 1`, tables)
	if len(badID) > 0 {
		t.Errorf("id columns without DEFAULT uuidv7():\n%s", strings.Join(badID, "\n"))
	}
	// information_schema hides the talent columns from app_rw: count as the owner.
	owner := testutil.OwnerConn(t, w.d)
	count := func(sql string) int {
		t.Helper()
		var n int
		if err := owner.QueryRow(t.Context(), sql, tables).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM pg_attribute a WHERE a.attname = 'id' AND NOT a.attisdropped AND a.attrelid IN ` + p1cTableOIDs); n != 15 {
		t.Errorf("tables with an id column = %d, want 15", n)
	}
	if n := count(`SELECT count(*) FROM pg_attribute a JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attname = 'id' AND pg_get_expr(d.adbin, d.adrelid) = 'uuidv7()' AND a.attrelid IN ` + p1cTableOIDs); n != 15 {
		t.Errorf("id columns with DEFAULT uuidv7() = %d, want 15", n)
	}
	missing := queryStrings(t, w, `
		SELECT c.relname FROM pg_class c
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'updated_at' AND NOT a.attisdropped
		 WHERE c.oid IN `+p1cTableOIDs+`
		   AND NOT EXISTS (SELECT 1 FROM pg_trigger g
		        WHERE g.tgrelid = c.oid AND NOT g.tgisinternal
		          AND g.tgname = c.relname || '_touch_updated_at'
		          AND g.tgfoid = 'public.koro_touch_updated_at'::regproc
		          AND g.tgtype & 1 = 1 AND g.tgtype & 2 = 2 AND g.tgtype & 16 = 16)   -- ROW, BEFORE, UPDATE
		 ORDER BY 1`, tables)
	if len(missing) > 0 {
		t.Errorf("tables with updated_at but without the koro_touch_updated_at trigger: %v", missing)
	}
	if n := count(`SELECT count(*) FROM pg_attribute a WHERE a.attname = 'updated_at' AND NOT a.attisdropped AND a.attrelid IN ` + p1cTableOIDs); n != 12 {
		t.Errorf("tables with updated_at = %d, want 12", n)
	}
	others := queryStrings(t, w, `
		SELECT c.relname || '.' || g.tgname || ' ' || p.proname FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid JOIN pg_proc p ON p.oid = g.tgfoid
		 WHERE NOT g.tgisinternal AND c.oid IN `+p1cTableOIDs+`
		   AND g.tgname <> c.relname || '_touch_updated_at'
		 ORDER BY 1`, tables)
	wantOthers := []string{
		"config_versions.config_versions_immutable koro_forbid_mutation",
		"config_versions.config_versions_no_truncate koro_forbid_mutation",
		"destruction_logs.destruction_logs_immutable koro_forbid_mutation",
		"destruction_logs.destruction_logs_no_truncate koro_forbid_mutation",
		"moderation_actions.moderation_actions_immutable moderation_actions_forbid_mutation",
		"moderation_actions.moderation_actions_no_truncate koro_forbid_mutation",
		"point_events.point_events_immutable koro_forbid_mutation",
		"point_events.point_events_no_truncate koro_forbid_mutation",
	}
	if !slices.Equal(others, wantOthers) {
		t.Errorf("triggers besides touch_updated_at:\n got %v\nwant %v", others, wantOthers)
	}
}

// TestSchema_P1Schema3TurkishCollationOnNameColumns (02 §0.4): the human
// name and title columns are COLLATE "tr".
func TestSchema_P1Schema3TurkishCollationOnNameColumns(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	for table, columns := range map[string][]string{
		"posts":                 {"title"},
		"segments":              {"name"},
		"data_subject_requests": {"requester_name"},
		"breach_incidents":      {"title"},
		"subprocessors":         {"name"},
	} {
		for _, column := range columns {
			got := queryString(t, w.d, `SELECT collation_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, column)
			if got != "tr" {
				t.Errorf("%s.%s collation = %q, want tr", table, column, got)
			}
		}
	}
}

// TestSchema_P1Schema3QueryIndexes pins the indexes of 02 §12–§16 (partial
// predicates, key order, GIN and full-text): only their definition tells
// whether the query paths of the modules (feed, moderation queue, expiry
// sweeps, the talent band scan) are served. Unique indexes are rules.
func TestSchema_P1Schema3QueryIndexes(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	want := map[string][]string{
		"posts_feed_idx":                 {"(published_at DESC, id DESC)", "status = 'published'"},
		"posts_audience_idx":             {"USING gin (audience_keys)", "status = 'published'"},
		"posts_author_idx":               {"(org_id, published_at DESC, id DESC)", "status = 'published'"},
		"posts_review_idx":               {"(created_at)", "status = 'pending_review'"},
		"posts_removed_idx":              {"(removed_at)", "status = 'removed'"},
		"posts_fts_idx":                  {"USING gin (search_vector)", "status = 'published'"},
		"posts_id_org_uq":                {"UNIQUE", "(id, org_id)"},
		"post_reactions_user_idx":        {"(user_id, created_at DESC)"},
		"org_follows_org_idx":            {"(org_id)"},
		"content_reports_open_uq":        {"UNIQUE", "(reporter_user_id, target_type, target_id)", "'open'", "'in_review'"},
		"content_reports_queue_idx":      {"(created_at)", "'open'", "'in_review'"},
		"moderation_actions_target_idx":  {"(target_type, target_id, created_at)"},
		"segments_org_idx":               {"(org_id) WHERE (archived_at IS NULL)"},
		"segments_id_org_uq":             {"UNIQUE", "(id, org_id)"},
		"outreach_dispatches_org_idx":    {"(org_id, created_at DESC)"},
		"outreach_dispatches_post_id_uq": {"UNIQUE", "(post_id)"},
		"contact_requests_pending_uq":    {"UNIQUE", "(org_id, student_user_id)", "status = 'pending'"},
		"contact_requests_student_idx":   {"(student_user_id, status, created_at DESC)"},
		"contact_requests_org_idx":       {"(org_id, created_at DESC)"},
		"contact_requests_expiry_idx":    {"(expires_at)", "status = 'pending'"},
		"point_events_reversal_uq":       {"UNIQUE", "(reversal_of) WHERE (reversal_of IS NOT NULL)"},
		"point_events_user_idx":          {"(user_id, occurred_at DESC)"},
		"point_events_dedupe_uq":         {"UNIQUE", "(user_id, signal_code, source_type, source_id)"},
		"cohorts_key_idx":                {"(cohort_key)"},
		"scores_band_idx":                {"(band) WHERE (band > 0)"},
		"neutralizations_active_uq":      {"UNIQUE", "(user_id, reason) WHERE (lifted_at IS NULL)"},
		"data_subject_requests_open_idx": {"(due_at)", "'completed'", "'rejected'", "'cancelled'"},
		"data_subject_requests_user_idx": {"(user_id, received_at DESC)"},
		"destruction_logs_policy_idx":    {"(policy_code, created_at DESC)"},
	}
	for name, parts := range want {
		def := queryString(t, w.d, `SELECT indexdef FROM pg_indexes WHERE schemaname IN ('public', 'talent') AND indexname = $1`, name)
		if def == "<NULL>" {
			t.Errorf("index %s does not exist", name)
			continue
		}
		for _, part := range parts {
			if !strings.Contains(def, part) {
				t.Errorf("index %s lacks %q:\n  %s", name, part, def)
			}
		}
	}
}

// TestMigrations_P1Schema3UpDownUp is TestMigrations_UpDownUp pinned to the
// range of this schema ticket: the migrations up to 000093 are applied to an
// empty database, reverted completely and applied again; down leaves
// nothing behind (the talent schema keeps only what 000001 gave it) and the
// second up is identical to the first, seeds included.
func TestMigrations_P1Schema3UpDownUp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ownerURL := testutil.NewEmptyDB(t)
	m, err := migratekit.New(migrationsUpTo(t, 93), ownerURL, obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	empty := fingerprint(t, conn)
	if err := m.Up(ctx); err != nil {
		t.Fatalf("first up: %v", err)
	}
	if v, dirty, err := m.Version(); err != nil || dirty || v != 93 {
		t.Fatalf("version after up = %d, dirty %v, %v; want 93", v, dirty, err)
	}
	first := fingerprint(t, conn)
	for _, want := range []string{
		"rel public.posts r", "rel public.moderation_actions r", "rel public.outreach_dispatches r", "rel public.contact_requests r",
		"rel talent.config_versions r", "rel talent.point_events r", "rel talent.neutralizations r",
		"rel public.retention_policies r", "rel public.subprocessors r", "fn moderation_actions_forbid_mutation()",
		"trg CREATE TRIGGER point_events_immutable", "nsp talent app_owner",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("schema after up lacks %q", want)
		}
	}
	seeds := func() string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM talent.signal_defs)::text || ' ' || (SELECT count(*) FROM retention_policies)::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := seeds(); got != "10 44" {
		t.Errorf("seed rows after the first up = %s, want 10 signal defs and 44 policies", got)
	}
	if err := m.Down(ctx, 93); err != nil {
		t.Fatalf("down 93: %v", err)
	}
	if got := fingerprint(t, conn); got != empty {
		t.Fatalf("down left objects behind:\n%s", diff(empty, got))
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if got := fingerprint(t, conn); got != first {
		t.Fatalf("second up differs from the first:\n%s", diff(first, got))
	}
	if got := seeds(); got != "10 44" {
		t.Errorf("seed rows after the second up = %s, want 10 and 44", got)
	}
}
