//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// TestSchema_CollectionAccountConstraints (02 §20.1, 03 §6.1): the Koro
// account is created by one admin and decided (approved or rejected) by
// another; it belongs to the platform party; one active default; an IBAN is
// open once; its IBAN and holder never change.
func TestSchema_CollectionAccountConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	platform := w.platformParty()
	creator, approver := w.user(), w.user()
	ins := `INSERT INTO collection_accounts (id, label, legal_party_id, iban, holder_name, bank_name, created_by, status, approved_by, approved_at)
		VALUES ($1, 'Ana hesap', $2, $3, 'Koro Teknoloji A.Ş.', 'Örnek Bankası', $4, $5, $6, $7)`
	payeeParty := w.legalParty("payee", "foundation", nil)
	expectFail(t, w.d.Pool, []p1aCase{
		{"born active", ins, []any{uuid.NewV7(), platform, w.iban(), creator, "active", approver, "2026-10-03T00:00:00Z"}, chk, "collection_accounts_initial_status_chk"},
		{"partial: pending with an approver", ins, []any{uuid.NewV7(), platform, w.iban(), creator, "pending_approval", approver, nil}, chk, "collection_accounts_decided_chk"},
		{"an account of a payee party", ins, []any{uuid.NewV7(), payeeParty, w.iban(), creator, "pending_approval", nil, nil}, db.CodeForeignKeyViolation, "collection_accounts_legal_party_id_party_kind_fkey"},
		{"an invalid IBAN", ins, []any{uuid.NewV7(), platform, "TR340006100519786457841326", creator, "pending_approval", nil, nil}, chk, "collection_accounts_iban_chk"},
	})
	id, number := uuid.NewV7(), w.iban()
	w.exec(ins, id, platform, number, creator, "pending_approval", nil, nil)
	decide := `UPDATE collection_accounts SET status = $2, approved_by = $3, approved_at = now() WHERE id = $1`
	expectFail(t, w.d.Pool, []p1aCase{
		{"the creator approves", decide, []any{id, "active", creator}, chk, "collection_accounts_four_eyes_chk"},
		{"the creator rejects", decide, []any{id, "rejected", creator}, chk, "collection_accounts_four_eyes_chk"},
		{"active without the decider", `UPDATE collection_accounts SET status = 'active' WHERE id = $1`, []any{id}, chk, "collection_accounts_decided_chk"},
		{"a default that is pending", `UPDATE collection_accounts SET is_default = true WHERE id = $1`, []any{id}, chk, "collection_accounts_default_chk"},
		{"the same IBAN opened twice", ins, []any{uuid.NewV7(), platform, number, creator, "pending_approval", nil, nil}, db.CodeUniqueViolation, "collection_accounts_iban_uq"},
		{"the IBAN changes", `UPDATE collection_accounts SET iban = $2 WHERE id = $1`, []any{id, w.iban()}, db.CodeRestrictViolation, ""},
		{"the creator is rewritten", `UPDATE collection_accounts SET created_by = $2 WHERE id = $1`, []any{id, approver}, db.CodeRestrictViolation, ""},
	})
	w.exec(decide, id, "active", approver)
	w.exec(`UPDATE collection_accounts SET is_default = true WHERE id = $1`, id)
	second := w.collectionAccount()
	expectFail(t, w.d.Pool, []p1aCase{
		{"two active defaults", `UPDATE collection_accounts SET is_default = true WHERE id = $1`, []any{second}, db.CodeUniqueViolation, "collection_accounts_default_uq"},
		{"retired without the time", `UPDATE collection_accounts SET status = 'retired', is_default = false WHERE id = $1`, []any{second}, chk, "collection_accounts_retired_chk"},
		{"partial: active with a retirement time", `UPDATE collection_accounts SET retired_at = now() WHERE id = $1`, []any{second}, chk, "collection_accounts_retired_chk"},
		{"back to pending while decided", `UPDATE collection_accounts SET status = 'pending_approval' WHERE id = $1`, []any{second}, chk, "collection_accounts_decided_chk"},
		{"app_rw deletes an account", `DELETE FROM collection_accounts`, nil, db.CodeInsufficientPriv, ""},
	})
	w.exec(`UPDATE collection_accounts SET status = 'retired', retired_at = now() WHERE id = $1`, second)
	rejected := uuid.NewV7()
	w.exec(ins, rejected, platform, w.iban(), creator, "pending_approval", nil, nil)
	w.exec(decide, rejected, "rejected", approver)
}

