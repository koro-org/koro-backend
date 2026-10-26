package feed

import (
	"context"
	"fmt"
	"strings"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// NewGormStore returns the PostgreSQL store over posts, post_media and
// feed_org_settings (02 §12.1–12.3) and the reader tables (02 §12.4–12.8).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// postColumns are the columns of posts (search_vector is never read).
var postColumns = []string{
	"id", "org_id", "author_org_kind", "author_user_id", "kind", "title", "body_json", "body_text", "schema_version",
	"event_id", "opportunity", "audience_kind", "audience_keys", "audience_dim_count", "audience_snapshot",
	"audience_size_estimate", "commercial_flag", "status", "published_at", "edited_at", "reviewed_by", "reviewed_at",
	"removed_at", "like_count", "save_count", "entitlement_usage_id", "created_at", "updated_at",
}

var (
	// postCols is the select / RETURNING list of posts.
	postCols = strings.Join(postColumns, ", ")
	// prefixed is postCols qualified with the alias p.
	prefixed = "p." + strings.Join(postColumns, ", p.")
)

const mediaCols = `id, post_id, file_id, position, alt_text, width, height, created_at`

// constraints maps the named constraints of 02 §12.1–12.8 onto module
// errors. Unlisted CHECKs are 422 and FKs/uniques 409 (db.Translate).
var constraints = db.Constraints{
	"posts_org_id_author_org_kind_fkey": problem.ErrNotFound,
	"posts_event_id_fkey":               problem.ErrValidation,
	"post_media_post_id_fkey":           problem.ErrNotFound,
	"post_media_file_id_fkey":           problem.ErrValidation,
	// The post or org of a reader row vanished meanwhile (02 §12.4–12.8).
	"post_reactions_post_id_fkey": problem.ErrNotFound,
	"post_saves_post_id_fkey":     problem.ErrNotFound,
	"post_hides_post_id_fkey":     problem.ErrNotFound,
	"org_follows_org_id_fkey":     problem.ErrNotFound,
	"org_mutes_org_id_fkey":       problem.ErrNotFound,
	// The report of a moderation action vanished meanwhile (BE-032).
	"moderation_actions_report_id_fkey": problem.ErrNotFound,
	// A trust change of an org that does not exist (BE-032).
	"feed_org_settings_org_id_fkey": problem.ErrNotFound,
}

func wrap(op string, err error) error {
	return fmt.Errorf("feed: %s: %w", op, constraints.Translate(err))
}

// take reads one row of q into T or ErrNotFound.
func take[T any](op, cols string, q *gorm.DB) (T, error) {
	var v T
	err := q.Select(cols).Take(&v).Error
	switch {
	case db.IsNotFound(err):
		return v, ErrNotFound
	case err != nil:
		return v, wrap(op, err)
	}
	return v, nil
}

// returning runs a raw INSERT/UPDATE … RETURNING and returns its row or
// ErrNotFound when nothing was written.
func returning[T any](ctx context.Context, orm *gorm.DB, op, sql string, args map[string]any) (T, error) {
	var rows []T
	if err := db.Conn(ctx, orm).Raw(sql, args).Scan(&rows).Error; err != nil {
		var zero T
		return zero, wrap(op, err)
	}
	if len(rows) == 0 {
		var zero T
		return zero, ErrNotFound
	}
	return rows[0], nil
}

// postArgs are the named arguments of the insert and update statements.
func postArgs(p Post) map[string]any {
	keys := p.AudienceKeys
	if keys == nil {
		keys = db.TextArray{}
	}
	return map[string]any{
		"id": p.ID, "org_id": p.OrgID, "author_org_kind": string(p.AuthorOrgKind), "author_user_id": p.AuthorUserID,
		"kind": string(p.Kind), "title": p.Title, "body_json": p.BodyJSON, "body_text": p.BodyText,
		"schema_version": p.SchemaVersion, "event_id": p.EventID, "opportunity": p.Opportunity,
		"audience_kind": string(p.AudienceKind), "audience_keys": keys, "audience_dim_count": p.AudienceDimCount,
		"audience_snapshot": p.AudienceSnapshot, "audience_size_estimate": p.AudienceSizeEstimate,
		"commercial_flag": p.CommercialFlag, "status": string(p.Status), "published_at": p.PublishedAt,
		"edited_at": p.EditedAt, "reviewed_by": p.ReviewedBy, "reviewed_at": p.ReviewedAt, "removed_at": p.RemovedAt,
		"entitlement_usage_id": p.EntitlementUsageID,
	}
}

// InsertPost implements Store.
//
// cross-org: a new post of the org the service authorised (authz.Can on
// the org's management surface); the row is the root of its own scope.
func (s gormStore) InsertPost(ctx context.Context, p Post) (Post, error) {
	if p.ID == uuid.Nil() {
		return Post{}, fmt.Errorf("feed: insert post: %w", problem.ErrInternal)
	}
	if p.SchemaVersion == 0 {
		p.SchemaVersion = 1
	}
	return returning[Post](ctx, s.orm, "insert post", `INSERT INTO posts
		(id, org_id, author_org_kind, author_user_id, kind, title, body_json, body_text, schema_version, event_id,
		 opportunity, audience_kind, audience_keys, audience_dim_count, audience_snapshot, audience_size_estimate,
		 commercial_flag, status, published_at, edited_at, reviewed_by, reviewed_at, removed_at, entitlement_usage_id)
		VALUES ( @id, @org_id, @author_org_kind, @author_user_id, @kind, @title, @body_json, @body_text, @schema_version,
		 @event_id, @opportunity, @audience_kind, @audience_keys, @audience_dim_count, @audience_snapshot,
		 @audience_size_estimate, @commercial_flag, @status, @published_at, @edited_at, @reviewed_by, @reviewed_at,
		 @removed_at, @entitlement_usage_id)
		RETURNING `+postCols, postArgs(p))
}

// GetPost implements Store.
//
// cross-org: a post is addressed by its id; the service takes org_id from
// the row (06 H-38) and authz.Can decides its visibility first.
func (s gormStore) GetPost(ctx context.Context, id uuid.UUID) (Post, error) {
	return take[Post]("get post", postCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// LockPost implements Store.
//
// cross-org: as GetPost, with the row locked.
func (s gormStore) LockPost(ctx context.Context, id uuid.UUID) (Post, error) {
	q := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	return take[Post]("lock post", postCols, q.Where("id = ?", id))
}

// UpdatePost implements Store. The counters (like_count, save_count) and
// the author are not written: BE-029 and the reconciliation job own them.
func (s gormStore) UpdatePost(ctx context.Context, p Post) (Post, error) {
	return returning[Post](ctx, s.orm, "update post", `UPDATE posts SET
		kind = @kind, title = @title, body_json = @body_json, body_text = @body_text, schema_version = @schema_version,
		event_id = @event_id, opportunity = @opportunity, audience_kind = @audience_kind, audience_keys = @audience_keys,
		audience_dim_count = @audience_dim_count, audience_snapshot = @audience_snapshot,
		audience_size_estimate = @audience_size_estimate, commercial_flag = @commercial_flag, status = @status,
		published_at = @published_at, edited_at = @edited_at, reviewed_by = @reviewed_by, reviewed_at = @reviewed_at,
		removed_at = @removed_at, entitlement_usage_id = @entitlement_usage_id
		WHERE id = @id AND org_id = @org_id RETURNING `+postCols, postArgs(p))
}

// DeletePost implements Store (post_media, post_hides, post_reactions and
// post_saves go with the row: ON DELETE CASCADE).
func (s gormStore) DeletePost(ctx context.Context, orgID, id uuid.UUID) error {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM posts WHERE id = @id AND org_id = @org_id
		AND status IN ('draft', 'pending_review')`, map[string]any{"id": id, "org_id": orgID})
	switch {
	case res.Error != nil:
		return wrap("delete post", res.Error)
	case res.RowsAffected == 0:
		return ErrNotFound
	}
	return nil
}

// ReplaceMedia implements Store: the rows are written only through a posts
// row of orgID (06 H-38: post_media has no org_id of its own).
func (s gormStore) ReplaceMedia(ctx context.Context, orgID, id uuid.UUID, media []Media) error {
	conn := db.Conn(ctx, s.orm)
	args := map[string]any{"post_id": id, "org_id": orgID}
	var n int64
	if err := conn.Raw(`SELECT count(*) FROM posts WHERE id = @post_id AND org_id = @org_id`, args).Scan(&n).Error; err != nil {
		return wrap("replace media", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := conn.Exec(`DELETE FROM post_media m USING posts p
		WHERE m.post_id = p.id AND p.id = @post_id AND p.org_id = @org_id`, args).Error; err != nil {
		return wrap("replace media", err)
	}
	for i, m := range media {
		a := map[string]any{
			"id": m.ID, "post_id": id, "org_id": orgID, "file_id": m.FileID, "position": int16(i), "alt_text": m.AltText,
			"width": m.Width, "height": m.Height,
		}
		res := conn.Exec(`INSERT INTO post_media (id, post_id, file_id, position, alt_text, width, height)
			SELECT @id, p.id, @file_id, @position, @alt_text, @width, @height FROM posts p
			WHERE p.id = @post_id AND p.org_id = @org_id`, a)
		switch {
		case res.Error != nil:
			return wrap("replace media", res.Error)
		case res.RowsAffected == 0:
			return ErrNotFound
		}
	}
	return nil
}

// MediaOf implements Store.
//
// cross-org: the media of posts the service has already authorised (a
// page of the feed or one post).
func (s gormStore) MediaOf(ctx context.Context, postIDs []uuid.UUID) (map[uuid.UUID][]Media, error) {
	out := map[uuid.UUID][]Media{}
	if len(postIDs) == 0 {
		return out, nil
	}
	var rows []Media
	if err := db.Conn(ctx, s.orm).Select(mediaCols).Where("post_id IN ?", postIDs).
		Order("post_id, position").Find(&rows).Error; err != nil {
		return nil, wrap("media of", err)
	}
	for _, m := range rows {
		out[m.PostID] = append(out[m.PostID], m)
	}
	return out, nil
}

// readerConds are the visibility conditions of 03 §3.6 on published posts
// (alias p): the audience keys (02 §9.1: the GIN overlap pre-filter, then
// every dimension of the post matched), no commercial post for a minor,
// a followers post only for a follower or a member of the author org, and
// for an anonymous reader public non-commercial posts only (T-57).
func readerConds(r Reader) (string, []any) {
	if r.All {
		return "TRUE", nil
	}
	if r.UserID == nil {
		return "p.audience_kind = 'public' AND NOT p.commercial_flag", nil
	}
	keys := db.TextArray(r.Keys)
	if keys == nil {
		keys = db.TextArray{}
	}
	conds := []string{
		"p.audience_keys && ?::text[]",
		`(SELECT count(DISTINCT split_part(k, ':', 1)) FROM unnest(p.audience_keys) AS k
			WHERE k = ANY( ?::text[])) = p.audience_dim_count`,
		`(p.audience_kind <> 'followers' OR p.org_id = ANY( ?::uuid[])
			OR EXISTS (SELECT 1 FROM org_follows f WHERE f.user_id = ? AND f.org_id = p.org_id))`,
	}
	args := []any{keys, keys, db.UUIDArray(r.MemberOrgs), *r.UserID}
	if r.Minor {
		conds = append(conds, "NOT p.commercial_flag")
	}
	return strings.Join(conds, " AND "), args
}

// ListFeed implements Store: fan-out on read (R-003 §1.5.2) over
// posts_feed_idx / posts_audience_idx.
//
// cross-org: the feed spans every author org; the reader's visibility
// (03 §3.6) is applied in the query.
func (s gormStore) ListFeed(ctx context.Context, f FeedFilter, after *After, limit int) ([]Post, error) {
	q := db.Conn(ctx, s.orm).Table("posts AS p").Select(prefixed).Where("p.status = 'published'")
	cond, args := readerConds(f.Reader)
	q = q.Where(cond, args...)
	if u := f.UserID; u != nil {
		q = q.Where("NOT EXISTS (SELECT 1 FROM org_mutes m WHERE m.user_id = ? AND m.org_id = p.org_id)", *u).
			Where("NOT EXISTS (SELECT 1 FROM post_hides h WHERE h.user_id = ? AND h.post_id = p.id)", *u)
		if f.Following {
			q = q.Where(`(p.org_id = ANY( ?::uuid[])
				OR EXISTS (SELECT 1 FROM org_follows f WHERE f.user_id = ? AND f.org_id = p.org_id))`,
				db.UUIDArray(f.MemberOrgs), *u)
		}
	} else if f.Following {
		return []Post{}, nil
	}
	if f.Kind != "" {
		q = q.Where("p.kind = ?", string(f.Kind))
	}
	if after != nil {
		q = q.Where("(p.published_at, p.id) < ( ?, ?)", after.At, after.ID)
	}
	var out []Post
	if err := q.Order("p.published_at DESC, p.id DESC").Limit(limit).Scan(&out).Error; err != nil {
		return nil, wrap("list feed", err)
	}
	return out, nil
}

// ListOrgPosts implements Store over posts_author_idx.
func (s gormStore) ListOrgPosts(ctx context.Context, orgID uuid.UUID, r Reader, after *After, limit int) ([]Post, error) {
	q := db.Conn(ctx, s.orm).Table("posts AS p").Select(prefixed).Scopes(scoped(orgID)).
		Where("p.status = 'published'")
	cond, args := readerConds(r)
	q = q.Where(cond, args...)
	if after != nil {
		q = q.Where("(p.published_at, p.id) < ( ?, ?)", after.At, after.ID)
	}
	var out []Post
	if err := q.Order("p.published_at DESC, p.id DESC").Limit(limit).Scan(&out).Error; err != nil {
		return nil, wrap("list org posts", err)
	}
	return out, nil
}

// scoped is the org scope of the aliased posts queries (05 §5.5).
func scoped(orgID uuid.UUID) func(*gorm.DB) *gorm.DB {
	return func(q *gorm.DB) *gorm.DB { return q.Where("p.org_id = ?", orgID) }
}

// ListOrgManage implements Store.
func (s gormStore) ListOrgManage(ctx context.Context, orgID uuid.UUID, f ManageFilter, after *uuid.UUID, limit int) ([]Post, error) {
	q := db.Conn(ctx, s.orm).Table("posts AS p").Select(prefixed).Scopes(scoped(orgID))
	if len(f.Statuses) > 0 {
		ss := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			ss[i] = string(st)
		}
		q = q.Where("p.status IN ?", ss)
	}
	if f.Kind != "" {
		q = q.Where("p.kind = ?", string(f.Kind))
	}
	if after != nil {
		q = q.Where("p.id < ?", *after)
	}
	var out []Post
	if err := q.Order("p.id DESC").Limit(limit).Scan(&out).Error; err != nil {
		return nil, wrap("list org manage", err)
	}
	return out, nil
}

// OrgSettings implements Store.
func (s gormStore) OrgSettings(ctx context.Context, orgID uuid.UUID) (OrgSettings, bool, error) {
	var rows []OrgSettings
	if err := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Limit(1).Find(&rows).Error; err != nil {
		return OrgSettings{}, false, wrap("org settings", err)
	}
	if len(rows) == 0 {
		return OrgSettings{}, false, nil
	}
	return rows[0], true, nil
}

// Follows implements Store.
//
// cross-org: a reader's own follow row (personal data of the reader).
func (s gormStore) Follows(ctx context.Context, userID, orgID uuid.UUID) (bool, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Table("org_follows").Where("user_id = ? AND org_id = ?", userID, orgID).
		Count(&n).Error; err != nil {
		return false, wrap("follows", err)
	}
	return n > 0, nil
}

// ViewerState implements Store in one statement.
//
// cross-org: the reader's own rows (reactions, saves, follows).
func (s gormStore) ViewerState(ctx context.Context, userID uuid.UUID, postIDs, orgIDs []uuid.UUID) (ViewerState, error) {
	out := newViewerState()
	if len(postIDs) == 0 && len(orgIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		K  string    `gorm:"column:k"`
		ID uuid.UUID `gorm:"column:id"`
	}
	err := db.Conn(ctx, s.orm).Raw(`
		SELECT 'like' AS k, post_id AS id FROM post_reactions WHERE user_id = @user AND post_id = ANY(CAST( @posts AS uuid[]))
		UNION ALL
		SELECT 'save', post_id FROM post_saves WHERE user_id = @user AND post_id = ANY(CAST( @posts AS uuid[]))
		UNION ALL
		SELECT 'follow', org_id FROM org_follows WHERE user_id = @user AND org_id = ANY(CAST( @orgs AS uuid[]))
		UNION ALL
		SELECT 'mute', org_id FROM org_mutes WHERE user_id = @user AND org_id = ANY(CAST( @orgs AS uuid[]))`,
		map[string]any{"user": userID, "posts": db.UUIDArray(postIDs), "orgs": db.UUIDArray(orgIDs)}).Scan(&rows).Error
	if err != nil {
		return ViewerState{}, wrap("viewer state", err)
	}
	for _, r := range rows {
		switch r.K {
		case "like":
			out.Liked[r.ID] = true
		case "save":
			out.Saved[r.ID] = true
		case "follow":
			out.Following[r.ID] = true
		case "mute":
			out.Muted[r.ID] = true
		}
	}
	return out, nil
}

// PostsOfAuthor implements Store.
//
// cross-org: the data subject's own posts in every org (KVKK export).
func (s gormStore) PostsOfAuthor(ctx context.Context, userID uuid.UUID) ([]Post, error) {
	var out []Post
	if err := db.Conn(ctx, s.orm).Select(postCols).Where("author_user_id = ?", userID).Order("id").
		Find(&out).Error; err != nil {
		return nil, wrap("posts of author", err)
	}
	return out, nil
}
