package ledger

import (
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// Errors of the ledger module (05 §4.2; 04 §1.10). ledger.nothing_payable
// belongs to the payout endpoints (BE-044).
var (
	// ErrUnbalanced: the entries of a transaction do not sum to zero, or
	// there are fewer than two. Raised by Post before the database, and by
	// the deferred trigger ledger_txn_balanced for anything that bypassed
	// Post (T-44; TranslateError).
	ErrUnbalanced = problem.New(problem.Invalid, "ledger.unbalanced", "Ledger transaction is not balanced")
	// ErrInvalidTxn: a transaction breaks a shape rule (kind ↔ reference,
	// unknown account, the payee rule, a zero amount, an overflow), or a
	// template got inconsistent amounts.
	ErrInvalidTxn = problem.New(problem.Invalid, "ledger.invalid_transaction", "Ledger transaction is invalid")
	// ErrRefConflict: the business event (kind, ref_type, ref_id) was
	// already posted with other entries. A replay must post exactly the same
	// entries; a correction is a reversing transaction (Reverse).
	ErrRefConflict = problem.New(problem.Conflict, "ledger.ref_conflict", "Ledger event was already posted with other entries")
	// ErrNegativeBalance: the transaction would leave Koro owing a payee
	// less than nothing (02 §21: payee balance ≥ 0), e.g. paying out or
	// refunding more than the payee earned.
	ErrNegativeBalance = problem.New(problem.Conflict, "ledger.negative_payee_balance", "Payee balance would become negative")
	// ErrModelNotEnabled: a template of a commerce model whose ledger flow
	// is not decided yet (T-11: only model A and A2 post in the MVP).
	ErrModelNotEnabled = problem.New(problem.Invalid, "ledger.model_not_enabled", "Commerce model has no ledger template yet")

	// ErrNotFound: the transaction does not exist.
	ErrNotFound = problem.ErrNotFound
)

// constraints maps the named constraints of 02 §21 onto module errors. The
// deferred trigger reports ledger_txn_balanced and ledger_entries_account_chk
// as check_violation with the constraint name (000112).
var constraints = db.Constraints{
	"ledger_txn_balanced":                        ErrUnbalanced,
	"ledger_entries_account_chk":                 ErrInvalidTxn,
	"ledger_entries_amount_chk":                  ErrInvalidTxn,
	"ledger_entries_account_code_fkey":           ErrInvalidTxn,
	"ledger_entries_payee_id_fkey":               ErrInvalidTxn,
	"ledger_entries_order_line_id_fkey":          ErrInvalidTxn,
	"ledger_entries_order_line_id_payee_id_fkey": ErrInvalidTxn,
	"ledger_transactions_kind_chk":               ErrInvalidTxn,
	"ledger_transactions_ref_type_chk":           ErrInvalidTxn,
	"ledger_transactions_memo_chk":               ErrInvalidTxn,
	"ledger_transactions_created_by_fkey":        ErrInvalidTxn,
}

// TranslateError maps a database error that names a ledger constraint onto
// the ledger's problem (ledger.unbalanced, ledger.invalid_transaction) and
// anything else through db.Translate. Callers whose own WithTx commits
// ledger rows may pass the commit error through it: a deferred
// ledger_txn_balanced failure then reaches the client as ledger.unbalanced
// instead of validation_failed (T-44).
func TranslateError(err error) error { return constraints.Translate(err) }
