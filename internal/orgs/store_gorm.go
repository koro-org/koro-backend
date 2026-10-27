package orgs

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// NewGormStore returns the PostgreSQL store over organizations, companies,
// org_members and org_invitations (02 §7.1–7.4).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// Column lists (search_vector is never read).
const (
	orgCols = `id, kind, name, slug, status, description, logo_file_id, website_url, city,
		verified_at, verified_by, closed_at, rejection_reason, created_by, created_at, updated_at`
	memberCols = `id, org_id, org_kind, user_id, role, status, source, requested_at, joined_at, ended_at,
		end_reason, invited_by, approved_by, created_at, updated_at`
)

// constraints maps the named constraints of 02 §7 onto module errors.
var constraints = db.Constraints{
	"organizations_slug_uq":            ErrSlugTaken,
	"organizations_logo_file_id_fkey":  problem.ErrNotFound,
	"companies_org_id_org_kind_fkey":   problem.ErrNotFound,
	"org_members_open_uq":              problem.ErrConflict,
	"org_members_one_owner_uq":         problem.ErrConflict,
	"organizations_single_platform_uq": problem.ErrConflict,
	"org_invitations_open_uq":          ErrInvitationExists,
	"org_invitations_token_hash_uq":    problem.ErrConflict,
	"org_members_org_id_org_kind_fkey": problem.ErrNotFound,
}

func wrap(op string, err error) error {
	return fmt.Errorf("orgs: %s: %w", op, constraints.Translate(err))
}

// InsertOrg implements Store.
//
// cross-org: the new row is the org root itself.
func (s gormStore) InsertOrg(ctx context.Context, o Org) (Org, bool, error) {
	var rows []Org
	err := db.Conn(ctx, s.orm).Raw(`INSERT INTO organizations
		(id, kind, name, slug, status, description, logo_file_id, website_url, city, verified_at, verified_by, closed_at, created_by)
		VALUES ( @id, @kind, @name, @slug, @status, @description, @logo, @website, @city, @verified_at, @verified_by, @closed_at, @created_by)
		ON CONFLICT (slug) DO NOTHING
		RETURNING `+orgCols, map[string]any{
		"id": o.ID, "kind": string(o.Kind), "name": o.Name, "slug": o.Slug, "status": string(o.Status),
		"description": o.Description, "logo": o.LogoFileID, "website": o.WebsiteURL, "city": o.City,
		"verified_at": o.VerifiedAt, "verified_by": o.VerifiedBy, "closed_at": o.ClosedAt, "created_by": o.CreatedBy,
	}).Scan(&rows).Error
	if err != nil {
		return Org{}, false, wrap("insert org", err)
	}
	if len(rows) == 0 {
		return Org{}, false, nil
	}
	return rows[0], true, nil
}

func takeOrg(op string, q *gorm.DB) (Org, error) {
	var o Org
	err := q.Select(orgCols).Take(&o).Error
	switch {
	case db.IsNotFound(err):
		return Org{}, ErrNotFound
	case err != nil:
		return Org{}, wrap(op, err)
	}
	return o, nil
}

// GetOrg implements Store.
//
// cross-org: organizations is the org root, addressed by its own id.
func (s gormStore) GetOrg(ctx context.Context, id uuid.UUID) (Org, error) {
	return takeOrg("get org", db.Conn(ctx, s.orm).Where("id = ?", id))
}

// GetOrgBySlug implements Store.
//
// cross-org: organizations is the org root, addressed by its slug.
func (s gormStore) GetOrgBySlug(ctx context.Context, slug string) (Org, error) {
	return takeOrg("get org by slug", db.Conn(ctx, s.orm).Where("slug = ?", slug))
}

// GetOrgs implements Store.
//
// cross-org: organizations is the org root, addressed by id (directory
// lookups of other modules).
func (s gormStore) GetOrgs(ctx context.Context, ids []uuid.UUID) ([]Org, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []Org
	if err := db.Conn(ctx, s.orm).Select(orgCols).Where("id IN ?", ids).Find(&out).Error; err != nil {
		return nil, wrap("get orgs", err)
	}
	return out, nil
}

// LockOrg implements Store.
//
// cross-org: organizations is the org root, addressed by its own id.
func (s gormStore) LockOrg(ctx context.Context, id uuid.UUID) (Org, error) {
	q := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).Where("id = ?", id)
	return takeOrg("lock org", q)
}

