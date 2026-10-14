package ledger

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/money"
)

// MemStore is the in-memory Store of service and matrix tests (05 §13.2).
// It emulates what the database enforces (the idempotency key, the balance
// and payee rules of ledger_txn_balanced); a lost transaction cannot be
// rolled back (db.NopTx). It is also the in-memory PayoutStore
// (store_mem_payout.go); MarkPayoutPaid records a bare paid payout for the
// integrity check's tests.
type MemStore struct {
	now func() time.Time

	mu      sync.Mutex
	txns    []Posted
	payouts map[uuid.UUID]memPayout // paid payouts recorded by MarkPayoutPaid
	po      *memPayouts             // the PayoutStore rows (BE-044)
}

type memPayout struct {
	payeeID uuid.UUID
	amount  int64
}

// NewMemStore returns an empty in-memory store; now dates the rows.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{now: now, payouts: map[uuid.UUID]memPayout{}}
}

var _ Store = (*MemStore)(nil)

// MarkPayoutPaid records a paid payout (tests of the integrity check).
func (s *MemStore) MarkPayoutPaid(payoutID, payeeID uuid.UUID, amount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payouts[payoutID] = memPayout{payeeID: payeeID, amount: amount}
}

// InsertTxn implements Store.
func (s *MemStore) InsertTxn(_ context.Context, h Transaction, entries []EntryRow) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.txns, func(p Posted) bool {
		return p.Kind == h.Kind && p.RefType == h.RefType && p.RefID == h.RefID
	}) {
		return false, nil
	}
	if len(entries) < 2 {
		return false, ErrUnbalanced
	}
	amounts := make([]int64, len(entries))
	for i, e := range entries {
		info, ok := accountInfo(e.Account)
		if !ok || info.PerPayee != (e.PayeeID != nil) || e.Amount == 0 {
			return false, ErrInvalidTxn
		}
		amounts[i] = e.Amount
	}
	if sum, err := money.Sum(amounts...); err != nil || sum != 0 {
		return false, ErrUnbalanced
	}
	now := s.now().UTC()
	h.CreatedAt = now
	p := Posted{Transaction: h, Entries: make([]EntryRow, len(entries))}
	for i, e := range entries {
		e.TransactionID, e.Currency, e.CreatedAt = h.ID, Currency, now
		p.Entries[i] = e
	}
	slices.SortFunc(p.Entries, func(a, b EntryRow) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	s.txns = append(s.txns, p)
	return true, nil
}

// ByRef implements Store.
func (s *MemStore) ByRef(_ context.Context, k Kind, r RefType, refID uuid.UUID) (Posted, error) {
	return s.find(func(p Posted) bool { return p.Kind == k && p.RefType == r && p.RefID == refID })
}

// ByID implements Store.
func (s *MemStore) ByID(_ context.Context, id uuid.UUID) (Posted, error) {
	return s.find(func(p Posted) bool { return p.ID == id })
}

func (s *MemStore) find(match func(Posted) bool) (Posted, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.txns, match)
	if i < 0 {
		return Posted{}, ErrNotFound
	}
	return clonePosted(s.txns[i]), nil
}

func clonePosted(p Posted) Posted {
	p.Entries = slices.Clone(p.Entries)
	return p
}

// LockPayees implements Store. Without transactions (db.NopTx) there is
// nothing to hold a lock until: service tests are sequential, and the
// race-free balance check is proven on PostgreSQL.
func (s *MemStore) LockPayees(context.Context, []uuid.UUID) error { return nil }

// PayeeBalance implements Store.
func (s *MemStore) PayeeBalance(_ context.Context, payeeID uuid.UUID) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return -s.payeeSum(payeeID), nil
}

// payeeSum is the sum of the payee's per-payee entries; mu is held.
func (s *MemStore) payeeSum(payeeID uuid.UUID) int64 {
	var sum int64
	for _, p := range s.txns {
		for _, e := range p.Entries {
			if info, _ := accountInfo(e.Account); info.PerPayee && e.PayeeID != nil && *e.PayeeID == payeeID {
				sum += e.Amount
			}
		}
	}
	return sum
}

