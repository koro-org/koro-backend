package payments

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/textx"
)

// gormStore is the PostgreSQL Store (05 §6). The payments tables are Koro's
// finance records without an org scope: every method documents cross-org.
type gormStore struct{ orm *gorm.DB }

// NewGormStore returns the PostgreSQL store over the app_rw pool.
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

func take[T any](op string, q *gorm.DB) (T, error) {
	var v T
	err := q.Take(&v).Error
	switch {
	case db.IsNotFound(err):
		return v, ErrNotFound
	case err != nil:
		return v, fmt.Errorf("payments: %s: %w", op, err)
	}
	return v, nil
}

// ---- collection accounts

// InsertAccount implements Store.
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) InsertAccount(ctx context.Context, c CollectionAccount) (CollectionAccount, error) {
	if c.PartyKind == "" {
		c.PartyKind = PartyKindPlatform
	}
	if err := db.Conn(ctx, s.orm).Create(&c).Error; err != nil {
		return CollectionAccount{}, fmt.Errorf("payments: insert account: %w", constraints.Translate(err))
	}
	return s.GetAccount(ctx, c.ID, false)
}

// GetAccount implements Store.
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) GetAccount(ctx context.Context, id uuid.UUID, lock bool) (CollectionAccount, error) {
	return take[CollectionAccount]("get account", db.LockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// ListAccounts implements Store.
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) ListAccounts(ctx context.Context) ([]CollectionAccount, error) {
	var out []CollectionAccount
	if err := db.Conn(ctx, s.orm).Order("created_at DESC, id DESC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: list accounts: %w", err)
	}
	return out, nil
}

// DefaultAccount implements Store.
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) DefaultAccount(ctx context.Context) (CollectionAccount, bool, error) {
	var out []CollectionAccount
	if err := db.Conn(ctx, s.orm).Where("is_default AND status = 'active'").Limit(1).Find(&out).Error; err != nil {
		return CollectionAccount{}, false, fmt.Errorf("payments: default account: %w", err)
	}
	if len(out) == 0 {
		return CollectionAccount{}, false, nil
	}
	return out[0], true, nil
}

// UpdateAccount implements Store: a targeted UPDATE of the running columns
// (the frozen ones are never written; updated_at is the trigger's),
// guarded by the current status, RETURNING the row.
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) UpdateAccount(ctx context.Context, next CollectionAccount, from AccountStatus) (CollectionAccount, error) {
	var rows []CollectionAccount
	err := db.Conn(ctx, s.orm).Raw(`UPDATE collection_accounts SET
		status = @status, is_default = @is_default, approved_by = @approved_by, approved_at = @approved_at, retired_at = @retired_at
		WHERE id = @id AND status = @from
		RETURNING *`, map[string]any{
		"id": next.ID, "from": string(from), "status": string(next.Status), "is_default": next.IsDefault,
		"approved_by": next.ApprovedBy, "approved_at": next.ApprovedAt, "retired_at": next.RetiredAt,
	}).Scan(&rows).Error
	if err != nil {
		return CollectionAccount{}, fmt.Errorf("payments: update account: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetAccount(ctx, next.ID, false); err != nil {
			return CollectionAccount{}, err
		}
		return CollectionAccount{}, ErrInvalidState
	}
	return rows[0], nil
}

// ---- transfers

// InsertTransfer implements Store: INSERT … ON CONFLICT (account, bank
// reference) DO NOTHING; no row inserted means a duplicate.
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) InsertTransfer(ctx context.Context, t IncomingTransfer) (IncomingTransfer, bool, error) {
	if t.Currency == "" {
		t.Currency = Currency
	}
	res := db.Conn(ctx, s.orm).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "collection_account_id"}, {Name: "bank_reference"}}, DoNothing: true,
	}).Create(&t)
	if res.Error != nil {
		return IncomingTransfer{}, false, fmt.Errorf("payments: insert transfer: %w", constraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return IncomingTransfer{}, false, nil
	}
	stored, err := s.GetTransfer(ctx, t.ID, false)
	return stored, err == nil, err
}

