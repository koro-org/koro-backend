package entitlements

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// NewGormStore returns the PostgreSQL store over entitlements and
// entitlement_usages (02 §10).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// usableLimit bounds the candidates of one consumption: an org holds a few
// packages per feature, never hundreds.
const usableLimit = 50

// constraints maps the named constraints of 02 §10 onto module errors.
var constraints = db.Constraints{
	// An admin grant for an org that does not exist (the org is the path).
	"entitlements_org_id_fkey": problem.ErrNotFound,
}

// Insert implements Store.
//
// cross-org: the new row carries its org_id (the grant's org).
func (s gormStore) Insert(ctx context.Context, e Entitlement) error {
	if err := db.Conn(ctx, s.orm).Create(&e).Error; err != nil {
		return fmt.Errorf("entitlements: insert: %w", constraints.Translate(err))
	}
	return nil
}

// Get implements Store.
func (s gormStore) Get(ctx context.Context, orgID, id uuid.UUID) (Entitlement, error) {
	var e Entitlement
	err := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("id = ?", id).Take(&e).Error
	switch {
	case db.IsNotFound(err):
		return Entitlement{}, ErrNotFound
	case err != nil:
		return Entitlement{}, fmt.Errorf("entitlements: get: %w", err)
	}
	return e, nil
}

// Lock implements Store.
//
// cross-org: addressed by ID from the admin surface or a usage row (the
// caller has checked authz or holds the parent row).
func (s gormStore) Lock(ctx context.Context, id uuid.UUID) (Entitlement, error) {
	var e Entitlement
	err := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("id = ?", id).Take(&e).Error
	switch {
	case db.IsNotFound(err):
		return Entitlement{}, ErrNotFound
	case err != nil:
		return Entitlement{}, fmt.Errorf("entitlements: lock: %w", err)
	}
	return e, nil
}

// List implements Store.
func (s gormStore) List(ctx context.Context, orgID uuid.UUID, f ListFilter, after *uuid.UUID, limit int) ([]Entitlement, error) {
	q := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID))
	if f.Feature != "" {
		q = q.Where("feature = ?", f.Feature)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if after != nil {
		q = q.Where("id < ?", *after)
	}
	var out []Entitlement
	if err := q.Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("entitlements: list: %w", err)
	}
	return out, nil
}

// Usable implements Store.
func (s gormStore) Usable(ctx context.Context, orgID uuid.UUID, f Feature, now time.Time) ([]Entitlement, error) {
	var out []Entitlement
	err := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).
		Where("feature = ? AND status = 'active' AND valid_from <= ? AND valid_to > ?", f, now, now).
		Order("valid_to, id").Limit(usableLimit).Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("entitlements: usable: %w", err)
	}
	return out, nil
}

// BySource implements Store.
//
// cross-org: a source (order line) is global; the rows carry their org.
func (s gormStore) BySource(ctx context.Context, t SourceType, sourceID uuid.UUID) ([]Entitlement, error) {
	var out []Entitlement
	err := db.Conn(ctx, s.orm).Where("source_type = ? AND source_id = ?", t, sourceID).Order("id").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("entitlements: by source: %w", err)
	}
	return out, nil
}

// TryConsume implements Store: the conditional UPDATE of 02 §10.1. In READ
// COMMITTED a concurrent UPDATE of the same row waits for the first
// transaction and re-checks the WHERE clause on the committed row, so the
// guard quantity_used + qty <= quantity_total holds for every caller.
func (s gormStore) TryConsume(ctx context.Context, orgID, id uuid.UUID, qty int, now time.Time) (Entitlement, bool, error) {
	var rows []Entitlement
	err := db.Conn(ctx, s.orm).Raw(`UPDATE entitlements
		SET quantity_used = quantity_used + @qty,
		    status = CASE WHEN quantity_used + @qty = quantity_total THEN 'exhausted' ELSE status END
		WHERE id = @id AND org_id = @org_id AND kind = 'quota' AND status = 'active'
		  AND valid_from <= @now AND valid_to > @now AND quantity_used + @qty <= quantity_total
		RETURNING *`,
		map[string]any{"qty": qty, "id": id, "org_id": orgID, "now": now}).Scan(&rows).Error
	if err != nil {
		return Entitlement{}, false, fmt.Errorf("entitlements: consume: %w", db.Translate(err))
	}
	if len(rows) == 0 {
		return Entitlement{}, false, nil
	}
	return rows[0], true, nil
}

