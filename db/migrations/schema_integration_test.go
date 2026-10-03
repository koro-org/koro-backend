//go:build integration

package migrations_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"koro/db/migrations"
	"koro/internal/platform/db"
	"koro/internal/platform/iban"
	"koro/internal/platform/iban/ibantest"
	"koro/internal/platform/migratekit"
	"koro/internal/platform/obs"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// schemaFingerprint lists every object the migrations own (relations with
// grants, columns, constraints, indexes, functions with grants and bodies,
// triggers, extensions, schemas with grants, collations, FTS configs). A
// NULL ACL is shown as its explicit default: REVOKE materialises it.
const schemaFingerprint = `
SELECT coalesce(string_agg(x, E'\n' ORDER BY x), '') FROM (
  SELECT 'rel ' || n.nspname || '.' || c.relname || ' ' || c.relkind::text || ' ' || coalesce(c.relacl, acldefault('r', c.relowner))::text
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
  UNION ALL
  SELECT 'col ' || table_schema || '.' || table_name || '.' || column_name || ' ' || data_type || ' ' ||
         coalesce(column_default, '') || ' ' || is_nullable || ' ' || coalesce(collation_name, '')
    FROM information_schema.columns WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
  UNION ALL
  SELECT 'con ' || conrelid::regclass || ' ' || conname || ' ' || pg_get_constraintdef(oid)
    FROM pg_constraint WHERE connamespace = 'public'::regnamespace
  UNION ALL
  SELECT 'idx ' || indexdef FROM pg_indexes WHERE schemaname NOT IN ('pg_catalog')
  UNION ALL
  SELECT 'fn ' || p.oid::regprocedure::text || ' ' || p.provolatile::text || p.prosecdef::text || ' ' ||
         coalesce(p.proacl, acldefault('f', p.proowner))::text || ' ' || md5(p.prosrc)
    FROM pg_proc p
   WHERE p.pronamespace = 'public'::regnamespace
     AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
  UNION ALL
  SELECT 'trg ' || pg_get_triggerdef(oid) FROM pg_trigger WHERE NOT tgisinternal
  UNION ALL
  SELECT 'ext ' || extname FROM pg_extension
  UNION ALL
  SELECT 'nsp ' || nspname || ' ' || pg_get_userbyid(nspowner) || ' ' || coalesce(nspacl, acldefault('n', nspowner))::text
    FROM pg_namespace WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
  UNION ALL
  SELECT 'coll ' || collname || ' ' || colllocale FROM pg_collation WHERE collnamespace = 'public'::regnamespace
  UNION ALL
  SELECT 'ts ' || cfgname FROM pg_ts_config WHERE cfgnamespace = 'public'::regnamespace
) s(x)`

func fingerprint(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var fp string
	if err := conn.QueryRow(t.Context(), schemaFingerprint).Scan(&fp); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fp
}

// TestMigrations_UpDownUp applies every migration to an empty database as
// app_owner, reverts all of them, applies them again (05 §7.5) and checks
// that down is complete and up is repeatable.
func TestMigrations_UpDownUp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ownerURL := testutil.NewEmptyDB(t)
	latest, err := migratekit.Latest(migrations.FS)
	if err != nil || latest == 0 {
		t.Fatalf("Latest() = %d, %v", latest, err)
	}

	m, err := migratekit.New(migrations.FS, ownerURL, obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	empty := fingerprint(t, conn) // schema_migrations exists already (golang-migrate)
	version := func() uint {
		t.Helper()
		v, dirty, err := m.Version()
		if err != nil || dirty {
			t.Fatalf("Version() = %d, dirty %v, %v", v, dirty, err)
		}
		return v
	}

	if err := m.Up(ctx); err != nil {
		t.Fatalf("first up: %v", err)
	}
	if v := version(); v != latest {
		t.Fatalf("version after up = %d, want %d", v, latest)
	}
	first := fingerprint(t, conn)
	for _, want := range []string{"rel public.outbox r", "rel public.files r", "trg CREATE TRIGGER audit_events_guard", "fn audit_purge_until(date)", "nsp talent app_owner"} {
		if !strings.Contains(first, want) {
			t.Errorf("schema after up lacks %q", want)
		}
	}

	if err := m.Down(ctx, int(latest)); err != nil {
		t.Fatalf("down %d: %v", latest, err)
	}
	if v := version(); v != 0 {
		t.Fatalf("version after full down = %d", v)
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
	if err := m.Up(ctx); err != nil { // nothing to do is not an error
		t.Fatalf("third up: %v", err)
	}
	if err := m.Down(ctx, 1); err != nil || version() != latest-1 {
		t.Fatalf("down 1: %v", err)
	}
	if err := m.Down(ctx, int(latest)); err == nil {
		t.Fatal("down more steps than applied must fail")
	}
}

func diff(a, b string) string {
	in := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, l := range strings.Split(s, "\n") {
			m[l] = true
		}
		return m
	}
	ma, mb := in(a), in(b)
	var out []string
	for l := range ma {
		if !mb[l] {
			out = append(out, "- "+l)
		}
	}
	for l := range mb {
		if !ma[l] {
			out = append(out, "+ "+l)
		}
	}
	return strings.Join(out, "\n")
}