// UpdateOrg implements Store.
//
// cross-org: organizations is the org root, addressed by its own id.
func (s gormStore) UpdateOrg(ctx context.Context, o Org) (Org, error) {
	var rows []Org
	err := db.Conn(ctx, s.orm).Raw(`UPDATE organizations SET
		name = @name, status = @status, description = @description, logo_file_id = @logo, website_url = @website,
		city = @city, verified_at = @verified_at, verified_by = @verified_by, closed_at = @closed_at,
		rejection_reason = @rejection_reason
		WHERE id = @id
		RETURNING `+orgCols, map[string]any{
		"id": o.ID, "name": o.Name, "status": string(o.Status), "description": o.Description, "logo": o.LogoFileID,
		"website": o.WebsiteURL, "city": o.City, "verified_at": o.VerifiedAt, "verified_by": o.VerifiedBy,
		"closed_at": o.ClosedAt, "rejection_reason": o.RejectionReason,
	}).Scan(&rows).Error
	if err != nil {
		return Org{}, wrap("update org", err)
	}
	if len(rows) == 0 {
		return Org{}, ErrNotFound
	}
	return rows[0], nil
}

// ListOrgs implements Store.
//
// cross-org: the public directory and the admin list span every org; the
// service applies the visibility rules (status) through the filter.
func (s gormStore) ListOrgs(ctx context.Context, f ListFilter, after *ListAfter, limit int) ([]Org, error) {
	if f.OrgIDs != nil && len(f.OrgIDs) == 0 {
		return nil, nil
	}
	q := db.Conn(ctx, s.orm).Model(&Org{}).Select(orgCols)
	if len(f.Kinds) > 0 {
		kinds := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			kinds[i] = string(k)
		}
		q = q.Where("kind IN ?", kinds)
	}
	if len(f.Statuses) > 0 {
		sts := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			sts[i] = string(st)
		}
		q = q.Where("status IN ?", sts)
	}
	if f.City != "" {
		q = q.Where("public.koro_norm(city) = public.koro_norm( ?)", f.City)
	}
	if f.OrgIDs != nil {
		q = q.Where("id IN ?", f.OrgIDs)
	}
	if f.Q != "" {
		// Full text on name + description (koro_tr), or a substring of the
		// normalised name (trigram index organizations_trgm_idx).
		q = q.Where(`(search_vector @@ plainto_tsquery('public.koro_tr', ?)
			OR public.koro_norm(name) LIKE '%' || public.koro_norm( ?) || '%')`, f.Q, textx.EscapeLike(f.Q))
	}
	if f.Sort == SortName {
		// Turkish collation (organizations.name is COLLATE public.tr, ICU
		// tr-TR): Ç after C, I before İ, Ö after O; spelled out so the
		// keyset comparison uses the same order (T-203).
		if after != nil {
			q = q.Where("(name COLLATE public.tr, id) > ( ? COLLATE public.tr, ?)", after.Name, after.ID)
		}
		q = q.Order("name COLLATE public.tr, id")
	} else {
		if after != nil {
			q = q.Where("id < ?", after.ID)
		}
		q = q.Order("id DESC")
	}
	var out []Org
	if err := q.Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list orgs", err)
	}
	return out, nil
}

