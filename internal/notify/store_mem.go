package notify

import (
	"bytes"
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"
	"uuid"
)

// memStore is the in-memory Store. It implements the same contract as the
// GORM store (store_contract_test.go) and is for tests and wiring tests only
// (forbidigo bans NewMemStore outside _test.go files).
type memStore struct {
	mu    sync.Mutex
	rows  map[uuid.UUID]Delivery
	byKey map[string]uuid.UUID

	suppressions map[string]Suppression // key: string(index)
	prefs        map[prefKey]bool
	inApp        map[uuid.UUID]InAppNotification
	tokens       map[uuid.UUID]PushToken
}

type prefKey struct {
	user uuid.UUID
	cat  PrefCategory
	ch   Channel
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{
		rows: map[uuid.UUID]Delivery{}, byKey: map[string]uuid.UUID{},
		suppressions: map[string]Suppression{}, prefs: map[prefKey]bool{},
		inApp: map[uuid.UUID]InAppNotification{}, tokens: map[uuid.UUID]PushToken{},
	}
}

// ---------------------------------------------------------------- deliveries

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
	d.Provider, d.ProviderMessageID, d.LastError, d.SentAt, d.DeliveredAt = "", "", "", nil, nil
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

func (s *memStore) ListOfUser(_ context.Context, userID uuid.UUID, after *Position, limit int) ([]Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Delivery
	for _, d := range s.rows {
		if d.UserID != nil && *d.UserID == userID && before(d.CreatedAt, d.ID, after) {
			out = append(out, d.clone())
		}
	}
	slices.SortFunc(out, func(a, b Delivery) int { return newestFirst(a.CreatedAt, a.ID, b.CreatedAt, b.ID) })
	return head(out, limit), nil
}

func (s *memStore) MarkProviderOutcome(_ context.Context, providerMessageID string, status Status, at time.Time) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, d := range s.rows {
		if d.Channel != ChannelEmail || d.ProviderMessageID == "" || d.ProviderMessageID != providerMessageID {
			continue
		}
		if !outcomeAllowed(d.Status, status) {
			return true, false, nil
		}
		d.Status = status
		if status == StatusDelivered {
			d.DeliveredAt = &at
		}
		d.UpdatedAt = at
		s.rows[id] = d
		return true, true, nil
	}
	return false, false, nil
}

// outcomeAllowed is the webhook transition rule of MarkProviderOutcome.
func outcomeAllowed(from, to Status) bool {
	switch to {
	case StatusDelivered:
		return from == StatusSent
	case StatusBounced, StatusComplained:
		return from == StatusSent || from == StatusDelivered
	}
	return false
}

func (s *memStore) DeleteDeliveriesOf(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, d := range s.rows {
		if d.UserID != nil && *d.UserID == userID {
			delete(s.rows, id)
			delete(s.byKey, d.DedupeKey)
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- suppressions

func (s *memStore) Suppress(_ context.Context, sup Suppression) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := string(sup.EmailIndex)
	if _, ok := s.suppressions[k]; ok {
		return false, nil
	}
	sup.EmailIndex = slices.Clone(sup.EmailIndex)
	s.suppressions[k] = sup
	return true, nil
}

func (s *memStore) IsSuppressed(_ context.Context, idx []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.suppressions[string(idx)]
	return ok, nil
}

func (s *memStore) GetSuppression(_ context.Context, idx []byte) (Suppression, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sup, ok := s.suppressions[string(idx)]
	if !ok {
		return Suppression{}, ErrSuppressionNotFound
	}
	sup.EmailIndex = slices.Clone(sup.EmailIndex)
	return sup, nil
}

func (s *memStore) ListSuppressions(_ context.Context, after *SuppressionPosition, limit int) ([]Suppression, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Suppression
	for _, sup := range s.suppressions {
		if after != nil {
			c := cmp.Compare(sup.CreatedAt.UnixNano(), after.At.UnixNano())
			if c > 0 || (c == 0 && bytes.Compare(sup.EmailIndex, after.Index) >= 0) {
				continue
			}
		}
		sup.EmailIndex = slices.Clone(sup.EmailIndex)
		out = append(out, sup)
	}
	slices.SortFunc(out, func(a, b Suppression) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return bytes.Compare(b.EmailIndex, a.EmailIndex)
	})
	return head(out, limit), nil
}

func (s *memStore) DeleteSuppression(_ context.Context, idx []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.suppressions[string(idx)]; !ok {
		return ErrSuppressionNotFound
	}
	delete(s.suppressions, string(idx))
	return nil
}

// ---------------------------------------------------------------- preferences

func (s *memStore) Preferences(_ context.Context, userID uuid.UUID) ([]Preference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Preference
	for _, k := range slices.SortedFunc(maps.Keys(s.prefs), func(a, b prefKey) int {
		return cmp.Or(cmp.Compare(a.cat, b.cat), cmp.Compare(a.ch, b.ch))
	}) {
		if k.user == userID {
			out = append(out, Preference{Category: k.cat, Channel: k.ch, Enabled: s.prefs[k]})
		}
	}
	return out, nil
}

func (s *memStore) Preference(_ context.Context, userID uuid.UUID, c PrefCategory, ch Channel) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.prefs[prefKey{userID, c, ch}]
	return v, ok, nil
}

func (s *memStore) UpsertPreferences(_ context.Context, userID uuid.UUID, prefs []Preference, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range prefs { // the table CHECK, all or nothing
		if p.Category == PrefSecurity && !p.Enabled {
			return ErrSecurityLocked
		}
	}
	for _, p := range prefs {
		s.prefs[prefKey{userID, p.Category, p.Channel}] = p.Enabled
	}
	return nil
}

