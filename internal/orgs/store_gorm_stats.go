package orgs

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// OrgStats implements statsStore.
//
// cross-org: the platform panel's counts of every org.
func (s gormStore) OrgStats(ctx context.Context, from, to time.Time) (OrgStats, error) {
	var rows []struct {
		Kind   string
		Status string
		N      int
		Fresh  int
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT kind, status, count(*) AS n,
	        count(*) FILTER (WHERE created_at >= ? AND created_at < ?) AS fresh
	   FROM organizations GROUP BY kind, status ORDER BY kind, status`, from, to).Scan(&rows).Error
	if err != nil {
		return OrgStats{}, fmt.Errorf("orgs: org stats: %w", err)
	}
	st := OrgStats{Counts: []OrgCount{}, NewInRange: map[Kind]int{}}
	for _, r := range rows {
		st.Counts = append(st.Counts, OrgCount{Kind: Kind(r.Kind), Status: Status(r.Status), Count: r.N})
		if r.Fresh > 0 {
			st.NewInRange[Kind(r.Kind)] += r.Fresh
		}
	}
	return st, nil
}

// MemberStats implements statsStore. The rows of the org are classified
// once: a join is a started row that is not the successor of a role change
// (the old row ended role_changed at the instant the new one started), a
// departure an ended started row whose end was not a role change. Months
// are Europe/Istanbul.
func (s gormStore) MemberStats(ctx context.Context, orgID uuid.UUID, from, to time.Time) (MemberStats, error) {
	var rows []struct {
		Kind  string
		Month *string
		A     int
		B     int
	}
	err := db.Conn(ctx, s.orm).Raw(`WITH r AS (
  SELECT m.status, m.joined_at, m.ended_at,
         m.joined_at IS NOT NULL AND NOT EXISTS (
           SELECT 1 FROM org_members p
            WHERE p.org_id = m.org_id AND p.user_id = m.user_id AND p.end_reason = 'role_changed'
              AND p.ended_at = m.joined_at) AS joined,
         m.joined_at IS NOT NULL AND m.ended_at IS NOT NULL AND m.end_reason <> 'role_changed' AS departed
    FROM org_members m WHERE m.org_id = @org_id
)
SELECT 'now' AS kind, NULL::text AS month,
       count(*) FILTER (WHERE status = 'active') AS a, count(*) FILTER (WHERE status = 'pending') AS b
  FROM r
UNION ALL
SELECT 'joined', to_char(joined_at AT TIME ZONE 'Europe/Istanbul', 'YYYY-MM'), count(*), 0
  FROM r WHERE joined AND joined_at >= @from AND joined_at < @to GROUP BY 2
UNION ALL
SELECT 'left', to_char(ended_at AT TIME ZONE 'Europe/Istanbul', 'YYYY-MM'), count(*), 0
  FROM r WHERE departed AND ended_at >= @from AND ended_at < @to GROUP BY 2`,
		map[string]any{"org_id": orgID, "from": from, "to": to}).Scan(&rows).Error
	if err != nil {
		return MemberStats{}, fmt.Errorf("orgs: member stats: %w", err)
	}
	st := MemberStats{}
	months := map[string]*MemberMonth{}
	for _, r := range rows {
		switch r.Kind {
		case "now":
			st.Active, st.Pending = r.A, r.B
		case "joined":
			addMonth(months, *r.Month).Joined += r.A
			st.JoinedInRange += r.A
		case "left":
			addMonth(months, *r.Month).Left += r.A
			st.LeftInRange += r.A
		}
	}
	st.Monthly = sortedMonths(months)
	return st, nil
}