// queryString runs a one-value query as app_rw.
func queryString(t *testing.T, d *db.DB, sql string, args ...any) string {
	t.Helper()
	var s *string
	if err := d.Pool.QueryRow(t.Context(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if s == nil {
		return "<NULL>"
	}
	return *s
}

// TestICU_LowerTurkishDottedI (T-40): the database is created with the ICU
// tr-TR locale, so lower()/upper(), ordering and FTS follow Turkish rules.
func TestICU_LowerTurkishDottedI(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	tests := []struct{ sql, want string }{
		{`SELECT datlocprovider::text || ':' || datlocale FROM pg_database WHERE datname = current_database()`, "i:tr-TR"},
		{`SELECT lower('I')`, "ı"},
		{`SELECT lower('İ')`, "i"},
		{`SELECT upper('i')`, "İ"},
		{`SELECT upper('ı')`, "I"},
		{`SELECT lower('IŞIK İSTANBUL ÇAĞLA')`, "ışık istanbul çağla"},
		{`SELECT lower('I' COLLATE "C")`, "i"}, // the non-Turkish rule, for contrast
		{`SELECT string_agg(x, ',' ORDER BY x) FROM unnest(ARRAY['d','ç','c','ı','i','ğ','g']) x`, "c,ç,d,g,ğ,ı,i"},
		{`SELECT string_agg(x, ',' ORDER BY x COLLATE public.tr) FROM unnest(ARRAY['Şeker','Sabun','Zeytin']) x`, "Sabun,Şeker,Zeytin"},
		{`SELECT (to_tsvector('koro_tr', 'İSTANBUL Üniversitesi') @@ websearch_to_tsquery('koro_tr', 'istanbul'))::text`, "true"},
		{`SELECT (to_tsvector('koro_tr', 'IŞIK') @@ websearch_to_tsquery('koro_tr', 'ışık'))::text`, "true"},
		{`SELECT ('ışık' = 'IŞIK' COLLATE public.tr_ci)::text`, "true"},
	}
	for _, tt := range tests {
		if got := queryString(t, d, tt.sql); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.sql, got, tt.want)
		}
	}
}