// TestSchema_IncomingTransferConstraints (02 §20.2): a transfer is recorded
// unmatched; the match columns belong to 'matched' (06 H-37); one bank
// reference per account; the bank data never changes; a matched transfer
// stays matched while its payment exists.
func TestSchema_IncomingTransferConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	account := w.collectionAccount()
	s := w.shop()
	order, _ := w.order(s)
	admin := w.user()
	expectFail(t, w.d.Pool, []p1aCase{
		{"recorded matched", insertSQL("incoming_bank_transfers", transferDefaults, map[string]string{"status": "'matched'", "matched_order_id": "$5",
			"matched_by": "$3", "matched_at": "now()"}), []any{uuid.NewV7(), account, admin, "R-1", order}, chk, "incoming_bank_transfers_initial_status_chk"},
		{"a zero amount", insertSQL("incoming_bank_transfers", transferDefaults, map[string]string{"amount": "0"}), []any{uuid.NewV7(), account, admin, "R-2"}, chk, "incoming_bank_transfers_amount_chk"},
		{"a malformed extracted reference", insertSQL("incoming_bank_transfers", transferDefaults, map[string]string{"extracted_order_no": "'KR12'"}), []any{uuid.NewV7(), account, admin, "R-3"}, chk, "incoming_bank_transfers_extracted_order_no_chk"},
	})
	tr := w.transfer(account, nil)
	ref := queryString(t, w.d, `SELECT bank_reference FROM incoming_bank_transfers WHERE id = $1`, tr)
	match := `UPDATE incoming_bank_transfers SET status = $2, matched_order_id = $3, matched_by = $4, matched_at = $5 WHERE id = $1`
	now := "2026-10-03T00:00:00Z"
	expectFail(t, w.d.Pool, []p1aCase{
		{"the same bank reference twice", insertSQL("incoming_bank_transfers", transferDefaults, nil), []any{uuid.NewV7(), account, admin, ref}, db.CodeUniqueViolation, "incoming_bank_transfers_account_reference_uq"},
		{"matched without the time", match, []any{tr, "matched", order, admin, nil}, chk, "incoming_bank_transfers_matched_chk"},
		{"matched without the order", match, []any{tr, "matched", nil, admin, now}, chk, "incoming_bank_transfers_matched_chk"},
		{"partial: unmatched with an order", match, []any{tr, "unmatched", order, nil, nil}, chk, "incoming_bank_transfers_matched_chk"},
		{"partial: unmatched with a matcher", match, []any{tr, "unmatched", nil, admin, nil}, chk, "incoming_bank_transfers_matched_chk"},
		{"ignored without a reason", `UPDATE incoming_bank_transfers SET status = 'ignored' WHERE id = $1`, []any{tr}, chk, "incoming_bank_transfers_ignored_chk"},
		{"partial: unmatched with an ignore reason", `UPDATE incoming_bank_transfers SET ignore_reason = 'Mükerrer' WHERE id = $1`, []any{tr}, chk, "incoming_bank_transfers_ignored_chk"},
		{"the amount changes", `UPDATE incoming_bank_transfers SET amount = 1 WHERE id = $1`, []any{tr}, db.CodeRestrictViolation, ""},
		{"the bank reference changes", `UPDATE incoming_bank_transfers SET bank_reference = 'X' WHERE id = $1`, []any{tr}, db.CodeRestrictViolation, ""},
		{"app_rw deletes a transfer", `DELETE FROM incoming_bank_transfers`, nil, db.CodeInsufficientPriv, ""},
	})
	w.exec(match, tr, "matched", order, admin, now)
	w.exec(`INSERT INTO payments (order_id, org_id, provider, incoming_transfer_id, amount, status, captured_at, captured_by)
		VALUES ($1, $2, 'bank_transfer', $3, 1200000, 'captured', now(), $4)`, order, s.buyer.org, tr, admin)
	expectFail(t, w.d.Pool, []p1aCase{
		{"unmatched under its payment", match, []any{tr, "unmatched", nil, nil, nil}, db.CodeForeignKeyViolation, "payments_incoming_transfer_id_order_id_fkey"},
	})
}

