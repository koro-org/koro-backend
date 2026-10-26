package payments

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/iban"
)

// The refund requests of MemStore (BE-132) mirror 000156: the initial
// status, one waiting request per line, the frozen request, the review,
// decision and withdrawal once set are final, one CHECK per state and the
// eyes.

// InsertRefundRequest implements RefundRequestStore.
func (m *MemStore) InsertRefundRequest(_ context.Context, r RefundRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requests == nil {
		m.requests = map[uuid.UUID]RefundRequest{}
	}
	if _, dup := m.requests[r.ID]; dup {
		return fmt.Errorf("payments: insert refund request %s: duplicate id", r.ID)
	}
	if r.Status != RequestOpen {
		return errors.New("payments: insert refund request: a new request is open (refund_requests_initial_status)")
	}
	if err := checkRequest(r); err != nil {
		return err
	}
	for _, o := range m.requests {
		if o.OrderLineID == r.OrderLineID && o.Status.Pending() {
			return ErrRefundRequestOpen // refund_requests_line_pending_uq
		}
	}
	if r.Currency == "" {
		r.Currency = Currency
	}
	t := m.stamp()
	r.CreatedAt, r.UpdatedAt = t, t
	m.requests[r.ID] = r
	return nil
}

// GetRefundRequest implements RefundRequestStore.
func (m *MemStore) GetRefundRequest(_ context.Context, id uuid.UUID, _ bool) (RefundRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.requests[id]
	if !ok {
		return RefundRequest{}, ErrNotFound
	}
	return r, nil
}

// ListRefundRequests implements RefundRequestStore.
func (m *MemStore) ListRefundRequests(_ context.Context, f RefundRequestFilter, after *TimeCursor, limit int) ([]RefundRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RefundRequest
	for _, r := range m.requests {
		switch {
		case f.OrgID != nil && r.OrgID != *f.OrgID,
			f.SellerOrgID != nil && r.SellerOrgID != *f.SellerOrgID,
			f.OrderID != nil && r.OrderID != *f.OrderID,
			f.Status != "" && r.Status != f.Status,
			f.Pending && !r.Status.Pending():
			continue
		case after != nil && !f.Oldest && compareTime(r.CreatedAt, r.ID, after.CreatedAt, after.ID) >= 0,
			after != nil && f.Oldest && compareTime(r.CreatedAt, r.ID, after.CreatedAt, after.ID) <= 0:
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b RefundRequest) int {
		if f.Oldest {
			return compareTime(a.CreatedAt, a.ID, b.CreatedAt, b.ID)
		}
		return compareTime(b.CreatedAt, b.ID, a.CreatedAt, a.ID)
	})
	return firstN(out, limit), nil
}

// RefundRequestsOfOrder implements RefundRequestStore.
func (m *MemStore) RefundRequestsOfOrder(_ context.Context, orderID uuid.UUID) ([]RefundRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RefundRequest
	for _, r := range m.requests {
		if r.OrderID == orderID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b RefundRequest) int { return compareTime(a.CreatedAt, a.ID, b.CreatedAt, b.ID) })
	return out, nil
}

// UpdateRefundRequest implements RefundRequestStore.
func (m *MemStore) UpdateRefundRequest(_ context.Context, next RefundRequest, from RefundRequestStatus) (RefundRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.requests[next.ID]
	switch {
	case !ok:
		return RefundRequest{}, ErrNotFound
	case cur.Status != from:
		return RefundRequest{}, ErrInvalidState
	case cur.OrderID != next.OrderID || cur.OrderLineID != next.OrderLineID || cur.OrgID != next.OrgID ||
		cur.SellerOrgID != next.SellerOrgID || cur.OrderNo != next.OrderNo || cur.LineTitle != next.LineTitle ||
		cur.Amount != next.Amount || cur.Reason != next.Reason || cur.Explanation != next.Explanation ||
		cur.TargetIBAN != next.TargetIBAN || !sameID(cur.EvidenceFileID, next.EvidenceFileID) || cur.RequestedBy != next.RequestedBy:
		return RefundRequest{}, fmt.Errorf("payments: refund request %s: a frozen column changed (refund_requests_frozen)", next.ID)
	case cur.ReviewedAt != nil && (!sameID(cur.ReviewedBy, next.ReviewedBy) || !sameTime(cur.ReviewedAt, next.ReviewedAt)):
		return RefundRequest{}, fmt.Errorf("payments: refund request %s: the review is frozen (refund_requests_review_frozen)", next.ID)
	case cur.DecidedAt != nil || cur.WithdrawnAt != nil:
		return RefundRequest{}, fmt.Errorf("payments: refund request %s: a decided or withdrawn request is final", next.ID)
	}
	if err := checkRequest(next); err != nil {
		return RefundRequest{}, err
	}
	for _, o := range m.requests {
		if next.RefundID != nil && o.ID != next.ID && sameID(o.RefundID, next.RefundID) {
			return RefundRequest{}, fmt.Errorf("payments: refund request %s: refund_requests_refund_uq", next.ID)
		}
	}
	next.CreatedAt, next.UpdatedAt = cur.CreatedAt, m.touch(cur.UpdatedAt)
	m.requests[next.ID] = next
	return next, nil
}

