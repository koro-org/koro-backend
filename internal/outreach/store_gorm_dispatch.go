package outreach

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

const dispatchCols = `id, org_id, post_id, segment_id, audience_keys, audience_dim_count, channels, title_snapshot, deep_link,
	commercial, status, eligible_count, consented_count, sent_count, entitlement_usage_id, started_at, completed_at,
	last_error, created_by, created_at, updated_at`

// dispatchConstraints maps the named constraints of 02 §13.2 onto module
// errors (unlisted CHECKs are 422, FKs 409: db.Translate).
var dispatchConstraints = db.Constraints{
	"outreach_dispatches_post_id_uq":             ErrDispatchExists,
	"outreach_dispatches_post_id_org_id_fkey":    problem.ErrNotFound,
	"outreach_dispatches_segment_id_org_id_fkey": problem.ErrNotFound,
	"outreach_dispatches_org_id_fkey":            problem.ErrNotFound,
}

func wrapDispatch(op string, err error) error {
	return fmt.Errorf("outreach: %s: %w", op, dispatchConstraints.Translate(err))
}

// terminalStatuses are the statuses that stamp completed_at.
var terminalStatuses = []string{string(DispatchCompleted), string(DispatchCancelled), string(DispatchFailed)}

func dispatchRows(ctx context.Context, orm *gorm.DB, op, sql string, args map[string]any) ([]Dispatch, error) {
	var rows []Dispatch
	if err := db.Conn(ctx, orm).Raw(sql, args).Scan(&rows).Error; err != nil {
		return nil, wrapDispatch(op, err)
	}
	return rows, nil
}

func takeDispatch(op string, q *gorm.DB) (Dispatch, error) {
	var d Dispatch
	err := q.Take(&d).Error
	switch {
	case db.IsNotFound(err):
		return Dispatch{}, ErrNotFound
	case err != nil:
		return Dispatch{}, wrapDispatch(op, err)
	}
	return d, nil
}

// InsertDispatch implements Store.
//
// cross-org: a new dispatch of the post's org (the service authorised it on
// that org's surface; the composite FKs hold the post and the segment to
// it, 06 H-38).
func (s gormStore) InsertDispatch(ctx context.Context, d Dispatch) (Dispatch, error) {
	if d.ID == uuid.Nil() || d.OrgID == uuid.Nil() {
		return Dispatch{}, fmt.Errorf("outreach: insert dispatch: %w", db.ErrMissingOrgScope)
	}
	keys, chans := d.AudienceKeys, d.Channels
	if keys == nil {
		keys = db.TextArray{}
	}
	if chans == nil {
		chans = db.TextArray{}
	}
	rows, err := dispatchRows(ctx, s.orm, "insert dispatch", `INSERT INTO outreach_dispatches
		(id, org_id, post_id, segment_id, audience_keys, audience_dim_count, channels, title_snapshot, deep_link,
		 commercial, status, entitlement_usage_id, created_by)
		VALUES ( @id, @org_id, @post_id, @segment_id, @audience_keys, @audience_dim_count, @channels, @title_snapshot,
		        @deep_link, @commercial, @status, @entitlement_usage_id, @created_by)
		RETURNING `+dispatchCols, map[string]any{
		"id": d.ID, "org_id": d.OrgID, "post_id": d.PostID, "segment_id": d.SegmentID, "audience_keys": keys,
		"audience_dim_count": d.AudienceDimCount, "channels": chans, "title_snapshot": d.TitleSnapshot,
		"deep_link": d.DeepLink, "commercial": d.Commercial, "status": string(d.Status),
		"entitlement_usage_id": d.EntitlementUsageID, "created_by": d.CreatedBy,
	})
	if err != nil {
		return Dispatch{}, err
	}
	if len(rows) != 1 {
		return Dispatch{}, fmt.Errorf("outreach: insert dispatch returned %d rows", len(rows))
	}
	return rows[0], nil
}

// GetDispatch implements Store.
func (s gormStore) GetDispatch(ctx context.Context, orgID, id uuid.UUID) (Dispatch, error) {
	return takeDispatch("get dispatch", db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("id = ?", id))
}

// LockDispatch implements Store.
func (s gormStore) LockDispatch(ctx context.Context, orgID, id uuid.UUID) (Dispatch, error) {
	return takeDispatch("lock dispatch", db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).
		Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).Where("id = ?", id))
}

// DispatchOfPost implements Store.
func (s gormStore) DispatchOfPost(ctx context.Context, orgID, postID uuid.UUID, lock bool) (Dispatch, error) {
	q := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("post_id = ?", postID)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	return takeDispatch("dispatch of post", q)
}

// ListDispatches implements Store.
func (s gormStore) ListDispatches(ctx context.Context, orgID uuid.UUID, after *uuid.UUID, limit int) ([]Dispatch, error) {
	q := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID))
	if after != nil {
		q = q.Where("id < ?", *after)
	}
	var out []Dispatch
	if err := q.Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrapDispatch("list dispatches", err)
	}
	return out, nil
}