// TestFourEyes_TransferReturn_Check (06 H-18, T-60): sending an unmatched
// transfer back is a money exit with four eyes, as refunds: requester A,
// approver B ≠ A, then the EFT receipt. Every return column belongs to its
// state (06 H-37); the destination cannot change once approved and
// 'returned' is final.
func TestFourEyes_TransferReturn_Check(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	account := w.collectionAccount()
	a, b := w.user(), w.user()
	target := w.iban()
	receipt := w.file("transfer_receipt")
	expectFail(t, w.d.Pool, []p1aCase{
		{"recorded as returned", insertSQL("incoming_bank_transfers", transferDefaults, map[string]string{"status": "'returned'",
			"return_requested_by": "$3", "return_requested_at": "now()", "return_reason": "'İade'", "return_target_iban": "$5",
			"return_approved_by": "$6", "return_approved_at": "now()", "returned_at": "now()", "return_bank_reference": "'EFT-1'", "return_receipt_file_id": "$7"}),
			[]any{uuid.NewV7(), account, a, "R-9", target, b, receipt}, chk, "incoming_bank_transfers_initial_status_chk"},
	})
	tr := w.transfer(account, nil)
	request := `UPDATE incoming_bank_transfers SET status = 'return_requested', return_requested_by = $2, return_requested_at = now(),
		return_reason = $3, return_target_iban = $4 WHERE id = $1`
	expectFail(t, w.d.Pool, []p1aCase{
		{"a request without the destination", request, []any{tr, a, "Sipariş yok", nil}, chk, "incoming_bank_transfers_return_request_chk"},
		{"a request without a reason", request, []any{tr, a, nil, target}, chk, "incoming_bank_transfers_return_request_chk"},
		{"partial: unmatched with a return destination", `UPDATE incoming_bank_transfers SET return_target_iban = $2 WHERE id = $1`, []any{tr, target}, chk, "incoming_bank_transfers_return_request_chk"},
		{"partial: unmatched with a return receipt", `UPDATE incoming_bank_transfers SET return_receipt_file_id = $2 WHERE id = $1`, []any{tr, receipt}, chk, "incoming_bank_transfers_returned_chk"},
		{"an invalid destination", request, []any{tr, a, "Sipariş yok", "TR330006100519786457841327"}, chk, "incoming_bank_transfers_return_target_iban_chk"},
		{"approved without a request", `UPDATE incoming_bank_transfers SET status = 'return_approved', return_approved_by = $2, return_approved_at = now() WHERE id = $1`, []any{tr, b}, chk, "incoming_bank_transfers_return_request_chk"},
	})
	w.exec(request, tr, a, "Sipariş yok", target)
	approve := `UPDATE incoming_bank_transfers SET status = 'return_approved', return_approved_by = $2, return_approved_at = now() WHERE id = $1`
	expectFail(t, w.d.Pool, []p1aCase{
		{"the requester approves", approve, []any{tr, a}, chk, "incoming_bank_transfers_return_four_eyes_chk"},
		{"approved without the approver", `UPDATE incoming_bank_transfers SET status = 'return_approved', return_approved_at = now() WHERE id = $1`, []any{tr}, chk, "incoming_bank_transfers_return_approval_chk"},
		{"partial: requested with an approver", `UPDATE incoming_bank_transfers SET return_approved_by = $2, return_approved_at = now() WHERE id = $1`, []any{tr, b}, chk, "incoming_bank_transfers_return_approval_chk"},
		{"returned without approval", `UPDATE incoming_bank_transfers SET status = 'returned', returned_at = now(), return_bank_reference = 'EFT-1', return_receipt_file_id = $2 WHERE id = $1`, []any{tr, receipt}, chk, "incoming_bank_transfers_return_approval_chk"},
		{"matched while a return is requested", `UPDATE incoming_bank_transfers SET status = 'matched', matched_order_id = $2, matched_by = $3, matched_at = now() WHERE id = $1`,
			[]any{tr, func() uuid.UUID { o, _ := w.order(w.shop()); return o }(), b}, chk, "incoming_bank_transfers_return_request_chk"},
	})
	w.exec(approve, tr, b)
	complete := `UPDATE incoming_bank_transfers SET status = 'returned', returned_at = now(), return_bank_reference = $2, return_receipt_file_id = $3 WHERE id = $1`
	expectFail(t, w.d.Pool, []p1aCase{
		{"the destination changes after approval", `UPDATE incoming_bank_transfers SET return_target_iban = $2 WHERE id = $1`, []any{tr, w.iban()}, db.CodeRestrictViolation, ""},
		{"the approver is replaced", `UPDATE incoming_bank_transfers SET return_approved_by = $2 WHERE id = $1`, []any{tr, w.user()}, db.CodeRestrictViolation, ""},
		{"the requester is replaced", `UPDATE incoming_bank_transfers SET return_requested_by = $2 WHERE id = $1`, []any{tr, w.user()}, db.CodeRestrictViolation, ""},
		{"returned without the receipt", complete, []any{tr, "EFT-1", nil}, chk, "incoming_bank_transfers_returned_chk"},
		{"returned without the bank reference", complete, []any{tr, nil, receipt}, chk, "incoming_bank_transfers_returned_chk"},
	})
	w.exec(complete, tr, "EFT-1", receipt)
	expectFail(t, w.d.Pool, []p1aCase{
		{"a returned transfer reopened", `UPDATE incoming_bank_transfers SET status = 'return_approved', returned_at = NULL, return_bank_reference = NULL,
			return_receipt_file_id = NULL WHERE id = $1`, []any{tr}, db.CodeRestrictViolation, ""},
	})

	// A rejected request clears the return columns: the transfer is open again.
	other := w.transfer(account, nil)
	w.exec(request, other, a, "Yanlış hesap", target)
	w.exec(`UPDATE incoming_bank_transfers SET status = 'unmatched', return_requested_by = NULL, return_requested_at = NULL, return_reason = NULL,
		return_target_iban = NULL WHERE id = $1`, other)
}

