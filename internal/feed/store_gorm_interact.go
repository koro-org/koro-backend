package feed

import (
	"context"
	"slices"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// The reader tables of 02 §12.4–12.8 (BE-029). Each table is personal data
// of the reader; no org scope applies (02 "Org kapsamı: yok"): every
// statement is keyed by the reader's user_id, and the post or org ids come
// from rows the service has already authorised.

// counted runs a statement whose single row is a count and reports
// whether it is positive.
func counted(ctx context.Context, orm *gorm.DB, op, sql string, args map[string]any) (bool, error) {
	var n int64
	if err := db.Conn(ctx, orm).Raw(sql, args).Scan(&n).Error; err != nil {
		return false, wrap(op, err)
	}
	return n > 0, nil
}

// counter statements: the row change and the counter change are one
// statement (02 §12.2 like_count/save_count). ON CONFLICT DO NOTHING makes
// a repeated or concurrent insert a no-op whose counter stays (the second
// inserter waits for the first one's commit on the primary key); the
// decrement happens only for a deleted row and never below zero. The
// posts_touch_updated_at trigger skips counter-only updates (000066), so
// the ETag of a post does not move with its likes.
const (
	addLikeSQL = `WITH ins AS (
		INSERT INTO post_reactions (post_id, user_id) VALUES ( @post, @user) ON CONFLICT DO NOTHING RETURNING post_id),
	upd AS (UPDATE posts SET like_count = like_count + 1 WHERE id IN (SELECT post_id FROM ins))
	SELECT count(*) FROM ins`
	removeLikeSQL = `WITH del AS (
		DELETE FROM post_reactions WHERE post_id = @post AND user_id = @user RETURNING post_id),
	upd AS (UPDATE posts SET like_count = like_count - 1 WHERE id IN (SELECT post_id FROM del) AND like_count > 0)
	SELECT count(*) FROM del`
	addSaveSQL = `WITH ins AS (
		INSERT INTO post_saves (user_id, post_id) VALUES ( @user, @post) ON CONFLICT DO NOTHING RETURNING post_id),
	upd AS (UPDATE posts SET save_count = save_count + 1 WHERE id IN (SELECT post_id FROM ins))
	SELECT count(*) FROM ins`
	removeSaveSQL = `WITH del AS (
		DELETE FROM post_saves WHERE user_id = @user AND post_id = @post RETURNING post_id),
	upd AS (UPDATE posts SET save_count = save_count - 1 WHERE id IN (SELECT post_id FROM del) AND save_count > 0)
	SELECT count(*) FROM del`
)

func pair(userID, postID uuid.UUID) map[string]any {
	return map[string]any{"user": userID, "post": postID}
}

// AddLike implements Store.
//
// cross-org: the reader's own row on a post the service authorised.
func (s gormStore) AddLike(ctx context.Context, userID, postID uuid.UUID) (bool, error) {
	return counted(ctx, s.orm, "add like", addLikeSQL, pair(userID, postID))
}

// RemoveLike implements Store.
//
// cross-org: the reader's own row.
func (s gormStore) RemoveLike(ctx context.Context, userID, postID uuid.UUID) (bool, error) {
	return counted(ctx, s.orm, "remove like", removeLikeSQL, pair(userID, postID))
}

// WithdrawnLikes implements Store.
//
// cross-org: the pairs come from talent's ledger (reconciliation of a
// closed period, BE-090); only the withdrawn pairs leave.
func (s gormStore) WithdrawnLikes(ctx context.Context, likes []LikeKey) (map[LikeKey]bool, error) {
	out := make(map[LikeKey]bool, len(likes))
	if len(likes) == 0 {
		return out, nil
	}
	users, posts := make([]uuid.UUID, len(likes)), make([]uuid.UUID, len(likes))
	for i, l := range likes {
		users[i], posts[i] = l.UserID, l.PostID
	}
	var rows []struct {
		UserID uuid.UUID `gorm:"column:user_id"`
		PostID uuid.UUID `gorm:"column:post_id"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT DISTINCT x.user_id, x.post_id
		FROM unnest(CAST( ? AS uuid[]), CAST( ? AS uuid[])) AS x (user_id, post_id)
		WHERE EXISTS (SELECT 1 FROM posts p WHERE p.id = x.post_id)
		  AND NOT EXISTS (SELECT 1 FROM post_reactions r WHERE r.post_id = x.post_id AND r.user_id = x.user_id)`,
		db.UUIDArray(users), db.UUIDArray(posts)).Scan(&rows).Error
	if err != nil {
		return nil, wrap("withdrawn likes", err)
	}
	for _, r := range rows {
		out[LikeKey{UserID: r.UserID, PostID: r.PostID}] = true
	}
	return out, nil
}

// AddSave implements Store.
//
// cross-org: the reader's own row on a post the service authorised.
func (s gormStore) AddSave(ctx context.Context, userID, postID uuid.UUID) (bool, error) {
	return counted(ctx, s.orm, "add save", addSaveSQL, pair(userID, postID))
}

// RemoveSave implements Store.
//
// cross-org: the reader's own row.
func (s gormStore) RemoveSave(ctx context.Context, userID, postID uuid.UUID) (bool, error) {
	return counted(ctx, s.orm, "remove save", removeSaveSQL, pair(userID, postID))
}

// AddHide implements Store.
//
// cross-org: the reader's own row on a post the service authorised.
func (s gormStore) AddHide(ctx context.Context, userID, postID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO post_hides (user_id, post_id) VALUES ( @user, @post)
		ON CONFLICT DO NOTHING`, pair(userID, postID))
	if res.Error != nil {
		return false, wrap("add hide", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// RemoveHide implements Store.
//
// cross-org: the reader's own row.
func (s gormStore) RemoveHide(ctx context.Context, userID, postID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM post_hides WHERE user_id = @user AND post_id = @post`, pair(userID, postID))
	if res.Error != nil {
		return false, wrap("remove hide", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// listMarked lists the posts of the reader's rows in table (post_saves or
// post_hides: a constant of the caller, never a request value)
// with the visibility of the feed (readerConds), so a post that was
// removed, hidden by a moderator or moved out of the reader's audience is
// skipped and the page stays full. The (user_id, post_id) primary key
// finds the reader's rows; a reader holds few, so they are sorted in
// place (no extra index).
//
// cross-org: the reader's own rows, filtered by 03 §3.6.
func (s gormStore) listMarked(ctx context.Context, table string, r Reader, after *After, limit int) ([]MarkedPost, error) {
	if r.UserID == nil {
		return []MarkedPost{}, nil
	}
	q := db.Conn(ctx, s.orm).Table(table+" AS m").Joins("JOIN posts AS p ON p.id = m.post_id").
		Select(prefixed+", m.created_at AS marked_at").
		Where("m.user_id = ?", *r.UserID).Where("p.status = 'published'")
	cond, args := readerConds(r)
	q = q.Where(cond, args...)
	if after != nil {
		q = q.Where("(m.created_at, m.post_id) < ( ?, ?)", after.At, after.ID)
	}
	var out []MarkedPost
	if err := q.Order("m.created_at DESC, m.post_id DESC").Limit(limit).Scan(&out).Error; err != nil {
		return nil, wrap("list "+table, err)
	}
	if out == nil {
		out = []MarkedPost{}
	}
	return out, nil
}

// ListSaved implements Store (listMarked over post_saves).
//
// cross-org: the reader's own saves, filtered by 03 §3.6 (listMarked).
func (s gormStore) ListSaved(ctx context.Context, r Reader, after *After, limit int) ([]MarkedPost, error) {
	return s.listMarked(ctx, "post_saves", r, after, limit)
}

// ListHidden implements Store (listMarked over post_hides).
//
// cross-org: the reader's own hides, filtered by 03 §3.6 (listMarked).
func (s gormStore) ListHidden(ctx context.Context, r Reader, after *After, limit int) ([]MarkedPost, error) {
	return s.listMarked(ctx, "post_hides", r, after, limit)
}

// FollowManual implements Store. xmax = 0 marks a row the statement
// inserted (not one it updated).
//
// cross-org: the reader's own follow of an org the service authorised.
func (s gormStore) FollowManual(ctx context.Context, userID, orgID uuid.UUID, notify *bool) (bool, error) {
	return counted(ctx, s.orm, "follow", `INSERT INTO org_follows (user_id, org_id, source, notify)
		VALUES ( @user, @org, 'manual', coalesce(CAST( @notify AS boolean), false))
		ON CONFLICT (user_id, org_id) DO UPDATE
		SET source = 'manual', notify = coalesce(CAST( @notify AS boolean), org_follows.notify)
		RETURNING CASE WHEN xmax = 0 THEN 1 ELSE 0 END`,
		map[string]any{"user": userID, "org": orgID, "notify": notify})
}

// AddAutoFollow implements Store.
//
// cross-org: the member's own follow (membership.started).
func (s gormStore) AddAutoFollow(ctx context.Context, userID, orgID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO org_follows (user_id, org_id, source)
		VALUES ( @user, @org, 'auto_membership') ON CONFLICT DO NOTHING`, map[string]any{"user": userID, "org": orgID})
	if res.Error != nil {
		return false, wrap("add auto follow", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// RemoveAutoFollow implements Store.
//
// cross-org: the member's own follow (membership.ended).
func (s gormStore) RemoveAutoFollow(ctx context.Context, userID, orgID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM org_follows WHERE user_id = ? AND org_id = ? AND source = 'auto_membership'`,
		userID, orgID)
	if res.Error != nil {
		return false, wrap("remove auto follow", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// RemoveAutoFollowsOf implements Store.
//
// cross-org: the automatic follows of one org (club.sensitivity_changed).
func (s gormStore) RemoveAutoFollowsOf(ctx context.Context, orgID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM org_follows WHERE org_id = ? AND source = 'auto_membership'`, orgID)
	if res.Error != nil {
		return 0, wrap("remove auto follows of", res.Error)
	}
	return int(res.RowsAffected), nil
}

// RemoveFollow implements Store.
//
// cross-org: the reader's own follow.
func (s gormStore) RemoveFollow(ctx context.Context, userID, orgID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM org_follows WHERE user_id = ? AND org_id = ?`, userID, orgID)
	if res.Error != nil {
		return false, wrap("remove follow", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// ListFollows implements Store.
//
// cross-org: the reader's own follows.
func (s gormStore) ListFollows(ctx context.Context, userID uuid.UUID, after *After, limit int) ([]Follow, error) {
	q := db.Conn(ctx, s.orm).Table("org_follows").Select("user_id, org_id, source, notify, created_at").
		Where("user_id = ?", userID)
	if after != nil {
		q = q.Where("(created_at, org_id) < ( ?, ?)", after.At, after.ID)
	}
	out := []Follow{}
	if err := q.Order("created_at DESC, org_id DESC").Limit(limit).Scan(&out).Error; err != nil {
		return nil, wrap("list follows", err)
	}
	return out, nil
}

// AddMute implements Store.
//
// cross-org: the reader's own mute of an org the service authorised.
func (s gormStore) AddMute(ctx context.Context, userID, orgID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO org_mutes (user_id, org_id) VALUES ( @user, @org) ON CONFLICT DO NOTHING`,
		map[string]any{"user": userID, "org": orgID})
	if res.Error != nil {
		return false, wrap("add mute", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// RemoveMute implements Store.
//
// cross-org: the reader's own mute.
func (s gormStore) RemoveMute(ctx context.Context, userID, orgID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM org_mutes WHERE user_id = ? AND org_id = ?`, userID, orgID)
	if res.Error != nil {
		return false, wrap("remove mute", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// ListMutes implements Store.
//
// cross-org: the reader's own mutes.
func (s gormStore) ListMutes(ctx context.Context, userID uuid.UUID, after *After, limit int) ([]Mute, error) {
	q := db.Conn(ctx, s.orm).Table("org_mutes").Select("user_id, org_id, created_at").Where("user_id = ?", userID)
	if after != nil {
		q = q.Where("(created_at, org_id) < ( ?, ?)", after.At, after.ID)
	}
	out := []Mute{}
	if err := q.Order("created_at DESC, org_id DESC").Limit(limit).Scan(&out).Error; err != nil {
		return nil, wrap("list mutes", err)
	}
	return out, nil
}

// reconcileBatch is the number of drifted posts locked and corrected in one
// transaction.
const reconcileBatch = 500

// driftSQL finds the posts whose counters differ from their rows. It reads
// a snapshot: a candidate is only corrected after its row is locked.
const driftSQL = `SELECT p.id FROM posts p
	LEFT JOIN (SELECT post_id, count(*) AS n FROM post_reactions GROUP BY post_id) l ON l.post_id = p.id
	LEFT JOIN (SELECT post_id, count(*) AS n FROM post_saves GROUP BY post_id) s ON s.post_id = p.id
	WHERE p.like_count <> coalesce(l.n, 0) OR p.save_count <> coalesce(s.n, 0)
	ORDER BY p.id`

// fixSQL sets the counters of the locked candidates from a fresh snapshot
// (a new statement after the lock: every like committed before the lock is
// counted; a like waiting on the lock adds itself after the commit).
const fixSQL = `UPDATE posts p SET
	like_count = (SELECT count(*) FROM post_reactions r WHERE r.post_id = p.id),
	save_count = (SELECT count(*) FROM post_saves s WHERE s.post_id = p.id)
	WHERE p.id IN ?
	  AND (p.like_count <> (SELECT count(*) FROM post_reactions r WHERE r.post_id = p.id)
	    OR p.save_count <> (SELECT count(*) FROM post_saves s WHERE s.post_id = p.id))`

// ReconcileCounters implements Store.
//
// cross-org: a maintenance job over every post's counters.
func (s gormStore) ReconcileCounters(ctx context.Context) (int, error) {
	var ids []uuid.UUID
	if err := db.Conn(ctx, s.orm).Raw(driftSQL).Scan(&ids).Error; err != nil {
		return 0, wrap("reconcile counters", err)
	}
	fixed := 0
	for batch := range slices.Chunk(ids, reconcileBatch) {
		err := db.Conn(ctx, s.orm).Transaction(func(tx *gorm.DB) error {
			var locked []uuid.UUID
			if err := tx.Table("posts").Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
				Where("id IN ?", batch).Order("id").Pluck("id", &locked).Error; err != nil {
				return err
			}
			if len(locked) == 0 {
				return nil
			}
			res := tx.Exec(fixSQL, locked)
			fixed += int(res.RowsAffected)
			return res.Error
		})
		if err != nil {
			return fixed, wrap("reconcile counters", err)
		}
	}
	return fixed, nil
}

// InteractionsOf implements Store.
//
// cross-org: the data subject's own rows (KVKK export).
func (s gormStore) InteractionsOf(ctx context.Context, userID uuid.UUID) (Interactions, error) {
	conn := db.Conn(ctx, s.orm)
	out := Interactions{Likes: []Interaction{}, Saves: []Interaction{}, Hides: []Interaction{}, Follows: []Follow{}, Mutes: []Mute{}}
	for _, q := range []struct {
		table string
		dst   *[]Interaction
	}{{"post_reactions", &out.Likes}, {"post_saves", &out.Saves}, {"post_hides", &out.Hides}} {
		if err := conn.Table(q.table).Select("post_id, created_at").Where("user_id = ?", userID).
			Order("created_at, post_id").Scan(q.dst).Error; err != nil {
			return Interactions{}, wrap("interactions of", err)
		}
	}
	if err := conn.Table("org_follows").Select("user_id, org_id, source, notify, created_at").Where("user_id = ?", userID).
		Order("created_at, org_id").Scan(&out.Follows).Error; err != nil {
		return Interactions{}, wrap("interactions of", err)
	}
	if err := conn.Table("org_mutes").Select("user_id, org_id, created_at").Where("user_id = ?", userID).
		Order("created_at, org_id").Scan(&out.Mutes).Error; err != nil {
		return Interactions{}, wrap("interactions of", err)
	}
	return out, nil
}

// EraseInteractions implements Store in the transaction carried by ctx.
//
// cross-org: the data subject's own rows (KVKK erase).
func (s gormStore) EraseInteractions(ctx context.Context, userID uuid.UUID) (int, error) {
	conn := db.Conn(ctx, s.orm)
	total := 0
	for _, sql := range []string{
		`WITH del AS (DELETE FROM post_reactions WHERE user_id = @user RETURNING post_id),
		upd AS (UPDATE posts p SET like_count = p.like_count - 1 FROM del WHERE p.id = del.post_id AND p.like_count > 0)
		SELECT count(*) FROM del`,
		`WITH del AS (DELETE FROM post_saves WHERE user_id = @user RETURNING post_id),
		upd AS (UPDATE posts p SET save_count = p.save_count - 1 FROM del WHERE p.id = del.post_id AND p.save_count > 0)
		SELECT count(*) FROM del`,
		`WITH del AS (DELETE FROM post_hides WHERE user_id = @user RETURNING 1) SELECT count(*) FROM del`,
		`WITH del AS (DELETE FROM org_follows WHERE user_id = @user RETURNING 1) SELECT count(*) FROM del`,
		`WITH del AS (DELETE FROM org_mutes WHERE user_id = @user RETURNING 1) SELECT count(*) FROM del`,
	} {
		var n int64
		if err := conn.Raw(sql, map[string]any{"user": userID}).Scan(&n).Error; err != nil {
			return total, wrap("erase interactions", err)
		}
		total += int(n)
	}
	return total, nil
}
