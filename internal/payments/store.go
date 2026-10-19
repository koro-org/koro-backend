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
// row's current status. The store stamps created_at and updated_at of
// every insert; a caller's values are ignored (one clock, BE-069).
type Store interface {
	CollectionStore
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
	// GetPayment returns a payment (FOR UPDATE when lock) or ErrNotFound.
	GetPayment(ctx context.Context, id uuid.UUID, lock bool) (Payment, error)
	// ListPayments returns a page of payments, newest first (created_at
	// DESC, id DESC), after the cursor row.
	ListPayments(ctx context.Context, f PaymentFilter, after *TimeCursor, limit int) ([]Payment, error)
	// PaymentsOfOrder returns the payments of an order, oldest first.
	PaymentsOfOrder(ctx context.Context, orderID uuid.UUID) ([]Payment, error)
	// AddPaymentRefund adds a completed refund to the payment: its refunded
	// amount grows, its status becomes partially_refunded, or refunded once
	// all of it went back. Only a captured or partially refunded payment
	// (ErrInvalidState); beyond its amount ErrRefundExceedsPaid
	// (payments_refunded_limit_chk).
	AddPaymentRefund(ctx context.Context, paymentID uuid.UUID, amount int64) (Payment, error)
	// PaymentsCapturedBy returns the payments a user captured by hand (KVKK
	// export of the staff references).
	PaymentsCapturedBy(ctx context.Context, userID uuid.UUID) ([]Payment, error)
	// InsertSplits writes the splits of a paid order's payment.
	InsertSplits(ctx context.Context, ss []Split) error
	// SplitsOf returns the splits of a payment (the notices of a paid order).
	SplitsOf(ctx context.Context, paymentID uuid.UUID) ([]Split, error)
	// InsertRefund writes a refund request (requested).
	InsertRefund(ctx context.Context, r Refund) error
	// GetRefund returns a refund (FOR UPDATE when lock) or ErrNotFound.
	GetRefund(ctx context.Context, id uuid.UUID, lock bool) (Refund, error)
	// ListRefunds returns a page of refunds, newest first (created_at DESC,
	// id DESC), after the cursor row.
	ListRefunds(ctx context.Context, f RefundFilter, after *TimeCursor, limit int) ([]Refund, error)
	// RefundsOfOrder returns every refund of an order (any status), oldest
	// first: the limits of a new request and of a completion.
	RefundsOfOrder(ctx context.Context, orderID uuid.UUID) ([]Refund, error)
	// UpdateRefund writes the running columns of next (status, approval,
	// rejection, bank reference, receipt, failure, completion) when the row
	// is still in status from; otherwise ErrInvalidState (ErrNotFound for a
	// missing row). The four-eyes CHECK answers authz.ErrFourEyes.
	UpdateRefund(ctx context.Context, next Refund, from RefundStatus) (Refund, error)
	// RefundsOfUser returns the refunds a user requested, approved or
	// rejected (KVKK export of the staff references).
	RefundsOfUser(ctx context.Context, userID uuid.UUID) ([]Refund, error)
}

// TimeCursor is the last row of a payments or refunds page (created_at
// DESC, id DESC).
type TimeCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// PaymentFilter narrows ListPayments. Zero fields do not filter.
type PaymentFilter struct {
	OrderID *uuid.UUID
	Status  PaymentStatus
}

// RefundFilter narrows ListRefunds. Zero fields do not filter.
type RefundFilter struct {
	Status    RefundStatus
	OrderID   *uuid.UUID
	PaymentID *uuid.UUID
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
