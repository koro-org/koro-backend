package messaging

import (
	"context"
	"fmt"
	"time"
	"uuid"
)

// Statistics is the contact request read of a company's panel (stats,
// BE-163; SR §8 "iletişim talepleri ve kabul oranı"): counts only, no
// student leaves (the company sees each request's status in its list
// already). It is a system read without authorisation: the caller
// authorises its own read (stats.company.read).
type Statistics interface {
	// ContactRequestStats returns the requests orgID sent in [from, to)
	// by effective status (a pending request past its expiry is expired).
	ContactRequestStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (ContactRequestStats, error)
}

// ContactRequestStats are a company's requests of a range.
type ContactRequestStats struct {
	Sent     int
	ByStatus map[Status]int
}

// StatsStore is the persistence of Statistics, part of Store, keyed by the
// company of the caller's path (contact_requests_org_idx).
type StatsStore interface {
	// ContactRequestStats returns orgID's requests created in [from, to)
	// by status, a pending one past its expiry at now counted as expired.
	ContactRequestStats(ctx context.Context, orgID uuid.UUID, from, to, now time.Time) (ContactRequestStats, error)
}

var _ Statistics = (*service)(nil)

// ContactRequestStats implements Statistics.
func (s *service) ContactRequestStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (ContactRequestStats, error) {
	st, err := s.stats().ContactRequestStats(ctx, orgID, from, to, s.now())
	if err != nil {
		return ContactRequestStats{}, fmt.Errorf("messaging: contact request stats: %w", err)
	}
	return st, nil
}