// List implements Store.
func (s *MemStore) List(_ context.Context, f JournalFilter, after *JournalCursor, limit int) ([]Posted, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Posted
	for _, p := range s.txns {
		switch {
		case f.Kind != "" && p.Kind != f.Kind,
			f.RefType != "" && p.RefType != f.RefType,
			f.RefID != nil && p.RefID != *f.RefID,
			f.From != nil && p.OccurredAt.Before(*f.From),
			f.To != nil && !p.OccurredAt.Before(*f.To),
			after != nil && compareCursor(cursorOf(p), *after) >= 0:
			continue
		}
		out = append(out, clonePosted(p))
	}
	slices.SortFunc(out, func(a, b Posted) int { return compareCursor(cursorOf(b), cursorOf(a)) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RefundedShares implements Store.
func (s *MemStore) RefundedShares(_ context.Context, lineID uuid.UUID) (LineShares, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out LineShares
	for _, p := range s.txns {
		if p.Kind != KindRefundCompleted {
			continue
		}
		for _, e := range p.Entries {
			if e.OrderLineID != nil && *e.OrderLineID == lineID {
				out.add(e.Account, e.Amount)
			}
		}
	}
	return out, nil
}

// LineAllocations implements Store.
func (s *MemStore) LineAllocations(_ context.Context, lineIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uuid.UUID]int64, len(lineIDs))
	for _, p := range s.txns {
		if p.Kind != KindOrderAllocated && p.Kind != KindAdjustment {
			continue
		}
		for _, e := range p.Entries {
			if e.Account == AccountBuyerAdvances && e.OrderLineID != nil && slices.Contains(lineIDs, *e.OrderLineID) {
				out[*e.OrderLineID] += e.Amount
			}
		}
	}
	return out, nil
}

func cursorOf(p Posted) JournalCursor { return JournalCursor{OccurredAt: p.OccurredAt, ID: p.ID} }

// compareCursor orders journal positions ascending (occurred_at, id).
func compareCursor(a, b JournalCursor) int {
	return cmp.Or(a.OccurredAt.Compare(b.OccurredAt), bytes.Compare(a.ID[:], b.ID[:]))
}

// Unbalanced implements Store.
func (s *MemStore) Unbalanced(_ context.Context, limit int) ([]UnbalancedTxn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []UnbalancedTxn
	for _, p := range s.txns {
		var sum int64
		for _, e := range p.Entries {
			sum += e.Amount
		}
		if len(p.Entries) < 2 || sum != 0 {
			out = append(out, UnbalancedTxn{TransactionID: p.ID, Entries: len(p.Entries), Sum: &sum})
		}
	}
	return firstN(out, limit, func(a, b UnbalancedTxn) int { return bytes.Compare(a.TransactionID[:], b.TransactionID[:]) }), nil
}

// NegativePayeeBalances implements Store.
func (s *MemStore) NegativePayeeBalances(_ context.Context, limit int) ([]PayeeBalance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[uuid.UUID]bool{}
	var out []PayeeBalance
	for _, p := range s.txns {
		for _, e := range p.Entries {
			if e.PayeeID == nil || seen[*e.PayeeID] {
				continue
			}
			seen[*e.PayeeID] = true
			if sum := s.payeeSum(*e.PayeeID); sum > 0 {
				out = append(out, PayeeBalance{PayeeID: *e.PayeeID, Balance: -sum})
			}
		}
	}
	return firstN(out, limit, func(a, b PayeeBalance) int { return bytes.Compare(a.PayeeID[:], b.PayeeID[:]) }), nil
}

// LateEntries implements Store.
func (s *MemStore) LateEntries(_ context.Context, limit int) ([]LateEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LateEntry
	for _, p := range s.txns {
		for _, e := range p.Entries {
			if !e.CreatedAt.Equal(p.CreatedAt) {
				out = append(out, LateEntry{TransactionID: p.ID, EntryID: e.ID})
			}
		}
	}
	return firstN(out, limit, func(a, b LateEntry) int { return bytes.Compare(a.EntryID[:], b.EntryID[:]) }), nil
}

// PayoutMismatches implements Store.
func (s *MemStore) PayoutMismatches(_ context.Context, limit int) ([]PayoutMismatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PayoutMismatch
	paid := s.paidPayouts()
	for id, po := range paid {
		i := slices.IndexFunc(s.txns, func(p Posted) bool {
			return p.Kind == KindPayoutPaid && p.RefType == RefPayout && p.RefID == id
		})
		if i < 0 {
			out = append(out, PayoutMismatch{PayoutID: id, Problem: MismatchMissingTxn})
			continue
		}
		p := s.txns[i]
		pid := po.payeeID
		want := []Entry{{Account: AccountSellerPayable, PayeeID: &pid, Amount: po.amount}, {Account: AccountKoroBank, Amount: -po.amount}}
		got := make([]Entry, len(p.Entries))
		for j, e := range p.Entries {
			got[j] = e.Entry()
			got[j].OrderLineID = nil // the payout entries name no line
		}
		if !sameEntries(got, want) {
			txn := p.ID
			out = append(out, PayoutMismatch{PayoutID: id, TransactionID: &txn, Problem: MismatchEntries})
		}
	}
	for _, p := range s.txns {
		if _, ok := paid[p.RefID]; p.Kind == KindPayoutPaid && !ok {
			txn := p.ID
			out = append(out, PayoutMismatch{PayoutID: p.RefID, TransactionID: &txn, Problem: MismatchNotPaid})
		}
	}
	return firstN(out, limit, func(a, b PayoutMismatch) int { return bytes.Compare(a.PayoutID[:], b.PayoutID[:]) }), nil
}

// firstN sorts xs and keeps at most n.
func firstN[T any](xs []T, n int, by func(a, b T) int) []T {
	slices.SortFunc(xs, by)
	if len(xs) > n {
		xs = xs[:n]
	}
	return xs
}

// PurgeFinancialRecords implements Store. The in-memory ledger keeps no
// orders, payments, invoices or parties, the aggregates the purge deletes
// as a whole: it purges nothing. The rules are 000121's
// financial_records_purge, tested on PostgreSQL (db/migrations).
func (m *MemStore) PurgeFinancialRecords(context.Context, time.Time, int, bool) (int, error) {
	return 0, nil
}