// TestSchema_PaymentConstraints (02 §20.3, 06 H-37/H-38): a bank-transfer
// payment has the transfer that was matched to its order; the captured
// states carry captured_at; a manual capture names the admin; the refund
// states match refunded_amount; the order, provider and amount never change.
func TestSchema_PaymentConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	s := w.shop()
	order, _ := w.order(s)
	otherOrder, _ := w.order(s)
	admin := w.user()
	account := w.collectionAccount()
	tr := w.transfer(account, nil)
	w.exec(`UPDATE incoming_bank_transfers SET status = 'matched', matched_order_id = $2, matched_by = $3, matched_at = now() WHERE id = $1`, tr, order, admin)
	ins := `INSERT INTO payments (id, order_id, org_id, provider, incoming_transfer_id, amount, status, captured_at, captured_by, refunded_amount, failure_code)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	now := "2026-10-03T00:00:00Z"
	org := s.buyer.org
	expectFail(t, w.d.Pool, []p1aCase{
		{"a bank transfer without its transfer", ins, []any{uuid.NewV7(), order, org, "bank_transfer", nil, 1200000, "captured", now, admin, 0, nil}, chk, "payments_transfer_chk"},
		{"a manual payment with a transfer", ins, []any{uuid.NewV7(), order, org, "manual", tr, 1200000, "captured", now, admin, 0, nil}, chk, "payments_transfer_chk"},
		{"the transfer of another order", ins, []any{uuid.NewV7(), otherOrder, org, "bank_transfer", tr, 1200000, "captured", now, admin, 0, nil}, db.CodeForeignKeyViolation, "payments_incoming_transfer_id_order_id_fkey"},
		{"an unmatched transfer", ins, []any{uuid.NewV7(), order, org, "bank_transfer", w.transfer(account, nil), 1200000, "captured", now, admin, 0, nil}, db.CodeForeignKeyViolation, "payments_incoming_transfer_id_order_id_fkey"},
		{"a buyer other than the order's", ins, []any{uuid.NewV7(), order, w.org("company"), "bank_transfer", tr, 1200000, "captured", now, admin, 0, nil}, db.CodeForeignKeyViolation, "payments_order_id_org_id_fkey"},
		{"captured without the time", ins, []any{uuid.NewV7(), order, org, "bank_transfer", tr, 1200000, "captured", nil, nil, 0, nil}, chk, "payments_captured_chk"},
		{"partial: initiated with a capture time", ins, []any{uuid.NewV7(), order, org, "iyzico", nil, 1200000, "initiated", now, nil, 0, nil}, chk, "payments_captured_chk"},
		{"a manual capture without the admin", ins, []any{uuid.NewV7(), order, org, "bank_transfer", tr, 1200000, "captured", now, nil, 0, nil}, chk, "payments_manual_capture_chk"},
		{"a capturer without a capture", ins, []any{uuid.NewV7(), order, org, "iyzico", nil, 1200000, "initiated", nil, admin, 0, nil}, chk, "payments_captured_by_chk"},
		{"refunded with money left", ins, []any{uuid.NewV7(), order, org, "manual", nil, 1200000, "refunded", now, admin, 1000, nil}, chk, "payments_refunded_chk"},
		{"partially refunded with nothing refunded", ins, []any{uuid.NewV7(), order, org, "manual", nil, 1200000, "partially_refunded", now, admin, 0, nil}, chk, "payments_partially_refunded_chk"},
		{"refunded above the amount", ins, []any{uuid.NewV7(), order, org, "manual", nil, 1200000, "captured", now, admin, 1200001, nil}, chk, "payments_refunded_limit_chk"},
		{"failed without a code", ins, []any{uuid.NewV7(), order, org, "iyzico", nil, 1200000, "failed", nil, nil, 0, nil}, chk, "payments_failed_chk"},
		{"a zero payment", ins, []any{uuid.NewV7(), order, org, "manual", nil, 0, "initiated", nil, nil, 0, nil}, chk, "payments_amount_chk"},
	})
	id := uuid.NewV7()
	w.exec(ins, id, order, org, "bank_transfer", tr, 1200000, "captured", now, admin, 0, nil)
	expectFail(t, w.d.Pool, []p1aCase{
		{"a second payment of the transfer", ins, []any{uuid.NewV7(), order, org, "bank_transfer", tr, 1200000, "captured", now, admin, 0, nil}, db.CodeUniqueViolation, "payments_incoming_transfer_id_uq"},
		{"the amount changes", `UPDATE payments SET amount = 1 WHERE id = $1`, []any{id}, db.CodeRestrictViolation, ""},
		{"the order changes", `UPDATE payments SET order_id = $2 WHERE id = $1`, []any{id, otherOrder}, db.CodeRestrictViolation, ""},
		{"app_rw deletes a payment", `DELETE FROM payments`, nil, db.CodeInsufficientPriv, ""},
	})
	w.exec(`UPDATE payments SET status = 'partially_refunded', refunded_amount = 1000 WHERE id = $1`, id)
}

// TestSchema_PaymentSplitConstraints (02 §20.4, 06 H-38): a split's line
// belongs to its payment's order; one split per payment and line.
func TestSchema_PaymentSplitConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	order, line := w.order(s)
	otherOrder, otherLine := w.order(s)
	payment := w.payment(order, s.buyer.org)
	ins := `INSERT INTO payment_splits (payment_id, order_id, order_line_id, seller_amount, platform_amount, status, approved_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"a line of another order", ins, []any{payment, order, otherLine, 1080000, 120000, "released", nil}, db.CodeForeignKeyViolation, "payment_splits_order_line_id_order_id_fkey"},
		{"the payment of another order", ins, []any{payment, otherOrder, otherLine, 1080000, 120000, "released", nil}, db.CodeForeignKeyViolation, "payment_splits_payment_id_order_id_fkey"},
		{"a split of nothing", ins, []any{payment, order, line, 0, 0, "released", nil}, db.CodeCheckViolation, "payment_splits_amount_chk"},
		{"approved without the time", ins, []any{payment, order, line, 1080000, 120000, "approved", nil}, db.CodeCheckViolation, "payment_splits_approved_chk"},
		{"partial: held with an approval time", ins, []any{payment, order, line, 1080000, 120000, "held", "2026-10-03T00:00:00Z"}, db.CodeCheckViolation, "payment_splits_approved_chk"},
	})
	w.exec(ins, payment, order, line, 1080000, 120000, "released", nil)
	expectFail(t, w.d.Pool, []p1aCase{
		{"a second split of the line", ins, []any{payment, order, line, 1080000, 120000, "released", nil}, db.CodeUniqueViolation, "payment_splits_payment_line_uq"},
		{"the amounts change", `UPDATE payment_splits SET seller_amount = 1 WHERE order_line_id = $1`, []any{line}, db.CodeRestrictViolation, ""},
	})
}