// Unconsume implements Store.
//
// cross-org: the row is the parent of a usage the caller holds.
func (s gormStore) Unconsume(ctx context.Context, id uuid.UUID, qty int, now time.Time) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE entitlements
		SET quantity_used = quantity_used - ?,
		    status = CASE WHEN status = 'exhausted' THEN CASE WHEN valid_to > ? THEN 'active' ELSE 'expired' END
		                  ELSE status END
		WHERE id = ? AND quantity_used >= ?`, qty, now, id, qty)
	if res.Error != nil {
		return fmt.Errorf("entitlements: unconsume: %w", db.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// Activate implements Store.
//
// cross-org: a source (order line) is global; the rows carry their org.
func (s gormStore) Activate(ctx context.Context, t SourceType, sourceID uuid.UUID, now time.Time) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE entitlements SET status = 'active', activated_at = ?
		WHERE source_type = ? AND source_id = ? AND status = 'pending'`, now, t, sourceID)
	if res.Error != nil {
		return 0, fmt.Errorf("entitlements: activate: %w", db.Translate(res.Error))
	}
	return int(res.RowsAffected), nil
}

// Revoke implements Store.
//
// cross-org: a source (order line) is global; the rows carry their org.
func (s gormStore) Revoke(ctx context.Context, t SourceType, sourceID uuid.UUID, now time.Time, reason string) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE entitlements SET status = 'revoked', revoked_at = ?, revoke_reason = NULLIF( ?, '')
		WHERE source_type = ? AND source_id = ? AND status IN ('pending', 'active', 'exhausted')`,
		now, reason, t, sourceID)
	if res.Error != nil {
		return 0, fmt.Errorf("entitlements: revoke: %w", db.Translate(res.Error))
	}
	return int(res.RowsAffected), nil
}

// RevokeOne implements Store.
//
// cross-org: addressed by ID from the admin surface (authz checked first).
func (s gormStore) RevokeOne(ctx context.Context, id uuid.UUID, now time.Time, reason string) (Entitlement, error) {
	var rows []Entitlement
	err := db.Conn(ctx, s.orm).Raw(`UPDATE entitlements SET status = 'revoked', revoked_at = ?, revoke_reason = NULLIF( ?, '')
		WHERE id = ? AND status IN ('pending', 'active', 'exhausted') RETURNING *`, now, reason, id).Scan(&rows).Error
	if err != nil {
		return Entitlement{}, fmt.Errorf("entitlements: revoke: %w", db.Translate(err))
	}
	if len(rows) == 0 {
		return Entitlement{}, ErrNotFound
	}
	return rows[0], nil
}

// Expire implements Store.
//
// cross-org: a platform job over every org (01 §7 entitlements.expire).
func (s gormStore) Expire(ctx context.Context, now time.Time, limit int) ([]Entitlement, error) {
	var rows []Entitlement
	// A materialised CTE runs the LIMIT … SKIP LOCKED select once (an IN
	// subquery may be re-run per row and expire more than limit rows).
	err := db.Conn(ctx, s.orm).Raw(`WITH due AS MATERIALIZED (
			SELECT id FROM entitlements WHERE status = 'active' AND valid_to <= ?
			ORDER BY valid_to, id LIMIT ? FOR UPDATE SKIP LOCKED)
		UPDATE entitlements e SET status = 'expired' FROM due
		WHERE e.id = due.id AND e.status = 'active'
		RETURNING e.*`, now, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("entitlements: expire: %w", db.Translate(err))
	}
	slices.SortFunc(rows, func(a, b Entitlement) int { return a.ValidTo.Compare(b.ValidTo) })
	return rows, nil
}

// InsertUsage implements Store. org_id and the entitlement come from the
// entitlement row (06 H-38); a reversal copies entitlement, org, ref and
// the negated quantity from the usage it reverses (T-134: a reversal can
// neither point at another entitlement nor create quota).
//
// cross-org: org_id is copied from the parent row inside the statement.
func (s gormStore) InsertUsage(ctx context.Context, u Usage) (Usage, bool, error) {
	conn := db.Conn(ctx, s.orm)
	var rows []Usage
	var err error
	if u.ReversalOf == nil {
		err = conn.Raw(`INSERT INTO entitlement_usages
			(id, entitlement_id, org_id, quantity, ref_type, ref_id, actor_user_id, created_at)
			SELECT ?, e.id, e.org_id, ?, ?, ?, ?, ? FROM entitlements e WHERE e.id = ?
			ON CONFLICT DO NOTHING
			RETURNING *`,
			u.ID, u.Quantity, u.RefType, u.RefID, u.ActorUserID, u.CreatedAt, u.EntitlementID).Scan(&rows).Error
	} else {
		err = conn.Raw(`INSERT INTO entitlement_usages
			(id, entitlement_id, org_id, quantity, ref_type, ref_id, reversal_of, actor_user_id, created_at)
			SELECT ?, o.entitlement_id, o.org_id, -o.quantity, o.ref_type, o.ref_id, o.id, ?, ?
			FROM entitlement_usages o WHERE o.id = ? AND o.reversal_of IS NULL
			ON CONFLICT DO NOTHING
			RETURNING *`,
			u.ID, u.ActorUserID, u.CreatedAt, *u.ReversalOf).Scan(&rows).Error
	}
	if err != nil {
		return Usage{}, false, fmt.Errorf("entitlements: insert usage: %w", db.Translate(err))
	}
	if len(rows) == 0 {
		return Usage{}, false, nil
	}
	return rows[0], true, nil
}

// usageRow is a consumption with its reversal flag.
type usageRow struct {
	Usage    `gorm:"embedded"`
	Reversed bool `gorm:"column:reversed"`
}

// UsageByRef implements Store.
//
// cross-org: a Ref is the caller's own record ID, unique across orgs; the
// service compares the row's org with its caller's.
func (s gormStore) UsageByRef(ctx context.Context, ref Ref) (Usage, bool, error) {
	var rows []usageRow
	err := db.Conn(ctx, s.orm).Raw(`SELECT u.*,
			EXISTS ( SELECT 1 FROM entitlement_usages r WHERE r.reversal_of = u.id) AS reversed
		FROM entitlement_usages u
		WHERE u.ref_type = ? AND u.ref_id = ? AND u.reversal_of IS NULL`, ref.Type, ref.ID).Scan(&rows).Error
	switch {
	case err != nil:
		return Usage{}, false, fmt.Errorf("entitlements: usage by ref: %w", err)
	case len(rows) == 0:
		return Usage{}, false, ErrNotFound
	}
	return rows[0].Usage, rows[0].Reversed, nil
}

// ListUsages implements Store.
func (s gormStore) ListUsages(ctx context.Context, orgID uuid.UUID, f UsageFilter, after *uuid.UUID, limit int) ([]UsageView, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	var where strings.Builder
	args := map[string]any{"org_id": orgID, "limit": limit}
	where.WriteString(`u.org_id = @org_id`)
	if f.Feature != "" {
		where.WriteString(` AND e.feature = @feature`)
		args["feature"] = f.Feature
	}
	if after != nil {
		where.WriteString(` AND u.id < @after`)
		args["after"] = *after
	}
	var out []UsageView
	err := db.Conn(ctx, s.orm).Raw(`SELECT u.*, e.feature
		FROM entitlement_usages u JOIN entitlements e ON e.id = u.entitlement_id
		WHERE `+where.String()+`
		ORDER BY u.id DESC LIMIT @limit`, args).Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("entitlements: list usages: %w", err)
	}
	return out, nil
}

// UsagesOfActor implements Store.
//
// cross-org: the KVKK export of one data subject spans every org.
func (s gormStore) UsagesOfActor(ctx context.Context, userID uuid.UUID) ([]Usage, error) {
	var rows []Usage
	if err := db.Conn(ctx, s.orm).Where("actor_user_id = ?", userID).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("entitlements: usages of actor: %w", db.Translate(err))
	}
	return rows, nil
}

// PurgeEndedGrants implements Store.
//
// cross-org: retention spans every org.
func (s gormStore) PurgeEndedGrants(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT entitlements_purge( ?, ?, ?)`, cutoff, max(limit, 1), dryRun).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("entitlements: purge ended grants: %w", db.Translate(err))
	}
	return int(n), nil
}
