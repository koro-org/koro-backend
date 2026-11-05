package events

import (
	"context"
	"strings"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// RecommendSignals implements RecommendStore.
//
// cross-org: one user's own registrations over every organiser (BE-174);
// only the category, the organiser and a count leave. A registration is
// joined through its event and the event's org (06 H-38).
func (s gormStore) RecommendSignals(ctx context.Context, userID uuid.UUID, since time.Time) ([]SignalRow, error) {
	var rows []struct {
		Category string    `gorm:"column:category"`
		OrgID    uuid.UUID `gorm:"column:org_id"`
		Events   int       `gorm:"column:events"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.category, e.org_id, count(*) AS events
		FROM event_registrations r JOIN events e ON e.id = r.event_id AND e.org_id = r.org_id
		WHERE r.user_id = ? AND r.status IN ('confirmed','checked_in')
		  AND e.status IN ('published','completed') AND NOT e.is_sensitive_category
		  AND e.starts_at >= ?
		GROUP BY e.category, e.org_id
		ORDER BY count(*) DESC, e.category, e.org_id
		LIMIT ?`, userID, since, maxSignalRows).Scan(&rows).Error
	if err != nil {
		return nil, wrap("recommend signals", err)
	}
	out := make([]SignalRow, len(rows))
	for i, r := range rows {
		out[i] = SignalRow{Category: Category(r.Category), OrgID: r.OrgID, Events: r.Events}
	}
	return out, nil
}

// RecommendableEvents implements RecommendStore.
//
// cross-org: the recommendation candidates span every organiser (BE-174);
// the viewer's visibility (03 §3.6) is applied in the query, as in
// ListEvents.
func (s gormStore) RecommendableEvents(ctx context.Context, f RecommendFilter, limit int) ([]Event, error) {
	var match []string
	var args []any
	if len(f.OrgIDs) > 0 {
		match, args = append(match, "org_id IN ?"), append(args, f.OrgIDs)
	}
	if len(f.Categories) > 0 {
		cats := make([]string, len(f.Categories))
		for i, c := range f.Categories {
			cats[i] = string(c)
		}
		match, args = append(match, "category IN ?"), append(args, cats)
	}
	if f.MinConfirmed > 0 {
		match, args = append(match, "confirmed_count >= ?"), append(args, f.MinConfirmed)
	}
	if len(match) == 0 || limit <= 0 {
		return []Event{}, nil
	}
	q := db.Conn(ctx, s.orm).Model(&Event{}).Select(eventCols).
		Where("status = ? AND NOT is_sensitive_category AND starts_at >= ? AND starts_at < ?", string(StatusPublished), f.From, f.To)
	q = visibleTo(q, f.Viewer).Where("("+strings.Join(match, " OR ")+")", args...).
		Where("NOT EXISTS (SELECT 1 FROM event_registrations r WHERE r.event_id = events.id AND r.user_id = ?)", f.UserID)
	var out []Event
	if err := q.Order("starts_at, id").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("recommendable events", err)
	}
	return out, nil
}
