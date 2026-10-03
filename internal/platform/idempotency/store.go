package idempotency

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

// NewGormStore returns the PostgreSQL store of idempotency_keys (02 §1.3).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// Reserve inserts the key or takes over an expired row. ON CONFLICT waits
// for a concurrent transaction holding the same key, then sees its committed
// row (or inserts if it rolled back).
//
// cross-org: infrastructure table, scoped by actor (02 §1.3).
func (s gormStore) Reserve(ctx context.Context, scope, key string, hash []byte, now, lockedUntil, expiresAt time.Time) (Record, bool, error) {
	c := db.Conn(ctx, s.orm)
	res := c.Exec(`INSERT INTO idempotency_keys (scope, key, request_hash, status, locked_until, created_at, expires_at)
		VALUES ( ?, ?, ?, 'in_progress', ?, ?, ? )
		ON CONFLICT (scope, key) DO UPDATE SET
			request_hash = EXCLUDED.request_hash, status = 'in_progress',
			response_status = NULL, response_headers = NULL, response_body = NULL,
			locked_until = EXCLUDED.locked_until, created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at
		WHERE idempotency_keys.expires_at <= EXCLUDED.created_at`,
		scope, key, hash, lockedUntil, now, expiresAt)
	if res.Error != nil {
		return Record{}, false, fmt.Errorf("idempotency: reserve: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		return Record{}, true, nil
	}
	var row struct {
		RequestHash    []byte
		Status         string
		ResponseStatus *int
		ResponseBody   *string
	}
	err := c.Raw(`SELECT request_hash, status, response_status, response_body::text AS response_body
		FROM idempotency_keys WHERE scope = ? AND key = ?`, scope, key).Scan(&row).Error
	if err != nil {
		return Record{}, false, fmt.Errorf("idempotency: read key: %w", err)
	}
	rec := Record{RequestHash: row.RequestHash, Status: row.Status}
	if row.ResponseStatus != nil {
		rec.ResponseStatus = *row.ResponseStatus
	}
	if row.ResponseBody != nil {
		rec.ResponseBody = []byte(*row.ResponseBody)
	}
	return rec, false, nil
}

// Complete stores the response.
//
// cross-org: infrastructure table, scoped by actor (02 §1.3).
func (s gormStore) Complete(ctx context.Context, scope, key string, status int, body []byte) error {
	err := db.Conn(ctx, s.orm).Exec(`UPDATE idempotency_keys
		SET status = 'completed', response_status = ?, response_body = CAST( ? AS jsonb)
		WHERE scope = ? AND key = ?`, status, string(body), scope, key).Error
	if err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	return nil
}

// DeleteExpired deletes a batch of expired keys.
//
// cross-org: infrastructure table, scoped by actor (02 §1.3).
func (s gormStore) DeleteExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM idempotency_keys WHERE ctid IN (
		SELECT ctid FROM idempotency_keys WHERE expires_at < ? LIMIT ?)`, now, limit)
	if res.Error != nil {
		return 0, fmt.Errorf("idempotency: delete expired: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// NewMemStore returns the in-memory store of service tests. Like the real
// store, a key exists only once its transaction commits (db.AfterCommit;
// db.NopTx commits when the outermost function returns nil), so a failed
// call does not consume the key. Concurrent waiting is not emulated.
func NewMemStore() *MemStore { return &MemStore{rows: map[[2]string]memRow{}} }

// MemStore is the in-memory Store.
type MemStore struct {
	mu   sync.Mutex
	rows map[[2]string]memRow
}

type memRow struct {
	rec       Record
	expiresAt time.Time
}

// Len returns the number of stored keys.
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// Reserve implements Store.
func (m *MemStore) Reserve(ctx context.Context, scope, key string, hash []byte, now, _, expiresAt time.Time) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[[2]string{scope, key}]; ok && r.expiresAt.After(now) {
		return r.rec, false, nil
	}
	k := [2]string{scope, key}
	rec := Record{RequestHash: slices.Clone(hash), Status: StatusInProgress}
	db.AfterCommit(ctx, func(context.Context) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r, ok := m.rows[k]; !ok || r.rec.Status != StatusCompleted || !r.expiresAt.After(now) {
			m.rows[k] = memRow{rec: rec, expiresAt: expiresAt}
		}
	})
	return Record{}, true, nil
}

// Complete implements Store (applied at commit, after Reserve's hook).
func (m *MemStore) Complete(ctx context.Context, scope, key string, status int, body []byte) error {
	k := [2]string{scope, key}
	db.AfterCommit(ctx, func(context.Context) {
		m.mu.Lock()
		defer m.mu.Unlock()
		r := m.rows[k]
		r.rec.Status, r.rec.ResponseStatus, r.rec.ResponseBody = StatusCompleted, status, slices.Clone(body)
		m.rows[k] = r
	})
	return nil
}

// DeleteExpired implements Store.
func (m *MemStore) DeleteExpired(_ context.Context, now time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, r := range m.rows {
		if n < limit && r.expiresAt.Before(now) {
			delete(m.rows, k)
			n++
		}
	}
	return n, nil
}
