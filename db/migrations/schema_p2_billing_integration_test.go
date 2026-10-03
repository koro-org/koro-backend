//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// invoiceDefaults are the columns of a planned service-sale invoice the
// payee uploads (model A); $1 id, $2 order, $3 issuer party, $4 recipient
// party, $5 idempotency key.
var invoiceDefaults = map[string]string{
	"id":                 "$1",
	"order_id":           "$2",
	"kind":               "'service_sale'",
	"issuer_party_id":    "$3",
	"recipient_party_id": "$4",
	"issuer_snapshot":    `'{"title":"Vakıf"}'::jsonb`,
	"recipient_snapshot": `'{"title":"Örnek A.Ş."}'::jsonb`,
	"issuing_mode":       "'seller_uploaded'",
	"trigger":            "'on_fulfillment'",
	"net_amount":         "1000000",
	"vat_amount":         "200000",
	"total_amount":       "1200000",
	"payable_amount":     "1200000",
	"idempotency_key":    "$5",
}

// TestInvoice_VerifierEqualsUploader_Check (06 H-19, T-66): the invoice
// verifier is not the uploader, in the database: verified_by = uploaded_by
// fails, and a verification without an uploader cannot slip through as
// NULL; an uploaded invoice is issued only once verified.
func TestInvoice_VerifierEqualsUploader_Check(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	s := w.shop()
	order, _ := w.order(s)
	issuer, recipient := s.seller.party, s.buyer.party
	uploader, verifier := w.user(), w.user()
	id := uuid.NewV7()
	w.exec(insertSQL("invoices", invoiceDefaults, nil), id, order, issuer, recipient, "service_sale:"+id.String())
	pdf := w.file("invoice_pdf")
	upload := `UPDATE invoices SET invoice_no = 'ABC2026000000001', issue_date = current_date, pdf_file_id = $2, uploaded_by = $3 WHERE id = $1`
	verify := `UPDATE invoices SET verified_by = $2, verified_at = now() WHERE id = $1`
	issue := `UPDATE invoices SET status = 'issued', issued_at = now() WHERE id = $1`
	expectFail(t, w.d.Pool, []p1aCase{
		{"verified before any upload", verify, []any{id, verifier}, chk, "invoices_verifier_chk"},
		{"issued without the document", issue, []any{id}, chk, "invoices_issued_chk"},
	})
	w.exec(upload, id, pdf, uploader)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the uploader verifies", verify, []any{id, uploader}, chk, "invoices_verifier_chk"},
		{"issued without verification", issue, []any{id}, chk, "invoices_manual_verified_chk"},
		{"partial: a verifier without the time", `UPDATE invoices SET verified_by = $2 WHERE id = $1`, []any{id, verifier}, chk, "invoices_verified_pair_chk"},
	})
	w.exec(verify, id, verifier)
	w.exec(issue, id)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the verifier becomes the uploader", `UPDATE invoices SET uploaded_by = $2 WHERE id = $1`, []any{id, verifier}, db.CodeRestrictViolation, ""},
	})
}

