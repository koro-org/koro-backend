package ledger

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/money"
)

// The in-memory payout store emulates the CHECKs and guards of 000113 and
// 000115 that the service relies on (four eyes, one CHECK per state, a line
// in one live payout, Σ items = amount, items deleted only with a failed or
// cancelled payout). It writes only when the whole call is valid: db.NopTx
// cannot roll back. The account guard and the deferred "every line of a
// paid payout marked paid out" check need the other modules' rows and are
// proven on PostgreSQL.

var _ PayoutStore = (*MemStore)(nil)

// memPayouts holds the payout rows of a MemStore (mu of the store).
type memPayouts struct {
	rows  map[uuid.UUID]Payout
	items map[uuid.UUID][]PayoutItem // by payout
}

func (s *MemStore) payoutRows() *memPayouts {
	if s.po == nil {
		s.po = &memPayouts{rows: map[uuid.UUID]Payout{}, items: map[uuid.UUID][]PayoutItem{}}
	}
	return s.po
}

// InsertPayout implements PayoutStore.
func (s *MemStore) InsertPayout(_ context.Context, p Payout, items []PayoutItem) (Payout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	po := s.payoutRows()
	switch {
	case p.ID == uuid.Nil() || p.PayeeID == uuid.Nil() || p.OrgID == uuid.Nil() || p.CreatedBy == uuid.Nil():
		return Payout{}, ErrInvalidTxn
	case p.Status != StatusPendingApproval || p.ApprovedBy != nil || p.PaidAt != nil || p.CancelledAt != nil:
		return Payout{}, ErrInvalidState // payouts_initial_status_chk and the state CHECKs
	case p.Amount <= 0:
		return Payout{}, ErrInvalidTxn
	case len(items) == 0:
		return Payout{}, ErrInvalidTxn // payout_items_sum: a live payout has items
	}
	if _, dup := po.rows[p.ID]; dup {
		return Payout{}, ErrInvalidState
	}
	amounts := make([]int64, len(items))
	seen := map[uuid.UUID]bool{}
	for i, it := range items {
		if it.PayoutID != p.ID || it.PayeeID != p.PayeeID || it.Amount <= 0 || seen[it.OrderLineID] {
			return Payout{}, ErrLineNotPayable
		}
		seen[it.OrderLineID] = true
		if _, in := s.lineInPayout(it.OrderLineID); in {
			return Payout{}, ErrLineNotPayable // payout_items_line_uq
		}
		amounts[i] = it.Amount
	}
	if sum, err := money.Sum(amounts...); err != nil || sum != p.Amount {
		return Payout{}, ErrInvalidTxn // payout_items_sum
	}
	if p.Currency == "" {
		p.Currency = Currency
	}
	// The store owns both timestamps, as the column defaults of gormStore
	// (one clock, BE-069): the caller's are ignored.
	now := s.now().UTC().Truncate(time.Microsecond)
	p.CreatedAt, p.UpdatedAt = now, now
	po.rows[p.ID] = p
	stored := make([]PayoutItem, len(items))
	for i, it := range items {
		it.CreatedAt = p.CreatedAt
		stored[i] = it
	}
	slices.SortFunc(stored, func(a, b PayoutItem) int { return bytes.Compare(a.OrderLineID[:], b.OrderLineID[:]) })
	po.items[p.ID] = stored
	return p, nil
}

// lineInPayout reports the live payout holding a line; mu is held.
func (s *MemStore) lineInPayout(lineID uuid.UUID) (uuid.UUID, bool) {
	for id, its := range s.payoutRows().items {
		if slices.ContainsFunc(its, func(it PayoutItem) bool { return it.OrderLineID == lineID }) {
			return id, true
		}
	}
	return uuid.Nil(), false
}

// GetPayout implements PayoutStore.
func (s *MemStore) GetPayout(_ context.Context, id uuid.UUID, _ bool) (Payout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payoutRows().rows[id]
	if !ok {
		return Payout{}, ErrNotFound
	}
	return p, nil
}

// PayoutItems implements PayoutStore.
func (s *MemStore) PayoutItems(_ context.Context, payoutID uuid.UUID) ([]PayoutItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.payoutRows().items[payoutID]), nil
}

// TransitionPayout implements PayoutStore.
func (s *MemStore) TransitionPayout(_ context.Context, next Payout, from PayoutStatus) (Payout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	po := s.payoutRows()
	cur, ok := po.rows[next.ID]
	if !ok {
		return Payout{}, ErrNotFound
	}
	if cur.Status != from {
		return Payout{}, ErrInvalidState
	}
	if err := checkPayoutRow(cur, next); err != nil {
		return Payout{}, err
	}
	// Only the state columns change (payouts_frozen).
	cur.Status, cur.ApprovedBy, cur.ApprovedAt = next.Status, next.ApprovedBy, next.ApprovedAt
	cur.PaidBy, cur.PaidAt, cur.BankReference, cur.ReceiptFileID = next.PaidBy, next.PaidAt, next.BankReference, next.ReceiptFileID
	cur.FailureReason, cur.CancelledAt, cur.CancelReason = next.FailureReason, next.CancelledAt, next.CancelReason
	if now := s.now().UTC().Truncate(time.Microsecond); now.After(cur.UpdatedAt) {
		cur.UpdatedAt = now // never before the last stamp (a test clock set back)
	}
	po.rows[cur.ID] = cur
	return cur, nil
}

