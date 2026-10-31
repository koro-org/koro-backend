package orgs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"
)

// Statistics is the org read of the panels (stats, BE-163; SR §8): counts
// only, no member leaves. It is a system read without authorisation: the
// caller authorises its own read (stats.platform.read, stats.club.read).
type Statistics interface {
	// OrgStats returns the platform's org counts; NewInRange counts the
	// orgs created in [from, to).
	OrgStats(ctx context.Context, from, to time.Time) (OrgStats, error)
	// MemberStats returns the membership figures of one org (a club's
	// members and their growth) over [from, to). An unknown org has none.
	MemberStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (MemberStats, error)
}

// OrgCount is the number of orgs of one kind in one status.
type OrgCount struct {
	Kind   Kind
	Status Status
	Count  int
}

// OrgStats are the org figures of the platform panel.
type OrgStats struct {
	Counts     []OrgCount   // every (kind, status) pair with at least one org, by kind then status
	NewInRange map[Kind]int // orgs created in [from, to) by kind
}

// MemberMonth is the membership change of one calendar month
// (Europe/Istanbul).
type MemberMonth struct {
	Month  string // "2006-01"
	Joined int
	Left   int
}

// MemberStats are the membership figures of one org. A role change ends
// one row and starts another for the same person at the same instant
// (role_changed): neither side counts as a join or a departure.
type MemberStats struct {
	Active  int // active memberships now (every role of the org)
	Pending int // join requests awaiting a decision now
	// JoinedInRange and LeftInRange count the memberships that started
	// (joined_at) and ended (ended_at, after having started) in
	// [from, to); Monthly splits them by month, oldest first, months
	// without a change omitted.
	JoinedInRange int
	LeftInRange   int
	Monthly       []MemberMonth
}

// statsStore is the persistence of Statistics, part of Store.
//
// cross-org (OrgStats): platform-wide counts; MemberStats is keyed by the
// org of the caller's path. No row content leaves.
type statsStore interface {
	// OrgStats returns the org counts.
	OrgStats(ctx context.Context, from, to time.Time) (OrgStats, error)
	// MemberStats returns the membership figures of orgID.
	MemberStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (MemberStats, error)
}

var _ Statistics = (*service)(nil)

// OrgStats implements Statistics.
func (s *service) OrgStats(ctx context.Context, from, to time.Time) (OrgStats, error) {
	st, err := s.stats().OrgStats(ctx, from, to)
	if err != nil {
		return OrgStats{}, fmt.Errorf("orgs: org stats: %w", err)
	}
	return st, nil
}

// MemberStats implements Statistics.
func (s *service) MemberStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (MemberStats, error) {
	st, err := s.stats().MemberStats(ctx, orgID, from, to)
	if err != nil {
		return MemberStats{}, fmt.Errorf("orgs: member stats: %w", err)
	}
	return st, nil
}

// monthKey is the "2006-01" key of t's month in Europe/Istanbul.
func monthKey(t time.Time, loc *time.Location) string { return t.In(loc).Format("2006-01") }

// addMonth returns the month entry of key, created on first use.
func addMonth(months map[string]*MemberMonth, key string) *MemberMonth {
	m, ok := months[key]
	if !ok {
		m = &MemberMonth{Month: key}
		months[key] = m
	}
	return m
}

// sortedMonths returns the months oldest first.
func sortedMonths(months map[string]*MemberMonth) []MemberMonth {
	out := make([]MemberMonth, 0, len(months))
	for _, m := range months {
		out = append(out, *m)
	}
	slices.SortFunc(out, func(a, b MemberMonth) int { return strings.Compare(a.Month, b.Month) })
	return out
}