// TestSchema_InvoiceConstraints (02 §22.1–§22.2): the amounts add up, a
// credit note refers to another invoice, an invoice has a source and two
// different parties, an issued invoice carries number, date and document
// and is frozen; one invoice number per issuer.
func TestSchema_InvoiceConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	s := w.shop()
	order, line := w.order(s)
	issuer, recipient := s.seller.party, s.buyer.party
	ins := func(over map[string]string) string { return insertSQL("invoices", invoiceDefaults, over) }
	args := func() []any { id := uuid.NewV7(); return []any{id, order, issuer, recipient, "k:" + id.String()} }
	for _, c := range []struct {
		name, constraint string
		over             map[string]string
	}{
		{"a total that is not net + VAT", "invoices_total_chk", map[string]string{"total_amount": "1", "payable_amount": "1"}},
		{"a payable that ignores the withholding", "invoices_payable_chk", map[string]string{"vat_withheld_amount": "100000"}},
		{"withholding above the VAT", "invoices_withheld_chk", map[string]string{"vat_withheld_amount": "200001", "payable_amount": "999999"}},
		{"a credit note without its invoice", "invoices_credit_note_chk", map[string]string{"kind": "'credit_note'"}},
		{"an invoice without a source", "invoices_source_chk", map[string]string{"order_id": "NULLIF($2::uuid, $2::uuid)"}},
		{"the same party on both sides", "invoices_parties_chk", map[string]string{"recipient_party_id": "COALESCE($3::uuid, $4::uuid)"}},
		{"a provider invoice without a provider", "invoices_provider_mode_chk", map[string]string{"issuing_mode": "'provider'"}},
		{"issued without the number", "invoices_issued_chk", map[string]string{"issuing_mode": "'koro_on_behalf'", "status": "'issued'", "issued_at": "now()", "issue_date": "current_date"}},
		{"partial: planned with an issue time", "invoices_issued_at_chk", map[string]string{"issued_at": "now()"}},
		{"a snapshot that is not an object", "invoices_snapshots_chk", map[string]string{"issuer_snapshot": "'[]'::jsonb"}},
		{"a digest of the wrong size", "invoices_pdf_sha256_chk", map[string]string{"pdf_sha256": `'\x00'::bytea`}},
	} {
		expectFail(t, w.d.Pool, []p1aCase{{c.name, ins(c.over), args(), chk, c.constraint}})
	}
	issued := uuid.NewV7()
	pdf := w.file("invoice_pdf")
	w.exec(ins(map[string]string{"issuing_mode": "'koro_on_behalf'", "status": "'issued'", "issued_at": "now()", "issue_date": "current_date",
		"invoice_no": "'KOR2026000000001'", "pdf_file_id": "$6"}), issued, order, issuer, recipient, "k:issued", pdf)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the same number of the issuer", ins(map[string]string{"invoice_no": "'KOR2026000000001'"}), args(), db.CodeUniqueViolation, "invoices_no_uq"},
		{"the same idempotency key", insertSQL("invoices", invoiceDefaults, nil), []any{uuid.NewV7(), order, issuer, recipient, "k:issued"}, db.CodeUniqueViolation, "invoices_idempotency_key_uq"},
		{"an issued amount changes", `UPDATE invoices SET net_amount = 1, total_amount = 200001, payable_amount = 200001 WHERE id = $1`, []any{issued}, db.CodeRestrictViolation, ""},
		{"an issued number changes", `UPDATE invoices SET invoice_no = 'KOR2026000000002' WHERE id = $1`, []any{issued}, db.CodeRestrictViolation, ""},
		{"a credit note of itself", ins(map[string]string{"kind": "'credit_note'", "refers_to_invoice_id": "$1"}), args(), chk, "invoices_credit_note_chk"},
		{"app_rw deletes an invoice", `DELETE FROM invoices`, nil, db.CodeInsufficientPriv, ""},
	})
	w.exec(`UPDATE invoices SET status = 'cancelled' WHERE id = $1`, issued)
	w.exec(ins(map[string]string{"kind": "'credit_note'", "refers_to_invoice_id": "$6"}), uuid.NewV7(), order, issuer, recipient, "k:credit", issued)

	lines := `INSERT INTO invoice_lines (invoice_id, position, order_line_id, description, quantity, unit_price_amount, net_amount, vat_rate_bp, vat_amount,
		vat_withheld_amount, total_amount) VALUES ($1, $2, $3, 'Stant', 1, 1000000, 1000000, 2000, 200000, $4, $5)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"a line total that is not net + VAT", lines, []any{issued, 1, line, 0, 1}, chk, "invoice_lines_total_chk"},
		{"a line withheld above its VAT", lines, []any{issued, 1, line, 200001, 1200000}, chk, "invoice_lines_withheld_chk"},
		{"position zero", lines, []any{issued, 0, line, 0, 1200000}, chk, "invoice_lines_position_chk"},
	})
	w.exec(lines, issued, 1, line, 0, 1200000)
	expectFail(t, w.d.Pool, []p1aCase{
		{"two lines at one position", lines, []any{issued, 1, line, 0, 1200000}, db.CodeUniqueViolation, "invoice_lines_position_uq"},
		{"app_rw deletes a line", `DELETE FROM invoice_lines`, nil, db.CodeInsufficientPriv, ""},
	})
}

// TestSchema_BillingMapsAndCredentials (02 §22.3–§22.5): one provider id per
// party / service and provider; the provider lists are closed; the caches
// and the tokens are hard-deleted.
func TestSchema_BillingMapsAndCredentials(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	party, service := w.legalParty("payee", "foundation", nil), w.service()
	w.exec(`INSERT INTO provider_contact_map (legal_party_id, provider, provider_contact_id) VALUES ($1, 'parasut', 'c-1')`, party)
	w.exec(`INSERT INTO provider_product_map (service_id, provider, provider_product_id) VALUES ($1, 'parasut', 'p-1')`, service)
	w.exec(`INSERT INTO integration_credentials (provider, access_token_enc) VALUES ('parasut', '\x01')`)
	expectFail(t, w.d.Pool, []p1aCase{
		{"a second contact id at the provider", `INSERT INTO provider_contact_map (legal_party_id, provider, provider_contact_id) VALUES ($1, 'parasut', 'c-2')`, []any{party}, db.CodeUniqueViolation, "provider_contact_map_pkey"},
		{"an unknown contact provider", `INSERT INTO provider_contact_map (legal_party_id, provider, provider_contact_id) VALUES ($1, 'logo', 'c-2')`, []any{party}, db.CodeCheckViolation, "provider_contact_map_provider_chk"},
		{"a second product id", `INSERT INTO provider_product_map (service_id, provider, provider_product_id) VALUES ($1, 'parasut', 'p-2')`, []any{service}, db.CodeUniqueViolation, "provider_product_map_pkey"},
		{"iyzico as a product provider", `INSERT INTO provider_product_map (service_id, provider, provider_product_id) VALUES ($1, 'iyzico', 'p-2')`, []any{service}, db.CodeCheckViolation, "provider_product_map_provider_chk"},
		{"a second credential of the provider", `INSERT INTO integration_credentials (provider) VALUES ('parasut')`, nil, db.CodeUniqueViolation, "integration_credentials_pkey"},
		{"an unknown integration", `INSERT INTO integration_credentials (provider) VALUES ('stripe')`, nil, db.CodeCheckViolation, "integration_credentials_provider_chk"},
	})
	w.exec(`UPDATE integration_credentials SET refresh_token_enc = '\x02', rotated_at = now() WHERE provider = 'parasut'`)
	w.exec(`DELETE FROM provider_contact_map`)
	w.exec(`DELETE FROM provider_product_map`)
	w.exec(`DELETE FROM integration_credentials`)
}
