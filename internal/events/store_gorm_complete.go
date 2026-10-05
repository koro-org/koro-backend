package events

import (
	"context"
	"time"

	"koro/internal/platform/db"
)

// CompleteEnded implements Store.
//
// cross-org: the system job events.complete_ended runs over every event.
func (s gormStore) CompleteEnded(ctx context.Context, endedBy time.Time, limit int) ([]Event, error) {
	var out []Event
	err := db.Conn(ctx, s.orm).Raw(`UPDATE events e SET status = 'completed'
		FROM (SELECT x.id FROM events x WHERE x.status = 'published' AND x.ends_at <= @ended_by
		      ORDER BY x.ends_at, x.id LIMIT @limit FOR UPDATE OF x SKIP LOCKED) t
		WHERE e.id = t.id RETURNING `+prefixed(eventCols, "e."), map[string]any{"ended_by": endedBy, "limit": limit}).
		Scan(&out).Error
	if err != nil {
		return nil, wrap("complete ended events", err)
	}
	return out, nil
}
