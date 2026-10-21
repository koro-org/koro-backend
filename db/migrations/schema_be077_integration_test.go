//go:build integration

package migrations_test

import (
	"strings"
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// TestSchema_ProfileAccessGrantSuspension (000130, BE-077): a grant cannot
// be suspended before it was granted; a suspended grant may later be
// revoked; the company list index covers the active grants only.
func TestSchema_ProfileAccessGrantSuspension(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	student := w.student()
	company := w.org("company")
	base := map[string]string{"student_user_id": "$1", "company_org_id": "$2", "source": "'contact_request'", "source_id": "$3",
		"granted_at": "'2026-10-01T10:00:00Z'"}
	g := func(name string, over map[string]string, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("profile_access_grants", base, over), []any{student, company, uuid.NewV7()}, state, constraint}
	}
	expectFail(t, w.d.Pool, []p1aCase{
		g("suspended before it was granted", map[string]string{"suspended_at": "'2026-09-30T10:00:00Z'"},
			db.CodeCheckViolation, "profile_access_grants_suspended_chk"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		g("suspended at the grant time", map[string]string{"suspended_at": "'2026-10-01T10:00:00Z'"}, "", ""),
		g("suspended later", map[string]string{"suspended_at": "'2026-11-01T10:00:00Z'"}, "", ""),
		g("suspended, then revoked", map[string]string{"suspended_at": "'2026-11-01T10:00:00Z'",
			"revoked_at": "'2026-11-02T10:00:00Z'", "revoked_by_kind": lit("student")}, "", ""),
	})
	var def string
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes WHERE indexname = 'profile_access_grants_company_idx'`).
		Scan(&def); err != nil {
		t.Fatal(err)
	}
	if want := "WHERE ((revoked_at IS NULL) AND (suspended_at IS NULL))"; !strings.Contains(def, want) {
		t.Fatalf("company index = %s, want the predicate %s", def, want)
	}
}
