package outreach

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

// OutreachStats implements statsStore. A key group is one array overlap
// (&&) on audience_keys; the rows are the org's own (org_id first).
func (s gormStore) OutreachStats(ctx context.Context, orgID uuid.UUID, from, to time.Time, limit int, keyGroups [][]string) (StatsCounts, error) {
	conn := db.Conn(ctx, s.orm)
	c := StatsCounts{ByStatus: map[DispatchStatus]int{}}
	audience := func(tx *gorm.DB) *gorm.DB {
		for _, g := range keyGroups {
			tx = tx.Where("audience_keys && ?::text[]", db.TextArray(g))
		}
		return tx
	}
	if err := conn.Scopes(db.ScopeOrg(orgID), audience).Where("archived_at IS NULL").Order("last_count DESC NULLS LAST, id").
		Limit(limit).Find(&c.Segments).Error; err != nil {
		return StatsCounts{}, fmt.Errorf("outreach: segment stats: %w", err)
	}
	var active int64
	if err := conn.Model(&Segment{}).Scopes(db.ScopeOrg(orgID), audience).Where("archived_at IS NULL").Count(&active).Error; err != nil {
		return StatsCounts{}, fmt.Errorf("outreach: active segments: %w", err)
	}
	c.Active = int(active)
	var rows []struct {
		Status   string
		N        int
		Eligible int
		Sent     int
	}
	if err := conn.Table("outreach_dispatches").Scopes(db.ScopeOrg(orgID), audience).
		Select("status, count(*) AS n, coalesce(sum(eligible_count), 0) AS eligible, coalesce(sum(sent_count), 0) AS sent").
		Where("created_at >= ? AND created_at < ?", from, to).Group("status").Scan(&rows).Error; err != nil {
		return StatsCounts{}, fmt.Errorf("outreach: dispatch stats: %w", err)
	}
	for _, r := range rows {
		c.ByStatus[DispatchStatus(r.Status)] = r.N
		c.Eligible += r.Eligible
		c.Sent += r.Sent
	}
	return c, nil
}