// TestFourEyes_Refund_Check (03 §6.1, 02 §20.5): a refund is requested by A
// and approved by B ≠ A, never born approved; the destination IBAN is part
// of the request and cannot change; each state carries exactly its columns
// (06 H-37); 'completed' needs the receipt and is final.
func TestFourEyes_Refund_Check(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	s := w.shop()
	order, line := w.order(s)
	w.markPaid(order)
	payment := w.payment(order, s.buyer.org)
	otherOrder, otherLine := w.order(s)
	a, b := w.user(), w.user()
	target := w.iban()
	receipt := w.file("refund_receipt")
	ins := `INSERT INTO refunds (id, payment_id, order_id, order_line_id, amount, reason, requested_by, method, target_iban, status, approved_by, approved_at)
		VALUES ($1, $2, $3, $4, $5, 'not_fulfilled', $6, $7, $8, $9, $10, $11)`
	now := "2026-10-03T00:00:00Z"
	expectFail(t, w.d.Pool, []p1aCase{
		{"born approved (by another admin)", ins, []any{uuid.NewV7(), payment, order, line, 1000, a, "bank_eft", target, "approved", b, now}, chk, "refunds_initial_status_chk"},
		{"a bank refund without the destination", ins, []any{uuid.NewV7(), payment, order, line, 1000, a, "bank_eft", nil, "requested", nil, nil}, chk, "refunds_target_chk"},
		{"a card refund with an IBAN", ins, []any{uuid.NewV7(), payment, order, line, 1000, a, "iyzico", target, "requested", nil, nil}, chk, "refunds_target_chk"},
		{"partial: requested with an approver", ins, []any{uuid.NewV7(), payment, order, line, 1000, a, "bank_eft", target, "requested", b, nil}, chk, "refunds_approved_chk"},
		{"a zero refund", ins, []any{uuid.NewV7(), payment, order, line, 0, a, "bank_eft", target, "requested", nil, nil}, chk, "refunds_amount_chk"},
		{"a line of another order", ins, []any{uuid.NewV7(), payment, order, otherLine, 1000, a, "bank_eft", target, "requested", nil, nil}, db.CodeForeignKeyViolation, "refunds_order_line_id_order_id_fkey"},
		{"the payment of another order", ins, []any{uuid.NewV7(), payment, otherOrder, otherLine, 1000, a, "bank_eft", target, "requested", nil, nil}, db.CodeForeignKeyViolation, "refunds_payment_id_order_id_fkey"},
	})
	id := uuid.NewV7()
	w.exec(ins, id, payment, order, line, 1000, a, "bank_eft", target, "requested", nil, nil)
	approve := `UPDATE refunds SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`
	expectFail(t, w.d.Pool, []p1aCase{
		{"the requester approves", approve, []any{id, a}, chk, "refunds_four_eyes_chk"},
		{"approved without the approver", `UPDATE refunds SET status = 'approved', approved_at = now() WHERE id = $1`, []any{id}, chk, "refunds_approved_chk"},
		{"the destination changes before approval", `UPDATE refunds SET target_iban = $2 WHERE id = $1`, []any{id, w.iban()}, db.CodeRestrictViolation, ""},
		{"the amount changes", `UPDATE refunds SET amount = 999999 WHERE id = $1`, []any{id}, db.CodeRestrictViolation, ""},
		{"rejected without the rejecter", `UPDATE refunds SET status = 'rejected', rejected_at = now() WHERE id = $1`, []any{id}, chk, "refunds_rejected_chk"},
		{"partial: requested with a rejection note", `UPDATE refunds SET rejection_reason = 'Hayır' WHERE id = $1`, []any{id}, chk, "refunds_rejected_chk"},
		{"partial: requested with a bank reference", `UPDATE refunds SET bank_reference = 'EFT-1' WHERE id = $1`, []any{id}, chk, "refunds_completed_chk"},
		{"completed without approval", `UPDATE refunds SET status = 'completed', completed_at = now(), bank_reference = 'EFT-1', receipt_file_id = $2 WHERE id = $1`, []any{id, receipt}, chk, "refunds_approved_chk"},
		{"app_rw deletes a refund", `DELETE FROM refunds`, nil, db.CodeInsufficientPriv, ""},
	})
	w.exec(approve, id, b)
	complete := `UPDATE refunds SET status = 'completed', completed_at = now(), bank_reference = $2, receipt_file_id = $3 WHERE id = $1`
	expectFail(t, w.d.Pool, []p1aCase{
		{"the approver is replaced", `UPDATE refunds SET approved_by = $2 WHERE id = $1`, []any{id, w.user()}, db.CodeRestrictViolation, ""},
		{"completed without the receipt", complete, []any{id, "EFT-1", nil}, chk, "refunds_completed_chk"},
		{"completed without the bank reference", complete, []any{id, nil, receipt}, chk, "refunds_completed_chk"},
		{"failed without a reason", `UPDATE refunds SET status = 'failed', closed_by=$2, closed_at=now() WHERE id = $1`, []any{id, b}, chk, "refunds_failed_chk"},
	})
	w.exec(`UPDATE refunds SET status = 'processing', bank_reference = 'EFT-1' WHERE id = $1`, id)
	w.exec(complete, id, "EFT-1", receipt)
	expectFail(t, w.d.Pool, []p1aCase{
		{"a completed refund reopened", `UPDATE refunds SET status = 'processing', completed_at = NULL, receipt_file_id = NULL WHERE id = $1`, []any{id}, db.CodeRestrictViolation, ""},
	})

	// A rejected request: the rejecter and the time, the note optional.
	rej := uuid.NewV7()
	w.exec(ins, rej, payment, order, nil, 500, a, "bank_eft", target, "requested", nil, nil)
	w.exec(`UPDATE refunds SET status = 'rejected', rejected_by = $2, rejected_at = now() WHERE id = $1`, rej, b)
}
