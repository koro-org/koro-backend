package feed

import (
	"context"
	"fmt"
	"time"
	"uuid"
)

// Statistics is the post and report read of the panels (stats, BE-163; SR
// §8): counts only, no post or reporter leaves. It is a system read
// without authorisation: the caller authorises its own read
// (stats.platform.read, stats.company.read).
type Statistics interface {
	// ContentStats returns the platform's post and content report counts
	// over [from, to).
	ContentStats(ctx context.Context, from, to time.Time) (ContentStats, error)
	// OrgPostStats returns the reach of the posts orgID published in
	// [from, to).
	OrgPostStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (OrgPostStats, error)
}

// ContentStats are the content figures of the platform panel.
type ContentStats struct {
	PostsByStatus    map[Status]int       // every post by its current status
	PublishedInRange int                  // posts first published in [from, to) (published_at), whatever their status now
	ReportsByStatus  map[ReportStatus]int // every content report by status
	ReportsInRange   int                  // reports filed in [from, to)
}

// OrgPostStats are the post figures of one org's panel: the posts it
// published in the range (published_at, whatever their status now) and
// the likes and saves they hold now (the author org sees each post's
// counts already: getPostStats).
type OrgPostStats struct {
	Published int // posts published in the range
	Targeted  int // of which targeted (audience_kind segment)
	Likes     int
	Saves     int
}

// statsStore is the persistence of Statistics, part of Store.
//
// cross-org (ContentStats): platform-wide counts; OrgPostStats is keyed by
// the org of the caller's path. No row content leaves.
type statsStore interface {
	// ContentStats returns the post and report counts.
	ContentStats(ctx context.Context, from, to time.Time) (ContentStats, error)
	// OrgPostStats returns the figures of orgID's posts published in the range.
	OrgPostStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (OrgPostStats, error)
}

var _ Statistics = (*service)(nil)

// ContentStats implements Statistics.
func (s *service) ContentStats(ctx context.Context, from, to time.Time) (ContentStats, error) {
	st, err := s.stats().ContentStats(ctx, from, to)
	if err != nil {
		return ContentStats{}, fmt.Errorf("feed: content stats: %w", err)
	}
	return st, nil
}

// OrgPostStats implements Statistics.
func (s *service) OrgPostStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (OrgPostStats, error) {
	st, err := s.stats().OrgPostStats(ctx, orgID, from, to)
	if err != nil {
		return OrgPostStats{}, fmt.Errorf("feed: org post stats: %w", err)
	}
	return st, nil
}

// newContentStats returns figures with empty maps.
func newContentStats() ContentStats {
	return ContentStats{PostsByStatus: map[Status]int{}, ReportsByStatus: map[ReportStatus]int{}}
}
