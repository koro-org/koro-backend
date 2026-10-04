package students

import (
	"context"
	"database/sql"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

const cvCols = `id, user_id, file_id, is_current, created_at`

func takeCV(op string, q *gorm.DB) (CV, error) {
	var c CV
	err := q.Select(cvCols).Take(&c).Error
	switch {
	case db.IsNotFound(err):
		return CV{}, ErrNotFound
	case err != nil:
		return CV{}, wrap(op, err)
	}
	return c, nil
}

// CVs implements Store.
//
// cross-org: a student's CVs belong to no org (02 §9.6).
func (s gormStore) CVs(ctx context.Context, userID uuid.UUID) ([]CV, error) {
	var out []CV
	err := db.Conn(ctx, s.orm).Select(cvCols).Where("user_id = ?", userID).Order("created_at, id").Find(&out).Error
	if err != nil {
		return nil, wrap("cvs", err)
	}
	return out, nil
}

// GetCV implements Store.
//
// cross-org: a student's CVs belong to no org; the row is addressed by its
// owner too (IDOR).
func (s gormStore) GetCV(ctx context.Context, userID, id uuid.UUID) (CV, error) {
	return takeCV("get cv", db.Conn(ctx, s.orm).Where("id = ? AND user_id = ?", id, userID))
}

// CVByFile implements Store.
//
// cross-org: the CV of a rejected file (file.rejected), whoever owns it.
func (s gormStore) CVByFile(ctx context.Context, fileID uuid.UUID) (CV, error) {
	return takeCV("cv by file", db.Conn(ctx, s.orm).Where("file_id = ?", fileID))
}

// InsertCV implements Store.
//
// cross-org: a student's CVs belong to no org (02 §9.6).
func (s gormStore) InsertCV(ctx context.Context, c CV) (CV, error) {
	var rows []CV
	err := db.Conn(ctx, s.orm).Raw(`INSERT INTO student_cvs (id, user_id, file_id, is_current)
		VALUES ( @id, @user_id, @file_id, false)
		RETURNING `+cvCols, map[string]any{"id": c.ID, "user_id": c.UserID, "file_id": c.FileID}).Scan(&rows).Error
	if err != nil {
		return CV{}, wrap("insert cv", err)
	}
	if len(rows) == 0 {
		return CV{}, ErrNotFound
	}
	return rows[0], nil
}

// SetCurrentCV implements Store. Two statements: the unique index
// student_cvs_current_uq is checked row by row, so the old current CV is
// cleared before the new one is set.
//
// cross-org: a student's CVs belong to no org; the row is addressed by its
// owner too (IDOR).
func (s gormStore) SetCurrentCV(ctx context.Context, userID, id uuid.UUID) (CV, error) {
	var out CV
	err := db.Conn(ctx, s.orm).Transaction(func(tx *gorm.DB) error {
		cur, err := takeCV("set current cv", tx.Where("id = ? AND user_id = ?", id, userID))
		if err != nil {
			return err
		}
		if err := tx.Exec(`UPDATE student_cvs SET is_current = false WHERE user_id = ? AND is_current AND id <> ?`,
			userID, id).Error; err != nil {
			return wrap("set current cv", err)
		}
		if !cur.IsCurrent {
			if err := tx.Exec(`UPDATE student_cvs SET is_current = true WHERE id = ? AND user_id = ?`, id, userID).Error; err != nil {
				return wrap("set current cv", err)
			}
			cur.IsCurrent = true
		}
		out = cur
		return nil
	})
	if err != nil {
		return CV{}, err
	}
	return out, nil
}

// DeleteCV implements Store.
//
// cross-org: a student's CVs belong to no org; the row is addressed by its
// owner too (IDOR).
func (s gormStore) DeleteCV(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Where("id = ? AND user_id = ?", id, userID).Delete(&CV{})
	if res.Error != nil {
		return false, wrap("delete cv", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// CountSegment implements Store through count_segment (02 §9.1, 01 §1.4):
// the k ≥ 10 threshold is applied by the database as well.
//
// cross-org: an audience spans every eligible profile (a count, never a
// person).
func (s gormStore) CountSegment(ctx context.Context, keys []string, dims int16) (int, bool, error) {
	var n sql.NullInt64
	err := db.Conn(ctx, s.orm).Raw(`SELECT count_segment(CAST( @keys AS text[]), CAST( @dims AS smallint))`,
		map[string]any{"keys": db.TextArray(keys), "dims": dims}).Row().Scan(&n)
	if err != nil {
		return 0, false, wrap("count segment", err)
	}
	if !n.Valid {
		return 0, false, nil
	}
	return int(n.Int64), true, nil
}

// AudienceAfter implements Store with count_segment's predicate (02 §9.1):
// the same eligibility, key overlap and dimension count.
//
// cross-org: the audience walk of a dispatch (worker).
func (s gormStore) AudienceAfter(ctx context.Context, keys []string, dims int16, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	var rows []struct {
		UserID uuid.UUID `gorm:"type:uuid;column:user_id"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT sp.user_id FROM student_profiles sp
		WHERE sp.audience_eligible
		  AND sp.audience_keys && CAST( @keys AS text[])
		  AND (SELECT count(DISTINCT split_part(k, ':', 1)) FROM unnest(sp.audience_keys) k
		        WHERE k = ANY (CAST( @keys AS text[]))) = @dims
		  AND sp.user_id > @after
		ORDER BY sp.user_id
		LIMIT @limit`, map[string]any{"keys": db.TextArray(keys), "dims": int(dims), "after": after, "limit": limit}).
		Scan(&rows).Error
	if err != nil {
		return nil, wrap("audience after", err)
	}
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.UserID
	}
	return out, nil
}
