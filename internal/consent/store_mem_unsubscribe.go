package consent

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"
)

// InsertUnsubscribeLink implements UnsubscribeLinkStore with the CHECK and
// unique constraints of unsubscribe_links (000162); the users FK is the
// database's.
func (s *memStore) InsertUnsubscribeLink(_ context.Context, l UnsubscribeToken) error {
	switch {
	case len(l.TokenHash) != 32, // unsubscribe_links_token_hash_chk
		l.Purpose != PurposeCommercialEmail, // unsubscribe_links_purpose_code_chk
		l.UserID == uuid.Nil(),
		!l.ExpiresAt.After(l.CreatedAt): // unsubscribe_links_expiry_chk
		return errCheck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.links, func(o UnsubscribeToken) bool {
		return o.ID == l.ID || bytes.Equal(o.TokenHash, l.TokenHash) // pkey, unsubscribe_links_token_hash_uq
	}) {
		return errUnique
	}
	l.TokenHash = bytes.Clone(l.TokenHash)
	s.links = append(s.links, l)
	return nil
}

// UnsubscribeLinkByHash implements UnsubscribeLinkStore.
func (s *memStore) UnsubscribeLinkByHash(_ context.Context, hash []byte) (UnsubscribeToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		if bytes.Equal(l.TokenHash, hash) {
			l.TokenHash = bytes.Clone(l.TokenHash)
			return l, nil
		}
	}
	return UnsubscribeToken{}, ErrNotFound
}

// DeleteUnsubscribeLinksOf implements UnsubscribeLinkStore.
func (s *memStore) DeleteUnsubscribeLinksOf(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.links)
	s.links = slices.DeleteFunc(s.links, func(l UnsubscribeToken) bool { return l.UserID == userID })
	return n - len(s.links), nil
}

// DeleteExpiredUnsubscribeLinks implements UnsubscribeLinkStore.
func (s *memStore) DeleteExpiredUnsubscribeLinks(_ context.Context, cutoff time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []UnsubscribeToken
	for _, l := range s.links {
		if l.ExpiresAt.Before(cutoff) {
			due = append(due, l)
		}
	}
	slices.SortFunc(due, func(a, b UnsubscribeToken) int {
		return cmp.Or(a.ExpiresAt.Compare(b.ExpiresAt), a.ID.Compare(b.ID))
	})
	due = due[:min(len(due), max(limit, 1))]
	s.links = slices.DeleteFunc(s.links, func(l UnsubscribeToken) bool {
		return slices.ContainsFunc(due, func(d UnsubscribeToken) bool { return d.ID == l.ID })
	})
	return len(due), nil
}
