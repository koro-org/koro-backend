package notify

import (
	"context"
	"slices"
	"sync"
	"time"
	"uuid"
)

// memStore is the in-memory Store. It implements the same contract as the
// GORM store of BE-015 (store_contract_test.go) and is for tests and
// wiring tests only (forbidigo bans NewMemStore outside _test.go files).
type memStore struct {
	mu    sync.Mutex
	rows  map[uuid.UUID]Delivery
	byKey map[string]uuid.UUID
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{rows: map[uuid.UUID]Delivery{}, byKey: map[string]uuid.UUID{}}
}

func (s *memStore) Claim(_ context.Context, d Delivery, now time.Time, lease time.Duration) (Delivery, ClaimState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.byKey[d.DedupeKey]; ok {
		cur := s.rows[id]
		switch {
		case cur.Status == StatusFailed || (cur.Status == StatusQueued && now.Sub(cur.UpdatedAt) >= lease):
			cur.Status = StatusQueued
			cur.Attempts++
			cur.TemplateVersion = d.TemplateVersion
			cur.UpdatedAt = now
			s.rows[id] = cur
			return cur.clone(), ClaimAcquired, nil
		case cur.Status == StatusQueued:
			return cur.clone(), ClaimInFlight, nil
		default:
			return cur.clone(), ClaimDone, nil
		}
	}

	d.Status = StatusQueued
	d.Attempts = 1
	d.CreatedAt, d.UpdatedAt = now, now
	d = d.clone()
	s.rows[d.ID] = d
	s.byKey[d.DedupeKey] = d.ID
	return d.clone(), ClaimAcquired, nil
}

func (s *memStore) Finish(_ context.Context, id uuid.UUID, r Result, now time.Time) (Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, ok := s.rows[id]
	if !ok {
		return Delivery{}, ErrNotFound
	}
	if cur.Status != StatusQueued {
		return cur.clone(), nil
	}
	cur.Status = r.Status
	cur.Provider = r.Provider
	cur.ProviderMessageID = r.ProviderMessageID
	cur.LastError = r.LastError
	cur.SentAt = cloneTime(r.SentAt)
	cur.UpdatedAt = now
	s.rows[id] = cur
	return cur.clone(), nil
}

func (s *memStore) Get(_ context.Context, id uuid.UUID) (Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.rows[id]
	if !ok {
		return Delivery{}, ErrNotFound
	}
	return cur.clone(), nil
}

// clone returns a deep copy, so callers cannot mutate stored rows.
func (d Delivery) clone() Delivery {
	if d.UserID != nil {
		u := *d.UserID
		d.UserID = &u
	}
	d.RecipientIndex = slices.Clone(d.RecipientIndex)
	d.SentAt = cloneTime(d.SentAt)
	return d
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
