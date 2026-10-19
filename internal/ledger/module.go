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
// sum, every payee balance, entries written after their header (T-226),
// paid payouts against the ledger and the allocation of every settled
// order line (BE-067), and raises the alarm hook.
//
// Templates (templates.go) build model A transactions and model C's
// fee-only collection, allocation and refund from frozen order lines.
// Models A2 and B are disabled until T-11 is decided. Koro's own
// platform-native line (payee NULL) is Koro's
// sale (BE-067, T-293): order_allocated credits its net to
// platform_sales_revenue and its VAT, less the buyer's withholding (an
// entry of its own), to vat_output, in the same transaction as the payee
// lines of a mixed order; its refund takes them back. Lines paid before
// the template are recognised once by ledger.recognize_platform_sales
// (recognize.go).
//
// Payouts (BE-044; 02 §21.4–5, 03 §6.1, 04 §2.17, §C.5): finance admin A
// takes the payee's payable lines (orders.PayableLines: paid, fulfilled,
// payout hold elapsed, with a positive seller share remaining after refunds; never model C; minus the lines of a
// live payout) into a pending payout, freezing the IBAN and holder of the
// payee's approved account on it (Idempotency-Key; the ledger must owe at
// least the open payouts plus this one). Finance admin B ≠ A approves it
// with re-authentication (authz four eyes + payouts_four_eyes_chk; the
// frozen account must still be the approved one). After the EFT at the
// bank any finance admin marks it paid with the receipt (payout_receipt)
// and the bank reference: payout_paid, orders.MarkLinesPaidOut, the
// receipt's attachment, the paid state and payout.paid commit in one
// transaction (01 §6.3); the deferred payout_items_sum re-checks the sums
// and the lines at COMMIT. A failed transfer (approved only) or a
// cancellation (before paid) deletes the items: the lines are payable
// again. The seller org reads its payouts with the IBAN masked; an open
// payout blocks closing the org (hooks.openCommerce).
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
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
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
// is getPayeeBalance (PayoutService, BE-044: payee ∈ org through parties).
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
	// PayoutService is the payee balance and the payout flow (BE-044).
	PayoutService
}

// Deps are the dependencies of the ledger module. Store, Tx, Clock and Log
// are required; Alarm is optional.
//
// The payout flow (BE-044) is on when Payouts is set: then Orders, Payees,
// Parties, Orgs, Files, Audit, Outbox and Idempotency are required too
// (New fails otherwise). Without Payouts (a poster-only ledger, e.g. a
// consumer's tests) the balance and payout operations answer 503. Reauth
// nil → the approval is 503 (fail closed); Sender or Staff nil → the
// notice task fails and is retried.
type Deps struct {
	Store Store
	Tx    db.TxManager
	Clock clock.Clock
	Log   *slog.Logger
	// Alarm is raised for every finding of ledger.integrity_check (and when
	// the check itself fails). nil → an ERROR log with alarm=true only,
	// until GlitchTip/finance paging is wired (BE-058).
	Alarm func(ctx context.Context, f Finding)

	// Payout flow (BE-044).
	Payouts     PayoutStore
	Orders      Orders
	Payees      Payees
	Parties     Parties
	Orgs        Orgs
	Files       Files
	Audit       AuditLogger
	Outbox      outbox.Writer       // payout.approved/paid (O) in the transaction, payout.created (A) after it
	Idempotency *idempotency.Keeper // Idempotency-Key of adminCreatePayout (required) and the other POSTs (06 H-5)
	Reauth      Reauthenticator
	Sender      Sender
	Staff       Staff
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
	if d.Payouts == nil {
		return errors.Join(errs...)
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Orders", d.Orders != nil}, {"Payees", d.Payees != nil}, {"Parties", d.Parties != nil}, {"Orgs", d.Orgs != nil},
		{"Files", d.Files != nil}, {"Audit", d.Audit != nil}, {"Outbox", d.Outbox != nil},
		{"Idempotency", d.Idempotency != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("ledger.Deps.%s is missing (the payout flow needs it)", c.name))
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
	// Privacy is the KVKK provider of the payouts (the finance admins who
	// created, approved or paid them; financial records, retained).
	Privacy privacy.Provider

	svc *service
}

// New builds the ledger module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Sweeper: sweeper{s: s}, Privacy: privacyProvider{s: s}, svc: s}, nil
}