// GetTransfer implements Store.
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) GetTransfer(ctx context.Context, id uuid.UUID, lock bool) (IncomingTransfer, error) {
	return take[IncomingTransfer]("get transfer", db.LockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// ListTransfers implements Store.
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) ListTransfers(ctx context.Context, f TransferFilter, after *TransferCursor, limit int) ([]IncomingTransfer, error) {
	q := db.Conn(ctx, s.orm).Model(&IncomingTransfer{})
	if f.Status != "" {
		q = q.Where("status = ?", string(f.Status))
	}
	if f.ValueFrom != nil {
		q = q.Where("value_date >= ?", *f.ValueFrom)
	}
	if f.ValueTo != nil {
		q = q.Where("value_date <= ?", *f.ValueTo)
	}
	if f.Q != "" {
		like := "%" + textx.EscapeLike(f.Q) + "%"
		if f.QRef != "" {
			q = q.Where("(description ILIKE @q OR bank_reference ILIKE @q OR sender_name ILIKE @q OR extracted_order_no = @ref)",
				map[string]any{"q": like, "ref": f.QRef})
		} else {
			q = q.Where("(description ILIKE @q OR bank_reference ILIKE @q OR sender_name ILIKE @q)", map[string]any{"q": like})
		}
	}
	if after != nil {
		q = q.Where("(value_date < @d OR (value_date = @d AND id < @id))", map[string]any{"d": after.ValueDate, "id": after.ID})
	}
	var out []IncomingTransfer
	if err := q.Order("value_date DESC, id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: list transfers: %w", err)
	}
	return out, nil
}

// UpdateTransfer implements Store: a targeted UPDATE of the running columns
// (the bank data are never written), guarded by the current status,
// RETURNING the row.
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) UpdateTransfer(ctx context.Context, next IncomingTransfer, from TransferStatus) (IncomingTransfer, error) {
	var rows []IncomingTransfer
	err := db.Conn(ctx, s.orm).Raw(`UPDATE incoming_bank_transfers SET
		status = @status, matched_order_id = @matched_order_id, matched_by = @matched_by, matched_at = @matched_at,
		ignore_reason = @ignore_reason, return_requested_by = @return_requested_by, return_requested_at = @return_requested_at,
		return_reason = @return_reason, return_target_iban = @return_target_iban, return_approved_by = @return_approved_by,
		return_approved_at = @return_approved_at, returned_at = @returned_at, return_bank_reference = @return_bank_reference,
		return_receipt_file_id = @return_receipt_file_id
		WHERE id = @id AND status = @from
		RETURNING *`, map[string]any{
		"id": next.ID, "from": string(from), "status": string(next.Status), "matched_order_id": next.MatchedOrderID,
		"matched_by": next.MatchedBy, "matched_at": next.MatchedAt, "ignore_reason": next.IgnoreReason,
		"return_requested_by": next.ReturnRequestedBy, "return_requested_at": next.ReturnRequestedAt,
		"return_reason": next.ReturnReason, "return_target_iban": next.ReturnTargetIBAN, "return_approved_by": next.ReturnApprovedBy,
		"return_approved_at": next.ReturnApprovedAt, "returned_at": next.ReturnedAt, "return_bank_reference": next.ReturnBankReference,
		"return_receipt_file_id": next.ReturnReceiptFileID,
	}).Scan(&rows).Error
	if err != nil {
		return IncomingTransfer{}, fmt.Errorf("payments: update transfer: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetTransfer(ctx, next.ID, false); err != nil {
			return IncomingTransfer{}, err
		}
		return IncomingTransfer{}, ErrInvalidState
	}
	return rows[0], nil
}

