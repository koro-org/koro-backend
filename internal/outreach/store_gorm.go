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

// NewGormStore returns the PostgreSQL store over segments (02 §13.1).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

const segmentCols = `id, org_id, name, definition, audience_keys, audience_dim_count, last_count, last_counted_at,
	created_by, archived_at, created_at, updated_at`

// constraints maps the named constraints of 02 §13.1 onto module errors.
// Unlisted CHECKs are 422 and FKs 409 (db.Translate).
var constraints = db.Constraints{
	"segments_org_id_fkey": problem.ErrNotFound,
}

func wrap(op string, err error) error {
	return fmt.Errorf("outreach: %s: %w", op, constraints.Translate(err))
}

// segmentArgs are the named arguments of the insert and update statements.
func segmentArgs(s Segment) map[string]any {
	keys := s.AudienceKeys
	if keys == nil {
		keys = db.TextArray{}
	}
	return map[string]any{
		"id": s.ID, "org_id": s.OrgID, "name": s.Name, "definition": s.Definition, "audience_keys": keys,
		"audience_dim_count": s.AudienceDimCount, "last_count": s.LastCount, "last_counted_at": s.LastCountedAt,
		"created_by": s.CreatedBy, "archived_at": s.ArchivedAt,
	}
}

func returning(ctx context.Context, orm *gorm.DB, op, sql string, args map[string]any) (Segment, error) {
	var rows []Segment
	if err := db.Conn(ctx, orm).Raw(sql, args).Scan(&rows).Error; err != nil {
		return Segment{}, wrap(op, err)
	}
	if len(rows) == 0 {
		return Segment{}, ErrNotFound
	}
	return rows[0], nil
}

// Insert implements Store.
//
// cross-org: a new segment of the org the service authorised (authz.Can on
// the org's management surface); the row is the root of its own scope.
func (s gormStore) Insert(ctx context.Context, seg Segment) (Segment, error) {
	if seg.ID == uuid.Nil() || seg.OrgID == uuid.Nil() {
		return Segment{}, fmt.Errorf("outreach: insert segment: %w", db.ErrMissingOrgScope)
	}
	return returning(ctx, s.orm, "insert segment", `INSERT INTO segments
		(id, org_id, name, definition, audience_keys, audience_dim_count, last_count, last_counted_at, created_by, archived_at)
		VALUES ( @id, @org_id, @name, @definition, @audience_keys, @audience_dim_count, @last_count, @last_counted_at,
		        @created_by, @archived_at)
		RETURNING `+segmentCols, segmentArgs(seg))
}

// Get implements Store.
func (s gormStore) Get(ctx context.Context, orgID, id uuid.UUID) (Segment, error) {
	return take("get segment", db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("id = ?", id))
}

// Lock implements Store.
func (s gormStore) Lock(ctx context.Context, orgID, id uuid.UUID) (Segment, error) {
	return take("lock segment", db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).
		Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).Where("id = ?", id))
}

func take(op string, q *gorm.DB) (Segment, error) {
	var seg Segment
	err := q.Take(&seg).Error
	switch {
	case db.IsNotFound(err):
		return Segment{}, ErrNotFound
	case err != nil:
		return Segment{}, wrap(op, err)
	}
	return seg, nil
}

// Update implements Store.
func (s gormStore) Update(ctx context.Context, seg Segment) (Segment, error) {
	if seg.OrgID == uuid.Nil() {
		return Segment{}, fmt.Errorf("outreach: update segment: %w", db.ErrMissingOrgScope)
	}
	return returning(ctx, s.orm, "update segment", `UPDATE segments SET
		name = @name, definition = @definition, audience_keys = @audience_keys, audience_dim_count = @audience_dim_count,
		last_count = @last_count, last_counted_at = @last_counted_at, archived_at = @archived_at
		WHERE id = @id AND org_id = @org_id RETURNING `+segmentCols, segmentArgs(seg))
}

// List implements Store.
func (s gormStore) List(ctx context.Context, orgID uuid.UUID, archived bool, after *uuid.UUID, limit int) ([]Segment, error) {
	q := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID))
	if archived {
		q = q.Where("archived_at IS NOT NULL")
	} else {
		q = q.Where("archived_at IS NULL")
	}
	if after != nil {
		q = q.Where("id < ?", *after)
	}
	var out []Segment
	if err := q.Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list segments", err)
	}
	return out, nil
}

// ActiveAfter implements Store.
//
// cross-org: the nightly recount (outreach.recount_segments, a system job)
// walks the segments of every org; each row carries its org.
func (s gormStore) ActiveAfter(ctx context.Context, after uuid.UUID, limit int) ([]Segment, error) {
	var out []Segment
	err := db.Conn(ctx, s.orm).Where("archived_at IS NULL AND id > ?", after).Order("id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, wrap("active segments", err)
	}
	return out, nil
}

// SetCount implements Store.
func (s gormStore) SetCount(ctx context.Context, orgID, id uuid.UUID, count *int, at time.Time) (bool, error) {
	if orgID == uuid.Nil() {
		return false, fmt.Errorf("outreach: set count: %w", db.ErrMissingOrgScope)
	}
	res := db.Conn(ctx, s.orm).Exec(`UPDATE segments SET last_count = @count, last_counted_at = @at
		WHERE id = @id AND org_id = @org_id AND archived_at IS NULL`,
		map[string]any{"count": count, "at": at, "id": id, "org_id": orgID})
	if res.Error != nil {
		return false, wrap("set count", res.Error)
	}
	return res.RowsAffected > 0, nil
}
