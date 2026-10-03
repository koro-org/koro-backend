package files

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// NewGormStore returns the PostgreSQL store over the files table (02 §3.1).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// mutableColumns are written by Update (id and created_at never change,
// updated_at belongs to the files_touch_updated_at trigger).
var mutableColumns = []string{
	"purpose", "owner_user_id", "org_id", "attached_module", "attached_entity_id", "attached_at",
	"bucket", "storage_key", "original_name", "declared_mime", "detected_mime", "size_bytes", "sha256",
	"status", "scan_status", "scanned_at", "uploaded_by",
}

// cross-org: files rows are addressed by ID; org_id is optional metadata and
// access is decided by the owning module (01 §4.2, 03 §3.6).
func (s gormStore) Insert(ctx context.Context, f File) error {
	if err := db.Conn(ctx, s.orm).Create(&f).Error; err != nil {
		return fmt.Errorf("files: insert: %w", db.Translate(err))
	}
	return nil
}

// cross-org: see Insert.
func (s gormStore) Get(ctx context.Context, id uuid.UUID) (File, error) {
	var f File
	if err := db.Conn(ctx, s.orm).Where("id = ?", id).Take(&f).Error; err != nil {
		if db.IsNotFound(err) {
			return File{}, ErrNotFound
		}
		return File{}, fmt.Errorf("files: get: %w", err)
	}
	return f, nil
}

// cross-org: see Insert.
func (s gormStore) Lock(ctx context.Context, id uuid.UUID) (File, error) {
	var f File
	err := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("id = ?", id).Take(&f).Error
	if err != nil {
		if db.IsNotFound(err) {
			return File{}, ErrNotFound
		}
		return File{}, fmt.Errorf("files: lock: %w", err)
	}
	return f, nil
}

// cross-org: see Insert.
func (s gormStore) Update(ctx context.Context, f File) error {
	res := db.Conn(ctx, s.orm).Model(&File{ID: f.ID}).Select(mutableColumns).Updates(&f)
	if res.Error != nil {
		return fmt.Errorf("files: update: %w", db.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// cross-org: see Insert.
func (s gormStore) Delete(ctx context.Context, id uuid.UUID) error {
	err := db.Conn(ctx, s.orm).Where("id = ?", id).Delete(&File{}).Error
	switch {
	case err == nil:
		return nil
	case db.IsForeignKey(err):
		return ErrReferenced.Wrap(err)
	}
	return fmt.Errorf("files: delete: %w", err)
}

// cross-org: see Insert (a platform job over every org).
func (s gormStore) Orphans(ctx context.Context, before time.Time, after uuid.UUID, limit int) ([]File, error) {
	var out []File
	err := db.Conn(ctx, s.orm).
		Where("attached_at IS NULL AND created_at < ? AND id > ?", before, after).
		Order("id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("files: orphans: %w", err)
	}
	return out, nil
}

// cross-org: see Insert (a platform job over every org).
func (s gormStore) PendingScans(ctx context.Context, before time.Time, after uuid.UUID, limit int) ([]File, error) {
	var out []File
	err := db.Conn(ctx, s.orm).
		Where("scan_status = 'pending' AND status = 'active' AND updated_at < ? AND id > ?", before, after).
		Order("id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("files: pending scans: %w", err)
	}
	return out, nil
}

// cross-org: the data subject's own files across every org (KVKK).
func (s gormStore) UnattachedOf(ctx context.Context, userID uuid.UUID) ([]File, error) {
	var out []File
	err := db.Conn(ctx, s.orm).Where("owner_user_id = ? AND attached_at IS NULL", userID).Order("id").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("files: unattached of user: %w", err)
	}
	return out, nil
}