// TestKoroNorm_TurkishCases (T-41): koro_norm folds Turkish case and accents
// and is usable in a trigram index.
func TestKoroNorm_TurkishCases(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	cases := map[string]string{
		"IŞIK":                  "isik",
		"Işık":                  "isik",
		"İstanbul":              "istanbul",
		"İSTANBUL":              "istanbul",
		"Istanbul":              "istanbul", // ASCII I: Turkish lower gives ı, unaccent folds it
		"ÇAĞLA":                 "cagla",
		"Şükrü Öğüt":            "sukru ogut",
		"Boğaziçi Üniversitesi": "bogazici universitesi",
		"café":                  "cafe",
		"":                      "",
	}
	for in, want := range cases {
		if got := queryString(t, d, `SELECT koro_norm($1)`, in); got != want {
			t.Errorf("koro_norm(%q) = %q, want %q", in, got, want)
		}
	}
	if got := queryString(t, d, `SELECT koro_norm(NULL)`); got != "<NULL>" {
		t.Errorf("koro_norm(NULL) = %q", got)
	}
	// IMMUTABLE: usable in an expression index, which the planner matches.
	owner := testutil.OwnerConn(t, d)
	if _, err := owner.Exec(t.Context(), `
		CREATE TABLE names (name text COLLATE public.tr NOT NULL);
		CREATE INDEX names_trgm_idx ON names USING gin (koro_norm(name) gin_trgm_ops);
		INSERT INTO names SELECT 'Kulüp ' || g FROM generate_series(1, 2000) g;
		INSERT INTO names VALUES ('Işık Astronomi Kulübü');
		ANALYZE names;
		GRANT SELECT ON names TO app_rw`); err != nil {
		t.Fatal(err)
	}
	if got := queryString(t, d, `SELECT name FROM names WHERE koro_norm(name) % koro_norm($1) ORDER BY similarity(koro_norm(name), koro_norm($1)) DESC LIMIT 1`, "isik astronomi"); got != "Işık Astronomi Kulübü" {
		t.Errorf("trigram search found %q", got)
	}
	var plan strings.Builder
	rows, err := d.Pool.Query(t.Context(), `EXPLAIN SELECT name FROM names WHERE koro_norm(name) % koro_norm('isik')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "names_trgm_idx") {
		t.Errorf("trigram index not used:\n%s", plan.String())
	}
}

// TestKoroIbanTrValid_Vectors (T-42): the DB check gives the documented
// result on the vectors shared with Go internal/platform/iban (ibantest).
func TestKoroIbanTrValid_Vectors(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	vectors := ibantest.Vectors()
	if len(vectors) == 0 {
		t.Fatal("no IBAN vectors")
	}
	for _, v := range vectors {
		var got bool
		if err := d.Pool.QueryRow(t.Context(), `SELECT koro_iban_tr_valid($1)`, v.IBAN).Scan(&got); err != nil {
			t.Fatalf("%q: %v", v.IBAN, err)
		}
		if got != v.Valid {
			t.Errorf("koro_iban_tr_valid(%q) = %v, want %v (%s)", v.IBAN, got, v.Valid, v.Note)
		}
		if goValid := iban.Valid(v.IBAN); goValid != got {
			t.Errorf("%q: SQL %v, Go iban.Valid %v: the two implementations disagree", v.IBAN, got, goValid)
		}
	}
	var isNull bool
	if err := d.Pool.QueryRow(t.Context(), `SELECT koro_iban_tr_valid(NULL) IS NULL`).Scan(&isNull); err != nil || !isNull {
		t.Errorf("koro_iban_tr_valid(NULL) must be NULL (STRICT): %v", err)
	}
}

// execer is pgxpool.Pool, pgx.Tx or *pgx.Conn.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// execState runs sql and returns its SQLSTATE ("" on success).
func execState(t *testing.T, q execer, sql string, args ...any) string {
	t.Helper()
	_, err := q.Exec(t.Context(), sql, args...)
	return db.SQLState(err)
}

// inTx runs fn in a transaction of the app_rw pool and rolls it back.
func inTx(t *testing.T, d *db.DB, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := d.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	fn(tx)
}

// TestRoleBoundaries_AuditEvents (T-45): app_rw inserts (through
// audit_writer) and reads, never updates or deletes; the chainer role
// updates only the chain columns of unchained rows; even the owner cannot
// edit or delete outside audit_purge_until.
func TestRoleBoundaries_AuditEvents(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	insertEvent := func() uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		if _, err := d.Pool.Exec(ctx, `INSERT INTO audit_events (id, actor_kind, system_job, action, outcome)
			VALUES ($1, 'system', 'test', 'audit.test.run', 'success')`, id); err != nil {
			t.Fatalf("app_rw insert through audit_writer: %v", err)
		}
		return id
	}
	hash := make([]byte, 32)
	id := insertEvent()

	const denied = db.CodeInsufficientPriv
	for name, sql := range map[string]string{
		"update non-chain column": `UPDATE audit_events SET action = 'audit.test.edited' WHERE id = $1`,
		"update chain column":     `UPDATE audit_events SET chain_seq = 1, prev_hash = $2, row_hash = $2, chained_at = now() WHERE id = $1`,
		"delete":                  `DELETE FROM audit_events WHERE id = $1`,
	} {
		args := []any{id}
		if strings.Contains(sql, "$2") {
			args = append(args, hash)
		}
		if got := execState(t, d.Pool, sql, args...); got != denied {
			t.Errorf("app_rw %s: SQLSTATE %q, want %s", name, got, denied)
		}
	}
	if got := execState(t, d.Pool, `TRUNCATE audit_events`); got != denied {
		t.Errorf("app_rw truncate: SQLSTATE %q", got)
	}
	inTx(t, d, func(tx pgx.Tx) {
		if got := execState(t, tx, `SET LOCAL ROLE audit_writer`); got != denied {
			t.Errorf("SET ROLE audit_writer: SQLSTATE %q, want %s (membership is INHERIT only)", got, denied)
		}
	})

	// The chainer: chain columns of an unchained row only, once.
	inTx(t, d, func(tx pgx.Tx) {
		if got := execState(t, tx, `SET LOCAL ROLE audit_chainer`); got != "" {
			t.Fatalf("SET LOCAL ROLE audit_chainer: %s", got)
		}
		if got := execState(t, tx, `UPDATE audit_events SET chain_seq = 1, prev_hash = $2, row_hash = $2, chained_at = now() WHERE id = $1`, id, hash); got != "" {
			t.Fatalf("chainer chain update: SQLSTATE %q", got)
		}
		if got := execState(t, tx, `SAVEPOINT s1`); got != "" {
			t.Fatal(got)
		}
		if got := execState(t, tx, `UPDATE audit_events SET action = 'audit.test.edited' WHERE id = $1`, id); got != denied {
			t.Errorf("chainer non-chain update: SQLSTATE %q, want %s", got, denied)
		}
		_ = execState(t, tx, `ROLLBACK TO SAVEPOINT s1`)
		if got := execState(t, tx, `UPDATE audit_events SET row_hash = $2 WHERE id = $1`, id, make([]byte, 32)); got != db.CodeRestrictViolation {
			t.Errorf("chainer re-chaining a chained row: SQLSTATE %q, want %s (audit_events_guard)", got, db.CodeRestrictViolation)
		}
		_ = execState(t, tx, `ROLLBACK TO SAVEPOINT s1`)
		if got := execState(t, tx, `INSERT INTO audit_events (actor_kind, system_job, action, outcome) VALUES ('system','x','audit.x.y','success')`); got != denied {
			t.Errorf("chainer insert: SQLSTATE %q, want %s", got, denied)
		}
	})

	// The owner is stopped by audit_events_guard.
	owner := testutil.OwnerConn(t, d)
	for name, sql := range map[string]string{
		"update":   `UPDATE audit_events SET action = 'audit.test.edited' WHERE id = '` + id.String() + `'`,
		"delete":   `DELETE FROM audit_events WHERE id = '` + id.String() + `'`,
		"truncate": `TRUNCATE audit_events`,
	} {
		if got := execState(t, owner, sql); got != db.CodeRestrictViolation {
			t.Errorf("owner %s: SQLSTATE %q, want %s", name, got, db.CodeRestrictViolation)
		}
	}

	// Purge: only through audit_purge_until, only outside the retention
	// window, only up to a signed daily root.
	old := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)
	chained := []uuid.UUID{insertEvent(), insertEvent()}
	unchained := insertEvent()
	for i, cid := range chained {
		if _, err := owner.Exec(ctx, `UPDATE audit_events SET chain_seq = $2, prev_hash = $3, row_hash = $3, chained_at = now() WHERE id = $1`,
			cid, int64(i+1), hash); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO audit_daily_roots (day, last_chain_seq, root_hash, signature, key_id, external_ref)
		VALUES ($1, 2, $2, '\x00', 'k1', 'roots/2020-01-15')`, old, hash); err != nil {
		t.Fatalf("app_rw insert daily root: %v", err)
	}
	if got := execState(t, d.Pool, `UPDATE audit_daily_roots SET key_id = 'k2'`); got != denied {
		t.Errorf("app_rw update daily root: SQLSTATE %q", got)
	}
	inTx(t, d, func(tx pgx.Tx) {
		_ = execState(t, tx, `SET LOCAL koro.audit_purge = 'on'`)
		if got := execState(t, tx, `DELETE FROM audit_events`); got != denied {
			t.Errorf("app_rw delete with the purge flag: SQLSTATE %q, want %s", got, denied)
		}
	})
	if got := execState(t, d.Pool, `SELECT audit_purge_until(current_date - 30)`); got != "22023" {
		t.Errorf("purge inside the retention window: SQLSTATE %q, want 22023", got)
	}
	if got := execState(t, d.Pool, `SELECT audit_purge_until('2020-01-16')`); got != "22023" {
		t.Errorf("purge without a root: SQLSTATE %q, want 22023", got)
	}
	var deleted int64
	if err := d.Pool.QueryRow(ctx, `SELECT audit_purge_until($1)`, old).Scan(&deleted); err != nil || deleted != 2 {
		t.Fatalf("audit_purge_until = %d, %v; want the 2 rows with chain_seq <= 2", deleted, err)
	}
	rows, _ := d.Pool.Query(ctx, `SELECT id FROM audit_events ORDER BY seq`)
	left, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(left) != 2 || left[0] != id || left[1] != unchained {
		t.Fatalf("rows after purge = %v, %v; want only the two unchained ones", left, err)
	}
	var flag *string
	if err := d.Pool.QueryRow(ctx, `SELECT current_setting('koro.audit_purge', true)`).Scan(&flag); err != nil || (flag != nil && *flag == "on") {
		t.Fatalf("purge flag leaked out of the function: %v %v", flag, err)
	}
}