// InsertCompany implements Store: org_id and org_kind come from the parent
// organizations row (06 H-38).
func (s gormStore) InsertCompany(ctx context.Context, c Company) error {
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO companies (org_id, org_kind, sector_code, size_band, email_domain, hq_city)
		SELECT p.org_id, p.kind, @sector, @size, @domain, @hq
		FROM (SELECT id AS org_id, kind FROM organizations) p WHERE p.org_id = @org_id`,
		map[string]any{"org_id": c.OrgID, "sector": c.SectorCode, "size": c.SizeBand, "domain": c.EmailDomain, "hq": c.HQCity})
	if res.Error != nil {
		return wrap("insert company", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetCompany implements Store.
func (s gormStore) GetCompany(ctx context.Context, orgID uuid.UUID) (Company, error) {
	var c Company
	err := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Take(&c).Error
	switch {
	case db.IsNotFound(err):
		return Company{}, ErrNotFound
	case err != nil:
		return Company{}, wrap("get company", err)
	}
	return c, nil
}

// UpdateCompany implements Store.
func (s gormStore) UpdateCompany(ctx context.Context, c Company) (Company, error) {
	var rows []Company
	err := db.Conn(ctx, s.orm).Raw(`UPDATE companies SET sector_code = @sector, size_band = @size,
		email_domain = @domain, hq_city = @hq
		WHERE org_id = @org_id RETURNING *`,
		map[string]any{"org_id": c.OrgID, "sector": c.SectorCode, "size": c.SizeBand, "domain": c.EmailDomain, "hq": c.HQCity}).
		Scan(&rows).Error
	if err != nil {
		return Company{}, wrap("update company", err)
	}
	if len(rows) == 0 {
		return Company{}, ErrNotFound
	}
	return rows[0], nil
}

// InsertMember implements Store: org_id and org_kind come from the parent
// organizations row (06 H-38); a missing org writes nothing (ErrNotFound).
func (s gormStore) InsertMember(ctx context.Context, m Membership) (Membership, error) {
	var rows []Membership
	err := db.Conn(ctx, s.orm).Raw(`INSERT INTO org_members
		(id, org_id, org_kind, user_id, role, status, source, requested_at, joined_at, ended_at, end_reason, invited_by, approved_by)
		SELECT @id, p.org_id, p.kind, @user_id, @role, @status, @source, @requested_at, @joined_at, @ended_at, @end_reason,
		       @invited_by, @approved_by
		FROM (SELECT id AS org_id, kind FROM organizations) p WHERE p.org_id = @org_id
		RETURNING `+memberCols, map[string]any{
		"id": m.ID, "org_id": m.OrgID, "user_id": m.UserID, "role": string(m.Role), "status": string(m.Status),
		"source": string(m.Source), "requested_at": m.RequestedAt, "joined_at": m.JoinedAt, "ended_at": m.EndedAt,
		"end_reason": m.EndReason, "invited_by": m.InvitedBy, "approved_by": m.ApprovedBy,
	}).Scan(&rows).Error
	if err != nil {
		return Membership{}, wrap("insert member", err)
	}
	if len(rows) == 0 {
		return Membership{}, ErrNotFound
	}
	return rows[0], nil
}

// scoped locks one membership of orgID (ScopeOrg) matching where.
func (s gormStore) scoped(ctx context.Context, op string, orgID uuid.UUID, where string, args ...any) (Membership, error) {
	var m Membership
	err := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Scopes(db.ScopeOrg(orgID)).Where(where, args...).Take(&m).Error
	switch {
	case db.IsNotFound(err):
		return Membership{}, ErrNotFound
	case err != nil:
		return Membership{}, wrap(op, err)
	}
	return m, nil
}

// LockActiveMember implements Store.
func (s gormStore) LockActiveMember(ctx context.Context, orgID, userID uuid.UUID) (Membership, error) {
	return s.scoped(ctx, "lock member", orgID, "user_id = ? AND status = 'active'", userID)
}

// LockOwner implements Store.
func (s gormStore) LockOwner(ctx context.Context, orgID uuid.UUID) (Membership, error) {
	return s.scoped(ctx, "lock owner", orgID, "role = 'company_owner' AND status = 'active'")
}

// EndMember implements Store.
func (s gormStore) EndMember(ctx context.Context, orgID, id uuid.UUID, now time.Time, reason EndReason) (Membership, error) {
	var rows []Membership
	err := db.Conn(ctx, s.orm).Raw(`UPDATE org_members SET status = 'ended', ended_at = @now, end_reason = @reason
		WHERE id = @id AND org_id = @org_id AND status IN ('pending', 'active')
		RETURNING `+memberCols, map[string]any{"now": now, "reason": string(reason), "id": id, "org_id": orgID}).Scan(&rows).Error
	if err != nil {
		return Membership{}, wrap("end member", err)
	}
	if len(rows) == 0 {
		return Membership{}, ErrNotFound
	}
	return rows[0], nil
}

// EndOpenMembers implements Store. The CTE returns the rows as they were
// before the update (their previous status).
func (s gormStore) EndOpenMembers(ctx context.Context, orgID uuid.UUID, now time.Time, reason EndReason) ([]Membership, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	var rows []Membership
	err := db.Conn(ctx, s.orm).Raw(`WITH cur AS (
			SELECT `+memberCols+` FROM org_members
			WHERE org_id = @org_id AND status IN ('pending', 'active') FOR UPDATE
		), upd AS (
			UPDATE org_members m SET status = 'ended', ended_at = @now, end_reason = @reason
			FROM cur WHERE m.id = cur.id
		)
		SELECT * FROM cur ORDER BY id`, map[string]any{"org_id": orgID, "now": now, "reason": string(reason)}).
		Scan(&rows).Error
	if err != nil {
		return nil, wrap("end members", err)
	}
	return rows, nil
}

// MembershipsOfUser implements Store.
//
// cross-org: the KVKK export of one data subject spans every org.
func (s gormStore) MembershipsOfUser(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	var out []Membership
	if err := db.Conn(ctx, s.orm).Where("user_id = ?", userID).Order("id").Find(&out).Error; err != nil {
		return nil, wrap("memberships of user", err)
	}
	return out, nil
}

// OrgsCreatedBy implements Store.
//
// cross-org: the KVKK export of one data subject spans every org.
func (s gormStore) OrgsCreatedBy(ctx context.Context, userID uuid.UUID) ([]Org, error) {
	var out []Org
	if err := db.Conn(ctx, s.orm).Select(orgCols).Where("created_by = ?", userID).Order("id").Find(&out).Error; err != nil {
		return nil, wrap("orgs created by", err)
	}
	return out, nil
}