// DeleteDispatch implements Store.
func (s gormStore) DeleteDispatch(ctx context.Context, orgID, id uuid.UUID) error {
	if orgID == uuid.Nil() {
		return fmt.Errorf("outreach: delete dispatch: %w", db.ErrMissingOrgScope)
	}
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM outreach_dispatches WHERE id = @id AND org_id = @org_id`,
		map[string]any{"id": id, "org_id": orgID})
	switch {
	case res.Error != nil:
		return wrapDispatch("delete dispatch", res.Error)
	case res.RowsAffected == 0:
		return ErrNotFound
	}
	return nil
}

// TransitionDispatch implements Store.
func (s gormStore) TransitionDispatch(ctx context.Context, orgID, id uuid.UUID, from, to DispatchStatus, lastError *string, at time.Time) (Dispatch, bool, error) {
	if orgID == uuid.Nil() {
		return Dispatch{}, false, fmt.Errorf("outreach: transition dispatch: %w", db.ErrMissingOrgScope)
	}
	var completed *time.Time
	if to.Terminal() {
		completed = &at
	}
	rows, err := dispatchRows(ctx, s.orm, "transition dispatch", `UPDATE outreach_dispatches
		SET status = @to, last_error = @last_error, completed_at = @completed_at
		WHERE id = @id AND org_id = @org_id AND status = @from RETURNING `+dispatchCols, map[string]any{
		"to": string(to), "last_error": lastError, "completed_at": completed, "id": id, "org_id": orgID,
		"from": string(from),
	})
	if err != nil || len(rows) == 0 {
		return Dispatch{}, false, err
	}
	return rows[0], true, nil
}

// CompleteDispatch implements Store.
func (s gormStore) CompleteDispatch(ctx context.Context, orgID, id uuid.UUID, c DispatchCounts, at time.Time) (bool, error) {
	if orgID == uuid.Nil() {
		return false, fmt.Errorf("outreach: complete dispatch: %w", db.ErrMissingOrgScope)
	}
	res := db.Conn(ctx, s.orm).Exec(`UPDATE outreach_dispatches
		SET status = 'completed', completed_at = @at, last_error = NULL,
		    eligible_count = @eligible, consented_count = @consented, sent_count = @sent
		WHERE id = @id AND org_id = @org_id AND status = 'running'`, map[string]any{
		"at": at, "eligible": c.Eligible, "consented": c.Consented, "sent": c.Sent, "id": id, "org_id": orgID,
	})
	if res.Error != nil {
		return false, wrapDispatch("complete dispatch", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// DispatchByID implements Store.
//
// cross-org: the worker addresses the dispatch of its event (system); the
// row carries its org.
func (s gormStore) DispatchByID(ctx context.Context, id uuid.UUID) (Dispatch, error) {
	return takeDispatch("dispatch by id", db.Conn(ctx, s.orm).Where("id = ?", id))
}

// ClaimDispatch implements Store. The claim's age is measured in database
// time (updated_at is stamped by outreach_dispatches_touch_updated_at).
//
// cross-org: the worker claims the dispatch of its event (system).
func (s gormStore) ClaimDispatch(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (Dispatch, bool, error) {
	rows, err := dispatchRows(ctx, s.orm, "claim dispatch", `UPDATE outreach_dispatches
		SET status = 'running', started_at = coalesce(started_at, @now)
		WHERE id = @id AND (status = 'scheduled'
			OR (status = 'running' AND updated_at <= now() - ( @lease * interval '1 microsecond')))
		RETURNING `+dispatchCols, map[string]any{"now": now, "id": id, "lease": lease.Microseconds()})
	if err != nil || len(rows) == 0 {
		return Dispatch{}, false, err
	}
	return rows[0], true, nil
}

// TouchDispatch implements Store.
//
// cross-org: the worker renews its own claim (system).
func (s gormStore) TouchDispatch(ctx context.Context, id uuid.UUID) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE outreach_dispatches SET status = status WHERE id = @id AND status = 'running'`,
		map[string]any{"id": id})
	if res.Error != nil {
		return wrapDispatch("touch dispatch", res.Error)
	}
	return nil
}

// PendingDispatches implements Store.
//
// cross-org: the resume job (outreach.resume_dispatches, system) looks at
// every org's open dispatches; each row carries its org.
func (s gormStore) PendingDispatches(ctx context.Context, scheduledBefore time.Time, lease time.Duration, after uuid.UUID, limit int) ([]Dispatch, error) {
	return dispatchRows(ctx, s.orm, "pending dispatches", `SELECT `+dispatchCols+` FROM outreach_dispatches
		WHERE id > @after AND ((status = 'scheduled' AND created_at < @before)
			OR (status = 'running' AND updated_at <= now() - ( @lease * interval '1 microsecond')))
		ORDER BY id LIMIT @limit`, map[string]any{
		"after": after, "before": scheduledBefore, "lease": lease.Microseconds(), "limit": limit,
	})
}

// PurgeDispatches implements Store. Rows locked by a running transaction
// are skipped and taken by the next run.
//
// cross-org: retention applies to every org's dispatches.
func (s gormStore) PurgeDispatches(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	args := map[string]any{"cutoff": cutoff, "limit": limit, "done": terminalStatuses}
	if dryRun {
		var n int64
		if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM outreach_dispatches
			WHERE created_at < @cutoff AND status IN @done`, args).Scan(&n).Error; err != nil {
			return 0, wrapDispatch("count expired dispatches", err)
		}
		return int(n), nil
	}
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM outreach_dispatches WHERE id IN (SELECT id FROM outreach_dispatches
		WHERE created_at < @cutoff AND status IN @done ORDER BY created_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`, args)
	if res.Error != nil {
		return 0, wrapDispatch("purge dispatches", res.Error)
	}
	return int(res.RowsAffected), nil
}