// RefundRequestsOfUser implements RefundRequestStore.
func (m *MemStore) RefundRequestsOfUser(_ context.Context, userID uuid.UUID) ([]RefundRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	is := func(id *uuid.UUID) bool { return id != nil && *id == userID }
	var out []RefundRequest
	for _, r := range m.requests {
		if r.RequestedBy == userID || is(r.ReviewedBy) || is(r.DecidedBy) || is(r.WithdrawnBy) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b RefundRequest) int { return compareUUID(a.ID, b.ID) })
	return out, nil
}

// checkRequest applies the CHECKs of 000156 to r.
func checkRequest(r RefundRequest) error {
	decided := r.Status == RequestAccepted || r.Status == RequestRejected
	withdrawn := r.Status == RequestWithdrawn
	switch {
	case !r.Status.Valid() || r.Amount <= 0 || !slices.Contains(RequestReasons(), r.Reason) || r.OrderNo == "" || r.LineTitle == "":
		return fmt.Errorf("payments: refund request %s: a column CHECK of 000156 fails", r.ID)
	case r.Currency != "" && r.Currency != Currency:
		return fmt.Errorf("payments: refund request %s: refund_requests_currency_chk", r.ID)
	case !iban.Valid(r.TargetIBAN):
		return ErrIBANInvalid // refund_requests_target_iban_chk
	case r.ReviewedBy != nil && *r.ReviewedBy == r.RequestedBy, r.DecidedBy != nil && *r.DecidedBy == r.RequestedBy:
		return authz.ErrFourEyes // refund_requests_review_eyes_chk, refund_requests_decision_eyes_chk
	case (r.ReviewedBy == nil) != (r.ReviewedAt == nil), r.Status == RequestInReview && r.ReviewedBy == nil,
		r.Status == RequestOpen && r.ReviewedBy != nil:
		return fmt.Errorf("payments: refund request %s: review columns do not match %s (refund_requests_reviewed_chk)", r.ID, r.Status)
	case decided != (r.DecidedBy != nil) || decided != (r.DecidedAt != nil):
		return fmt.Errorf("payments: refund request %s: decision columns do not match %s (refund_requests_decided_chk)", r.ID, r.Status)
	case (r.Status == RequestAccepted) != (r.RefundID != nil):
		return fmt.Errorf("payments: refund request %s: refund_requests_accepted_chk", r.ID)
	case (r.Status == RequestRejected) != (r.RejectionReason != nil):
		return fmt.Errorf("payments: refund request %s: refund_requests_rejected_chk", r.ID)
	case withdrawn != (r.WithdrawnBy != nil) || withdrawn != (r.WithdrawnAt != nil):
		return fmt.Errorf("payments: refund request %s: refund_requests_withdrawn_chk", r.ID)
	}
	return nil
}

// SeedRefundRequest stores r as it is, any status (matrix fixtures); the
// store stamps created_at and updated_at, as on every insert.
func (m *MemStore) SeedRefundRequest(r RefundRequest) error {
	if r.Currency == "" {
		r.Currency = Currency
	}
	if err := checkRequest(r); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requests == nil {
		m.requests = map[uuid.UUID]RefundRequest{}
	}
	if _, dup := m.requests[r.ID]; dup {
		return fmt.Errorf("payments: seed refund request %s: duplicate id", r.ID)
	}
	t := m.stamp()
	r.CreatedAt, r.UpdatedAt = t, t
	m.requests[r.ID] = r
	return nil
}

// RefundRequests returns every stored request (test assertions), oldest first.
func (m *MemStore) RefundRequests() []RefundRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RefundRequest, 0, len(m.requests))
	for _, r := range m.requests {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b RefundRequest) int { return compareTime(a.CreatedAt, a.ID, b.CreatedAt, b.ID) })
	return out
}
