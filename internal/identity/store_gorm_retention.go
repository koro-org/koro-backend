package identity

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
)

// purgeSQL holds the retention statements of role_history: the count and
// the delete of one batch, oldest first (BE-057). A role ends at
// revoked_at or rejected_at (anchor revoked_or_rejected, T-121).
var purgeSQL = map[string]struct{ count, del string }{
	PolicyRoleHistory: {
		`SELECT count(*) FROM platform_role_assignments
			WHERE status IN ('revoked','rejected') AND COALESCE(revoked_at, rejected_at) < @cutoff`,
		`DELETE FROM platform_role_assignments WHERE id IN (SELECT id FROM platform_role_assignments
			WHERE status IN ('revoked','rejected') AND COALESCE(revoked_at, rejected_at) < @cutoff
			ORDER BY COALESCE(revoked_at, rejected_at), id LIMIT @limit FOR UPDATE SKIP LOCKED)`,
	},
}

// PurgeExpired implements Store. privileged_account_actions is purged by
// 000121's SECURITY DEFINER function (app_rw holds no DELETE on it); the
// function refuses a real purge of an unapproved policy (55000).
//
// cross-org: platform roles and account requests belong to no org.
func (s gormStore) PurgeExpired(ctx context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error) {
	conn := db.Conn(ctx, s.orm)
	if policy == PolicyPrivilegedActionHistory {
		var n int64
		if err := conn.Raw(`SELECT privileged_account_actions_purge( ?, ?, ?)`, cutoff, max(limit, 1), dryRun).Scan(&n).Error; err != nil {
			return 0, fmt.Errorf("identity: purge %s: %w", policy, db.Translate(err))
		}
		return int(n), nil
	}
	q, ok := purgeSQL[policy]
	if !ok {
		return 0, fmt.Errorf("identity: no retention purge for policy %q", policy)
	}
	args := map[string]any{"cutoff": cutoff, "limit": limit}
	if dryRun {
		var n int64
		if err := conn.Raw(q.count, args).Scan(&n).Error; err != nil {
			return 0, fmt.Errorf("identity: count %s: %w", policy, db.Translate(err))
		}
		return int(n), nil
	}
	res := conn.Exec(q.del, args)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: purge %s: %w", policy, db.Translate(res.Error))
	}
	return int(res.RowsAffected), nil
}

// eduDueSQL is the condition (and the anchor) of each student verification
// policy: edu_document, a decided document verification still holding its
// file, decided at verified_at or else at the last update; history, an
// ended verification, expired at expires_at, otherwise at the last update.
var eduDueSQL = map[string]struct{ where, anchor string }{
	PolicyEduDocument: {
		`method = 'document' AND document_file_id IS NOT NULL AND status <> 'pending' AND COALESCE(verified_at, updated_at) < @cutoff`,
		`COALESCE(verified_at, updated_at)`,
	},
	PolicyEduVerificationHistory: {
		`status IN ('rejected','expired','revoked','superseded')
			AND CASE WHEN status = 'expired' THEN COALESCE(expires_at, updated_at) ELSE updated_at END < @cutoff`,
		`CASE WHEN status = 'expired' THEN COALESCE(expires_at, updated_at) ELSE updated_at END`,
	},
}

// EduRetentionDue implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) EduRetentionDue(ctx context.Context, policy string, cutoff time.Time, limit int) ([]EduRecord, error) {
	q, ok := eduDueSQL[policy]
	if !ok {
		return nil, fmt.Errorf("identity: no retention purge for policy %q", policy)
	}
	var rows []eduRow
	err := db.Conn(ctx, s.orm).Raw(`SELECT * FROM edu_verifications WHERE `+q.where+
		` ORDER BY `+q.anchor+`, id LIMIT @limit FOR UPDATE SKIP LOCKED`, map[string]any{"cutoff": cutoff, "limit": limit}).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("identity: %s due: %w", policy, err)
	}
	return eduRecords(rows), nil
}

// CountEduRetentionDue implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) CountEduRetentionDue(ctx context.Context, policy string, cutoff time.Time) (int, error) {
	q, ok := eduDueSQL[policy]
	if !ok {
		return 0, fmt.Errorf("identity: no retention purge for policy %q", policy)
	}
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM edu_verifications WHERE `+q.where,
		map[string]any{"cutoff": cutoff}).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("identity: count %s: %w", policy, err)
	}
	return int(n), nil
}
