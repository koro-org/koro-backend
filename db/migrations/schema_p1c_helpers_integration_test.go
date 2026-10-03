//go:build integration

package migrations_test

import (
	"maps"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/db/migrations"
	"koro/internal/platform/db"
)

// Test helpers of the P1 schema-3 tests (migrations 000065–000093: feed,
// outreach, messaging, talent, kvkk). They extend p1aWorld: every public row
// is built through the app_rw pool and every talent row through
// SET LOCAL ROLE app_talent, so a missing GRANT shows up as a failing
// fixture.

// postDefaults are the columns of a published public text post of a club;
// $1 = org, $2 = author org kind, $3 = author, $4 = post id.
var postDefaults = map[string]string{
	"id":                 "$4",
	"org_id":             "$1",
	"author_org_kind":    "$2",
	"author_user_id":     "$3",
	"kind":               "'text'",
	"title":              "'Kulüp duyurusu'",
	"body_json":          `'{"type":"doc","content":[]}'::jsonb`,
	"body_text":          "'Haftalık toplantı perşembe.'",
	"audience_kind":      "'public'",
	"audience_keys":      "ARRAY['all']",
	"audience_dim_count": "1",
	"status":             "'published'",
	"published_at":       "now()",
}

// companySegmentPost returns the overrides of a company post to a segment
// that satisfies posts_company_segment_chk, paid with the usage.
func companySegmentPost(usage uuid.UUID) map[string]string {
	return map[string]string{
		"commercial_flag":        "true",
		"audience_kind":          "'segment'",
		"audience_keys":          "ARRAY['uni:x','year:3']",
		"audience_dim_count":     "2",
		"audience_snapshot":      `'{"uni":["x"],"year":[3]}'::jsonb`,
		"audience_size_estimate": "120",
		"entitlement_usage_id":   lit(usage.String()),
	}
}

// post inserts a post of the org (kind = the org's kind; columns replaced
// by over) and returns its id.
func (w *p1aWorld) post(org uuid.UUID, kind string, author uuid.UUID, over map[string]string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(insertSQL("posts", postDefaults, over), org, kind, author, id)
	return id
}

// usage records a consumption of a fresh targeted_post quota of the company
// for ref and returns the entitlement_usages id.
func (w *p1aWorld) usage(company uuid.UUID, refType string) uuid.UUID {
	w.t.Helper()
	ent := w.entitlement(company, nil)
	id := uuid.NewV7()
	w.exec(`INSERT INTO entitlement_usages (id, entitlement_id, org_id, quantity, ref_type, ref_id) VALUES ($1, $2, $3, 1, $4, $5)`,
		id, ent, company, refType, uuid.NewV7())
	return id
}

// segment inserts a segment of the org and returns its id.
func (w *p1aWorld) segment(org, creator uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO segments (id, org_id, name, definition, audience_keys, audience_dim_count, created_by)
		VALUES ($1, $2, 'Üçüncü sınıflar', '{"year":[3]}', ARRAY['year:3'], 1, $3)`, id, org, creator)
	return id
}

// asTalent runs fn in a transaction of the app_rw pool after
// SET LOCAL ROLE app_talent (the talent store's way in, 02 §15) and commits
// it; fn's error is returned and rolls the transaction back.
func (w *p1aWorld) asTalent(fn func(tx pgx.Tx) error) error {
	w.t.Helper()
	tx, err := w.d.Pool.Begin(w.t.Context())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(w.t.Context()) }()
	if _, err := tx.Exec(w.t.Context(), `SET LOCAL ROLE app_talent`); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(w.t.Context())
}

// talentExec runs one statement as app_talent and fails the test on error.
func (w *p1aWorld) talentExec(sql string, args ...any) {
	w.t.Helper()
	if err := w.asTalent(func(tx pgx.Tx) error {
		_, err := tx.Exec(w.t.Context(), sql, args...)
		return err
	}); err != nil {
		w.t.Fatalf("app_talent %s: %v", sql, err)
	}
}

// talentState runs one statement as app_talent and returns its SQLSTATE
// ("" on success) and constraint name.
func (w *p1aWorld) talentState(sql string, args ...any) (state, constraint string) {
	w.t.Helper()
	err := w.asTalent(func(tx pgx.Tx) error {
		_, err := tx.Exec(w.t.Context(), sql, args...)
		return err
	})
	return db.SQLState(err), db.ConstraintName(err)
}

// expectTalentFail is expectFail for statements run as app_talent.
func expectTalentFail(t *testing.T, w *p1aWorld, cases []p1aCase) {
	t.Helper()
	for _, c := range cases {
		state, constraint := w.talentState(c.sql, c.args...)
		if state != c.state || (c.constraint != "" && constraint != c.constraint) {
			t.Errorf("%s: SQLSTATE %q constraint %q, want %s %s", c.name, state, constraint, c.state, c.constraint)
		}
	}
}

// configVersion inserts a talent config version (as app_talent) and returns
// its id.
func (w *p1aWorld) configVersion(creator uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.talentExec(`INSERT INTO talent.config_versions (id, effective_from, params, created_by)
		VALUES ($1, now() + make_interval(secs => $3), '{"shrink_k":5,"cohort_min":30}', $2)`, id, creator, float64(w.seq.Add(1)))
	return id
}

// pointDefaults are the columns of a check-in ledger row; $1 = user,
// $2 = config version, $3 = row id, $4 = source id.
var pointDefaults = map[string]string{
	"id":                "$3",
	"user_id":           "$1",
	"signal_code":       "'event_checkin'",
	"source_type":       "'checkin'",
	"source_id":         "$4",
	"occurred_at":       "now()",
	"raw_points_cp":     "500",
	"points_cp":         "500",
	"reason_code":       "'event_checkin'",
	"config_version_id": "$2",
	"source_event_id":   "gen_random_uuid()",
}

// pointSQL builds the ledger insert with overrides.
func pointSQL(over map[string]string) string {
	return insertSQL("talent.point_events", maps.Clone(pointDefaults), over)
}

// dsrDefaults are the columns of a received in-app access request;
// $1 = user.
var dsrDefaults = map[string]string{
	"user_id": "$1",
	"type":    "'access'",
	"channel": "'in_app'",
	"due_at":  "now() + interval '30 days'",
}

// mustRead returns the text of an embedded migration file.
func mustRead(t *testing.T, name string) string {
	t.Helper()
	src, err := migrations.FS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// talentPrivilege reports has_table_privilege(role, talent.<table>, priv).
// The table is found through pg_class: resolving the name "talent.x" as
// app_rw would itself fail, because the lookup needs USAGE on the schema.
func talentPrivilege(t *testing.T, w *p1aWorld, role, table, priv string) string {
	t.Helper()
	return queryString(t, w.d, `SELECT has_table_privilege($1, c.oid, $3)::text FROM pg_class c
		WHERE c.relnamespace = 'talent'::regnamespace AND c.relname = $2`, role, table, priv)
}
