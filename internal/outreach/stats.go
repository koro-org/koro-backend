package outreach

import (
	"context"
	"fmt"
	"time"
	"uuid"
)

// Statistics is the targeting read of an org's panel (stats, BE-163; SR
// §8 "hedef kitle büyüklükleri, duyuru erişimi"): audience sizes and
// dispatch reach as published estimates only (06 H-61): the exact counts
// stay inside outreach. It is a system read without authorisation: the
// caller authorises its own read (stats.company.read).
type Statistics interface {
	// OrgOutreachStats returns orgID's active segments with their stored
	// estimates (no new count: the daily budget is not spent) and its
	// dispatches created in [from, to).
	OrgOutreachStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (OutreachStats, error)
}

// MaxStatsSegments bounds the segments of OrgOutreachStats (largest
// stored estimate first).
const MaxStatsSegments = 20

// SegmentEstimate is one active segment with its stored estimate (the
// nightly recount or the last count; below 10 → BelowThreshold).
type SegmentEstimate struct {
	ID        uuid.UUID
	Name      string
	Estimate  Estimate
	CountedAt *time.Time
}

// OutreachStats are an org's targeting figures. Eligible and Sent sum the
// published counts stored on the dispatches created in the range and are
// published again as one estimate each (PublicEstimate of the sum: null
// below 10, rounded down).
type OutreachStats struct {
	ActiveSegments int
	Segments       []SegmentEstimate // at most MaxStatsSegments
	Dispatches     int
	ByStatus       map[DispatchStatus]int
	Eligible       Estimate
	Sent           Estimate
}

// StatsCounts are the stored figures behind OutreachStats: the segments
// with their stored last_count and the sums of the dispatches' stored
// (already published) counts. Statistics publishes them as estimates only.
type StatsCounts struct {
	Segments []Segment // active, largest last_count first (NULL last), then id
	Active   int
	ByStatus map[DispatchStatus]int
	Eligible int
	Sent     int
}

// statsStore is the persistence of Statistics, part of Store, keyed by the
// org of the caller's path.
type statsStore interface {
	// OutreachStats returns orgID's figures: up to limit active segments.
	OutreachStats(ctx context.Context, orgID uuid.UUID, from, to time.Time, limit int) (StatsCounts, error)
}

var _ Statistics = (*service)(nil)

// OrgOutreachStats implements Statistics.
func (s *service) OrgOutreachStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (OutreachStats, error) {
	c, err := s.d.Store.OutreachStats(ctx, orgID, from, to, MaxStatsSegments)
	if err != nil {
		return OutreachStats{}, fmt.Errorf("outreach: stats: %w", err)
	}
	st := OutreachStats{ActiveSegments: c.Active, Segments: make([]SegmentEstimate, 0, len(c.Segments)), ByStatus: c.ByStatus,
		Eligible: PublicEstimate(c.Eligible), Sent: PublicEstimate(c.Sent)}
	for _, seg := range c.Segments {
		e := Estimate{BelowThreshold: true}
		if seg.LastCount != nil {
			e = PublicEstimate(*seg.LastCount)
		}
		st.Segments = append(st.Segments, SegmentEstimate{ID: seg.ID, Name: seg.Name, Estimate: e, CountedAt: seg.LastCountedAt})
	}
	for _, n := range c.ByStatus {
		st.Dispatches += n
	}
	return st, nil
}
