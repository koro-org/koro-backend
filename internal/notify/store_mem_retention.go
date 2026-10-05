package notify

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
	"uuid"
)

// expired is one row whose retention ended: its anchor and a delete.
type expired struct {
	at  time.Time
	key []byte
	del func()
}

// PurgeExpired implements Store (same contract as the GORM store).
func (s *memStore) PurgeExpired(_ context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []expired
	add := func(at time.Time, key []byte, del func()) {
		if at.Before(cutoff) {
			rows = append(rows, expired{at: at, key: key, del: del})
		}
	}
	idKey := func(id uuid.UUID) []byte { return id[:] }
	switch policy {
	case PolicyNotificationDelivery:
		for id, d := range s.rows {
			add(d.CreatedAt, idKey(id), func() { delete(s.rows, id); delete(s.byKey, d.DedupeKey) })
		}
	case PolicyEmailSuppression:
		for k, sup := range s.suppressions {
			add(sup.CreatedAt, []byte(k), func() { delete(s.suppressions, k) })
		}
	case PolicyPushToken:
		for id, t := range s.tokens {
			add(t.LastSeenAt, idKey(id), func() { delete(s.tokens, id) })
		}
	case PolicyInAppNotification:
		for id, n := range s.inApp {
			add(n.CreatedAt, idKey(id), func() { delete(s.inApp, id) })
		}
	default:
		return 0, fmt.Errorf("notify: no retention purge for policy %q", policy)
	}
	if dryRun {
		return len(rows), nil
	}
	slices.SortFunc(rows, func(a, b expired) int { return cmp.Or(a.at.Compare(b.at), bytes.Compare(a.key, b.key)) })
	rows = rows[:min(len(rows), max(limit, 0))]
	for _, r := range rows {
		r.del()
	}
	return len(rows), nil
}
