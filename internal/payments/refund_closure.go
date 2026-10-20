package payments

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/idempotency"
)

// CloseRefund releases an approved refund that never completed. Its original
// approval remains evidence; completed refunds require a separate accounting
// correction and cannot enter this path. No payment or ledger amounts change.
func (s *service) CloseRefund(ctx context.Context, a authz.Actor, id uuid.UUID, failed bool, reason string, proof identity.ReauthProof) (Refund, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundList, adminSurface); err != nil {
		return Refund{}, err
	}
	act, loc := authz.PaymentsRefundCancel, "body.cancel_reason"
	if failed {
		act, loc = authz.PaymentsRefundMarkFailed, "body.failure_reason"
	}
	r, err := s.d.Store.GetRefund(ctx, id, false)
	if err != nil {
		return Refund{}, err
	}
	if err := authz.Can(ctx, a, act, refundResource(r)); err != nil {
		return Refund{}, err
	}
	why, err := cleanText(loc, reason, 1, 500)
	if err != nil {
		return Refund{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(act, authz.TypeRefund, &r.ID, nil, nil))
	if err != nil {
		return Refund{}, err
	}
	hash, err := idempotency.Hash(struct {
		Action authz.Action
		ID     uuid.UUID
		Reason string
	}{act, id, why})
	if err != nil {
		return Refund{}, err
	}
	return s.transitionRefund(ctx, hash, id, func(ctx context.Context, cur Refund) (Refund, error) {
		if cur.Status != RefundStatusApproved {
			return Refund{}, ErrInvalidState
		}
		if err := authz.Can(ctx, a, act, refundResource(cur)); err != nil {
			return Refund{}, err
		}
		next, now, by := cur, s.now(), a.UserID
		next.ClosedAt, next.ClosedBy = &now, &by
		if failed {
			next.Status, next.FailureReason = RefundStatusFailed, &why
		} else {
			next.Status, next.CancelReason = RefundStatusCancelled, &why
		}
		stored, err := s.d.Store.UpdateRefund(ctx, next, RefundStatusApproved)
		if err != nil {
			return Refund{}, err
		}
		return stored, s.d.Audit.Log(ctx, refundAudit(a, act, stored, why, map[string]any{
			"from": string(cur.Status), "requested_by": cur.RequestedBy.String(), audit.MetaReauth: kind,
		}))
	})
}
