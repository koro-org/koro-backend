package messaging

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// ContactRequestStats implements StatsStore.
func (s gormStore) ContactRequestStats(ctx context.Context, orgID uuid.UUID, from, to, now time.Time) (ContactRequestStats, error) {
	var rows []struct {
		Status string
		N      int
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT CASE WHEN status = 'pending' AND expires_at <= @now THEN 'expired' ELSE status END AS status,
	        count(*) AS n
	   FROM contact_requests WHERE org_id = @org_id AND created_at >= @from AND created_at < @to GROUP BY 1`,
		map[string]any{"now": now, "org_id": orgID, "from": from, "to": to}).Scan(&rows).Error
	if err != nil {
		return ContactRequestStats{}, fmt.Errorf("messaging: contact request stats: %w", err)
	}
	st := ContactRequestStats{ByStatus: map[Status]int{}}
	for _, r := range rows {
		st.ByStatus[Status(r.Status)] += r.N
		st.Sent += r.N
	}
	return st, nil
}
