package ledger

import (
	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// Errors of the payout flow (BE-044; 04 §1.10). parties.no_approved_account
// (the payee has no approved account at creation) is the parties module's
// own error (06 H-52), passed through as is.
var (
	// ErrNothingPayable: the payee has no line a payout may take (paid,
	// fulfilled, its payout hold elapsed, not refunded, in no live payout).
	ErrNothingPayable = problem.New(problem.Invalid, "ledger.nothing_payable", "Nothing is payable to the payee")
	// ErrLineNotPayable: a requested line is not payable to the payee now:
	// another payee's, not fulfilled, still held, refunded, already paid out
	// or already in a live payout (payout_items_line_uq).
	ErrLineNotPayable = problem.New(problem.Conflict, "ledger.line_not_payable", "Order line is not payable")
	// ErrInvalidState: the payout is not in a state that allows the
	// transition (approve a pending payout, mark an approved one paid or
	// failed, cancel before it is paid), or the payee may not be paid.
	ErrInvalidState = problem.New(problem.Conflict, "ledger.invalid_state", "Payout state does not allow this")
	// ErrBankAccountChanged: the bank account snapshotted on the payout is
	// no longer the payee's approved account (superseded or revoked): the
	// payout cannot be approved; cancel it and create a new one.
	ErrBankAccountChanged = problem.New(problem.Conflict, "ledger.bank_account_changed",
		"The payee's approved bank account changed after the payout was created")
)

// payoutConstraints maps the named constraints of 000113 and 000115 onto
// the payout errors; the rest goes through the ledger's map. The
// four-eyes CHECK is authz's four_eyes_violation (03 §6.1: the database
// answers with the same problem as the policy).
var payoutConstraints = db.Constraints{
	"payouts_four_eyes_chk":     authz.ErrFourEyes,
	"payouts_bank_account_chk":  ErrBankAccountChanged,
	"payouts_approved_chk":      ErrInvalidState,
	"payouts_paid_chk":          ErrInvalidState,
	"payouts_cancelled_chk":     ErrInvalidState,
	"payouts_failed_chk":        ErrInvalidState,
	"payouts_lines_paid_out":    ErrInvalidState,
	"payout_items_line_chk":     ErrLineNotPayable,
	"payout_items_line_uq":      ErrLineNotPayable,
	"payout_items_pkey":         ErrLineNotPayable,
	"payout_items_sum":          ErrInvalidTxn,
	"payouts_amount_chk":        ErrInvalidTxn,
	"payouts_iban_snapshot_chk": ErrBankAccountChanged,
}

// translatePayout maps a payout-flow database error: the payout
// constraints first, then the ledger's (the same transaction posts
// payout_paid), then db.Translate.
func translatePayout(err error) error {
	if err == nil {
		return nil
	}
	if name := db.ConstraintName(err); name != "" {
		if _, ok := payoutConstraints[name]; ok {
			return payoutConstraints.Translate(err)
		}
	}
	return constraints.Translate(err)
}
