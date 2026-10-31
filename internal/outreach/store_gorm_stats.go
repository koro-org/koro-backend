package outreach

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// OutreachStats implements statsStore.
func (s gormStore) OutreachStats(ctx context.Context, orgID uuid.UUID, from, to time.Time, limit int) (StatsCounts, error) {
	conn := db.Conn(ctx, s.orm)
	c := StatsCounts{ByStatus: map[DispatchStatus]int{}}
	if err := conn.Scopes(db.ScopeOrg(orgID)).Where("archived_at IS NULL").Order("last_count DESC NULLS LAST, id").
		Limit(limit).Find(&c.Segments).Error; err != nil {
		return StatsCounts{}, fmt.Errorf("outreach: segment stats: %w", err)
	}
	var active int64
	if err := conn.Model(&Segment{}).Scopes(db.ScopeOrg(orgID)).Where("archived_at IS NULL").Count(&active).Error; err != nil {
		return StatsCounts{}, fmt.Errorf("outreach: active segments: %w", err)
	}
	c.Active = int(active)
	var rows []struct {
		Status   string
		N        int
		Eligible int
		Sent     int
	}
	if err := conn.Raw(`SELECT status, count(*) AS n, coalesce(sum(eligible_count), 0) AS eligible,
	        coalesce(sum(sent_count), 0) AS sent
	   FROM outreach_dispatches WHERE org_id = @org_id AND created_at >= @from AND created_at < @to GROUP BY status`,
		map[string]any{"org_id": orgID, "from": from, "to": to}).Scan(&rows).Error; err != nil {
		return StatsCounts{}, fmt.Errorf("outreach: dispatch stats: %w", err)
	}
	for _, r := range rows {
		c.ByStatus[DispatchStatus(r.Status)] = r.N
		c.Eligible += r.Eligible
		c.Sent += r.Sent
	}
	return c, nil
}
