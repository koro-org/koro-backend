package events

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// EventStats implements StatsStore.
//
// cross-org: the platform panel's counts of every event.
func (s gormStore) EventStats(ctx context.Context, from, to, now time.Time) (EventStats, error) {
	conn := db.Conn(ctx, s.orm)
	var st EventStats
	if err := conn.Raw(`SELECT count(*) FILTER (WHERE status = 'published' AND starts_at > ?) AS upcoming,
	        count(*) FILTER (WHERE status IN ('published','completed') AND starts_at >= ? AND starts_at < ? AND starts_at <= ?) AS held,
	        count(*) FILTER (WHERE status = 'cancelled' AND starts_at >= ? AND starts_at < ?) AS cancelled
	   FROM events`, now, from, to, now, from, to).Scan(&st).Error; err != nil {
		return EventStats{}, fmt.Errorf("events: event stats: %w", err)
	}
	var part struct {
		Registered int
		CheckedIn  int
	}
	if err := conn.Raw(`SELECT count(*) FILTER (WHERE r.status IN ('confirmed','checked_in','no_show')) AS registered,
	        count(*) FILTER (WHERE r.status = 'checked_in') AS checked_in
	   FROM event_registrations r JOIN events e ON e.id = r.event_id
	  WHERE e.status IN ('published','completed') AND e.starts_at >= ? AND e.starts_at < ? AND e.starts_at <= ?`,
		from, to, now).Scan(&part).Error; err != nil {
		return EventStats{}, fmt.Errorf("events: participation stats: %w", err)
	}
	st.Registered, st.CheckedIn = part.Registered, part.CheckedIn
	return st, nil
}

// OrgEventFigures implements StatsStore (events_org_idx).
//
// cross-org: keyed by the orgs the caller resolved for its own panel (one
// club); registrants never leave, only counts.
func (s gormStore) OrgEventFigures(ctx context.Context, orgIDs []uuid.UUID, from, to time.Time, limit int) ([]EventFigures, error) {
	var rows []EventFigures
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.id AS event_id, e.org_id, e.title, e.status, e.starts_at, e.ends_at,
	        e.is_sensitive_category AS sensitive,
	        count(r.id) FILTER (WHERE r.status IN ('confirmed','checked_in','no_show')) AS registered,
	        count(r.id) FILTER (WHERE r.status = 'checked_in') AS checked_in,
	        count(r.id) FILTER (WHERE r.status = 'no_show') AS no_show
	   FROM events e LEFT JOIN event_registrations r ON r.event_id = e.id AND r.org_id = e.org_id
	  WHERE e.org_id IN ? AND e.status <> 'draft' AND e.starts_at >= ? AND e.starts_at < ?
	  GROUP BY e.id ORDER BY e.starts_at DESC, e.id DESC LIMIT ?`, orgIDs, from, to, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("events: org event figures: %w", err)
	}
	if rows == nil {
		rows = []EventFigures{}
	}
	return rows, nil
}

// OrgEventTotals implements StatsStore (events_org_idx).
//
// cross-org: keyed by the orgs the caller resolved for its own panel (the
// clubs of one university); only counts leave.
func (s gormStore) OrgEventTotals(ctx context.Context, orgIDs []uuid.UUID, from, to, now time.Time) (map[uuid.UUID]EventTotals, error) {
	var rows []struct {
		OrgID      uuid.UUID
		Events     int
		Registered int
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.org_id, count(DISTINCT e.id) AS events,
	        count(r.id) FILTER (WHERE e.starts_at <= ? AND r.status IN ('confirmed','checked_in','no_show')) AS registered
	   FROM events e LEFT JOIN event_registrations r ON r.event_id = e.id AND r.org_id = e.org_id
	  WHERE e.org_id IN ? AND e.status IN ('published','completed') AND NOT e.is_sensitive_category
	    AND e.starts_at >= ? AND e.starts_at < ?
	  GROUP BY e.org_id`, now, orgIDs, from, to).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("events: org event totals: %w", err)
	}
	out := make(map[uuid.UUID]EventTotals, len(rows))
	for _, r := range rows {
		out[r.OrgID] = EventTotals{Events: r.Events, Registered: r.Registered}
	}
	return out, nil
}
