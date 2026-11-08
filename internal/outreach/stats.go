package outreach

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"
	"uuid"

	"koro/internal/directory"
)

// Statistics is the targeting read of an org's panel (stats, BE-163; SR
// §8 "hedef kitle büyüklükleri, duyuru erişimi"): audience sizes and
// dispatch reach as published estimates only (06 H-61): the exact counts
// stay inside outreach. It is a system read without authorisation: the
// caller authorises its own read (stats.company.read).
type Statistics interface {
	// OrgOutreachStats returns orgID's active segments with their stored
	// estimates (no new count: the daily budget is not spent) and its
	// dispatches created in [from, to), both narrowed by f.
	OrgOutreachStats(ctx context.Context, orgID uuid.UUID, from, to time.Time, f StatsFilter) (OutreachStats, error)
}

// StatsFilter narrows OrgOutreachStats to the segments and dispatches
// whose audience names the given values (BE-190: the audience dimensions
// of the company panel, SR §8 "üniversite, bölüm, sınıf"; BE-207: "kulüp
// kategorisi"). A zero field does not filter; the present ones all apply.
// It selects among the org's own rows and their stored, already published
// figures: no student is counted again (no daily budget, no new number
// about people). A stored segment estimate already carries its offset
// (BE-203) and passes through PublicEstimate unchanged: it is never offset
// twice.
type StatsFilter struct {
	// Uni keeps the audiences that name the university: its uni: key or
	// the dept: key of one of UniDepts (the caller resolves the
	// university's departments: a department implies its university).
	Uni      *uuid.UUID
	UniDepts []uuid.UUID
	Dept     *uuid.UUID
	Year     *int16
	// ClubCategory keeps the audiences that name the club category (a
	// targetable one: the caller validates it).
	ClubCategory *directory.Category
}

// keyGroups returns, per present dimension of f, the audience keys an
// audience must hold at least one of; nil without a filter.
func (f StatsFilter) keyGroups() [][]string {
	var out [][]string
	if f.Uni != nil {
		g := []string{keyUni + f.Uni.String()}
		for _, d := range f.UniDepts {
			g = append(g, keyDept+d.String())
		}
		out = append(out, g)
	}
	if f.Dept != nil {
		out = append(out, []string{keyDept + f.Dept.String()})
	}
	if f.Year != nil {
		out = append(out, []string{keyYear + strconv.Itoa(int(*f.Year))})
	}
	if f.ClubCategory != nil {
		out = append(out, []string{keyClubCategory + string(*f.ClubCategory)})
	}
	return out
}

// holdsGroups reports whether keys holds a key of every group.
func holdsGroups(keys []string, groups [][]string) bool {
	for _, g := range groups {
		if !slices.ContainsFunc(g, func(k string) bool { return slices.Contains(keys, k) }) {
			return false
		}
	}
	return true
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
	// Only the segments and dispatches whose audience keys hold a key of
	// every group of keyGroups count (none: every row).
	OutreachStats(ctx context.Context, orgID uuid.UUID, from, to time.Time, limit int, keyGroups [][]string) (StatsCounts, error)
}

var _ Statistics = (*service)(nil)

// OrgOutreachStats implements Statistics.
func (s *service) OrgOutreachStats(ctx context.Context, orgID uuid.UUID, from, to time.Time, f StatsFilter) (OutreachStats, error) {
	c, err := s.d.Store.OutreachStats(ctx, orgID, from, to, MaxStatsSegments, f.keyGroups())
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
