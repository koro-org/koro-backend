package events

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
)

// purgeSQL holds, per retention policy, the count of rows due and the
// statement handling one batch of them, oldest event end first (BE-057).
// Rows locked by a running transaction are skipped.
var purgeSQL = map[string]struct{ count, purge string }{
	PolicyFormAnswers: {
		`SELECT count(*) FROM event_registrations r JOIN events e ON e.id = r.event_id
			WHERE e.ends_at < @cutoff AND r.answers_purged_at IS NULL`,
		`UPDATE event_registrations SET answers = '{}'::jsonb, answers_sensitive_enc = NULL, answers_purged_at = now()
			WHERE id IN (SELECT r.id FROM event_registrations r JOIN events e ON e.id = r.event_id
				WHERE e.ends_at < @cutoff AND r.answers_purged_at IS NULL
				ORDER BY e.ends_at, r.id LIMIT @limit FOR UPDATE OF r SKIP LOCKED)`,
	},
	PolicyFormAnswersSensitive: {
		`SELECT count(*) FROM event_registrations r JOIN events e ON e.id = r.event_id
			WHERE e.ends_at < @cutoff AND r.answers_sensitive_enc IS NOT NULL`,
		`UPDATE event_registrations SET answers_sensitive_enc = NULL
			WHERE id IN (SELECT r.id FROM event_registrations r JOIN events e ON e.id = r.event_id
				WHERE e.ends_at < @cutoff AND r.answers_sensitive_enc IS NOT NULL
				ORDER BY e.ends_at, r.id LIMIT @limit FOR UPDATE OF r SKIP LOCKED)`,
	},
	PolicyStaff: {
		`SELECT count(*) FROM event_staff s JOIN events e ON e.id = s.event_id WHERE e.ends_at < @cutoff`,
		`DELETE FROM event_staff WHERE (event_id, user_id) IN (SELECT s.event_id, s.user_id FROM event_staff s
			JOIN events e ON e.id = s.event_id WHERE e.ends_at < @cutoff
			ORDER BY e.ends_at, s.event_id, s.user_id LIMIT @limit FOR UPDATE OF s SKIP LOCKED)`,
	},
}

// PurgeExpired implements Store.
//
// cross-org: retention applies to the events of every org.
func (s gormStore) PurgeExpired(ctx context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error) {
	q, ok := purgeSQL[policy]
	if !ok {
		return 0, fmt.Errorf("events: no retention purge for policy %q", policy)
	}
	args := map[string]any{"cutoff": cutoff, "limit": limit}
	if dryRun {
		var n int64
		if err := db.Conn(ctx, s.orm).Raw(q.count, args).Scan(&n).Error; err != nil {
			return 0, fmt.Errorf("events: count %s: %w", policy, db.Translate(err))
		}
		return int(n), nil
	}
	res := db.Conn(ctx, s.orm).Exec(q.purge, args)
	if res.Error != nil {
		return 0, fmt.Errorf("events: purge %s: %w", policy, db.Translate(res.Error))
	}
	return int(res.RowsAffected), nil
}
