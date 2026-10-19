//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/platform/testutil"
)

// TestLedger_UnbalancedTxn_FailsAtCommit (T-44, 02 §21.3): the ledger is
// double-entry. A transaction whose entries do not sum to zero, that has a
// single entry or none, or that breaks the payee rule of its accounts is
// rejected by the deferred constraint trigger at COMMIT — also through the
// GORM transaction manager, where the error becomes problem.ErrValidation
// with the constraint name. Posting is idempotent per (kind, ref) and the
// journal is append-only for everyone.
func TestLedger_UnbalancedTxn_FailsAtCommit(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	s := w.shop()
	payee := s.seller.payee
	e := func(account string, amount int64) ledgerEntry { return ledgerEntry{account, amount} }

	expectCommit(t, w, "debit and credit differ", chk, "ledger_txn_balanced",
		ledgerTxn(uuid.NewV7(), "payment_received", nil, e("koro_bank", 120000), e("buyer_advances", -119999))...)
	expectCommit(t, w, "a single entry", chk, "ledger_txn_balanced",
		ledgerTxn(uuid.NewV7(), "payment_received", nil, e("koro_bank", 120000))...)
	expectCommit(t, w, "a header without entries", chk, "ledger_txn_balanced",
		ledgerTxn(uuid.NewV7(), "fee", nil)...)
	expectCommit(t, w, "a seller payable without its payee", chk, "ledger_entries_account_chk",
		ledgerTxn(uuid.NewV7(), "payout_paid", nil, e("seller_payable", 1080000), e("koro_bank", -1080000))...)
	misplaced := uuid.NewV7()
	expectCommit(t, w, "a payee on the bank account", chk, "ledger_entries_account_chk",
		st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES ($1, 'fee', 'manual', gen_random_uuid(), now())`, misplaced),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, payee_id, amount) VALUES ($1, 'koro_bank', $2, 100)`, misplaced, payee),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, amount) VALUES ($1, 'bank_fee_expense', -100)`, misplaced))
	expectCommit(t, w, "an entry of zero", chk, "ledger_entries_amount_chk",
		ledgerTxn(uuid.NewV7(), "adjustment", nil, e("koro_bank", 0))...)

	// Model A (02 §21.3): payment received, then allocated to the line.
	_, line := w.order(s)
	w.mustCommit(ledgerTxn(uuid.NewV7(), "payment_received", nil, e("koro_bank", 1200000), e("buyer_advances", -1200000))...)
	alloc := uuid.NewV7()
	w.mustCommit(
		st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES ($1, 'order_allocated', 'order', gen_random_uuid(), now())`, alloc),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, amount) VALUES ($1, 'buyer_advances', 1200000)`, alloc),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, payee_id, order_line_id, amount) VALUES ($1, 'seller_payable', $2, $3, -1080000)`, alloc, payee, line),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, order_line_id, amount) VALUES ($1, 'platform_commission_revenue', $2, -100000)`, alloc, line),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, order_line_id, amount) VALUES ($1, 'vat_output', $2, -20000)`, alloc, line))
	if got := queryString(t, w.d, `SELECT (-sum(amount))::text FROM ledger_entries WHERE account_code = 'seller_payable' AND payee_id = $1`, payee); got != "1080000" {
		t.Errorf("payee balance = %s, want 1080000", got)
	}
	other := w.seller()
	wrong := uuid.NewV7()
	expectCommit(t, w, "an entry naming a line of another payee", db.CodeForeignKeyViolation, "ledger_entries_order_line_id_payee_id_fkey",
		st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES ($1, 'adjustment', 'manual', gen_random_uuid(), now())`, wrong),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, payee_id, order_line_id, amount) VALUES ($1, 'seller_payable', $2, $3, 1)`, wrong, other.payee, line))
	expectCommit(t, w, "entries appended to a committed transaction", chk, "ledger_txn_balanced",
		st(`INSERT INTO ledger_entries (transaction_id, account_code, amount) VALUES ($1, 'koro_bank', 5)`, alloc))

	// An inactive account takes no new entries.
	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), `UPDATE ledger_accounts SET active = false WHERE code = 'psp_clearing'`); err != nil {
		t.Fatal(err)
	}
	expectCommit(t, w, "an entry on an inactive account", chk, "ledger_entries_account_chk",
		ledgerTxn(uuid.NewV7(), "adjustment", nil, e("psp_clearing", 10), e("koro_bank", -10))...)

	// Through the GORM transaction manager: the deferred error arrives from
	// WithTx as problem.ErrValidation, wrapping the constraint name.
	ctx := t.Context()
	gormTxn := uuid.NewV7()
	err := w.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		conn := db.Conn(ctx, w.d.ORM)
		if err := conn.Exec(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES ( ?, 'fee', 'manual', gen_random_uuid(), now())`, gormTxn).Error; err != nil {
			return err
		}
		if err := conn.Exec(`INSERT INTO ledger_entries (transaction_id, account_code, amount) VALUES ( ?, 'bank_fee_expense', 250)`, gormTxn).Error; err != nil {
			return err
		}
		return conn.Exec(`INSERT INTO ledger_entries (transaction_id, account_code, amount) VALUES ( ?, 'koro_bank', -200)`, gormTxn).Error
	})
	if !errors.Is(err, problem.ErrValidation) || db.ConstraintName(err) != "ledger_txn_balanced" || db.SQLState(err) != chk {
		t.Errorf("WithTx with an unbalanced transaction = %v (constraint %q), want problem.ErrValidation / ledger_txn_balanced", err, db.ConstraintName(err))
	}
	if n := w.count(`SELECT count(*) FROM ledger_transactions WHERE id = $1`, gormTxn); n != 0 {
		t.Errorf("the unbalanced transaction was stored")
	}

	// Idempotent posting: one transaction per (kind, ref_type, ref_id), also
	// under concurrency.
	ref := uuid.NewV7()
	post := func() error {
		tx, err := w.d.Pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		id := uuid.NewV7()
		for _, s := range []stmt{
			st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES ($1, 'payment_received', 'payment', $2, now())`, id, ref),
			st(`INSERT INTO ledger_entries (transaction_id, account_code, amount) VALUES ($1, 'koro_bank', 500)`, id),
			st(`INSERT INTO ledger_entries (transaction_id, account_code, amount) VALUES ($1, 'buyer_advances', -500)`, id),
		} {
			if _, err := tx.Exec(t.Context(), s.sql, s.args...); err != nil {
				return err
			}
		}
		return tx.Commit(t.Context())
	}
	var wg sync.WaitGroup
	results := make([]error, 10)
	for i := range results {
		wg.Go(func() { results[i] = post() })
	}
	wg.Wait()
	stored := 0
	for _, err := range results {
		switch {
		case err == nil:
			stored++
		case db.SQLState(err) != db.CodeUniqueViolation || db.ConstraintName(err) != "ledger_transactions_ref_uq":
			t.Errorf("concurrent posting: %v", err)
		}
	}
	if stored != 1 {
		t.Errorf("concurrent postings of one event stored = %d, want 1", stored)
	}

	// Append-only for app_rw (no grant) and for the owner (trigger).
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw updates an entry", `UPDATE ledger_entries SET amount = amount * 2`, nil, db.CodeInsufficientPriv, ""},
		{"app_rw deletes an entry", `DELETE FROM ledger_entries`, nil, db.CodeInsufficientPriv, ""},
		{"app_rw updates a transaction", `UPDATE ledger_transactions SET memo = 'x'`, nil, db.CodeInsufficientPriv, ""},
	})
	expectFail(t, owner, []p1aCase{
		{"the owner updates an entry", `UPDATE ledger_entries SET amount = amount * 2`, nil, db.CodeRestrictViolation, ""},
		{"the owner deletes an entry", `DELETE FROM ledger_entries`, nil, db.CodeRestrictViolation, ""},
		{"the owner updates a transaction", `UPDATE ledger_transactions SET memo = 'x'`, nil, db.CodeRestrictViolation, ""},
		{"the owner truncates the journal", `TRUNCATE ledger_entries, ledger_transactions, receivable_collections`, nil, db.CodeRestrictViolation, ""},
	})
	if got := queryString(t, w.d, `SELECT coalesce(sum(amount), 0)::text FROM ledger_entries`); got != "0" {
		t.Errorf("the whole ledger sums to %s, want 0", got)
	}
}

