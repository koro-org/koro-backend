//go:build integration

package migrations_test

import (
	"testing"

	"koro/internal/platform/testutil"
)

// TestSchema_CompanyTaxIDRejection (BE-208, migration 000185): a company holds
// the time and the staff's reason of the rejection of its new tax identifier
// (3–500 characters), both or neither, never together with a pending review.
// The down migration drops both columns; the up one brings them back.
func TestSchema_CompanyTaxIDRejection(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		rejected, flagged, plain := w.org("company"), w.org("company"), w.org("company")
		w.exec(`INSERT INTO companies (org_id) VALUES ($1), ($2), ($3)`, rejected, flagged, plain)
		w.exec(`UPDATE companies SET tax_id_review_requested_at = now() WHERE org_id = $1`, flagged)
		expectOK(t, w.d.Pool, []p1aCase{
			{"a rejection with its reason", `UPDATE companies SET tax_id_rejected_at = now(),
				tax_id_rejection_reason = 'VKN unvanla eşleşmiyor' WHERE org_id = $1`, []any{rejected}, "", ""},
			{"the reason at its bounds", `UPDATE companies SET tax_id_rejection_reason = $2 WHERE org_id = $1`,
				[]any{rejected, repeat("ğ", 500)}, "", ""},
			{"a reactivation clears it", `UPDATE companies SET tax_id_rejected_at = NULL, tax_id_rejection_reason = NULL
				WHERE org_id = $1`, []any{rejected}, "", ""},
		})
		expectFail(t, w.d.Pool, []p1aCase{
			{"a reason too short", `UPDATE companies SET tax_id_rejected_at = now(), tax_id_rejection_reason = 'ab'
				WHERE org_id = $1`, []any{plain}, "23514", "companies_tax_id_rejection_reason_chk"},
			{"a reason too long", `UPDATE companies SET tax_id_rejected_at = now(), tax_id_rejection_reason = $2
				WHERE org_id = $1`, []any{plain, repeat("a", 501)}, "23514", "companies_tax_id_rejection_reason_chk"},
			{"a reason without its time", `UPDATE companies SET tax_id_rejection_reason = 'Gerekçe' WHERE org_id = $1`,
				[]any{plain}, "23514", "companies_tax_id_rejection_reason_at_chk"},
			{"a time without its reason", `UPDATE companies SET tax_id_rejected_at = now() WHERE org_id = $1`,
				[]any{plain}, "23514", "companies_tax_id_rejected_at_reason_chk"},
			{"a rejection while a review waits", `UPDATE companies SET tax_id_rejected_at = now(),
				tax_id_rejection_reason = 'Gerekçe' WHERE org_id = $1`, []any{flagged}, "23514",
				"companies_tax_id_rejection_review_chk"},
		})
		if n := w.count(`SELECT count(*) FROM companies WHERE org_id = $1 AND tax_id_rejected_at IS NULL
			AND tax_id_rejection_reason IS NULL`, plain); n != 1 {
			t.Fatalf("a new company carries a rejection (%d)", n)
		}
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000185_company_tax_id_rejection.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'companies'
		AND column_name IN ('tax_id_rejected_at', 'tax_id_rejection_reason')`); n != 0 {
		t.Fatalf("down kept %d rejection columns", n)
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000185_company_tax_id_rejection.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
