//go:build integration

package migrations_test

import (
	"testing"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_EventExportFormat (BE-050, migration 000126): an export keeps
// the file type its requester chose, csv or xlsx (default xlsx); the down
// migration drops the column and the up one brings it back with the same
// rule.
func TestSchema_EventExportFormat(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	org, user := w.org("club"), w.user()
	ev := w.event(org, user, nil)
	base := map[string]string{
		"event_id": "$1", "org_id": "$2", "requested_by": "$3",
		"reason": lit("Katılımcı listesi yoklama için"), "fields": "ARRAY['first_name']",
		"expires_at": "now() + interval '7 days'",
	}
	x := func(name string, over map[string]string, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("event_data_exports", base, over), []any{ev, org, user}, state, constraint}
	}
	check := func(t *testing.T) {
		t.Helper()
		expectFail(t, w.d.Pool, []p1aCase{
			x("an unknown format", map[string]string{"format": lit("pdf")}, db.CodeCheckViolation, "event_data_exports_format_chk"),
			x("a null format", map[string]string{"format": "NULL"}, codeNotNull, ""),
		})
		expectOK(t, w.d.Pool, []p1aCase{
			x("a csv export", map[string]string{"format": lit("csv")}, "", ""),
			x("an xlsx export", map[string]string{"format": lit("xlsx")}, "", ""),
			x("the default format", nil, "", ""),
		})
	}
	check(t)
	if got := w.count(`SELECT count(*) FROM event_data_exports WHERE format = 'xlsx'`); got != 2 {
		t.Fatalf("xlsx exports = %d, want 2 (the default is xlsx)", got)
	}
	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000126_event_export_format.down.sql")); err != nil {
		t.Fatal(err)
	}
	if got := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'event_data_exports' AND column_name = 'format'`); got != 0 {
		t.Fatal("down kept the format column")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000126_event_export_format.up.sql")); err != nil {
		t.Fatal(err)
	}
	if got := w.count(`SELECT count(*) FROM event_data_exports WHERE format = 'xlsx'`); got != 3 {
		t.Fatalf("after down and up: %d xlsx exports, want every existing row (3) as xlsx", got)
	}
	check(t)
}