// TestFourEyes_Payout_SameApprover_Check (03 §6.1, 02 §21.4): a payout is
// created by finance admin A and approved by B ≠ A; it is never born
// approved; it goes only to the payee's approved account with the exact
// IBAN, and is approved only while that account is still approved; the
// paid state needs receipt and bank reference, marks the lines paid out in
// the same transaction and is final.
func TestFourEyes_Payout_SameApprover_Check(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	s := w.shop()
	_, line := w.payableLine(s)
	seller := s.seller
	id, creator := w.payout(seller, line)
	b := w.user()
	approve := `UPDATE payouts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`
	expectFail(t, w.d.Pool, []p1aCase{
		{"the creator approves", approve, []any{id, creator}, chk, "payouts_four_eyes_chk"},
		{"approved without the approver", `UPDATE payouts SET status = 'approved', approved_at = now() WHERE id = $1`, []any{id}, chk, "payouts_approved_chk"},
		{"partial: pending with an approver", `UPDATE payouts SET approved_by = $2, approved_at = now() WHERE id = $1`, []any{id, b}, chk, "payouts_approved_chk"},
		{"paid without approval", `UPDATE payouts SET status = 'paid', paid_at = now(), paid_by = $2, bank_reference = 'EFT-1', receipt_file_id = $3 WHERE id = $1`,
			[]any{id, b, w.file("payout_receipt")}, chk, "payouts_approved_chk"},
		{"the amount changes", `UPDATE payouts SET amount = 1 WHERE id = $1`, []any{id}, db.CodeRestrictViolation, ""},
		{"the IBAN snapshot changes", `UPDATE payouts SET iban_snapshot = $2 WHERE id = $1`, []any{id, w.iban()}, db.CodeRestrictViolation, ""},
		{"the creator is rewritten", `UPDATE payouts SET created_by = $2 WHERE id = $1`, []any{id, b}, db.CodeRestrictViolation, ""},
		{"app_rw deletes a payout", `DELETE FROM payouts`, nil, db.CodeInsufficientPriv, ""},
	})
	born := uuid.NewV7()
	_, line2 := w.payableLine(s)
	bornStmts := payoutStmts(seller, born, w.user(), 1080000, map[uuid.UUID]int64{line2: 1080000})
	bornStmts[0] = st(`INSERT INTO payouts (id, payee_id, org_id, bank_account_id, iban_snapshot, holder_snapshot, amount, created_by, status, approved_by, approved_at)
		VALUES ($1, $2, $3, $4, $5, $6, 1080000, $7, 'approved', $8, now())`, born, seller.payee, seller.org, seller.account, seller.iban, seller.holder, w.user(), b)
	expectCommit(t, w, "born approved (by another admin)", chk, "payouts_initial_status_chk", bornStmts...)

	w.exec(approve, id, b)
	receipt := w.file("payout_receipt")
	pay := st(`UPDATE payouts SET status = 'paid', paid_at = now(), paid_by = $2, bank_reference = $3, receipt_file_id = $4 WHERE id = $1`, id, creator, "EFT-1", receipt)
	expectFail(t, w.d.Pool, []p1aCase{
		{"the approver is replaced", `UPDATE payouts SET approved_by = $2 WHERE id = $1`, []any{id, w.user()}, db.CodeRestrictViolation, ""},
		{"paid without the receipt", `UPDATE payouts SET status = 'paid', paid_at = now(), paid_by = $2, bank_reference = 'EFT-1' WHERE id = $1`, []any{id, creator}, chk, "payouts_paid_chk"},
		{"paid without the bank reference", `UPDATE payouts SET status = 'paid', paid_at = now(), paid_by = $2, receipt_file_id = $3 WHERE id = $1`, []any{id, creator, receipt}, chk, "payouts_paid_chk"},
		{"partial: approved with a receipt", `UPDATE payouts SET receipt_file_id = $2 WHERE id = $1`, []any{id, receipt}, chk, "payouts_paid_chk"},
		{"partial: approved with a bank reference", `UPDATE payouts SET bank_reference = 'EFT-1' WHERE id = $1`, []any{id}, chk, "payouts_paid_chk"},
		{"failed without a reason", `UPDATE payouts SET status = 'failed' WHERE id = $1`, []any{id}, chk, "payouts_failed_chk"},
		{"cancelled without a reason", `UPDATE payouts SET status = 'cancelled', cancelled_at = now() WHERE id = $1`, []any{id}, chk, "payouts_cancelled_chk"},
		{"partial: approved with a cancel reason", `UPDATE payouts SET cancel_reason = 'Vazgeçildi' WHERE id = $1`, []any{id}, chk, "payouts_cancelled_chk"},
	})
	expectCommit(t, w, "paid without marking the lines", chk, "payouts_lines_paid_out", pay)
	w.mustCommit(pay, st(`UPDATE order_lines SET payout_id = $2, paid_out_at = now() WHERE id = $1`, line, id))
	expectFail(t, w.d.Pool, []p1aCase{
		{"a paid payout reopened", `UPDATE payouts SET status = 'failed', paid_at = NULL, paid_by = NULL, receipt_file_id = NULL, failure_reason = 'Geri döndü' WHERE id = $1`, []any{id}, db.CodeRestrictViolation, ""},
		{"a paid line released", `UPDATE order_lines SET payout_id = NULL, paid_out_at = NULL WHERE id = $1`, []any{line}, db.CodeRestrictViolation, ""},
	})

	// The account: approved, exact snapshot, still approved at approval.
	_, line3 := w.payableLine(s)
	newPayout := func(account uuid.UUID, number, holder string) []stmt {
		sw := seller
		sw.account, sw.iban, sw.holder = account, number, holder
		return payoutStmts(sw, uuid.NewV7(), w.user(), 1080000, map[uuid.UUID]int64{line3: 1080000})
	}
	expectCommit(t, w, "another IBAN than the account's", chk, "payouts_bank_account_chk", newPayout(seller.account, w.iban(), seller.holder)...)
	expectCommit(t, w, "another holder than the account's", chk, "payouts_bank_account_chk", newPayout(seller.account, seller.iban, "Başka Vakıf")...)
	other := w.seller()
	expectCommit(t, w, "the account of another payee", chk, "payouts_bank_account_chk", newPayout(other.account, other.iban, other.holder)...)
	pendingAccount, pendingIBAN := uuid.NewV7(), w.iban()
	w.exec(bankAccountSQL, pendingAccount, seller.payee, seller.org, pendingIBAN, w.file("bank_account_evidence"), w.user())
	expectCommit(t, w, "an account waiting for approval", chk, "payouts_bank_account_chk", newPayout(pendingAccount, pendingIBAN, seller.holder)...)

	// Revoked between creation and approval: the approval fails.
	late := uuid.NewV7()
	w.mustCommit(payoutStmts(seller, late, w.user(), 1080000, map[uuid.UUID]int64{line3: 1080000})...)
	w.exec(`UPDATE payee_bank_accounts SET status = 'revoked', revoked_by = $2, revoked_at = now(), revoke_reason = 'Hesap kapandı' WHERE id = $1`, seller.account, w.user())
	expectFail(t, w.d.Pool, []p1aCase{{"approved after the account was revoked", approve, []any{late, b}, chk, "payouts_bank_account_chk"}})
	w.exec(`UPDATE payouts SET status = 'cancelled', cancelled_at = now(), cancel_reason = 'Hesap iptal edildi' WHERE id = $1`, late)
}