// UnmatchedBefore implements Store (incoming_bank_transfers_unmatched_idx
// narrows the scan to the unmatched rows).
//
// cross-org: Koro's own accounts (no org scope).
func (s gormStore) UnmatchedBefore(ctx context.Context, before time.Time) (UnmatchedSummary, error) {
	var row struct {
		N      int
		Oldest *time.Time
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) AS n, min(created_at) AS oldest FROM incoming_bank_transfers
		WHERE status = 'unmatched' AND created_at < ?`, before.UTC()).Scan(&row).Error
	if err != nil {
		return UnmatchedSummary{}, fmt.Errorf("payments: unmatched transfers: %w", err)
	}
	out := UnmatchedSummary{Count: row.N}
	if row.Oldest != nil {
		out.OldestAt = row.Oldest.UTC()
	}
	return out, nil
}

// TransfersOfUser implements Store.
//
// cross-org: a user's staff references across Koro's accounts (KVKK export).
func (s gormStore) TransfersOfUser(ctx context.Context, userID uuid.UUID) ([]IncomingTransfer, error) {
	var out []IncomingTransfer
	err := db.Conn(ctx, s.orm).Where("entered_by = @u OR matched_by = @u OR return_requested_by = @u OR return_approved_by = @u",
		map[string]any{"u": userID}).Order("id").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("payments: transfers of user: %w", err)
	}
	return out, nil
}

// ---- payments, splits, refunds

// InsertPayment implements Store.
//
// cross-org: the payment carries the buyer org of its order (org_id is
// derived from the order row, 06 H-38; composite FK).
func (s gormStore) InsertPayment(ctx context.Context, p Payment) error {
	if p.Currency == "" {
		p.Currency = Currency
	}
	if err := db.Conn(ctx, s.orm).Create(&p).Error; err != nil {
		return fmt.Errorf("payments: insert payment: %w", constraints.Translate(err))
	}
	return nil
}

// GetPayment implements Store.
//
// cross-org: the admin surface and in-process callers address it by ID.
func (s gormStore) GetPayment(ctx context.Context, id uuid.UUID, lock bool) (Payment, error) {
	return take[Payment]("get payment", db.LockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// ListPayments implements Store.
//
// cross-org: the admin surface lists every buyer's payments.
func (s gormStore) ListPayments(ctx context.Context, f PaymentFilter, after *TimeCursor, limit int) ([]Payment, error) {
	q := db.Conn(ctx, s.orm).Model(&Payment{})
	if f.OrderID != nil {
		q = q.Where("order_id = ?", *f.OrderID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", string(f.Status))
	}
	if after != nil {
		q = q.Where("(created_at, id) < ( ?, ?)", after.CreatedAt, after.ID)
	}
	var out []Payment
	if err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: list payments: %w", err)
	}
	return out, nil
}

// PaymentsOfOrder implements Store (payments_order_idx).
//
// cross-org: the payments of one order the caller resolved.
func (s gormStore) PaymentsOfOrder(ctx context.Context, orderID uuid.UUID) ([]Payment, error) {
	var out []Payment
	if err := db.Conn(ctx, s.orm).Where("order_id = ?", orderID).Order("created_at, id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: payments of order: %w", err)
	}
	return out, nil
}

// AddPaymentRefund implements Store: a targeted UPDATE (the frozen columns
// are never written) guarded by the status and the amount, RETURNING the
// row; the CHECKs of 000106 stay the last line.
//
// cross-org: the payment of a refund the caller resolved.
func (s gormStore) AddPaymentRefund(ctx context.Context, paymentID uuid.UUID, amount int64) (Payment, error) {
	var rows []Payment
	err := db.Conn(ctx, s.orm).Raw(`UPDATE payments SET refunded_amount = refunded_amount + @amount,
		status = CASE WHEN refunded_amount + @amount = amount THEN 'refunded' ELSE 'partially_refunded' END
		WHERE id = @id AND status IN ('captured', 'partially_refunded') AND refunded_amount + @amount <= amount
		RETURNING *`, map[string]any{"id": paymentID, "amount": amount}).Scan(&rows).Error
	if err != nil {
		return Payment{}, fmt.Errorf("payments: add payment refund: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		p, err := s.GetPayment(ctx, paymentID, false)
		switch {
		case err != nil:
			return Payment{}, err
		case p.Status != PaymentCaptured && p.Status != PaymentPartiallyRefunded:
			return Payment{}, ErrInvalidState
		}
		return Payment{}, ErrRefundExceedsPaid
	}
	return rows[0], nil
}

// PaymentsCapturedBy implements Store.
//
// cross-org: a user's staff references (KVKK export).
func (s gormStore) PaymentsCapturedBy(ctx context.Context, userID uuid.UUID) ([]Payment, error) {
	var out []Payment
	if err := db.Conn(ctx, s.orm).Where("captured_by = ?", userID).Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: payments captured by user: %w", err)
	}
	return out, nil
}

// InsertSplits implements Store.
//
// cross-org: the splits of a payment (scoped by its order, composite FKs).
func (s gormStore) InsertSplits(ctx context.Context, ss []Split) error {
	if len(ss) == 0 {
		return nil
	}
	if err := db.Conn(ctx, s.orm).Create(&ss).Error; err != nil {
		return fmt.Errorf("payments: insert splits: %w", constraints.Translate(err))
	}
	return nil
}

// SplitsOf implements Store.
//
// cross-org: the splits of a payment addressed by ID.
func (s gormStore) SplitsOf(ctx context.Context, paymentID uuid.UUID) ([]Split, error) {
	var out []Split
	if err := db.Conn(ctx, s.orm).Where("payment_id = ?", paymentID).Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: splits: %w", err)
	}
	return out, nil
}

// InsertRefund implements Store.
//
// cross-org: the refund of a payment (scoped by its order, composite FKs).
func (s gormStore) InsertRefund(ctx context.Context, r Refund) error {
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return fmt.Errorf("payments: insert refund: %w", constraints.Translate(err))
	}
	return nil
}

// GetRefund implements Store.
//
// cross-org: the admin surface addresses a refund by ID.
func (s gormStore) GetRefund(ctx context.Context, id uuid.UUID, lock bool) (Refund, error) {
	return take[Refund]("get refund", db.LockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// ListRefunds implements Store.
//
// cross-org: the admin surface lists every buyer's refunds.
func (s gormStore) ListRefunds(ctx context.Context, f RefundFilter, after *TimeCursor, limit int) ([]Refund, error) {
	q := db.Conn(ctx, s.orm).Model(&Refund{})
	if f.Status != "" {
		q = q.Where("status = ?", string(f.Status))
	}
	if f.OrderID != nil {
		q = q.Where("order_id = ?", *f.OrderID)
	}
	if f.PaymentID != nil {
		q = q.Where("payment_id = ?", *f.PaymentID)
	}
	if after != nil {
		q = q.Where("(created_at, id) < ( ?, ?)", after.CreatedAt, after.ID)
	}
	var out []Refund
	if err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: list refunds: %w", err)
	}
	return out, nil
}

// RefundsOfOrder implements Store (refunds_order_idx).
//
// cross-org: the refunds of one order the caller resolved.
func (s gormStore) RefundsOfOrder(ctx context.Context, orderID uuid.UUID) ([]Refund, error) {
	var out []Refund
	if err := db.Conn(ctx, s.orm).Where("order_id = ?", orderID).Order("created_at, id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: refunds of order: %w", err)
	}
	return out, nil
}

// UpdateRefund implements Store: a targeted UPDATE of the running columns
// (the frozen ones are never written; updated_at is the trigger's),
// guarded by the current status, RETURNING the row.
//
// cross-org: the admin surface addresses a refund by ID.
func (s gormStore) UpdateRefund(ctx context.Context, next Refund, from RefundStatus) (Refund, error) {
	var rows []Refund
	err := db.Conn(ctx, s.orm).Raw(`UPDATE refunds SET
		status = @status, approved_by = @approved_by, approved_at = @approved_at, rejected_by = @rejected_by,
		rejected_at = @rejected_at, rejection_reason = @rejection_reason, bank_reference = @bank_reference,
		receipt_file_id = @receipt_file_id, failure_reason = @failure_reason, completed_at = @completed_at, closed_by = @closed_by, closed_at = @closed_at, cancel_reason = @cancel_reason
		WHERE id = @id AND status = @from
		RETURNING *`, map[string]any{
		"id": next.ID, "from": string(from), "status": string(next.Status), "approved_by": next.ApprovedBy,
		"approved_at": next.ApprovedAt, "rejected_by": next.RejectedBy, "rejected_at": next.RejectedAt,
		"rejection_reason": next.RejectionReason, "bank_reference": next.BankReference, "receipt_file_id": next.ReceiptFileID,
		"failure_reason": next.FailureReason, "completed_at": next.CompletedAt,
		"closed_by": next.ClosedBy, "closed_at": next.ClosedAt, "cancel_reason": next.CancelReason,
	}).Scan(&rows).Error
	if err != nil {
		return Refund{}, fmt.Errorf("payments: update refund: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetRefund(ctx, next.ID, false); err != nil {
			return Refund{}, err
		}
		return Refund{}, ErrInvalidState
	}
	return rows[0], nil
}

// RefundsOfUser implements Store.
//
// cross-org: a user's staff references (KVKK export).
func (s gormStore) RefundsOfUser(ctx context.Context, userID uuid.UUID) ([]Refund, error) {
	var out []Refund
	err := db.Conn(ctx, s.orm).Where("requested_by = @u OR approved_by = @u OR rejected_by = @u OR closed_by = @u", map[string]any{"u": userID}).
		Order("id").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("payments: refunds of user: %w", err)
	}
	return out, nil
}
