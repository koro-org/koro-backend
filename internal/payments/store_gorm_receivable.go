package payments

import (
	"context"
	"fmt"
	"uuid"

	"koro/internal/platform/db"
)

// InsertCollection implements CollectionStore.
// cross-org: the finance operation derives both parents before this insert.
func (s gormStore) InsertCollection(ctx context.Context, c ReceivableCollection) error {
	if err := db.Conn(ctx, s.orm).Create(&c).Error; err != nil {
		return fmt.Errorf("payments: insert collection: %w", constraints.Translate(err))
	}
	return nil
}

// GetCollection implements CollectionStore.
// cross-org: admin surface, caller checks finance authorization first.
func (s gormStore) GetCollection(ctx context.Context, id uuid.UUID, lock bool) (ReceivableCollection, error) {
	return take[ReceivableCollection]("get collection", db.LockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// UpdateCollection implements CollectionStore with a requested-state guard.
// cross-org: financial fields remain frozen; only decision fields change.
func (s gormStore) UpdateCollection(ctx context.Context, c ReceivableCollection) (ReceivableCollection, error) {
	var rows []ReceivableCollection
	err := db.Conn(ctx, s.orm).Raw(`UPDATE receivable_collections SET status = @status,
 approved_by = @approved_by, approved_at = @approved_at, rejected_by = @rejected_by,
 rejected_at = @rejected_at, rejection_reason = @reason, ledger_transaction_id = @txn
 WHERE id = @id AND status = 'requested' RETURNING *`, map[string]any{
		"id": c.ID, "status": c.Status, "approved_by": c.ApprovedBy, "approved_at": c.ApprovedAt,
		"rejected_by": c.RejectedBy, "rejected_at": c.RejectedAt, "reason": c.RejectionReason, "txn": c.LedgerTransactionID,
	}).Scan(&rows).Error
	if err != nil {
		return ReceivableCollection{}, fmt.Errorf("payments: decide collection: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		return ReceivableCollection{}, ErrInvalidState
	}
	return rows[0], nil
}

// ListCollections implements CollectionStore.
// cross-org: finance queue, optionally narrowed to a payee.
func (s gormStore) ListCollections(ctx context.Context, f CollectionFilter, after *TimeCursor, limit int) ([]ReceivableCollection, error) {
	q := db.Conn(ctx, s.orm).Model(&ReceivableCollection{})
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.PayeeID != nil {
		q = q.Where("payee_id = ?", *f.PayeeID)
	}
	if after != nil {
		q = q.Where("(created_at, id) < ( ?, ?)", after.CreatedAt, after.ID)
	}
	var rows []ReceivableCollection
	if err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("payments: list collections: %w", err)
	}
	return rows, nil
}

// PendingCollections implements CollectionStore. The caller holds the payee lock.
// cross-org: one immutable source line of a finance operation.
func (s gormStore) PendingCollections(ctx context.Context, lineID, self uuid.UUID) (int64, error) {
	var amount int64
	err := db.Conn(ctx, s.orm).Raw(`SELECT coalesce(sum(amount),0)::bigint FROM receivable_collections WHERE order_line_id = ? AND status = 'requested' AND id <> ?`, lineID, self).Scan(&amount).Error
	return amount, err
}

// CollectionsOfUser implements CollectionStore.
// cross-org: a user's financial staff references for KVKK.
func (s gormStore) CollectionsOfUser(ctx context.Context, userID uuid.UUID) ([]ReceivableCollection, error) {
	var rows []ReceivableCollection
	err := db.Conn(ctx, s.orm).Where("requested_by = @u OR approved_by = @u OR rejected_by = @u", map[string]any{"u": userID}).Order("id").Find(&rows).Error
	return rows, err
}