func (s *memStore) DeletePreferencesOf(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.prefs {
		if k.user == userID {
			delete(s.prefs, k)
			n++
		}
	}
	return n, nil
}

// ---------------------------------------------------------------- in-app

func (s *memStore) InsertInApp(_ context.Context, n InAppNotification) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inApp[n.ID]; ok {
		return false, nil
	}
	s.inApp[n.ID] = n.clone()
	return true, nil
}

func (s *memStore) ListInApp(_ context.Context, userID uuid.UUID, unreadOnly bool, after *Position, limit int) ([]InAppNotification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []InAppNotification
	for _, n := range s.inApp {
		if n.UserID == userID && (!unreadOnly || n.ReadAt == nil) && before(n.CreatedAt, n.ID, after) {
			out = append(out, n.clone())
		}
	}
	slices.SortFunc(out, func(a, b InAppNotification) int { return newestFirst(a.CreatedAt, a.ID, b.CreatedAt, b.ID) })
	return head(out, limit), nil
}

func (s *memStore) MarkInAppRead(_ context.Context, userID, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.inApp[id]
	if !ok || n.UserID != userID {
		return ErrNotificationNotFound
	}
	if n.ReadAt == nil {
		n.ReadAt = &at
		s.inApp[id] = n
	}
	return nil
}

func (s *memStore) MarkAllInAppRead(_ context.Context, userID uuid.UUID, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := 0
	for id, n := range s.inApp {
		if n.UserID == userID && n.ReadAt == nil {
			n.ReadAt = &at
			s.inApp[id] = n
			c++
		}
	}
	return c, nil
}

func (s *memStore) DeleteInAppOf(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := 0
	for id, n := range s.inApp {
		if n.UserID == userID {
			delete(s.inApp, id)
			c++
		}
	}
	return c, nil
}

// ---------------------------------------------------------------- push tokens

func (s *memStore) UpsertPushToken(_ context.Context, t PushToken) (PushToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, cur := range s.tokens {
		if bytes.Equal(cur.TokenSHA256, t.TokenSHA256) {
			cur.UserID, cur.SessionID = t.UserID, cloneID(t.SessionID)
			cur.Platform, cur.AppVersion, cur.Token = t.Platform, t.AppVersion, t.Token
			cur.LastSeenAt, cur.RevokedAt = t.LastSeenAt, nil
			s.tokens[id] = cur
			return cur.clone(), nil
		}
	}
	t = t.clone()
	t.RevokedAt = nil
	s.tokens[t.ID] = t
	return t.clone(), nil
}

func (s *memStore) DeletePushToken(_ context.Context, userID, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok || t.UserID != userID {
		return ErrPushTokenNotFound
	}
	delete(s.tokens, id)
	return nil
}

func (s *memStore) ActivePushTokens(_ context.Context, userID uuid.UUID) ([]PushToken, error) {
	return s.tokensOf(userID, true), nil
}

func (s *memStore) PushTokensOf(_ context.Context, userID uuid.UUID) ([]PushToken, error) {
	return s.tokensOf(userID, false), nil
}

func (s *memStore) tokensOf(userID uuid.UUID, activeOnly bool) []PushToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PushToken
	for _, t := range s.tokens {
		if t.UserID == userID && (!activeOnly || t.RevokedAt == nil) {
			out = append(out, t.clone())
		}
	}
	slices.SortFunc(out, func(a, b PushToken) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out
}

func (s *memStore) DeletePushTokensOf(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := 0
	for id, t := range s.tokens {
		if t.UserID == userID {
			delete(s.tokens, id)
			c++
		}
	}
	return c, nil
}

// ---------------------------------------------------------------- helpers

// before reports whether (at, id) lies after the keyset position p in
// (created_at DESC, id DESC) order; nil p admits everything.
func before(at time.Time, id uuid.UUID, p *Position) bool {
	if p == nil {
		return true
	}
	if c := at.Compare(p.At); c != 0 {
		return c < 0
	}
	return bytes.Compare(id[:], p.ID[:]) < 0
}

// newestFirst orders by (created_at DESC, id DESC).
func newestFirst(aAt time.Time, aID uuid.UUID, bAt time.Time, bID uuid.UUID) int {
	if c := bAt.Compare(aAt); c != 0 {
		return c
	}
	return bytes.Compare(bID[:], aID[:])
}

func head[T any](rows []T, limit int) []T {
	if limit >= 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// clone returns a deep copy, so callers cannot mutate stored rows.
func (d Delivery) clone() Delivery {
	d.UserID = cloneID(d.UserID)
	d.RecipientIndex = slices.Clone(d.RecipientIndex)
	d.SentAt = cloneTime(d.SentAt)
	d.DeliveredAt = cloneTime(d.DeliveredAt)
	return d
}

func (n InAppNotification) clone() InAppNotification {
	n.SourceOrgID = cloneID(n.SourceOrgID)
	n.Data = maps.Clone(n.Data)
	n.ReadAt = cloneTime(n.ReadAt)
	return n
}

func (t PushToken) clone() PushToken {
	t.SessionID = cloneID(t.SessionID)
	t.TokenSHA256 = slices.Clone(t.TokenSHA256)
	t.RevokedAt = cloneTime(t.RevokedAt)
	return t
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneID(id *uuid.UUID) *uuid.UUID {
	if id == nil {
		return nil
	}
	c := *id
	return &c
}
