package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
)

// Row is one row of the outbox table (02 §1.1).
type Row struct {
	ID            uuid.UUID       `gorm:"type:uuid;primaryKey"`
	Topic         string          `gorm:"column:topic"`
	Version       int16           `gorm:"column:version"`
	AggregateType string          `gorm:"column:aggregate_type"`
	AggregateID   uuid.UUID       `gorm:"type:uuid;column:aggregate_id"`
	Payload       json.RawMessage `gorm:"type:jsonb;column:payload"`
	ActorUserID   *uuid.UUID      `gorm:"type:uuid;column:actor_user_id"`
	RequestID     *string         `gorm:"column:request_id"`
	OccurredAt    time.Time       `gorm:"column:occurred_at"`
	AvailableAt   time.Time       `gorm:"column:available_at"`
	PublishedAt   *time.Time      `gorm:"column:published_at"`
	Attempts      int             `gorm:"column:attempts"`
	LastError     *string         `gorm:"column:last_error"`
}

// TableName implements gorm's Tabler.
func (Row) TableName() string { return "outbox" }

// Envelope returns the event of the row.
func (r Row) Envelope() domainevent.Envelope {
	e := domainevent.Envelope{
		ID: r.ID, Topic: r.Topic, Version: int(r.Version), AggregateType: r.AggregateType, AggregateID: r.AggregateID,
		OccurredAt: r.OccurredAt, ActorUserID: r.ActorUserID, Payload: r.Payload,
	}
	if r.RequestID != nil {
		e.RequestID = *r.RequestID
	}
	return e
}

// Store persists outbox rows. The GORM store works in the transaction
// carried by ctx (db.Conn); the memory store backs service and relay tests.
type Store interface {
	// Insert writes a new row.
	Insert(ctx context.Context, r Row) error
	// Due returns up to limit unpublished rows with available_at <= now,
	// oldest first, locked FOR UPDATE SKIP LOCKED by the transaction in ctx
	// (several relays never take the same row).
	Due(ctx context.Context, now time.Time, limit int) ([]Row, error)
	// MarkPublished sets published_at.
	MarkPublished(ctx context.Context, id uuid.UUID, at time.Time) error
	// MarkFailed records a failed dispatch and postpones the row.
	MarkFailed(ctx context.Context, id uuid.UUID, attempts int, lastErr string, availableAt time.Time) error
	// DeletePublished deletes up to limit rows published before the cutoff
	// and returns how many it deleted.
	DeletePublished(ctx context.Context, before time.Time, limit int) (int, error)
}

// NewGormStore returns the PostgreSQL store.
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// cross-org: infrastructure table without org_id (02 §1.1).
func (s gormStore) Insert(ctx context.Context, r Row) error {
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// cross-org: infrastructure table without org_id (02 §1.1).
func (s gormStore) Due(ctx context.Context, now time.Time, limit int) ([]Row, error) {
	var rows []Row
	err := db.Conn(ctx, s.orm).
		Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate, Options: clause.LockingOptionsSkipLocked}).
		Where("published_at IS NULL AND available_at <= ?", now).
		Order("available_at, id").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("outbox: due rows: %w", err)
	}
	return rows, nil
}

// cross-org: infrastructure table without org_id (02 §1.1).
func (s gormStore) MarkPublished(ctx context.Context, id uuid.UUID, at time.Time) error {
	err := db.Conn(ctx, s.orm).Model(&Row{}).Where("id = ?", id).Update("published_at", at).Error
	if err != nil {
		return fmt.Errorf("outbox: mark published: %w", err)
	}
	return nil
}

// cross-org: infrastructure table without org_id (02 §1.1).
func (s gormStore) MarkFailed(ctx context.Context, id uuid.UUID, attempts int, lastErr string, availableAt time.Time) error {
	err := db.Conn(ctx, s.orm).Model(&Row{}).Where("id = ?", id).Updates(map[string]any{
		"attempts": attempts, "last_error": lastErr, "available_at": availableAt,
	}).Error
	if err != nil {
		return fmt.Errorf("outbox: mark failed: %w", err)
	}
	return nil
}

// cross-org: infrastructure table without org_id (02 §1.1).
func (s gormStore) DeletePublished(ctx context.Context, before time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM outbox WHERE id IN (
		SELECT id FROM outbox WHERE published_at < ? ORDER BY published_at LIMIT ?)`, before, limit)
	if res.Error != nil {
		return 0, fmt.Errorf("outbox: delete published: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}
