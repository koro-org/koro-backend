//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_ReplaceRejectedEvidence (BE-154, 00 T-393, Q-37, migration
// 000163): the evidence file of a financial or legal record stays frozen
// while it is served or still being scanned; once the file is rejected
// after its upload (status rejected, BE-059; scan infected, BE-048) the
// record takes another file in its place — never NULL — and nothing else
// of its frozen set thaws. Covered: the delivery evidence of an order line
// (order_lines_evidence_frozen), the receipt of a paid payout, the receipt
// of a transfer that cascades to its receivable collection, the evidence
// of a bank account (guarded from the insert on) and a party document (not
// frozen before: guarded once set). The down migration freezes them again
// (a rejected file stays too), the up one brings the rule back.
func TestSchema_ReplaceRejectedEvidence(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	// rejectSQL takes the file named after it out of use as BE-059 does
	// (files_active_chk: a rejected row has its checksum and size).
	const rejectSQL = `UPDATE files SET status = 'rejected', sha256 = sha256('rejected'::bytea), size_bytes = 42 WHERE id = `
	reject := func(id uuid.UUID, infected bool) {
		t.Helper()
		if infected { // BE-048: the scan found malware
			w.exec(`UPDATE files SET scan_status = 'infected' WHERE id = $1`, id)
			return
		}
		w.exec(rejectSQL+"$1", id)
	}
	frozen := func(name, sql string, args ...any) p1aCase {
		return p1aCase{name, sql, args, db.CodeRestrictViolation, ""}
	}
	check := func(t *testing.T) {
		t.Helper()
		// orders: the delivery evidence of a marked line.
		s := w.shop()
		order, line := w.order(s)
		w.markPaid(order)
		ev := w.file("delivery_evidence")
		w.exec(`UPDATE order_lines SET fulfillment_status = 'marked', fulfillment_marked_at = now(),
			dispute_window_ends_at = now() + interval '7 days', fulfillment_evidence_file_id = $2 WHERE id = $1`, line, ev)
		const lineEv = `UPDATE order_lines SET fulfillment_evidence_file_id = $2 WHERE id = $1`
		expectFail(t, w.d.Pool, []p1aCase{frozen("a served delivery evidence is replaced", lineEv, line, w.file("delivery_evidence"))})
		reject(ev, true)
		expectFail(t, w.d.Pool, []p1aCase{frozen("a rejected delivery evidence is dropped", lineEv, line, nil)})
		next := w.file("delivery_evidence")
		w.exec(lineEv, line, next)
		expectFail(t, w.d.Pool, []p1aCase{frozen("the replacement is replaced", lineEv, line, w.file("delivery_evidence"))})

		// ledger: the receipt of a paid payout; the rest stays frozen.
		_, _, payout := w.settledOrder(w.shop())
		const payoutRc = `UPDATE payouts SET receipt_file_id = $2 WHERE id = $1`
		expectFail(t, w.d.Pool, []p1aCase{frozen("a served payout receipt is replaced", payoutRc, payout, w.file("payout_receipt"))})
		w.exec(rejectSQL+"(SELECT receipt_file_id FROM payouts WHERE id = $1)", payout)
		expectFail(t, w.d.Pool, []p1aCase{
			frozen("a rejected payout receipt is dropped", payoutRc, payout, nil),
			frozen("the bank reference of a paid payout", `UPDATE payouts SET bank_reference = 'EFT-X', receipt_file_id = $2 WHERE id = $1`,
				payout, w.file("payout_receipt")),
		})
		w.exec(payoutRc, payout, w.file("payout_receipt"))

		// payments: the transfer receipt and, by the cascade, its collection's.
		c := w.receivableCollection(false)
		const transferRc = `UPDATE incoming_bank_transfers SET receipt_file_id = $2 WHERE id = $1`
		expectFail(t, w.d.Pool, []p1aCase{
			frozen("a served transfer receipt is replaced", transferRc, c.source, w.file("transfer_receipt")),
			frozen("a collection's receipt alone", `UPDATE receivable_collections SET receipt_file_id = $2 WHERE id = $1`,
				c.collection, w.file("transfer_receipt")),
		})
		w.exec(rejectSQL+"(SELECT receipt_file_id FROM incoming_bank_transfers WHERE id = $1)", c.source)
		rc := w.file("transfer_receipt")
		w.exec(transferRc, c.source, rc)
		if n := w.count(`SELECT count(*) FROM receivable_collections WHERE id = $1 AND receipt_file_id = $2`, c.collection, rc); n != 1 {
			t.Fatal("the collection did not follow its transfer's new receipt")
		}

		// parties: the bank account evidence (from the insert on) and a party document.
		payee := w.seller()
		const acctEv = `UPDATE payee_bank_accounts SET evidence_file_id = $2 WHERE id = $1`
		expectFail(t, w.d.Pool, []p1aCase{frozen("a served bank account evidence is replaced", acctEv, payee.account, w.file("bank_account_evidence"))})
		w.exec(rejectSQL+"(SELECT evidence_file_id FROM payee_bank_accounts WHERE id = $1)", payee.account)
		expectFail(t, w.d.Pool, []p1aCase{frozen("the IBAN with the evidence",
			`UPDATE payee_bank_accounts SET iban = $3, evidence_file_id = $2 WHERE id = $1`, payee.account, w.file("bank_account_evidence"), w.iban())})
		w.exec(acctEv, payee.account, w.file("bank_account_evidence"))
		doc, docFile := uuid.NewV7(), w.file("party_document")
		w.exec(`INSERT INTO legal_party_documents (id, legal_party_id, kind, file_id, uploaded_by) VALUES ($1, $2, 'tax_certificate', $3, $4)`,
			doc, payee.party, docFile, w.user())
		const docFileSQL = `UPDATE legal_party_documents SET file_id = $2 WHERE id = $1`
		expectFail(t, w.d.Pool, []p1aCase{frozen("a served party document is replaced", docFileSQL, doc, w.file("party_document"))})
		reject(docFile, false)
		w.exec(docFileSQL, doc, w.file("party_document"))
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	// The migrations built on 000163's guard go down before it and up after
	// it, in the migrate order (000164: invoices_pdf_guard, BE-154b).
	later := []string{"000164_replace_rejected_invoice_pdf"}
	for i := len(later) - 1; i >= 0; i-- {
		if _, err := owner.Exec(t.Context(), mustRead(t, later[i]+".down.sql")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000163_replace_rejected_evidence.down.sql")); err != nil {
		t.Fatal(err)
	}
	s := w.shop()
	order, line := w.order(s)
	w.markPaid(order)
	ev := w.file("delivery_evidence")
	w.exec(`UPDATE order_lines SET fulfillment_status = 'marked', fulfillment_marked_at = now(),
		dispute_window_ends_at = now() + interval '7 days', fulfillment_evidence_file_id = $2 WHERE id = $1`, line, ev)
	reject(ev, true)
	_, _, payout := w.settledOrder(w.shop())
	w.exec(rejectSQL+"(SELECT receipt_file_id FROM payouts WHERE id = $1)", payout)
	c := w.receivableCollection(false)
	expectFail(t, w.d.Pool, []p1aCase{
		frozen("down: a rejected delivery evidence is frozen again", `UPDATE order_lines SET fulfillment_evidence_file_id = $2 WHERE id = $1`,
			line, w.file("delivery_evidence")),
		frozen("down: a rejected payout receipt is frozen again", `UPDATE payouts SET receipt_file_id = $2 WHERE id = $1`,
			payout, w.file("payout_receipt")),
		{"down: the collection's source key does not cascade", `UPDATE incoming_bank_transfers SET receipt_file_id = $2 WHERE id = $1`,
			[]any{c.source, w.file("transfer_receipt")}, db.CodeForeignKeyViolation, "receivable_collections_source_fkey"},
	})
	if n := w.count(`SELECT count(*) FROM pg_proc WHERE proname = 'koro_evidence_file_guard'`); n != 0 {
		t.Fatal("down kept koro_evidence_file_guard")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000163_replace_rejected_evidence.up.sql")); err != nil {
		t.Fatal(err)
	}
	for _, m := range later {
		if _, err := owner.Exec(t.Context(), mustRead(t, m+".up.sql")); err != nil {
			t.Fatal(err)
		}
	}
	check(t)
}
