package audit

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// Programmer errors of SetChain (the GORM store gets them from the
// database: unique chain_seq, audit_events_guard).
var (
	errChainSeqTaken  = errors.New("audit: chain_seq already used")
	errAlreadyChained = errors.New("audit: row is already chained")
	errNoSuchRow      = errors.New("audit: no such unchained row")
)

// securityPrefixes are the action prefixes of listMySecurityEvents:
// identity (sign-in, password, sessions, MFA) and consent (04 §2.21).
var securityPrefixes = []string{"identity.", "consent."}

// MemStore is the in-memory Store of service and wiring tests (05 §2.3).
// It follows the GORM store's contract: Seq is assigned on insert, a
// chained row never changes, a root day is unique. It has no transactions
// (db.NopTx); the chain lock is its mutex.
type MemStore struct {
	mu    sync.Mutex
	seq   int64
	rows  []Record // seq order
	roots []DailyRoot
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore { return &MemStore{} }

func cloneRecord(r Record) Record {
	r.Metadata = slices.Clone(r.Metadata)
	r.PrevHash, r.RowHash = slices.Clone(r.PrevHash), slices.Clone(r.RowHash)
	if r.ChainSeq != nil {
		v := *r.ChainSeq
		r.ChainSeq = &v
	}
	if r.ChainedAt != nil {
		v := *r.ChainedAt
		r.ChainedAt = &v
	}
	return r
}

// Insert implements Store.
func (s *MemStore) Insert(_ context.Context, r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r = cloneRecord(r)
	r.Seq = s.seq
	r.OccurredAt = r.OccurredAt.UTC().Truncate(time.Microsecond)
	if len(r.Metadata) == 0 {
		r.Metadata = json.RawMessage(`{}`)
	}
	r.ChainSeq, r.PrevHash, r.RowHash, r.ChainedAt = nil, nil, nil, nil
	s.rows = append(s.rows, r)
	return nil
}

// BeginChain implements Store.
func (s *MemStore) BeginChain(context.Context) (Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	head := Link{Hash: zeroHash}
	for _, r := range s.rows {
		if r.ChainSeq != nil && *r.ChainSeq > head.Seq {
			head = Link{Seq: *r.ChainSeq, Hash: slices.Clone(r.RowHash)}
		}
	}
	if n := len(s.roots); n > 0 && s.roots[n-1].LastChainSeq > head.Seq {
		last := s.roots[n-1]
		head = Link{Seq: last.LastChainSeq, Hash: slices.Clone(last.RootHash)}
	}
	return head, nil
}

// Unchained implements Store.
func (s *MemStore) Unchained(_ context.Context, limit int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.rows {
		if r.ChainSeq == nil && len(out) < limit {
			out = append(out, cloneRecord(r))
		}
	}
	return out, nil
}

// SetChain implements Store.
func (s *MemStore) SetChain(_ context.Context, id uuid.UUID, seq int64, prev, hash []byte, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.ChainSeq != nil && *r.ChainSeq == seq {
			return ErrInvalidEvent.Wrap(errChainSeqTaken)
		}
	}
	for i := range s.rows {
		if s.rows[i].ID != id {
			continue
		}
		if s.rows[i].ChainSeq != nil {
			return ErrInvalidEvent.Wrap(errAlreadyChained)
		}
		at = at.UTC().Truncate(time.Microsecond)
		s.rows[i].ChainSeq, s.rows[i].PrevHash, s.rows[i].RowHash, s.rows[i].ChainedAt = &seq, slices.Clone(prev), slices.Clone(hash), &at
		return nil
	}
	return ErrInvalidEvent.Wrap(errNoSuchRow)
}

func (s *MemStore) chained() []Record {
	var out []Record
	for _, r := range s.rows {
		if r.ChainSeq != nil {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Record) int { return cmp.Compare(*a.ChainSeq, *b.ChainSeq) })
	return out
}

// Chained implements Store.
func (s *MemStore) Chained(_ context.Context, after int64, limit int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for _, r := range s.chained() {
		if *r.ChainSeq > after && len(out) < limit {
			out = append(out, cloneRecord(r))
		}
	}
	return out, nil
}

// HeadAt implements Store.
func (s *MemStore) HeadAt(_ context.Context, before time.Time) (Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	head := Link{Hash: zeroHash}
	for _, r := range s.chained() {
		if r.ChainedAt.Before(before) {
			head = Link{Seq: *r.ChainSeq, Hash: slices.Clone(r.RowHash)}
		}
	}
	return head, nil
}

// OldestUnchained implements Store.
func (s *MemStore) OldestUnchained(context.Context) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var oldest time.Time
	found := false
	for _, r := range s.rows {
		if r.ChainSeq == nil && (!found || r.OccurredAt.Before(oldest)) {
			oldest, found = r.OccurredAt, true
		}
	}
	return oldest, found, nil
}

// InsertRoot implements Store.
func (s *MemStore) InsertRoot(_ context.Context, r DailyRoot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.roots {
		if x.Day == r.Day {
			return ErrRootExists
		}
	}
	r.RootHash, r.Signature = slices.Clone(r.RootHash), slices.Clone(r.Signature)
	s.roots = append(s.roots, r)
	slices.SortFunc(s.roots, func(a, b DailyRoot) int { return a.Day.Compare(b.Day) })
	return nil
}

// LastRoot implements Store.
func (s *MemStore) LastRoot(context.Context) (DailyRoot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.roots) == 0 {
		return DailyRoot{}, false, nil
	}
	return s.roots[len(s.roots)-1], true, nil
}

