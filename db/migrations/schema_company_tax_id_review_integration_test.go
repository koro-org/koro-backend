//go:build integration

package migrations_test

import (
	"testing"

	"koro/internal/platform/testutil"
)

// TestSchema_CompanyTaxIDReview (BE-206, migration 000183): a company holds
// the time a member's new tax identifier started waiting for the platform
// admins (NULL: nothing to review), and only that: no copy of a number. The
// down migration drops the column; the up one brings it back.
func TestSchema_CompanyTaxIDReview(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		flagged, plain := w.org("company"), w.org("company")
		w.exec(`INSERT INTO companies (org_id) VALUES ($1), ($2)`, flagged, plain)
		expectOK(t, w.d.Pool, []p1aCase{
			{"a pending review", `UPDATE companies SET tax_id_kind = 'vkn', tax_no = '1234567890', tax_office = 'Kadıköy',
				tax_id_review_requested_at = now() WHERE org_id = $1`, []any{flagged}, "", ""},
			{"the decision clears it", `UPDATE companies SET tax_id_review_requested_at = NULL WHERE org_id = $1`,
				[]any{flagged}, "", ""},
		})
		if n := w.count(`SELECT count(*) FROM companies WHERE org_id = $1 AND tax_id_review_requested_at IS NULL`, plain); n != 1 {
			t.Fatalf("a new company has a pending review (%d)", n)
		}
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000183_company_tax_id_review.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'companies'
		AND column_name = 'tax_id_review_requested_at'`); n != 0 {
		t.Fatal("down kept companies.tax_id_review_requested_at")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000183_company_tax_id_review.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