// checkPayoutRow emulates the CHECKs and freezes of 000113 for a transition
// of cur to next.
func checkPayoutRow(cur, next Payout) error {
	set := func(v any) bool {
		switch x := v.(type) {
		case *uuid.UUID:
			return x != nil
		case *string:
			return x != nil
		}
		return false
	}
	paid := next.Status == StatusPaid
	switch {
	case !next.Status.Valid():
		return ErrInvalidState
	case next.ApprovedBy != nil && *next.ApprovedBy == cur.CreatedBy:
		return authz.ErrFourEyes // payouts_four_eyes_chk
	case cur.ApprovedBy != nil && (next.ApprovedBy == nil || *next.ApprovedBy != *cur.ApprovedBy):
		return ErrInvalidState // payouts_approval_frozen
	case (next.ApprovedBy == nil) != (next.ApprovedAt == nil),
		next.Status == StatusPendingApproval && next.ApprovedBy != nil,
		(next.Status == StatusApproved || paid || next.Status == StatusFailed) && next.ApprovedBy == nil:
		return ErrInvalidState // payouts_approved_chk
	case paid != (next.PaidAt != nil), paid != set(next.PaidBy), paid != set(next.ReceiptFileID),
		paid && next.BankReference == nil,
		next.BankReference != nil && !paid && next.Status != StatusFailed:
		return ErrInvalidState // payouts_paid_chk
	case (next.Status == StatusCancelled) != (next.CancelledAt != nil),
		(next.Status == StatusCancelled) != set(next.CancelReason):
		return ErrInvalidState // payouts_cancelled_chk
	case (next.Status == StatusFailed) != set(next.FailureReason):
		return ErrInvalidState // payouts_failed_chk
	}
	return nil
}

// DeleteItems implements PayoutStore.
func (s *MemStore) DeleteItems(_ context.Context, payoutID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	po := s.payoutRows()
	if p, ok := po.rows[payoutID]; ok && p.Status.Live() {
		return 0, ErrInvalidState // payout_items_guard: restrict_violation
	}
	n := len(po.items[payoutID])
	delete(po.items, payoutID)
	return n, nil
}

// ListPayouts implements PayoutStore.
func (s *MemStore) ListPayouts(_ context.Context, f PayoutFilter, after *PayoutCursor, limit int) ([]Payout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Payout
	for _, p := range s.payoutRows().rows {
		switch {
		case f.OrgID != nil && p.OrgID != *f.OrgID,
			f.PayeeID != nil && p.PayeeID != *f.PayeeID,
			f.Status != "" && p.Status != f.Status,
			after != nil && comparePayoutCursor(PayoutCursor{CreatedAt: p.CreatedAt, ID: p.ID}, *after) >= 0:
			continue
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Payout) int {
		return comparePayoutCursor(PayoutCursor{CreatedAt: b.CreatedAt, ID: b.ID}, PayoutCursor{CreatedAt: a.CreatedAt, ID: a.ID})
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// comparePayoutCursor orders payout positions ascending (created_at, id).
func comparePayoutCursor(a, b PayoutCursor) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), bytes.Compare(a.ID[:], b.ID[:]))
}

// LinesInPayouts implements PayoutStore.
func (s *MemStore) LinesInPayouts(_ context.Context, lineIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]uuid.UUID{}
	for _, id := range lineIDs {
		if p, ok := s.lineInPayout(id); ok {
			out[id] = p
		}
	}
	return out, nil
}

// PayoutAmounts implements PayoutStore.
func (s *MemStore) PayoutAmounts(_ context.Context, payeeID uuid.UUID) (PayoutAmounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out PayoutAmounts
	for _, p := range s.payoutRows().rows {
		switch {
		case p.PayeeID != payeeID:
		case p.Status.Open():
			out.Pending += p.Amount
		case p.Status == StatusPaid:
			out.PaidOut += p.Amount
		}
	}
	return out, nil
}

// PayeesOwed implements PayoutStore.
func (s *MemStore) PayeesOwed(_ context.Context, limit int) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, p := range s.txns {
		for _, e := range p.Entries {
			if e.PayeeID == nil || seen[*e.PayeeID] {
				continue
			}
			seen[*e.PayeeID] = true
			if s.payeeSum(*e.PayeeID) < 0 {
				out = append(out, *e.PayeeID)
			}
		}
	}
	return firstN(out, limit, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }), nil
}

// HasOpenPayouts implements PayoutStore.
func (s *MemStore) HasOpenPayouts(_ context.Context, orgID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.payoutRows().rows {
		if p.OrgID == orgID && p.Status.Open() {
			return true, nil
		}
	}
	return false, nil
}

// PayoutsBy implements PayoutStore.
func (s *MemStore) PayoutsBy(_ context.Context, userID uuid.UUID) ([]Payout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Payout
	for _, p := range s.payoutRows().rows {
		if p.CreatedBy == userID || p.ApprovedBy != nil && *p.ApprovedBy == userID || p.PaidBy != nil && *p.PaidBy == userID {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Payout) int {
		return comparePayoutCursor(PayoutCursor{CreatedAt: a.CreatedAt, ID: a.ID}, PayoutCursor{CreatedAt: b.CreatedAt, ID: b.ID})
	})
	return out, nil
}

// paidPayouts returns the paid payouts (the stored ones and those recorded
// with MarkPayoutPaid) for the integrity check; mu is held.
func (s *MemStore) paidPayouts() map[uuid.UUID]memPayout {
	out := make(map[uuid.UUID]memPayout, len(s.payouts))
	for id, p := range s.payouts {
		out[id] = p
	}
	for id, p := range s.payoutRows().rows {
		if p.Status == StatusPaid {
			out[id] = memPayout{payeeID: p.PayeeID, amount: p.Amount}
		}
	}
	return out
}
