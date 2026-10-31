package feed

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// ContentStats implements statsStore.
//
// cross-org: the platform panel's counts of every post and report.
func (s gormStore) ContentStats(ctx context.Context, from, to time.Time) (ContentStats, error) {
	conn := db.Conn(ctx, s.orm)
	var posts []struct {
		Status string
		N      int
		Fresh  int
	}
	if err := conn.Raw(`SELECT status, count(*) AS n,
	        count(*) FILTER (WHERE published_at >= ? AND published_at < ?) AS fresh
	   FROM posts GROUP BY status`, from, to).Scan(&posts).Error; err != nil {
		return ContentStats{}, fmt.Errorf("feed: post stats: %w", err)
	}
	var reports []struct {
		Status string
		N      int
		Fresh  int
	}
	if err := conn.Raw(`SELECT status, count(*) AS n,
	        count(*) FILTER (WHERE created_at >= ? AND created_at < ?) AS fresh
	   FROM content_reports GROUP BY status`, from, to).Scan(&reports).Error; err != nil {
		return ContentStats{}, fmt.Errorf("feed: report stats: %w", err)
	}
	st := newContentStats()
	for _, r := range posts {
		st.PostsByStatus[Status(r.Status)] = r.N
		st.PublishedInRange += r.Fresh
	}
	for _, r := range reports {
		st.ReportsByStatus[ReportStatus(r.Status)] = r.N
		st.ReportsInRange += r.Fresh
	}
	return st, nil
}

// OrgPostStats implements statsStore.
func (s gormStore) OrgPostStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (OrgPostStats, error) {
	var st OrgPostStats
	err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) AS published,
	        count(*) FILTER (WHERE audience_kind = 'segment') AS targeted,
	        coalesce(sum(like_count), 0) AS likes, coalesce(sum(save_count), 0) AS saves
	   FROM posts WHERE org_id = @org_id AND published_at >= @from AND published_at < @to`,
		map[string]any{"org_id": orgID, "from": from, "to": to}).Scan(&st).Error
	if err != nil {
		return OrgPostStats{}, fmt.Errorf("feed: org post stats: %w", err)
	}
	return st, nil
}