// TestRoleBoundaries_TalentSchemaNoAccess: app_rw has no privilege in the
// talent schema even while it is empty; only SET LOCAL ROLE app_talent
// enters it. app_rw cannot create objects anywhere.
func TestRoleBoundaries_TalentSchemaNoAccess(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	tests := []struct{ sql, want string }{
		{`SELECT has_schema_privilege('app_rw', 'talent', 'USAGE')::text`, "false"},
		{`SELECT has_schema_privilege('app_rw', 'talent', 'CREATE')::text`, "false"},
		{`SELECT has_schema_privilege('app_talent', 'talent', 'USAGE')::text`, "true"},
		{`SELECT has_schema_privilege('app_rw', 'analytics', 'USAGE')::text`, "false"},
		{`SELECT has_schema_privilege('app_ro', 'analytics', 'USAGE')::text`, "true"},
		{`SELECT has_schema_privilege('app_rw', 'public', 'CREATE')::text`, "false"},
		{`SELECT pg_has_role('app_rw', 'app_talent', 'USAGE')::text`, "false"}, // NOINHERIT
		{`SELECT pg_has_role('app_rw', 'app_talent', 'MEMBER')::text`, "true"}, // SET allowed
		{`SELECT pg_has_role('app_rw', 'audit_chainer', 'USAGE')::text`, "false"},
		{`SELECT pg_has_role('app_rw', 'audit_writer', 'USAGE')::text`, "true"},
		{`SELECT (SELECT rolsuper FROM pg_roles WHERE rolname = 'app_owner')::text`, "false"},
	}
	for _, tt := range tests {
		if got := queryString(t, d, tt.sql); got != tt.want {
			t.Errorf("%s = %s, want %s", tt.sql, got, tt.want)
		}
	}
	const denied = db.CodeInsufficientPriv
	for _, sql := range []string{
		`SELECT * FROM talent.point_events`,
		`CREATE TABLE talent.x (id int)`,
		`CREATE TABLE public.x (id int)`,
		`SELECT * FROM analytics.anything`,
	} {
		if got := execState(t, d.Pool, sql); got != denied {
			t.Errorf("app_rw %q: SQLSTATE %q, want %s", sql, got, denied)
		}
	}
	inTx(t, d, func(tx pgx.Tx) {
		if got := execState(t, tx, `SET LOCAL ROLE app_talent`); got != "" {
			t.Fatalf("SET LOCAL ROLE app_talent: %s", got)
		}
		var usage bool
		if err := tx.QueryRow(t.Context(), `SELECT has_schema_privilege('talent', 'USAGE')`).Scan(&usage); err != nil || !usage {
			t.Errorf("app_talent has no USAGE on talent: %v", err)
		}
		// The schema is empty: a lookup inside it now fails with "does not
		// exist" (42P01) instead of "permission denied".
		if got := execState(t, tx, `SELECT * FROM talent.point_events`); got != "42P01" {
			t.Errorf("app_talent lookup: SQLSTATE %q, want 42P01", got)
		}
	})
}

