package inbox

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// NewGormStore returns the PostgreSQL store of webhook_inbox.
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// Insert implements Store.
//
// cross-org: infrastructure table without org_id (02 §1.2).
func (s gormStore) Insert(ctx context.Context, r Row) (Row, bool, error) {
	var headers *string
	if r.Headers != nil {
		b, err := json.Marshal(r.Headers)
		if err != nil {
			return Row{}, false, fmt.Errorf("inbox: encode headers: %w", err)
		}
		h := string(b)
		headers = &h
	}
	c := db.Conn(ctx, s.orm)
	res := c.Exec(`INSERT INTO webhook_inbox (id, provider, event_key, received_at, signature_valid, headers, payload, payload_sha256)
		VALUES ( ?, ?, ?, ?, ?, CAST( ? AS jsonb), CAST( ? AS jsonb), ? )
		ON CONFLICT (provider, event_key) DO UPDATE SET
			received_at = EXCLUDED.received_at, signature_valid = true, headers = EXCLUDED.headers,
			payload = EXCLUDED.payload, payload_sha256 = EXCLUDED.payload_sha256, attempts = 0, last_error = NULL
		WHERE NOT webhook_inbox.signature_valid AND EXCLUDED.signature_valid AND webhook_inbox.processed_at IS NULL`,
		r.ID, r.Provider, r.EventKey, r.ReceivedAt, r.SignatureValid, headers, string(r.Payload), r.PayloadSHA256)
	if res.Error != nil {
		return Row{}, false, fmt.Errorf("inbox: insert: %w", db.Translate(res.Error))
	}
	var stored Row
	if err := c.Where("provider = ? AND event_key = ?", r.Provider, r.EventKey).Take(&stored).Error; err != nil {
		return Row{}, false, fmt.Errorf("inbox: read back: %w", err)
	}
	return stored, res.RowsAffected == 1, nil
}

// Lock implements Store.
//
// cross-org: infrastructure table without org_id (02 §1.2).
func (s gormStore) Lock(ctx context.Context, id uuid.UUID) (Row, error) {
	var r Row
	err := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("id = ?", id).Take(&r).Error
	if err != nil {
		return Row{}, fmt.Errorf("inbox: lock: %w", err)
	}
	return r, nil
}

// MarkProcessed implements Store.
//
// cross-org: infrastructure table without org_id (02 §1.2).
func (s gormStore) MarkProcessed(ctx context.Context, id uuid.UUID, at time.Time, attempts int) error {
	err := db.Conn(ctx, s.orm).Model(&Row{}).Where("id = ?", id).
		Updates(map[string]any{"processed_at": at, "attempts": attempts, "last_error": nil}).Error
	if err != nil {
		return fmt.Errorf("inbox: mark processed: %w", err)
	}
	return nil
}

// MarkFailed implements Store.
//
// cross-org: infrastructure table without org_id (02 §1.2).
func (s gormStore) MarkFailed(ctx context.Context, id uuid.UUID, attempts int, lastErr string) error {
	err := db.Conn(ctx, s.orm).Model(&Row{}).Where("id = ?", id).
		Updates(map[string]any{"attempts": attempts, "last_error": lastErr}).Error
	if err != nil {
		return fmt.Errorf("inbox: mark failed: %w", err)
	}
	return nil
}

// PurgePayloads implements Store.
//
// cross-org: infrastructure table without org_id (02 §1.2).
func (s gormStore) PurgePayloads(ctx context.Context, before, now time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE webhook_inbox SET payload = NULL, headers = NULL, payload_purged_at = ?
		WHERE id IN (SELECT id FROM webhook_inbox WHERE payload_purged_at IS NULL AND received_at < ?
			ORDER BY received_at LIMIT ?)`, now, before, limit)
	if res.Error != nil {
		return 0, fmt.Errorf("inbox: purge payloads: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DeleteOld implements Store.
//
// cross-org: infrastructure table without org_id (02 §1.2).
func (s gormStore) DeleteOld(ctx context.Context, before time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM webhook_inbox WHERE id IN (
		SELECT id FROM webhook_inbox WHERE received_at < ? ORDER BY received_at LIMIT ?)`, before, limit)
	if res.Error != nil {
		return 0, fmt.Errorf("inbox: delete old rows: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// NewMemStore returns the in-memory store of tests. Processing state
// changes apply at commit (db.AfterCommit), like the real store.
func NewMemStore() *MemStore { return &MemStore{rows: map[uuid.UUID]*Row{}} }

// MemStore is the in-memory Store.
type MemStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*Row
}

// Rows returns a copy of every row ordered by received_at.
func (m *MemStore) Rows() []Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Row, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b Row) int { return cmp.Or(a.ReceivedAt.Compare(b.ReceivedAt), a.ID.Compare(b.ID)) })
	return out
}

// Insert implements Store.
func (m *MemStore) Insert(_ context.Context, r Row) (Row, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, old := range m.rows {
		if old.Provider != r.Provider || old.EventKey != r.EventKey {
			continue
		}
		if !old.SignatureValid && r.SignatureValid && old.ProcessedAt == nil {
			old.ReceivedAt, old.SignatureValid, old.Headers, old.Payload, old.PayloadSHA256 = r.ReceivedAt, true, r.Headers, r.Payload, r.PayloadSHA256
			old.Attempts, old.LastError = 0, nil
			return *old, true, nil
		}
		return *old, false, nil
	}
	m.rows[r.ID] = &r
	return r, true, nil
}

// Lock implements Store.
func (m *MemStore) Lock(_ context.Context, id uuid.UUID) (Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return Row{}, fmt.Errorf("inbox: lock: %w", gorm.ErrRecordNotFound)
	}
	return *r, nil
}

// MarkProcessed implements Store (applied at commit).
func (m *MemStore) MarkProcessed(ctx context.Context, id uuid.UUID, at time.Time, attempts int) error {
	db.AfterCommit(ctx, func(context.Context) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r, ok := m.rows[id]; ok {
			r.ProcessedAt, r.Attempts, r.LastError = &at, attempts, nil
		}
	})
	return nil
}

// MarkFailed implements Store (applied at commit).
func (m *MemStore) MarkFailed(ctx context.Context, id uuid.UUID, attempts int, lastErr string) error {
	db.AfterCommit(ctx, func(context.Context) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r, ok := m.rows[id]; ok {
			r.Attempts, r.LastError = attempts, &lastErr
		}
	})
	return nil
}

// PurgePayloads implements Store.
func (m *MemStore) PurgePayloads(_ context.Context, before, now time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if n < limit && r.PayloadPurgedAt == nil && r.ReceivedAt.Before(before) {
			r.Payload, r.Headers, r.PayloadPurgedAt = nil, nil, &now
			n++
		}
	}
	return n, nil
}

// DeleteOld implements Store.
func (m *MemStore) DeleteOld(_ context.Context, before time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		if n < limit && r.ReceivedAt.Before(before) {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}
