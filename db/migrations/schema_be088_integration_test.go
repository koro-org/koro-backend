//go:build integration

package migrations_test

import (
	"strings"
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// TestSchema_ProfileAccessGrantResume (000135, BE-088): a grant cannot be
// resumed before it was granted, nor suspended before its resume; a
// resumed grant may be suspended again later and revoked; the expiry index
// covers the open suspended grants only.
func TestSchema_ProfileAccessGrantResume(t *testing.T) {
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
		g("resumed before it was granted", map[string]string{"resumed_at": "'2026-09-30T10:00:00Z'"},
			db.CodeCheckViolation, "profile_access_grants_resumed_chk"),
		g("suspended before its resume", map[string]string{"resumed_at": "'2026-11-01T10:00:00Z'",
			"suspended_at": "'2026-10-15T10:00:00Z'"}, db.CodeCheckViolation, "profile_access_grants_resumed_chk"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		g("resumed (active)", map[string]string{"resumed_at": "'2026-11-01T10:00:00Z'"}, "", ""),
		g("resumed at the grant time", map[string]string{"resumed_at": "'2026-10-01T10:00:00Z'"}, "", ""),
		g("resumed, then suspended again", map[string]string{"resumed_at": "'2026-11-01T10:00:00Z'",
			"suspended_at": "'2026-12-01T10:00:00Z'"}, "", ""),
		g("expired by the system", map[string]string{"suspended_at": "'2026-10-02T10:00:00Z'",
			"revoked_at": "'2027-01-01T10:00:00Z'", "revoked_by_kind": lit("system"), "revoke_reason": lit("suspension_expired")}, "", ""),
	})
	var def string
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes WHERE indexname = 'profile_access_grants_suspended_idx'`).
		Scan(&def); err != nil {
		t.Fatal(err)
	}
	if want := "(suspended_at) WHERE ((revoked_at IS NULL) AND (suspended_at IS NOT NULL))"; !strings.Contains(def, want) {
		t.Fatalf("expiry index = %s, want %s", def, want)
	}
}
