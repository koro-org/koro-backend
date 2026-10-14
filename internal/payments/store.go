package payments

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// Store is the persistence of payments (05 §6). Every method joins the
// transaction carried by ctx (db.Conn). The tables are Koro's finance
// records: they have no org scope (admin surface only). Updates are
// targeted (06 H-56: no Save over the frozen columns) and guarded by the
// row's current status.
type Store interface {
	// InsertAccount writes a new collection account (pending_approval).
	InsertAccount(ctx context.Context, c CollectionAccount) (CollectionAccount, error)
	// GetAccount returns an account (FOR UPDATE when lock) or ErrNotFound.
	GetAccount(ctx context.Context, id uuid.UUID, lock bool) (CollectionAccount, error)
	// ListAccounts returns every account, newest first.
	ListAccounts(ctx context.Context) ([]CollectionAccount, error)
	// DefaultAccount returns the active default account; found is false when
	// none is the default.
	DefaultAccount(ctx context.Context) (acct CollectionAccount, found bool, err error)
	// UpdateAccount writes the running columns of next (status, is_default,
	// approved_by/at, retired_at) when the row is still in status from;
	// otherwise ErrInvalidState (ErrNotFound for a missing row).
	UpdateAccount(ctx context.Context, next CollectionAccount, from AccountStatus) (CollectionAccount, error)

	// InsertTransfer writes a new transfer (unmatched). inserted is false and
	// nothing is written when the account already has a transfer with the
	// same bank reference (incoming_bank_transfers_account_reference_uq;
	// a concurrent insert of the same reference waits and then skips).
	InsertTransfer(ctx context.Context, t IncomingTransfer) (stored IncomingTransfer, inserted bool, err error)
	// GetTransfer returns a transfer (FOR UPDATE when lock) or ErrNotFound.
	GetTransfer(ctx context.Context, id uuid.UUID, lock bool) (IncomingTransfer, error)
	// ListTransfers returns a page of transfers, newest value date first
	// (value_date DESC, id DESC), after the cursor row.
	ListTransfers(ctx context.Context, f TransferFilter, after *TransferCursor, limit int) ([]IncomingTransfer, error)
	// UpdateTransfer writes the running columns of next (status, match,
	// ignore reason, return columns) when the row is still in status from;
	// otherwise ErrInvalidState.
	UpdateTransfer(ctx context.Context, next IncomingTransfer, from TransferStatus) (IncomingTransfer, error)
	// UnmatchedBefore counts the unmatched transfers recorded before t and
	// returns the oldest one's recording time (payments.unmatched_digest).
	UnmatchedBefore(ctx context.Context, t time.Time) (UnmatchedSummary, error)
	// TransfersOfUser returns the transfers a user entered, matched or
	// handled the return of (KVKK export of the staff references).
	TransfersOfUser(ctx context.Context, userID uuid.UUID) ([]IncomingTransfer, error)

	// InsertPayment writes a captured payment.
	InsertPayment(ctx context.Context, p Payment) error
	// GetPayment returns a payment or ErrNotFound.
	GetPayment(ctx context.Context, id uuid.UUID) (Payment, error)
	// PaymentsCapturedBy returns the payments a user captured by hand (KVKK
	// export of the staff references).
	PaymentsCapturedBy(ctx context.Context, userID uuid.UUID) ([]Payment, error)
	// InsertSplits writes the splits of a paid order's payment.
	InsertSplits(ctx context.Context, ss []Split) error
	// SplitsOf returns the splits of a payment (the notices of a paid order).
	SplitsOf(ctx context.Context, paymentID uuid.UUID) ([]Split, error)
	// InsertRefund writes a refund request (an overpayment's).
	InsertRefund(ctx context.Context, r Refund) error
}

// TransferFilter narrows ListTransfers. Zero fields do not filter.
type TransferFilter struct {
	Status    TransferStatus
	ValueFrom *civil.Date // value_date >= ValueFrom
	ValueTo   *civil.Date // value_date <= ValueTo
	// Q matches part of the description, the sender's name or the bank
	// reference (case-insensitive), or the extracted reference code exactly
	// when it is one (QRef).
	Q    string
	QRef string
}

// TransferCursor is the last row of a page (value_date DESC, id DESC).
type TransferCursor struct {
	ValueDate civil.Date
	ID        uuid.UUID
}

// UnmatchedSummary is what payments.unmatched_digest reports.
type UnmatchedSummary struct {
	Count    int
	OldestAt time.Time // created_at of the oldest one (zero when Count is 0)
}