// TestPayoutItems_SumMismatch_FailsAtCommit (02 §21.5): at commit the items
// of a live payout sum to its amount (payout_items_sum, deferred); items
// join only a pending payout, only for a fulfilled line of the same payee,
// never above the line's seller net; a line is in one payout at a time
// (also under concurrency); items go away only with a failed or cancelled
// payout; a line is marked paid out only by a payout that contains it.
func TestPayoutItems_SumMismatch_FailsAtCommit(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	chk := db.CodeCheckViolation
	s := w.shop()
	seller := s.seller
	_, line1 := w.payableLine(s)
	_, line2 := w.payableLine(s)
	creator := w.user()
	expectCommit(t, w, "items below the amount", chk, "payout_items_sum",
		payoutStmts(seller, uuid.NewV7(), creator, 1080000, map[uuid.UUID]int64{line1: 1000000})...)
	expectCommit(t, w, "items above the amount", chk, "payout_items_sum",
		payoutStmts(seller, uuid.NewV7(), creator, 1080000, map[uuid.UUID]int64{line1: 1080000, line2: 1})...)
	expectCommit(t, w, "a payout without items", chk, "payout_items_sum",
		payoutStmts(seller, uuid.NewV7(), creator, 1080000, nil)...)
	expectCommit(t, w, "an item above the line's seller net", chk, "payout_items_line_chk",
		payoutStmts(seller, uuid.NewV7(), creator, 1080001, map[uuid.UUID]int64{line1: 1080001})...)
	_, open := w.order(s)
	expectCommit(t, w, "a line that is not fulfilled", chk, "payout_items_line_chk",
		payoutStmts(seller, uuid.NewV7(), creator, 1080000, map[uuid.UUID]int64{open: 1080000})...)
	other := s
	other.seller = w.seller()
	other.offering = w.offering(other.seller, other.service)
	other.policy = w.policy(other.offering)
	_, foreign := w.payableLine(other)
	expectCommit(t, w, "a line of another payee", db.CodeForeignKeyViolation, "payout_items_order_line_id_payee_id_fkey",
		payoutStmts(seller, uuid.NewV7(), creator, 1080000, map[uuid.UUID]int64{foreign: 1080000})...)

	// Two lines in one payout: the sums match.
	two := uuid.NewV7()
	w.mustCommit(payoutStmts(seller, two, creator, 2160000, map[uuid.UUID]int64{line1: 1080000, line2: 1080000})...)
	expectFail(t, w.d.Pool, []p1aCase{
		{"an item removed from a pending payout", `DELETE FROM payout_items WHERE payout_id = $1 AND order_line_id = $2`, []any{two, line1}, db.CodeRestrictViolation, ""},
		{"app_rw changes an item", `UPDATE payout_items SET amount = 1 WHERE payout_id = $1`, []any{two}, db.CodeInsufficientPriv, ""},
		{"a line marked paid out by a payout that does not contain it", `UPDATE order_lines SET payout_id = $2, paid_out_at = now() WHERE id = $1`,
			[]any{foreign, two}, db.CodeForeignKeyViolation, ""},
	})
	expectCommit(t, w, "the same line in a second payout", db.CodeUniqueViolation, "payout_items_line_uq",
		payoutStmts(seller, uuid.NewV7(), creator, 1080000, map[uuid.UUID]int64{line1: 1080000})...)
	w.exec(`UPDATE payouts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`, two, w.user())
	_, line3 := w.payableLine(s)
	expectCommit(t, w, "an item added to an approved payout", chk, "payout_items_line_chk",
		st(`UPDATE payouts SET amount = amount WHERE id = $1`, two),
		st(`INSERT INTO payout_items (payout_id, order_line_id, payee_id, amount) VALUES ($1, $2, $3, 1080000)`, two, line3, seller.payee))

	// A failed payout releases its lines: the items go, the sum is not checked.
	w.mustCommit(
		st(`UPDATE payouts SET status = 'failed', failure_reason = 'IBAN kapalı' WHERE id = $1`, two),
		st(`DELETE FROM payout_items WHERE payout_id = $1`, two))
	if n := w.count(`SELECT count(*) FROM payout_items WHERE payout_id = $1`, two); n != 0 {
		t.Errorf("items left after the failure: %d", n)
	}

	// Concurrency: eight payouts race for one line; one commits.
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Go(func() {
			state, _ := w.commitAsync(payoutStmts(seller, uuid.NewV7(), creator, 1080000, map[uuid.UUID]int64{line3: 1080000}))
			results[i] = state
		})
	}
	wg.Wait()
	won := 0
	for _, r := range results {
		switch r {
		case "":
			won++
		case db.CodeUniqueViolation:
		default:
			t.Errorf("concurrent payout: SQLSTATE %s", r)
		}
	}
	if won != 1 {
		t.Errorf("concurrent payouts of one line stored = %d, want 1", won)
	}
}

// commitAsync is commit for goroutines: it never calls t.Fatal.
func (w *p1aWorld) commitAsync(list []stmt) (state, constraint string) {
	tx, err := w.d.Pool.Begin(context.Background())
	if err != nil {
		return db.SQLState(err), ""
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := execAll(tx, list); err != nil {
		return db.SQLState(err), db.ConstraintName(err)
	}
	err = tx.Commit(context.Background())
	return db.SQLState(err), db.ConstraintName(err)
}

func execAll(tx pgx.Tx, list []stmt) error {
	for _, s := range list {
		if _, err := tx.Exec(context.Background(), s.sql, s.args...); err != nil {
			return err
		}
	}
	return nil
}
