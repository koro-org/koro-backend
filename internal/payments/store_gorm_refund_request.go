package payments

import (
	"context"
	"fmt"
	"uuid"

	"koro/internal/platform/db"
)

// InsertRefundRequest implements RefundRequestStore.
//
// cross-org: the request of a buyer's order line; org_id and seller_org_id
// come from the order and the line (composite FKs, 06 H-38).
func (s gormStore) InsertRefundRequest(ctx context.Context, r RefundRequest) error {
	if r.Currency == "" {
		r.Currency = Currency
	}
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return fmt.Errorf("payments: insert refund request: %w", constraints.Translate(err))
	}
	return nil
}

// GetRefundRequest implements RefundRequestStore.
//
// cross-org: a request is addressed by ID; the service authorises the
// reader against its buyer and seller (requestResource).
func (s gormStore) GetRefundRequest(ctx context.Context, id uuid.UUID, lock bool) (RefundRequest, error) {
	return take[RefundRequest]("get refund request", db.LockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// ListRefundRequests implements RefundRequestStore (refund_requests_org_idx,
// _seller_idx, _pending_idx).
//
// cross-org: the buyer's list sets OrgID, the seller's SellerOrgID; only
// the admin queue lists every org's.
func (s gormStore) ListRefundRequests(ctx context.Context, f RefundRequestFilter, after *TimeCursor, limit int) ([]RefundRequest, error) {
	q := db.Conn(ctx, s.orm).Model(&RefundRequest{})
	if f.OrgID != nil {
		q = q.Where("org_id = ?", *f.OrgID)
	}
	if f.SellerOrgID != nil {
		q = q.Where("seller_org_id = ?", *f.SellerOrgID)
	}
	if f.OrderID != nil {
		q = q.Where("order_id = ?", *f.OrderID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", string(f.Status))
	}
	if f.Pending {
		q = q.Where("status IN ?", []string{string(RequestOpen), string(RequestInReview)})
	}
	order := "created_at DESC, id DESC"
	if f.Oldest {
		order = "created_at, id"
	}
	if after != nil {
		cmp := "<"
		if f.Oldest {
			cmp = ">"
		}
		q = q.Where("(created_at, id) "+cmp+" ( ?, ?)", after.CreatedAt, after.ID)
	}
	var out []RefundRequest
	if err := q.Order(order).Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: list refund requests: %w", err)
	}
	return out, nil
}

// RefundRequestsOfOrder implements RefundRequestStore (refund_requests_order_idx).
//
// cross-org: the requests of one order the caller resolved and authorised.
func (s gormStore) RefundRequestsOfOrder(ctx context.Context, orderID uuid.UUID) ([]RefundRequest, error) {
	var out []RefundRequest
	if err := db.Conn(ctx, s.orm).Where("order_id = ?", orderID).Order("created_at, id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("payments: refund requests of order: %w", err)
	}
	return out, nil
}

// UpdateRefundRequest implements RefundRequestStore: a targeted UPDATE of
// the running columns (the frozen ones are never written; updated_at is the
// trigger's), guarded by the current status, RETURNING the row.
//
// cross-org: a request is addressed by ID after its authorisation.
func (s gormStore) UpdateRefundRequest(ctx context.Context, next RefundRequest, from RefundRequestStatus) (RefundRequest, error) {
	var rows []RefundRequest
	err := db.Conn(ctx, s.orm).Raw(`UPDATE refund_requests SET
		status = @status, reviewed_by = @reviewed_by, reviewed_at = @reviewed_at, decided_by = @decided_by,
		decided_at = @decided_at, rejection_reason = @rejection_reason, refund_id = @refund_id,
		withdrawn_by = @withdrawn_by, withdrawn_at = @withdrawn_at
		WHERE id = @id AND status = @from
		RETURNING *`, map[string]any{
		"id": next.ID, "from": string(from), "status": string(next.Status), "reviewed_by": next.ReviewedBy,
		"reviewed_at": next.ReviewedAt, "decided_by": next.DecidedBy, "decided_at": next.DecidedAt,
		"rejection_reason": next.RejectionReason, "refund_id": next.RefundID, "withdrawn_by": next.WithdrawnBy,
		"withdrawn_at": next.WithdrawnAt,
	}).Scan(&rows).Error
	if err != nil {
		return RefundRequest{}, fmt.Errorf("payments: update refund request: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetRefundRequest(ctx, next.ID, false); err != nil {
			return RefundRequest{}, err
		}
		return RefundRequest{}, ErrInvalidState
	}
	return rows[0], nil
}

// RefundRequestsOfUser implements RefundRequestStore.
//
// cross-org: a user's own requests and staff references (KVKK export).
func (s gormStore) RefundRequestsOfUser(ctx context.Context, userID uuid.UUID) ([]RefundRequest, error) {
	var out []RefundRequest
	err := db.Conn(ctx, s.orm).Where("requested_by = @u OR reviewed_by = @u OR decided_by = @u OR withdrawn_by = @u",
		map[string]any{"u": userID}).Order("id").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("payments: refund requests of user: %w", err)
	}
	return out, nil
}
