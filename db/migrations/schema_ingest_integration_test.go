//go:build integration

package migrations_test

import (
	"strconv"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_Ingest (BE-210, migration 000187): the ingest tables.
//   - a source: a known kind and adapter, an http(s) URL once, a lower-case
//     host, an interval of 0 (once) or 1h–30d, enabled never with a pause
//     reason (H-37: one full CHECK per rule), a parent only for an events
//     source;
//   - an imported event: a known status and category, a hidden reason
//     exactly when hidden, a promoted event only when promoted, an end not
//     before its start, an expiry after its start, a 32-byte fingerprint
//     once;
//   - an imported club: one row per club and one key per university;
//   - the imported_event retention policy is seeded (unapproved, T-35);
//   - the down migration drops the tables and the policy, the up one brings
//     them back.
func TestSchema_Ingest(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		uni, _ := w.university()
		club := w.club(uni)
		parent := uuid.NewV7()
		const source = `INSERT INTO ingest_sources (id, kind, adapter, url, host, label, university_id, club_org_id, parent_id,
			interval_seconds, enabled, paused_reason) VALUES ($1, $2, $3, $4, $5, 'Kaynak', $6, $7, $8, $9, $10, $11)`
		url := func() string { return "https://h" + w.n() + ".example.org/e" }
		expectOK(t, w.d.Pool, []p1aCase{
			{name: "a club list", sql: source, args: []any{uuid.NewV7(), "club_list", "clublist_ytu", url(), "a.example.org", uni, nil, nil, 86400, true, nil}},
			{name: "a sitemap", sql: source, args: []any{parent, "events", "sitemap", url(), "b.example.org", uni, nil, nil, 21600, false, "robots"}},
			{name: "a page it named", sql: source, args: []any{uuid.NewV7(), "events", "htmlpage", url(), "b.example.org", uni, club, parent, 0, true, nil}},
		})
		dupURL := url()
		w.exec(source, uuid.NewV7(), "events", "rss", dupURL, "c.example.org", uni, nil, nil, 3600, false, nil)
		chk := db.CodeCheckViolation
		expectFail(t, w.d.Pool, []p1aCase{
			{name: "a URL twice", sql: source, args: []any{uuid.NewV7(), "events", "rss", dupURL, "c.example.org", uni, nil, nil, 3600, false, nil},
				state: db.CodeUniqueViolation, constraint: "ingest_sources_url_uq"},
			{name: "an ftp URL", sql: source, args: []any{uuid.NewV7(), "events", "rss", "ftp://x.example.org/", "x.example.org", uni, nil, nil, 3600, false, nil},
				state: chk, constraint: "ingest_sources_url_chk"},
			{name: "an upper-case host", sql: source, args: []any{uuid.NewV7(), "events", "rss", url(), "X.example.org", uni, nil, nil, 3600, false, nil},
				state: chk, constraint: "ingest_sources_host_chk"},
			{name: "an unknown adapter", sql: source, args: []any{uuid.NewV7(), "events", "scraper", url(), "x.example.org", uni, nil, nil, 3600, false, nil},
				state: chk, constraint: "ingest_sources_adapter_chk"},
			{name: "a ten-minute interval", sql: source, args: []any{uuid.NewV7(), "events", "rss", url(), "x.example.org", uni, nil, nil, 600, false, nil},
				state: chk, constraint: "ingest_sources_interval_chk"},
			{name: "enabled and paused", sql: source, args: []any{uuid.NewV7(), "events", "rss", url(), "x.example.org", uni, nil, nil, 3600, true, "admin"},
				state: chk, constraint: "ingest_sources_enabled_unpaused_chk"},
			{name: "a club list with a parent", sql: source, args: []any{uuid.NewV7(), "club_list", "clublist_boun", url(), "x.example.org", uni, nil, parent, 3600, false, nil},
				state: chk, constraint: "ingest_sources_parent_kind_chk"},
			{name: "an unknown university", sql: source, args: []any{uuid.NewV7(), "events", "rss", url(), "x.example.org", uuid.NewV7(), nil, nil, 3600, false, nil},
				state: db.CodeForeignKeyViolation, constraint: "ingest_sources_university_id_fkey"},
		})

		const event = `INSERT INTO imported_events (id, university_id, club_org_id, status, hidden_reason, title, starts_at, ends_at,
			category, source_url, evidence, confidence, fingerprint, promoted_event_id, expires_at, last_seen_at)
			VALUES ($1, $2, $3, $4, $5, 'Bahar Şenliği', now(), now() + $6::interval, $7, 'https://e.example.org/1', 'Bahar Şenliği 12 Mayıs',
			0.9, $8, $9, now() + $10::interval, now())`
		fp := bytes32(w.n())
		expectOK(t, w.d.Pool, []p1aCase{
			{name: "an imported event", sql: event, args: []any{uuid.NewV7(), uni, club, "imported", nil, "2 hours", "culture", fp, nil, "8 days"}},
			{name: "a hidden one", sql: event, args: []any{uuid.NewV7(), uni, nil, "hidden", "removal_request", "2 hours", "other", bytes32(w.n()), nil, "8 days"}},
			{name: "a promoted one whose draft is gone", sql: event, args: []any{uuid.NewV7(), uni, club, "promoted", nil, "2 hours", "talk", bytes32(w.n()), nil, "8 days"}},
		})
		expectFail(t, w.d.Pool, []p1aCase{
			{name: "a fingerprint twice", sql: event, args: []any{uuid.NewV7(), uni, nil, "imported", nil, "2 hours", "other", fp, nil, "8 days"},
				state: db.CodeUniqueViolation, constraint: "imported_events_fingerprint_uq"},
			{name: "hidden without a reason", sql: event, args: []any{uuid.NewV7(), uni, nil, "hidden", nil, "2 hours", "other", bytes32(w.n()), nil, "8 days"},
				state: chk, constraint: "imported_events_hidden_has_reason_chk"},
			{name: "a reason without hidden", sql: event, args: []any{uuid.NewV7(), uni, nil, "imported", "admin", "2 hours", "other", bytes32(w.n()), nil, "8 days"},
				state: chk, constraint: "imported_events_reason_only_hidden_chk"},
			{name: "a draft of an imported one", sql: event, args: []any{uuid.NewV7(), uni, nil, "imported", nil, "2 hours", "other", bytes32(w.n()), uuid.NewV7(), "8 days"},
				state: chk, constraint: "imported_events_promoted_only_chk"},
			{name: "an end before the start", sql: event, args: []any{uuid.NewV7(), uni, nil, "imported", nil, "-2 hours", "other", bytes32(w.n()), nil, "8 days"},
				state: chk, constraint: "imported_events_ends_chk"},
			{name: "expired at its start", sql: event, args: []any{uuid.NewV7(), uni, nil, "imported", nil, "2 hours", "other", bytes32(w.n()), nil, "0 days"},
				state: chk, constraint: "imported_events_expiry_chk"},
			{name: "a sensitive category", sql: event, args: []any{uuid.NewV7(), uni, nil, "imported", nil, "2 hours", "religious", bytes32(w.n()), nil, "8 days"},
				state: chk, constraint: "imported_events_category_chk"},
			{name: "a short fingerprint", sql: event, args: []any{uuid.NewV7(), uni, nil, "imported", nil, "2 hours", "other", make([]byte, 16), nil, "8 days"},
				state: chk, constraint: "imported_events_fingerprint_chk"},
		})

		const clubRow = `INSERT INTO imported_clubs (club_org_id, university_id, name_key, contact_email, first_seen_at, last_seen_at)
			VALUES ($1, $2, $3, $4, now(), now())`
		key := "kulup" + w.n()
		expectOK(t, w.d.Pool, []p1aCase{{name: "an imported club", sql: clubRow, args: []any{club, uni, key, "bilim@example.org"}}})
		expectFail(t, w.d.Pool, []p1aCase{
			{name: "a key twice", sql: clubRow, args: []any{w.club(uni), uni, key, nil}, state: db.CodeUniqueViolation, constraint: "imported_clubs_name_uq"},
			{name: "a malformed address", sql: clubRow, args: []any{w.club(uni), uni, "x" + w.n(), "Not An Address"}, state: chk,
				constraint: "imported_clubs_contact_email_chk"},
		})
		if n := w.count(`SELECT count(*) FROM retention_policies WHERE code = 'imported_event' AND owner_module = 'ingest'
			AND target = 'imported_events' AND NOT legal_approved`); n != 1 {
			t.Fatalf("imported_event policy rows = %d", n)
		}
		// app_rw: full rights on the state tables; ingest_runs is append-only
		// (no UPDATE: its purge takes no row lock, the store's DeleteRunsBefore).
		for table, privs := range map[string]map[string]bool{
			"ingest_sources":  {"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true},
			"ingest_runs":     {"SELECT": true, "INSERT": true, "UPDATE": false, "DELETE": true},
			"imported_clubs":  {"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true},
			"imported_events": {"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true},
		} {
			for priv, want := range privs {
				if got := queryString(t, w.d, `SELECT has_table_privilege('app_rw', $1, $2)::text`, table, priv); got != strconv.FormatBool(want) {
					t.Errorf("app_rw %s on %s = %s, want %v", priv, table, got, want)
				}
			}
		}
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000187_create_ingest.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN
		('ingest_sources','ingest_runs','imported_clubs','imported_events')`); n != 0 {
		t.Fatalf("down kept %d ingest tables", n)
	}
	if n := w.count(`SELECT count(*) FROM retention_policies WHERE code = 'imported_event'`); n != 0 {
		t.Fatal("down kept the imported_event policy")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000187_create_ingest.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
