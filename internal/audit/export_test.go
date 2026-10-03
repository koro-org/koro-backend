package audit

import (
	"context"
	"math"
	"slices"

	"koro/internal/platform/civil"
)

// Chain runs one audit.chain pass.
func Chain(ctx context.Context, m Module) (int, error) { return m.Service.(*service).chain(ctx) }

// DailyRoots runs audit.daily_root.
func DailyRoots(ctx context.Context, m Module) error { return m.Service.(*service).dailyRoots(ctx) }

// VerifyRecent runs audit.verify_chain.
func VerifyRecent(ctx context.Context, m Module) error { return m.Service.(*service).verifyRecent(ctx) }

// VerifyRange verifies the days from..to without authz or logging.
func VerifyRange(ctx context.Context, m Module, from, to civil.Date) (VerifyResult, error) {
	return m.Service.(*service).verifyRange(ctx, from, to)
}

// The Tamper* methods act like an attacker with owner rights who bypasses
// the grants and audit_events_guard (the database-side test does the same
// with ALTER TABLE ... DISABLE TRIGGER).

// TamperDelete removes the row at chainSeq.
func (s *MemStore) TamperDelete(chainSeq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = slices.DeleteFunc(s.rows, func(r Record) bool { return r.ChainSeq != nil && *r.ChainSeq == chainSeq })
}

// TamperEdit changes the row at chainSeq in place.
func (s *MemStore) TamperEdit(chainSeq int64, edit func(*Record)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if r := s.rows[i]; r.ChainSeq != nil && *r.ChainSeq == chainSeq {
			edit(&s.rows[i])
		}
	}
}

// TamperInsertAt shifts the rows at and after chainSeq by one and inserts r
// there, linked to its predecessor (a forged row in the middle). With
// rechain, every later hash is recomputed as well, so only the signed
// daily roots can reveal the forgery.
func (s *MemStore) TamperInsertAt(chainSeq int64, r Record, rechain bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if c := s.rows[i].ChainSeq; c != nil && *c >= chainSeq {
			v := *c + 1
			s.rows[i].ChainSeq = &v
		}
	}
	s.seq++
	r.Seq, r.ChainSeq = s.seq, &chainSeq
	s.rows = append(s.rows, r)
	to := chainSeq
	if rechain {
		to = math.MaxInt64
	}
	s.rehash(chainSeq, to)
}

// TamperRenumber closes the gap left by a deleted row at chainSeq: every
// later row moves down by one and the chain is re-hashed from there on.
func (s *MemStore) TamperRenumber(chainSeq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if c := s.rows[i].ChainSeq; c != nil && *c > chainSeq {
			v := *c - 1
			s.rows[i].ChainSeq = &v
		}
	}
	s.rehash(chainSeq, math.MaxInt64)
}

// rehash recomputes prev_hash and row_hash of the rows from..to (chain
// order), each linked to its stored predecessor.
func (s *MemStore) rehash(from, to int64) {
	prev := zeroHash
	for _, x := range s.chained() {
		if seq := *x.ChainSeq; seq >= from && seq <= to {
			h, _ := RowHash(prev, x)
			for i := range s.rows {
				if s.rows[i].ID == x.ID {
					s.rows[i].PrevHash, s.rows[i].RowHash = slices.Clone(prev), h
				}
			}
			prev = h
			continue
		}
		prev = x.RowHash
	}
}
