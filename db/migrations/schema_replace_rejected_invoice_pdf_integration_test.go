//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_ReplaceRejectedInvoicePDF (BE-154b, 00 T-397, migration
// 000164): an issued uploaded invoice keeps its parties, number, date and
// amounts frozen; its PDF changes only away from a rejected file, with its
// uploader, a later pdf_replaced_at and no verification; the reset
// verification is written once, never by the new uploader (06 H-19);
// otherwise the checksum, the uploader and the verification stay frozen.
// pdf_replaced_at is never set before the issue or at the insert. The
// down migration freezes the PDF again; the up one brings the rule back.
func TestSchema_ReplaceRejectedInvoicePDF(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	restrict, chk := db.CodeRestrictViolation, db.CodeCheckViolation
	const infected = `UPDATE files SET scan_status = 'infected' WHERE id = $1`
	// issued writes an issued seller-uploaded invoice of a new order: the
	// uploader, the verifier and the PDF.
	issued := func() (id, uploader, verifier, pdf uuid.UUID) {
		t.Helper()
		s := w.shop()
		order, _ := w.order(s)
		id, uploader, verifier, pdf = uuid.NewV7(), w.user(), w.user(), w.file("invoice_pdf")
		w.exec(insertSQL("invoices", invoiceDefaults, nil), id, order, s.seller.party, s.buyer.party, "service_sale:"+id.String())
		w.exec(`UPDATE invoices SET invoice_no = 'RPL'||substr(md5($1::text), 1, 12), issue_date = current_date, pdf_file_id = $2,
			uploaded_by = $3 WHERE id = $1`, id, pdf, uploader)
		w.exec(`UPDATE invoices SET verified_by = $2, verified_at = now() WHERE id = $1`, id, verifier)
		w.exec(`UPDATE invoices SET status = 'issued', issued_at = now() WHERE id = $1`, id)
		return id, uploader, verifier, pdf
	}
	const replace = `UPDATE invoices SET pdf_file_id = $2, uploaded_by = $3, verified_by = NULL, verified_at = NULL,
		pdf_sha256 = sha256('new'::bytea), pdf_replaced_at = now() WHERE id = $1`
	check := func(t *testing.T) {
		t.Helper()
		id, uploader, verifier, pdf := issued()
		replacer := w.user()
		expectFail(t, w.d.Pool, []p1aCase{
			{"a served PDF of an issued invoice is replaced", replace, []any{id, w.file("invoice_pdf"), replacer}, restrict, ""},
			{"pdf_replaced_at before a replacement", `UPDATE invoices SET pdf_replaced_at = now() WHERE id = $1`, []any{id}, restrict, ""},
			{"the verification is reset without a new PDF", `UPDATE invoices SET verified_by = NULL, verified_at = NULL WHERE id = $1`,
				[]any{id}, restrict, ""},
			{"the uploader changes without a new PDF", `UPDATE invoices SET uploaded_by = $2 WHERE id = $1`, []any{id, replacer}, restrict, ""},
		})
		w.exec(infected, pdf)
		next := w.file("invoice_pdf")
		expectFail(t, w.d.Pool, []p1aCase{
			{"a rejected PDF is dropped", `UPDATE invoices SET pdf_file_id = NULL, verified_by = NULL, verified_at = NULL,
				pdf_replaced_at = now() WHERE id = $1`, []any{id}, restrict, ""},
			{"a new PDF keeps the old verification", `UPDATE invoices SET pdf_file_id = $2, uploaded_by = $3, pdf_replaced_at = now()
				WHERE id = $1`, []any{id, next, replacer}, restrict, ""},
			{"a new PDF without pdf_replaced_at", `UPDATE invoices SET pdf_file_id = $2, uploaded_by = $3, verified_by = NULL,
				verified_at = NULL WHERE id = $1`, []any{id, next, replacer}, restrict, ""},
			{"the number with the PDF", `UPDATE invoices SET invoice_no = 'RPLX', pdf_file_id = $2, uploaded_by = $3, verified_by = NULL,
				verified_at = NULL, pdf_replaced_at = now() WHERE id = $1`, []any{id, next, replacer}, restrict, ""},
		})
		w.exec(replace, id, next, replacer)
		if n := w.count(`SELECT count(*) FROM invoices WHERE id = $1 AND status = 'issued' AND verified_by IS NULL AND pdf_replaced_at IS NOT NULL
			AND uploaded_by = $2 AND pdf_file_id = $3`, id, replacer, next); n != 1 {
			t.Fatal("the replaced invoice is not issued, unverified, with its new PDF and uploader")
		}
		expectFail(t, w.d.Pool, []p1aCase{
			{"the new uploader verifies", `UPDATE invoices SET verified_by = $2, verified_at = now() WHERE id = $1`, []any{id, replacer},
				chk, "invoices_verifier_chk"},
			{"the replacement is replaced while served", replace, []any{id, w.file("invoice_pdf"), w.user()}, restrict, ""},
		})
		w.exec(`UPDATE invoices SET verified_by = $2, verified_at = now() WHERE id = $1`, id, verifier)
		expectFail(t, w.d.Pool, []p1aCase{
			{"a second verification", `UPDATE invoices SET verified_by = $2, verified_at = now() WHERE id = $1`, []any{id, uploader}, restrict, ""},
		})

		// Before the issue the PDF is free (an upload), pdf_replaced_at is not.
		s := w.shop()
		order, _ := w.order(s)
		planned := uuid.NewV7()
		w.exec(insertSQL("invoices", invoiceDefaults, nil), planned, order, s.seller.party, s.buyer.party, "service_sale:"+planned.String())
		w.exec(`UPDATE invoices SET pdf_file_id = $2, uploaded_by = $3 WHERE id = $1`, planned, w.file("invoice_pdf"), w.user())
		w.exec(`UPDATE invoices SET pdf_file_id = $2 WHERE id = $1`, planned, w.file("invoice_pdf"))
		expectFail(t, w.d.Pool, []p1aCase{
			{"pdf_replaced_at before the issue", `UPDATE invoices SET pdf_replaced_at = now() WHERE id = $1`, []any{planned},
				chk, "invoices_pdf_replaced_chk"},
			{"pdf_replaced_at at the insert", insertSQL("invoices", invoiceDefaults, map[string]string{"pdf_replaced_at": "now()"}),
				[]any{uuid.NewV7(), order, s.seller.party, s.buyer.party, "k:replaced"}, chk, "invoices_pdf_replaced_chk"},
		})
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	id, _, _, pdf := issued()
	w.exec(infected, pdf)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000164_replace_rejected_invoice_pdf.down.sql")); err != nil {
		t.Fatal(err)
	}
	expectFail(t, w.d.Pool, []p1aCase{{"down: a rejected PDF of an issued invoice is frozen again",
		`UPDATE invoices SET pdf_file_id = $2, uploaded_by = $3, verified_by = NULL, verified_at = NULL WHERE id = $1`,
		[]any{id, w.file("invoice_pdf"), w.user()}, restrict, ""}})
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'invoices' AND column_name = 'pdf_replaced_at'`); n != 0 {
		t.Fatal("down kept invoices.pdf_replaced_at")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000164_replace_rejected_invoice_pdf.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
