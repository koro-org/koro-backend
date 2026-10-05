package ledger

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of the ledger (05 §2.2). The GORM store
// (store_gorm.go) and the in-memory store (store_mem.go) pass the same
// contract test (store_contract_test.go).
//
// The ledger tables have no org (02 §21: finance data, read by payee):
// every method is cross-org. Writes run in the transaction carried by ctx
// (the caller's, 01 §6.3); InsertTxn must be called inside one.
type Store interface {
	// InsertTxn writes the header and all its entries in one go (06 H-56,
	// T-226: there is no way to append entries to a stored transaction).
	// When (kind, ref_type, ref_id) already exists it writes nothing and
	// reports false; a concurrent insert of the same key waits for the
	// other transaction and then reports false (it committed) or inserts
	// (it rolled back). Before returning it forces the deferred balance
	// check of the running transaction (ErrUnbalanced, ErrInvalidTxn), so a
	// bad row fails here and not only at COMMIT.
	InsertTxn(ctx context.Context, h Transaction, entries []EntryRow) (inserted bool, err error)
	// ByRef returns the transaction of a business event, or ErrNotFound.
	ByRef(ctx context.Context, k Kind, r RefType, refID uuid.UUID) (Posted, error)
	// ByID returns a transaction, or ErrNotFound.
	ByID(ctx context.Context, id uuid.UUID) (Posted, error)
	// LockPayees serialises the balance checks of the payees (in the given
	// order) until the running transaction ends.
	LockPayees(ctx context.Context, payeeIDs []uuid.UUID) error
	// PayeeBalance returns what Koro owes the payee: minus the sum of its
	// entries on per-payee (liability) accounts, as one consistent read.
	// A payee without entries has 0.
	PayeeBalance(ctx context.Context, payeeID uuid.UUID) (int64, error)
	// List returns up to limit transactions matching f with their entries,
	// newest occurred_at first (then ID), strictly after the cursor.
	List(ctx context.Context, f JournalFilter, after *JournalCursor, limit int) ([]Posted, error)

	// Integrity queries (ledger.integrity_check); each returns at most limit rows.

	// Unbalanced returns the transactions with fewer than two entries or a
	// non-zero sum.
	Unbalanced(ctx context.Context, limit int) ([]UnbalancedTxn, error)
	// NegativePayeeBalances returns the payees Koro owes less than nothing.
	NegativePayeeBalances(ctx context.Context, limit int) ([]PayeeBalance, error)
	// LateEntries returns the entries written after their header's
	// transaction (created_at differs; T-226).
	LateEntries(ctx context.Context, limit int) ([]LateEntry, error)
	// PayoutMismatches returns the paid payouts without exactly one matching
	// payout_paid transaction (seller_payable +amount for the payee,
	// koro_bank −amount) and the payout_paid transactions whose payout is
	// missing or not paid.
	PayoutMismatches(ctx context.Context, limit int) ([]PayoutMismatch, error)

	RetentionStore
}

// JournalFilter narrows adminListLedgerTransactions (04 §2.17).
type JournalFilter struct {
	Kind    Kind       // "" = every kind
	RefType RefType    // "" = every ref type
	RefID   *uuid.UUID // nil = every reference
	From    *time.Time // occurred_at ≥ From
	To      *time.Time // occurred_at < To
}

// JournalCursor is the keyset position of a journal page.
type JournalCursor struct {
	OccurredAt time.Time
	ID         uuid.UUID
}

// UnbalancedTxn is a transaction the balance rule rejects. Sum is nil when
// the sum does not fit in int64.
type UnbalancedTxn struct {
	TransactionID uuid.UUID
	Entries       int
	Sum           *int64
}

// PayeeBalance is the balance of one payee.
type PayeeBalance struct {
	PayeeID uuid.UUID
	Balance int64
}

// LateEntry is an entry written in another transaction than its header.
type LateEntry struct {
	TransactionID uuid.UUID
	EntryID       uuid.UUID
}

// PayoutMismatch is a paid payout and the ledger disagreeing.
type PayoutMismatch struct {
	PayoutID      uuid.UUID
	TransactionID *uuid.UUID // the payout_paid transaction, if any
	Problem       string     // "missing_transaction" | "entries_mismatch" | "payout_not_paid"
}

// Payout mismatch problems.
const (
	MismatchMissingTxn = "missing_transaction" // paid payout, no payout_paid transaction
	MismatchEntries    = "entries_mismatch"    // the transaction does not move the payout amount
	MismatchNotPaid    = "payout_not_paid"     // payout_paid transaction, payout missing or not paid
)
