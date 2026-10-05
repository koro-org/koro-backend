package notify

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
)

// purgeSQL holds, per retention policy, the statement counting the rows
// whose retention ended (count) and the one deleting a batch of them,
// oldest first (del). Rows locked by a running transaction are skipped and
// taken by the next run.
var purgeSQL = map[string]struct{ count, del string }{
	PolicyNotificationDelivery: {
		`SELECT count(*) FROM notification_deliveries WHERE created_at < @cutoff`,
		`DELETE FROM notification_deliveries WHERE id IN (SELECT id FROM notification_deliveries
			WHERE created_at < @cutoff ORDER BY created_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`,
	},
	PolicyEmailSuppression: {
		`SELECT count(*) FROM email_suppressions WHERE created_at < @cutoff`,
		`DELETE FROM email_suppressions WHERE email_bidx IN (SELECT email_bidx FROM email_suppressions
			WHERE created_at < @cutoff ORDER BY created_at, email_bidx LIMIT @limit FOR UPDATE SKIP LOCKED)`,
	},
	PolicyPushToken: {
		`SELECT count(*) FROM push_tokens WHERE last_seen_at < @cutoff`,
		`DELETE FROM push_tokens WHERE id IN (SELECT id FROM push_tokens
			WHERE last_seen_at < @cutoff ORDER BY last_seen_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`,
	},
	PolicyInAppNotification: {
		`SELECT count(*) FROM in_app_notifications WHERE created_at < @cutoff`,
		`DELETE FROM in_app_notifications WHERE id IN (SELECT id FROM in_app_notifications
			WHERE created_at < @cutoff ORDER BY created_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`,
	},
}

// PurgeExpired implements Store.
//
// cross-org: retention applies to every recipient's rows.
func (s gormStore) PurgeExpired(ctx context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error) {
	q, ok := purgeSQL[policy]
	if !ok {
		return 0, fmt.Errorf("notify: no retention purge for policy %q", policy)
	}
	args := map[string]any{"cutoff": cutoff, "limit": limit}
	if dryRun {
		var n int64
		if err := db.Conn(ctx, s.orm).Raw(q.count, args).Scan(&n).Error; err != nil {
			return 0, fmt.Errorf("notify: count %s: %w", policy, db.Translate(err))
		}
		return int(n), nil
	}
	res := db.Conn(ctx, s.orm).Exec(q.del, args)
	if res.Error != nil {
		return 0, fmt.Errorf("notify: purge %s: %w", policy, db.Translate(res.Error))
	}
	return int(res.RowsAffected), nil
}