// RootBefore implements Store.
func (s *MemStore) RootBefore(_ context.Context, day civil.Date) (DailyRoot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.roots) - 1; i >= 0; i-- {
		if s.roots[i].Day.Before(day) {
			return s.roots[i], true, nil
		}
	}
	return DailyRoot{}, false, nil
}

// Roots implements Store.
func (s *MemStore) Roots(_ context.Context, from, to civil.Date) ([]DailyRoot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DailyRoot
	for _, r := range s.roots {
		if !r.Day.Before(from) && !r.Day.After(to) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ListOwn implements Store.
func (s *MemStore) ListOwn(_ context.Context, user uuid.UUID, after *After, limit int) ([]Record, error) {
	return s.list(after, limit, func(r Record) bool {
		if r.ActorUserID != nil && *r.ActorUserID == user && slices.ContainsFunc(securityPrefixes, func(p string) bool {
			return strings.HasPrefix(r.Action, p)
		}) {
			return true
		}
		m := metaOf(r)
		bg, _ := m[MetaBreakGlass].(bool)
		sub, _ := m[MetaSubjectUserID].(string)
		return bg && sub == user.String()
	}), nil
}

// ListOrg implements Store.
func (s *MemStore) ListOrg(_ context.Context, orgID uuid.UUID, f Filter, after *After, limit int) ([]Record, error) {
	f.OrgID = &orgID
	return s.list(after, limit, f.match), nil
}

// List implements Store.
func (s *MemStore) List(_ context.Context, f Filter, after *After, limit int) ([]Record, error) {
	return s.list(after, limit, f.match), nil
}

func (s *MemStore) list(after *After, limit int, keep func(Record) bool) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := slices.Clone(s.rows)
	slices.SortFunc(rows, func(a, b Record) int {
		if c := b.OccurredAt.Compare(a.OccurredAt); c != 0 {
			return c
		}
		return bytes.Compare(b.ID[:], a.ID[:])
	})
	var out []Record
	for _, r := range rows {
		if after != nil {
			if c := r.OccurredAt.Compare(after.At); c > 0 || (c == 0 && bytes.Compare(r.ID[:], after.ID[:]) >= 0) {
				continue
			}
		}
		if keep(r) {
			out = append(out, cloneRecord(r))
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

func metaOf(r Record) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Metadata, &m)
	return m
}

// match is the in-memory form of the GORM filter.
func (f Filter) match(r Record) bool {
	eq := func(want, got *uuid.UUID) bool { return want == nil || (got != nil && *got == *want) }
	m := metaOf(r)
	bg, _ := m[MetaBreakGlass].(bool)
	sensitive, _ := m[MetaSensitive].(bool)
	switch {
	case !f.From.IsZero() && r.OccurredAt.Before(f.From),
		!f.To.IsZero() && !r.OccurredAt.Before(f.To),
		!eq(f.ActorUserID, r.ActorUserID), !eq(f.OrgID, r.OrgID), !eq(f.ResourceID, r.ResourceID),
		f.ResourceType != "" && r.ResourceType != f.ResourceType,
		f.Action != "" && r.Action != f.Action,
		f.Outcome != "" && r.Outcome != f.Outcome,
		f.BreakGlass != nil && bg != *f.BreakGlass,
		f.HideSensitive && sensitive:
		return false
	}
	return true
}

// covered reports whether a chained row is covered by root r.
func covered(rec Record, r DailyRoot) bool {
	return rec.ChainSeq != nil && *rec.ChainSeq <= r.LastChainSeq
}

// PurgeDays implements Store.
func (s *MemStore) PurgeDays(_ context.Context, before civil.Date, limit int) ([]civil.Date, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []civil.Date
	for _, r := range s.roots { // day order
		if len(out) < limit && r.Day.Before(before) && slices.ContainsFunc(s.rows, func(rec Record) bool { return covered(rec, r) }) {
			out = append(out, r.Day)
		}
	}
	return out, nil
}

// CountPurgeable implements Store.
func (s *MemStore) CountPurgeable(_ context.Context, before civil.Date) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last *DailyRoot
	for i := range s.roots {
		if s.roots[i].Day.Before(before) {
			last = &s.roots[i]
		}
	}
	n := 0
	for _, rec := range s.rows {
		if last != nil && covered(rec, *last) {
			n++
		}
	}
	return n, nil
}

// PurgeUntil implements Store (the retention window check of
// audit_purge_until is the sweeper's cut-off here).
func (s *MemStore) PurgeUntil(_ context.Context, day civil.Date) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.roots, func(r DailyRoot) bool { return r.Day == day })
	if i < 0 {
		return 0, fmt.Errorf("audit: purge until %s: no daily root", day)
	}
	before := len(s.rows)
	s.rows = slices.DeleteFunc(s.rows, func(rec Record) bool { return covered(rec, s.roots[i]) })
	return before - len(s.rows), nil
}

// PurgeRoots implements Store (the approval check of the database function
// is kvkk's dry-run rule here).
func (s *MemStore) PurgeRoots(_ context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	purgedAfter := func(i int) bool {
		for _, later := range s.roots[i+1:] {
			if !slices.ContainsFunc(s.rows, func(rec Record) bool { return covered(rec, later) }) {
				return true
			}
		}
		return false
	}
	var doomed []civil.Date
	for i, r := range s.roots {
		if r.CreatedAt.Before(cutoff) && purgedAfter(i) {
			doomed = append(doomed, r.Day)
		}
	}
	if dryRun {
		return len(doomed), nil
	}
	doomed = doomed[:min(len(doomed), max(limit, 0))]
	s.roots = slices.DeleteFunc(s.roots, func(r DailyRoot) bool { return slices.Contains(doomed, r.Day) })
	return len(doomed), nil
}
