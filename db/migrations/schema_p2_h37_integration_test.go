//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// TestSchema_P2StateChecksRejectPartialRows (06 H-37, T-131): 02 wrote ten
// state CHECKs of the P2 money tables as (state) = (A AND B), which accepts
// a row outside the state that carries half of the state's columns
// (false = false). BE-034 ties every column to its state; each case below
// is a row the 02 form accepts and the migration rejects (the mutation run
// restored the 02 forms and saw exactly these cases pass).
func TestSchema_P2StateChecksRejectPartialRows(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	admin := w.user()
	s := w.shop()
	order, _ := w.order(s)

	// legal_parties: tckn (individual) and verification.
	party := `INSERT INTO legal_parties (kind, legal_type, title, tax_no, tckn_enc, verified_by) VALUES ('payee', 'foundation', 'Vakıf', $1, $2, $3)`
	// payee_bank_accounts: approval, rejection, revocation of a pending row.
	account := uuid.NewV7()
	w.exec(bankAccountSQL, account, s.seller.payee, s.seller.org, w.iban(), w.file("bank_account_evidence"), w.user())
	// collection_accounts, incoming_bank_transfers, payouts.
	ca := uuid.NewV7()
	w.exec(`INSERT INTO collection_accounts (id, label, legal_party_id, iban, holder_name, bank_name, created_by)
		VALUES ($1, 'Yedek', $2, $3, 'Koro', 'Banka', $4)`, ca, w.platformParty(), w.iban(), w.user())
	tr := w.transfer(w.collectionAccount(), nil)
	_, payable := w.payableLine(s)
	payout, _ := w.payout(s.seller, payable)
	w.exec(`UPDATE payouts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`, payout, admin)
	partialLine := lineStmt(s, uuid.NewV7(), order, map[string]string{"fulfilled_at": "now()"})

	// Each case runs in its own rolled-back transaction, so the cases are
	// independent of each other (and of the deferred order totals).
	expectTry(t, w, []p1aCase{
		{"legal_parties: a foundation with an encrypted TCKN", party, []any{w.taxNo(), []byte("enc"), nil}, chk, "legal_parties_tckn_chk"},
		{"legal_parties: a draft with a verifier", party, []any{w.taxNo(), nil, admin}, chk, "legal_parties_verified_chk"},
		{"payee_bank_accounts: pending with an approver", `UPDATE payee_bank_accounts SET approved_by = $2 WHERE id = $1`, []any{account, admin}, chk, "payee_bank_accounts_approved_chk"},
		{"payee_bank_accounts: pending with a rejecter", `UPDATE payee_bank_accounts SET rejected_by = $2 WHERE id = $1`, []any{account, admin}, chk, "payee_bank_accounts_rejected_chk"},
		{"payee_bank_accounts: pending with a revocation time", `UPDATE payee_bank_accounts SET revoked_at = now() WHERE id = $1`, []any{account}, chk, "payee_bank_accounts_revoked_chk"},
		{"payee_bank_accounts: pending with a revoke reason", `UPDATE payee_bank_accounts SET revoke_reason = 'x' WHERE id = $1`, []any{account}, chk, "payee_bank_accounts_revoked_chk"},
		{"collection_accounts: pending with an approver", `UPDATE collection_accounts SET approved_by = $2 WHERE id = $1`, []any{ca, admin}, chk, "collection_accounts_decided_chk"},
		{"incoming_bank_transfers: unmatched with an order", `UPDATE incoming_bank_transfers SET matched_order_id = $2 WHERE id = $1`, []any{tr, order}, chk, "incoming_bank_transfers_matched_chk"},
		{"order_lines: pending with a fulfilment time", partialLine.sql, partialLine.args, chk, "order_lines_fulfilled_chk"},
		{"payouts: approved with a payment time", `UPDATE payouts SET paid_at = now() WHERE id = $1`, []any{payout}, chk, "payouts_paid_chk"},
		{"payouts: approved with a receipt", `UPDATE payouts SET receipt_file_id = $2 WHERE id = $1`, []any{payout, w.file("payout_receipt")}, chk, "payouts_paid_chk"},
	})
}

// expectTry is expectFail with every case in a rolled-back transaction.
func expectTry(t *testing.T, w *p1aWorld, cases []p1aCase) {
	t.Helper()
	for _, c := range cases {
		state, constraint := w.try(c.sql, c.args...)
		if state != c.state || (c.constraint != "" && constraint != c.constraint) {
			t.Errorf("%s: SQLSTATE %q constraint %q, want %s %s", c.name, state, constraint, c.state, c.constraint)
		}
	}
}