// OpenCommerce is the closeOrg check of hooks.openCommerce (BE-044): a
// seller org with a payout waiting for approval or for its transfer cannot
// be closed. Without the payout flow nothing is open.
func (m Module) OpenCommerce(ctx context.Context, orgID uuid.UUID) (bool, error) {
	if m.svc.d.Payouts == nil {
		return false, nil
	}
	return m.svc.d.Payouts.HasOpenPayouts(ctx, orgID)
}

// LinesInPayouts is payments' refund check (BE-043, 06 H-59): which of the
// lines are items of a live payout (pending_approval, approved or paid),
// with the payout's ID. Without the payout flow there are none.
func (m Module) LinesInPayouts(ctx context.Context, lineIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	if m.svc.d.Payouts == nil {
		return map[uuid.UUID]uuid.UUID{}, nil
	}
	return m.svc.d.Payouts.LinesInPayouts(ctx, lineIDs)
}

// RefundedShares returns what the completed refunds took back from an order
// line so far, per share (BE-043: a partial refund is split against what is
// left of the line, so the last one takes the exact remainder). It reads in
// the transaction carried by ctx.
func (m Module) RefundedShares(ctx context.Context, lineID uuid.UUID) (LineShares, error) {
	return m.svc.d.Store.RefundedShares(ctx, lineID)
}

// LineAllocation returns what the ledger allocated of an order line: its
// payable amount once allocated (at payment, or by the late recognition of
// Koro's own line), 0 before (BE-067: payments recognises a platform-native
// line it refunds when it finds 0). It reads in the transaction carried by
// ctx.
func (m Module) LineAllocation(ctx context.Context, lineID uuid.UUID) (int64, error) {
	alloc, err := m.svc.d.Store.LineAllocations(ctx, []uuid.UUID{lineID})
	return alloc[lineID], err
}

// Jobs returns the scheduled jobs of ledger (01 §7): ledger.integrity_check
// (05:15) and the late recognition of Koro's own sales before it (05:05,
// ledger.recognize_platform_sales; BE-067).
func (m Module) Jobs() []queue.Job { return []queue.Job{m.svc.integrityJob(), m.svc.recognizeJob()} }

// RecognizePlatformSales runs ledger.recognize_platform_sales once (tests,
// an admin tool later): the late recognition of Koro's own lines paid
// before their template (BE-067, T-293). Idempotent.
func (m Module) RecognizePlatformSales(ctx context.Context) (RecognitionReport, error) {
	return m.svc.recognizePlatformSales(ctx)
}

// CheckIntegrity runs the checks of ledger.integrity_check once and returns
// the findings without raising the alarm (tests, an admin tool later).
func (m Module) CheckIntegrity(ctx context.Context) (IntegrityReport, error) {
	return m.svc.checkIntegrity(ctx)
}

// LockPayees lets a refund acquire the same lock as payouts before locking
// its order and line. The caller must carry an active transaction.
func (m Module) LockPayees(ctx context.Context, payeeIDs []uuid.UUID) error {
	return m.svc.d.Store.LockPayees(ctx, payeeIDs)
}

// LineReceivable returns the remaining debt of a paid-out line; collection
// adjustments reduce it without changing refunded allocation shares.
func (m Module) LineReceivable(ctx context.Context, lineID uuid.UUID) (int64, error) {
	return m.svc.d.Store.LineReceivable(ctx, lineID)
}
