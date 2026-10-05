// Package ledger is Koro's double-entry ledger (01 §4.19; BE-039): the
// single source of truth of money movements. Amounts are int64 kuruş
// (platform/money, 06 H-6), debit +, credit −; every transaction has at
// least two entries and sums to zero.
//
// Append-only: ledger_transactions and ledger_entries take INSERT only
// (koro_forbid_mutation and the GRANTs, 000111/000112). A correction is a
// new, reversing transaction (Reverse), never an UPDATE.
//
// Posting (Poster.Post) runs in the caller's transaction (01 §6.3: payment
// capture → order → entitlements → ledger): a rollback of the caller
// removes the entries. Defence in depth (T-44): Post checks the balance in
// Go before any statement; the deferred constraint trigger
// ledger_txn_balanced checks it again in the database, forced at the end of
// Post and again at COMMIT, so a row written around Post cannot commit
// unbalanced either. Header and entries are written by one store call
// (06 H-56, T-226: no API appends entries to a stored transaction).
//
// Idempotency: (kind, ref_type, ref_id) is the business key
// (ledger_transactions_ref_uq). Posting the same event again — a retry, a
// replayed request, a concurrent duplicate — returns the first
// transaction's ID when the entries are the same, and ErrRefConflict when
// they differ. The header is inserted with ON CONFLICT DO NOTHING, so a
// concurrent duplicate waits for the first transaction instead of aborting
// the caller's with a unique violation.
//
// Payee balance (02 §21): what Koro owes a payee is minus the sum of its
// seller_payable entries and never drops below zero. A transaction that
// debits a payee (payout, refund after allocation, reversal) takes a
// transaction-scoped advisory lock per payee, then checks the balance:
// concurrent debits are serialised and cannot overdraw it together. The
// nightly ledger.integrity_check (05:15) re-checks every transaction's
// sum, every payee balance, entries written after their header (T-226) and
// paid payouts against the ledger, and raises the alarm hook.
//
// Templates (templates.go) build the model A transactions of 02 §21.3 from
// the frozen amounts of the order lines; models B and C are skeletons until
// T-11 is decided.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// Poster is the cross-module write contract (01 §4.19). Consumers declare a
// port with it ("// sync-write: ledger.Poster.Post", 01 §6.3) and call it
// with the ctx of their own transaction.
type Poster interface {
	// Post writes a balanced transaction in the transaction carried by ctx
	// (or its own) and returns its ID. An unbalanced or malformed Txn is
	// rejected before the database (ErrUnbalanced, ErrInvalidTxn); a
	// transaction that would leave a payee balance negative is
	// ErrNegativeBalance; the same business event again returns the first
	// ID (same entries) or ErrRefConflict (other entries).
	Post(ctx context.Context, t Txn) (uuid.UUID, error)
}

// Balances is the cross-module read contract (01 §4.19). The HTTP endpoint
// getPayeeBalance comes with BE-044 (payee ∈ org needs parties).
type Balances interface {
	// PayeeBalance returns what Koro owes the payee now (kuruş, ≥ 0 in a
	// healthy ledger), read in one statement. Inside a transaction it
	// includes that transaction's own entries.
	PayeeBalance(ctx context.Context, payeeID uuid.UUID) (int64, error)
}

// Service is the ledger service: the contracts plus corrections and the
// HTTP use cases (04 §2.17).
type Service interface {
	Poster
	Balances
	// Reverse posts the correction of a stored transaction: an adjustment
	// (ref_type manual, ref_id = the reversed transaction) with every entry
	// negated, taken from the stored rows. Reversing again returns the
	// first reversal. The caller authorises and audits it.
	Reverse(ctx context.Context, txnID uuid.UUID, memo string) (uuid.UUID, error)
	// ListTransactions is adminListLedgerTransactions (ledger.journal.list).
	ListTransactions(ctx context.Context, a authz.Actor, q JournalQuery) (JournalPage, error)
}

// Deps are the dependencies of the ledger module; all but Alarm are required.
type Deps struct {
	Store Store
	Tx    db.TxManager
	Clock clock.Clock
	Log   *slog.Logger
	// Alarm is raised for every finding of ledger.integrity_check (and when
	// the check itself fails). nil → an ERROR log with alarm=true only,
	// until GlitchTip/finance paging is wired (BE-058).
	Alarm func(ctx context.Context, f Finding)
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("ledger.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what ledger hands to the composition root.
type Module struct {
	Service Service
	// Sweeper ends the retention of the financial records (10 years from
	// the end of the calendar year; 06 H-57, BE-057).
	Sweeper privacy.RetentionSweeper

	svc *service
}

// New builds the ledger module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Sweeper: sweeper{s: s}, svc: s}, nil
}

// Jobs returns the scheduled jobs of ledger (01 §7): ledger.integrity_check.
func (m Module) Jobs() []queue.Job { return []queue.Job{m.svc.integrityJob()} }

// CheckIntegrity runs the checks of ledger.integrity_check once and returns
// the findings without raising the alarm (tests, an admin tool later).
func (m Module) CheckIntegrity(ctx context.Context) (IntegrityReport, error) {
	return m.svc.checkIntegrity(ctx)
}
