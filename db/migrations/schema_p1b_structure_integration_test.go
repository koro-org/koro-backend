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

// p1bGrants lists the tables of migrations 000044–000064 with the privileges
// app_rw holds on them (S select, I insert, U update, D delete), by the rule
// of 02 §0.5: append-only tables (entitlement_usages, event_form_versions)
// are S+I; form_lint_rules is deactivated, never deleted (S+I+U); the rest
// are hard-delete tables (the account, the retention sweep, a draft event).
var p1bGrants = map[string]string{
	// students
	"student_profiles":         "SIUD",
	"student_privacy_settings": "SIUD",
	"student_skills":           "SIUD",
	"student_experiences":      "SIUD",
	"student_languages":        "SIUD",
	"student_cvs":              "SIUD",
	"profile_access_grants":    "SIUD",
	// entitlements
	"entitlements":       "SIUD",
	"entitlement_usages": "SI",
	// events
	"events":               "SIUD",
	"event_staff":          "SIUD",
	"event_forms":          "SIUD",
	"event_form_versions":  "SI",
	"form_lint_rules":      "SIU",
	"event_registrations":  "SIUD",
	"registration_tickets": "SIUD",
	"event_checkins":       "SIUD",
	"event_data_exports":   "SIUD",
}

// p1bTableNames returns the keys of p1bGrants, sorted.
func p1bTableNames() []string {
	names := make([]string, 0, len(p1bGrants))
	for n := range p1bGrants {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// TestSchema_P1Schema2GrantsAndOwnership: every table of 000044–000064
// belongs to app_owner; app_rw holds exactly the privileges of p1bGrants and
// nothing else (no TRUNCATE, REFERENCES, TRIGGER); no other role and not
// PUBLIC can touch them (app_ro reads only analytics views).
func TestSchema_P1Schema2GrantsAndOwnership(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	privs := map[byte]string{'S': "SELECT", 'I': "INSERT", 'U': "UPDATE", 'D': "DELETE"}
	for _, table := range p1bTableNames() {
		want := p1bGrants[table]
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
	for _, fn := range []string{"public.count_segment(text[], smallint)", "public.events_forbid_nondraft_delete()"} {
		if got := queryString(t, w.d, `SELECT proowner::regrole::text FROM pg_proc WHERE oid = $1::regprocedure`, fn); got != "app_owner" {
			t.Errorf("%s is owned by %q, want app_owner", fn, got)
		}
	}
}

// TestSchema_P1Schema2ExplicitConstraintAndIndexNames (02 §23): every
// constraint and index of the tables has an explicit name of the form
// <table>_<purpose>_{chk,uq,fkey,idx} (the primary key keeps <table>_pkey).
func TestSchema_P1Schema2ExplicitConstraintAndIndexNames(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	tables := p1bTableNames()
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
	if n := queryString(t, w.d, `SELECT count(*) FROM pg_constraint WHERE contype = 'c'
		AND conrelid IN (SELECT ('public.' || quote_ident(t))::regclass FROM unnest($1::text[]) t)`, tables); n == "0" {
		t.Errorf("no CHECK constraints found for the P1 schema-2 tables")
	}
}

// TestSchema_P1Schema2ForeignKeyActions (02 §0.1): FKs are NO ACTION unless
// the DDL writes otherwise. The exceptions of 000044–000064 are the ticket
// and the check-ins of a registration (they go with it).
func TestSchema_P1Schema2ForeignKeyActions(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	got := queryStrings(t, w, `
		SELECT conrelid::regclass::text || '.' || conname || ' delete=' || confdeltype::text || ' update=' || confupdtype::text
		  FROM pg_constraint
		 WHERE contype = 'f' AND (confdeltype <> 'a' OR confupdtype <> 'a')
		   AND conrelid IN (SELECT ('public.' || quote_ident(t))::regclass FROM unnest($1::text[]) t)
		 ORDER BY 1`, p1bTableNames())
	want := []string{
		"event_checkins.event_checkins_registration_id_fkey delete=c update=a",
		"event_checkins.event_checkins_ticket_id_fkey delete=c update=a",
		"registration_tickets.registration_tickets_registration_id_fkey delete=c update=a",
	}
	if !slices.Equal(got, want) {
		t.Errorf("foreign keys with an action other than NO ACTION:\n got %v\nwant %v", got, want)
	}
	if n := queryString(t, w.d, `SELECT count(*) FROM pg_constraint WHERE contype = 'f'
		AND conrelid IN (SELECT ('public.' || quote_ident(t))::regclass FROM unnest($1::text[]) t)`, p1bTableNames()); n == "0" {
		t.Errorf("no foreign keys found")
	}
}

// TestSchema_P1Schema2IDColumnsAndTriggers: every `id uuid` column defaults to
// uuidv7() (02 §0.2), every table with updated_at has the BEFORE UPDATE
// koro_touch_updated_at trigger named <table>_touch_updated_at, and the
// other triggers on the tables are exactly the immutability guards and the
// draft-only delete guard.
func TestSchema_P1Schema2IDColumnsAndTriggers(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	tables := p1bTableNames()

	badID := queryStrings(t, w, `
		SELECT table_name || '.id default ' || coalesce(column_default, '<none>') FROM information_schema.columns
		 WHERE table_schema = 'public' AND column_name = 'id' AND table_name = ANY ($1::text[])
		   AND (data_type <> 'uuid' OR column_default IS DISTINCT FROM 'uuidv7()')
		 ORDER BY 1`, tables)
	if len(badID) > 0 {
		t.Errorf("id columns without DEFAULT uuidv7():\n%s", strings.Join(badID, "\n"))
	}
	if n := queryString(t, w.d, `SELECT count(*)::text FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'id' AND table_name = ANY ($1::text[])`, tables); n != "13" {
		t.Errorf("tables with an id column = %s, want 13", n)
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
		WHERE a.attname = 'updated_at' AND NOT a.attisdropped AND c.relnamespace = 'public'::regnamespace AND c.relname = ANY ($1::text[])`, tables); n != "10" {
		t.Errorf("tables with updated_at = %s, want 10", n)
	}

	others := queryStrings(t, w, `
		SELECT c.relname || '.' || g.tgname FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid
		 WHERE NOT g.tgisinternal AND c.relnamespace = 'public'::regnamespace AND c.relname = ANY ($1::text[])
		   AND g.tgname <> c.relname || '_touch_updated_at'
		 ORDER BY 1`, tables)
	wantOthers := []string{
		"entitlement_usages.entitlement_usages_immutable",
		"entitlement_usages.entitlement_usages_no_truncate",
		"event_form_versions.event_form_versions_immutable",
		"event_form_versions.event_form_versions_no_truncate",
		"events.events_draft_delete_only",
	}
	if !slices.Equal(others, wantOthers) {
		t.Errorf("triggers besides touch_updated_at:\n got %v\nwant %v", others, wantOthers)
	}
}

// TestSchema_P1Schema2TurkishCollationOnNameColumns (02 §0.4): the title and
// city columns are COLLATE "tr".
func TestSchema_P1Schema2TurkishCollationOnNameColumns(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	for table, columns := range map[string][]string{
		"student_profiles": {"city"},
		"events":           {"title", "city"},
	} {
		for _, column := range columns {
			got := queryString(t, w.d, `SELECT collation_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, column)
			if got != "tr" {
				t.Errorf("%s.%s collation = %q, want tr", table, column, got)
			}
		}
	}
}

// TestSchema_P1Schema2QueryIndexes pins the non-unique indexes of 02
// (partial predicates, key order, the GIN, trigram and full-text indexes):
// they carry no constraint, so only their definition tells whether the query
// paths of the modules (audience targeting, waiting list, purge sweeps, the
// event feed) are served. Unique indexes are pinned too: they are rules.
func TestSchema_P1Schema2QueryIndexes(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	want := map[string][]string{
		"student_profiles_audience_idx":     {"USING gin (audience_keys)", "WHERE audience_eligible"},
		"student_profiles_filter_idx":       {"(university_id, department_id, class_year)", "status = 'active'"},
		"student_skills_skill_idx":          {"(skill_id)"},
		"student_experiences_user_idx":      {"(user_id, start_month DESC)"},
		"student_cvs_current_uq":            {"UNIQUE", "(user_id) WHERE is_current"},
		"profile_access_grants_active_idx":  {"(student_user_id, company_org_id) WHERE (revoked_at IS NULL)"},
		"profile_access_grants_company_idx": {"(company_org_id, granted_at DESC) WHERE ((revoked_at IS NULL) AND (suspended_at IS NULL))"}, // 000130 (BE-077): active grants only
		"entitlements_org_feature_idx":      {"(org_id, feature, valid_to)", "status = 'active'"},
		"entitlements_source_uq":            {"UNIQUE", "(source_type, source_id, feature) WHERE (source_id IS NOT NULL)"},
		"entitlements_expiry_idx":           {"(valid_to)", "status = 'active'"},
		"entitlement_usages_ref_uq":         {"UNIQUE", "(ref_type, ref_id) WHERE (reversal_of IS NULL)"},
		"entitlement_usages_reversal_uq":    {"UNIQUE", "(reversal_of) WHERE (reversal_of IS NOT NULL)"},
		"entitlement_usages_ent_idx":        {"(entitlement_id, created_at)"},
		"events_org_idx":                    {"(org_id, starts_at DESC)"},
		"events_listing_idx":                {"(starts_at, id)", "status = 'published'"},
		"events_fts_idx":                    {"USING gin (search_vector)"},
		"events_trgm_idx":                   {"gin_trgm_ops", "koro_norm(title)"},
		"event_staff_user_idx":              {"(user_id)"},
		"event_registrations_active_uq":     {"UNIQUE", "(event_id, user_id)", "'cancelled'", "'rejected'", "'expired'", "'withdrawn'"},
		"event_registrations_event_idx":     {"(event_id, status, created_at)"},
		"event_registrations_user_idx":      {"(user_id, created_at DESC)"},
		"event_registrations_waitlist_idx":  {"(event_id, waitlisted_at, id)", "status = 'waitlisted'"},
		"event_registrations_purge_idx":     {"(answers_purge_at) WHERE (answers_purged_at IS NULL)"},
		"registration_tickets_active_uq":    {"UNIQUE", "(registration_id) WHERE (status = 'active'"},
		"event_checkins_one_active_uq":      {"UNIQUE", "(registration_id) WHERE (voided_at IS NULL)"},
		"event_checkins_event_idx":          {"(event_id, scanned_at)"},
		"event_data_exports_event_idx":      {"(event_id, created_at DESC)"},
		"event_data_exports_expiry_idx":     {"(expires_at)", "status = 'ready'"},
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
	}
}

// TestMigrations_P1Schema2UpDownUp is TestMigrations_UpDownUp pinned to the
// range of this schema ticket: the migrations up to 000064 are applied to an
// empty database, reverted completely and applied again; down leaves nothing
// behind and the second up is identical to the first. (TestMigrations_UpDownUp
// runs to the newest migration; this one keeps proving 000064 once later
// schema tickets add files.)
func TestMigrations_P1Schema2UpDownUp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ownerURL := testutil.NewEmptyDB(t)
	m, err := migratekit.New(migrationsUpTo(t, 64), ownerURL, obs.Discard())
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
	if v, dirty, err := m.Version(); err != nil || dirty || v != 64 {
		t.Fatalf("version after up = %d, dirty %v, %v; want 64", v, dirty, err)
	}
	first := fingerprint(t, conn)
	for _, want := range []string{
		"rel public.student_profiles r", "rel public.profile_access_grants r", "rel public.entitlement_usages r",
		"rel public.event_form_versions r", "rel public.event_data_exports r", "fn count_segment(text[],smallint)",
		"con event_forms event_forms_current_version_id_fkey", "trg CREATE TRIGGER events_draft_delete_only",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("schema after up lacks %q", want)
		}
	}
	if err := m.Down(ctx, 64); err != nil {
		t.Fatalf("down 64: %v", err)
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
	var rules int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM form_lint_rules`).Scan(&rules); err != nil || rules != 19 {
		t.Errorf("lint rules after the second up = %d, %v; want 19 (the seed is part of up)", rules, err)
	}
}
