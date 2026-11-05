package notify

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"uuid"
)

// Web push subscriptions of the in-memory store (same contract as the GORM
// store, store_contract_test.go).

func (s *memStore) UpsertWebPushSubscription(_ context.Context, w WebPushSubscription, keep int) (WebPushSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := w.clone()
	for _, cur := range s.webSubs {
		if bytes.Equal(cur.EndpointSHA256, w.EndpointSHA256) {
			cur.UserID, cur.SessionID, cur.Endpoint = w.UserID, cloneID(w.SessionID), w.Endpoint
			cur.P256DH, cur.Auth, cur.LastSeenAt = slices.Clone(w.P256DH), slices.Clone(w.Auth), w.LastSeenAt
			stored = cur
			break
		}
	}
	s.webSubs[stored.ID] = stored
	// The cap: keep the stored row and the keep-1 most recently seen others.
	var others []WebPushSubscription
	for _, cur := range s.webSubs {
		if cur.UserID == stored.UserID && cur.ID != stored.ID {
			others = append(others, cur)
		}
	}
	slices.SortFunc(others, func(a, b WebPushSubscription) int {
		return cmp.Or(b.LastSeenAt.Compare(a.LastSeenAt), bytes.Compare(b.ID[:], a.ID[:]))
	})
	for i, cur := range others {
		if i >= max(keep-1, 0) {
			delete(s.webSubs, cur.ID)
		}
	}
	return stored.clone(), nil
}

func (s *memStore) DeleteWebPushSubscription(_ context.Context, userID, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.webSubs[id]
	if !ok || w.UserID != userID {
		return ErrWebPushSubscriptionNotFound
	}
	delete(s.webSubs, id)
	return nil
}

func (s *memStore) WebPushSubscriptionsOf(_ context.Context, userID uuid.UUID) ([]WebPushSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []WebPushSubscription
	for _, w := range s.webSubs {
		if w.UserID == userID {
			out = append(out, w.clone())
		}
	}
	slices.SortFunc(out, func(a, b WebPushSubscription) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	return out, nil
}

func (s *memStore) DropWebPushSubscription(_ context.Context, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.webSubs[id]; !ok {
		return false, nil
	}
	delete(s.webSubs, id)
	return true, nil
}

func (s *memStore) DeleteWebPushSubscriptionsOf(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := 0
	for id, w := range s.webSubs {
		if w.UserID == userID {
			delete(s.webSubs, id)
			c++
		}
	}
	return c, nil
}

func (w WebPushSubscription) clone() WebPushSubscription {
	w.SessionID = cloneID(w.SessionID)
	w.EndpointSHA256 = slices.Clone(w.EndpointSHA256)
	w.P256DH = slices.Clone(w.P256DH)
	w.Auth = slices.Clone(w.Auth)
	return w
}
