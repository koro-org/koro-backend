package ledger

import (
	"context"
	"fmt"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// NewGormPayoutStore returns the PostgreSQL payout store over payouts and
// payout_items (02 §21.4–5), read with order_lines/ledger_entries where a
// query needs them (01 §1.4: the ledger owns payouts; the other tables are
// only read).
func NewGormPayoutStore(orm *gorm.DB) PayoutStore { return gormStore{orm: orm} }

var _ PayoutStore = gormStore{}

// InsertPayout implements PayoutStore: the payout, then its items (the
// item guard needs the pending payout; payout_items_sum checks the sum at
// COMMIT).
//
// cross-org: the payout's seller org comes from its payee row (06 H-38);
// the payouts_payee_id_org_id_fkey pins it.
func (s gormStore) InsertPayout(ctx context.Context, p Payout, items []PayoutItem) (Payout, error) {
	conn := db.Conn(ctx, s.orm)
	if err := conn.Create(&p).Error; err != nil {
		return Payout{}, fmt.Errorf("ledger: insert payout: %w", translatePayout(err))
	}
	if len(items) > 0 {
		if err := conn.Create(&items).Error; err != nil {
			return Payout{}, fmt.Errorf("ledger: insert payout items: %w", translatePayout(err))
		}
	}
	return s.GetPayout(ctx, p.ID, false)
}

// GetPayout implements PayoutStore.
//
// cross-org: addressed by ID (the admin surface); the org surface compares
// the payout's org with the org of the path.
func (s gormStore) GetPayout(ctx context.Context, id uuid.UUID, lock bool) (Payout, error) {
	q := db.Conn(ctx, s.orm)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var p Payout
	err := q.Where("id = ?", id).Take(&p).Error
	switch {
	case db.IsNotFound(err):
		return Payout{}, ErrNotFound
	case err != nil:
		return Payout{}, fmt.Errorf("ledger: get payout: %w", err)
	}
	return p, nil
}

// PayoutItems implements PayoutStore.
//
// cross-org: the items of a payout the caller resolved.
func (s gormStore) PayoutItems(ctx context.Context, payoutID uuid.UUID) ([]PayoutItem, error) {
	var out []PayoutItem
	if err := db.Conn(ctx, s.orm).Where("payout_id = ?", payoutID).Order("order_line_id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("ledger: payout items: %w", err)
	}
	return out, nil
}

// TransitionPayout implements PayoutStore with a targeted UPDATE of the
// state columns (06 H-56: the frozen columns are never written).
//
// cross-org: addressed by ID; the service authorised the payout.
func (s gormStore) TransitionPayout(ctx context.Context, next Payout, from PayoutStatus) (Payout, error) {
	var rows []Payout
	err := db.Conn(ctx, s.orm).Raw(`UPDATE payouts SET
		status = @status, approved_by = @approved_by, approved_at = @approved_at,
		paid_by = @paid_by, paid_at = @paid_at, bank_reference = @bank_reference, receipt_file_id = @receipt_file_id,
		failure_reason = @failure_reason, cancelled_at = @cancelled_at, cancel_reason = @cancel_reason
		WHERE id = @id AND status = @from
		RETURNING *`, map[string]any{
		"id": next.ID, "from": from, "status": next.Status, "approved_by": next.ApprovedBy, "approved_at": next.ApprovedAt,
		"paid_by": next.PaidBy, "paid_at": next.PaidAt, "bank_reference": next.BankReference,
		"receipt_file_id": next.ReceiptFileID, "failure_reason": next.FailureReason, "cancelled_at": next.CancelledAt,
		"cancel_reason": next.CancelReason,
	}).Scan(&rows).Error
	if err != nil {
		return Payout{}, fmt.Errorf("ledger: payout %s: %w", next.Status, translatePayout(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetPayout(ctx, next.ID, false); err != nil {
			return Payout{}, err
		}
		return Payout{}, ErrInvalidState
	}
	return rows[0], nil
}

// ReplaceReceipt implements PayoutStore with a targeted UPDATE of the
// receipt alone.
//
// cross-org: addressed by ID; the service authorised the payout.
func (s gormStore) ReplaceReceipt(ctx context.Context, id, cur, next uuid.UUID) (Payout, error) {
	var rows []Payout
	err := db.Conn(ctx, s.orm).Raw(`UPDATE payouts SET receipt_file_id = @next
		WHERE id = @id AND status = 'paid' AND receipt_file_id = @cur RETURNING *`,
		map[string]any{"id": id, "cur": cur, "next": next}).Scan(&rows).Error
	if err != nil {
		return Payout{}, fmt.Errorf("ledger: replace payout receipt: %w", translatePayout(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetPayout(ctx, id, false); err != nil {
			return Payout{}, err
		}
		return Payout{}, ErrInvalidState
	}
	return rows[0], nil
}

// DeleteItems implements PayoutStore (payout_items_guard allows it for a
// failed or cancelled payout only).
//
// cross-org: the items of a payout the caller resolved.
func (s gormStore) DeleteItems(ctx context.Context, payoutID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM payout_items WHERE payout_id = ?`, payoutID)
	if res.Error != nil {
		return 0, fmt.Errorf("ledger: delete payout items: %w", translatePayout(res.Error))
	}
	return int(res.RowsAffected), nil
}

// ListPayouts implements PayoutStore.
//
// cross-org: f.OrgID scopes the org surface (org_id = the org of the
// path); the admin list spans every org (ledger.payout.list on /admin).
func (s gormStore) ListPayouts(ctx context.Context, f PayoutFilter, after *PayoutCursor, limit int) ([]Payout, error) {
	q := db.Conn(ctx, s.orm).Model(&Payout{})
	if f.OrgID != nil {
		q = q.Scopes(db.ScopeOrg(*f.OrgID))
	}
	if f.PayeeID != nil {
		q = q.Where("payee_id = ?", *f.PayeeID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if after != nil {
		q = q.Where("(created_at, id) < ( ?, ?)", after.CreatedAt, after.ID)
	}
	var out []Payout
	if err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("ledger: list payouts: %w", err)
	}
	return out, nil
}

// LinesInPayouts implements PayoutStore. Items of failed and cancelled
// payouts are deleted, so every item is of a live payout.
//
// cross-org: the lines of one payee the caller resolved.
func (s gormStore) LinesInPayouts(ctx context.Context, lineIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := map[uuid.UUID]uuid.UUID{}
	if len(lineIDs) == 0 {
		return out, nil
	}
	var rows []PayoutItem
	if err := db.Conn(ctx, s.orm).Where("order_line_id IN ?", lineIDs).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("ledger: lines in payouts: %w", err)
	}
	for _, r := range rows {
		out[r.OrderLineID] = r.PayoutID
	}
	return out, nil
}

// PayoutAmounts implements PayoutStore.
//
// cross-org: one payee the caller resolved.
func (s gormStore) PayoutAmounts(ctx context.Context, payeeID uuid.UUID) (PayoutAmounts, error) {
	var row struct {
		Pending int64 `gorm:"column:pending"`
		PaidOut int64 `gorm:"column:paid_out"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT
		coalesce(sum(amount) FILTER (WHERE status IN ('pending_approval', 'approved')), 0)::bigint AS pending,
		coalesce(sum(amount) FILTER (WHERE status = 'paid'), 0)::bigint AS paid_out
		FROM payouts WHERE payee_id = ?`, payeeID).Scan(&row).Error
	if err != nil {
		return PayoutAmounts{}, fmt.Errorf("ledger: payout amounts: %w", db.Translate(err))
	}
	return PayoutAmounts{Pending: row.Pending, PaidOut: row.PaidOut}, nil
}

// PayeesOwed implements PayoutStore.
//
// cross-org: finance's view of every payee (ledger.payable.list).
func (s gormStore) PayeesOwed(ctx context.Context, limit int) ([]uuid.UUID, error) {
	var rows []struct {
		PayeeID uuid.UUID `gorm:"column:payee_id"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.payee_id
		FROM ledger_entries e JOIN ledger_accounts a ON a.code = e.account_code
		WHERE a.per_payee AND e.payee_id IS NOT NULL
		GROUP BY e.payee_id
		HAVING sum(e.amount) < 0
		ORDER BY e.payee_id LIMIT ?`, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("ledger: payees owed: %w", err)
	}
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.PayeeID
	}
	return out, nil
}

// HasOpenPayouts implements PayoutStore.
func (s gormStore) HasOpenPayouts(ctx context.Context, orgID uuid.UUID) (bool, error) {
	var open bool
	err := db.Conn(ctx, s.orm).Raw(`SELECT EXISTS (SELECT 1 FROM payouts
		WHERE org_id = @org_id AND status IN ('pending_approval', 'approved'))`, map[string]any{"org_id": orgID}).Scan(&open).Error
	if err != nil {
		return false, fmt.Errorf("ledger: open payouts: %w", err)
	}
	return open, nil
}

// PayoutsBy implements PayoutStore.
//
// cross-org: a data subject's own records (KVKK export).
func (s gormStore) PayoutsBy(ctx context.Context, userID uuid.UUID) ([]Payout, error) {
	var out []Payout
	err := db.Conn(ctx, s.orm).Where("created_by = ? OR approved_by = ? OR paid_by = ?", userID, userID, userID).
		Order("created_at, id").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("ledger: payouts by a user: %w", err)
	}
	return out, nil
}