// TestHelperFunctions_Triggers: koro_touch_updated_at (files),
// koro_forbid_mutation and koro_policy_close_only on scratch tables.
func TestHelperFunctions_Triggers(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	fid := uuid.NewV7()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime)
		VALUES ($1, 'avatar', 'koro-files-dev', $2, 'image/png')`, fid, "avatar/2026/10/"+fid.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE files SET status = 'active' WHERE id = $1`, fid); db.SQLState(err) != db.CodeCheckViolation || db.ConstraintName(err) != "files_active_chk" {
		t.Errorf("active without sha256/size: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE files SET scan_status = 'skipped' WHERE id = $1`, fid); err != nil {
		t.Fatal(err)
	}
	if got := queryString(t, d, `SELECT (updated_at > created_at)::text FROM files WHERE id = $1`, fid); got != "true" {
		t.Errorf("koro_touch_updated_at did not stamp updated_at")
	}

	owner := testutil.OwnerConn(t, d)
	if _, err := owner.Exec(ctx, `
		CREATE TABLE ledger_like (id int PRIMARY KEY, amount bigint NOT NULL);
		CREATE TRIGGER ledger_like_immutable BEFORE UPDATE OR DELETE ON ledger_like
		  FOR EACH ROW EXECUTE FUNCTION koro_forbid_mutation();
		CREATE TRIGGER ledger_like_no_truncate BEFORE TRUNCATE ON ledger_like
		  FOR EACH STATEMENT EXECUTE FUNCTION koro_forbid_mutation();
		CREATE TABLE policy_like (id int PRIMARY KEY, rate_bp int NOT NULL, valid_to timestamptz);
		CREATE TRIGGER policy_like_close_only BEFORE UPDATE OR DELETE ON policy_like
		  FOR EACH ROW EXECUTE FUNCTION koro_policy_close_only();
		INSERT INTO ledger_like VALUES (1, 100);
		INSERT INTO policy_like VALUES (1, 1000, NULL), (2, 1000, NULL)`); err != nil {
		t.Fatal(err)
	}
	restricted := []string{
		`UPDATE ledger_like SET amount = 1`,
		`DELETE FROM ledger_like`,
		`TRUNCATE ledger_like`,
		`UPDATE policy_like SET rate_bp = 1 WHERE id = 1`,
		`UPDATE policy_like SET rate_bp = 1, valid_to = now() WHERE id = 1`,
		`DELETE FROM policy_like WHERE id = 1`,
	}
	for _, sql := range restricted {
		if got := execState(t, owner, sql); got != db.CodeRestrictViolation {
			t.Errorf("%s: SQLSTATE %q, want %s", sql, got, db.CodeRestrictViolation)
		}
	}
	if got := execState(t, owner, `UPDATE policy_like SET valid_to = now() WHERE id = 2`); got != "" {
		t.Errorf("closing an open policy: SQLSTATE %q", got)
	}
	if got := execState(t, owner, `UPDATE policy_like SET valid_to = now() + interval '1 day' WHERE id = 2`); got != db.CodeRestrictViolation {
		t.Errorf("re-closing a closed policy: SQLSTATE %q", got)
	}
}
