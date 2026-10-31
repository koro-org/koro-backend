//go:build integration

package migrations_test

import (
	"testing"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_CompanyTaxID (BE-165, migration 000173): a company keeps the
// kind of the tax identifier it gave, the VKN only with kind 'vkn' (a
// TCKN is never stored: kind 'tckn' has no number) and a trimmed tax
// office of 1–100 characters. The down migration drops the columns; the
// up one brings the rules back.
func TestSchema_CompanyTaxID(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	check := func(t *testing.T) {
		t.Helper()
		vkn, tckn, none := w.org("company"), w.org("company"), w.org("company")
		w.exec(`INSERT INTO companies (org_id) VALUES ($1), ($2), ($3)`, vkn, tckn, none)
		expectOK(t, w.d.Pool, []p1aCase{
			{"a VKN with its tax office", `UPDATE companies SET tax_id_kind = 'vkn', tax_no = '1234567890', tax_office = 'Kadıköy'
				WHERE org_id = $1`, []any{vkn}, "", ""},
			{"a TCKN kept as its kind only", `UPDATE companies SET tax_id_kind = 'tckn', tax_office = 'Çankaya' WHERE org_id = $1`,
				[]any{tckn}, "", ""},
		})
		expectFail(t, w.d.Pool, []p1aCase{
			{"an unknown kind", `UPDATE companies SET tax_id_kind = 'mersis' WHERE org_id = $1`, []any{none}, chk, "companies_tax_id_kind_chk"},
			{"kind vkn without a number", `UPDATE companies SET tax_id_kind = 'vkn' WHERE org_id = $1`, []any{none}, chk, "companies_tax_vkn_chk"},
			{"a number without a kind", `UPDATE companies SET tax_no = '1234567890' WHERE org_id = $1`, []any{none}, chk,
				"companies_tax_no_kind_chk"},
			{"a TCKN stored as the number", `UPDATE companies SET tax_no = '1000000014' WHERE org_id = $1`, []any{tckn}, chk,
				"companies_tax_no_kind_chk"},
			{"an 11-digit number", `UPDATE companies SET tax_no = '10000000146' WHERE org_id = $1`, []any{vkn}, chk, "companies_tax_no_chk"},
			{"a number with a separator", `UPDATE companies SET tax_no = '123456789-' WHERE org_id = $1`, []any{vkn}, chk, "companies_tax_no_chk"},
			{"an empty tax office", `UPDATE companies SET tax_office = '' WHERE org_id = $1`, []any{vkn}, chk, "companies_tax_office_chk"},
			{"an untrimmed tax office", `UPDATE companies SET tax_office = ' Kadıköy' WHERE org_id = $1`, []any{vkn}, chk,
				"companies_tax_office_chk"},
			{"a tax office of 101 characters", `UPDATE companies SET tax_office = repeat('a', 101) WHERE org_id = $1`, []any{vkn}, chk,
				"companies_tax_office_chk"},
		})
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000173_company_tax_id.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'companies'
		AND column_name IN ('tax_id_kind','tax_no','tax_office')`); n != 0 {
		t.Fatalf("down kept %d tax columns of companies", n)
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000173_company_tax_id.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
