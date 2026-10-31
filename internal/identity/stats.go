package identity

import (
	"context"
	"fmt"
	"time"
)

// UserStatistics is the account read of the platform panel (stats,
// BE-163; SR §8): counts only, no account leaves. It is a system read
// without authorisation: the caller (stats.platform.read, platform_admin)
// authorises its own read.
type UserStatistics interface {
	// UserStats returns the account figures; NewInRange counts the
	// accounts created in [from, to).
	UserStats(ctx context.Context, from, to time.Time) (UserStats, error)
}

// UserStats are the account figures of the platform panel.
type UserStats struct {
	// ByStatus counts every account by status, tombstones (erased)
	// included: the account existed.
	ByStatus map[Status]int
	// ByPersona counts the accounts that are not erased by sign-up
	// persona (users.signup_persona: the role chosen at sign-up).
	ByPersona map[Persona]int
	// EmailVerified counts the accounts that are not erased and verified
	// their e-mail address.
	EmailVerified int
	// NewInRange counts the accounts created in [from, to), tombstones
	// included.
	NewInRange int
	// VerifiedStudents counts the users with a student verification in
	// effect at the time of the read (03 §2.1: verified, not expired).
	VerifiedStudents int
}

// StatsStore is the persistence of UserStatistics, part of Store.
//
// cross-org: platform-wide counts; no row content leaves.
type StatsStore interface {
	// UserStats returns the figures over every account; the student
	// verifications count at now.
	UserStats(ctx context.Context, from, to, now time.Time) (UserStats, error)
}

var _ UserStatistics = (*service)(nil)

// UserStats implements UserStatistics.
func (s *service) UserStats(ctx context.Context, from, to time.Time) (UserStats, error) {
	st, err := s.stats().UserStats(ctx, from, to, s.d.Clock.Now())
	if err != nil {
		return UserStats{}, fmt.Errorf("identity: user stats: %w", err)
	}
	return st, nil
}

// newUserStats returns figures with empty maps.
func newUserStats() UserStats {
	return UserStats{ByStatus: map[Status]int{}, ByPersona: map[Persona]int{}}
}
