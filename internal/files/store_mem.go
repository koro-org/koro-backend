package files

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// memStore is the in-memory Store of service, wiring and matrix tests. It
// implements the GORM store's contract (store_contract_test.go), including
// the CHECK constraints a test could otherwise only meet in PostgreSQL.
// Lock does not lock: tests run one operation at a time per file.
type memStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]File
	// refs simulates foreign keys of other tables (organizations.logo_file_id,
	// club_claims.evidence_file_id, ...): a referenced row cannot be deleted.
	refs map[uuid.UUID]bool
	// now stamps updated_at like the files_touch_updated_at trigger.
	now func() time.Time
}

// NewMemStore returns an empty in-memory Store; now stamps updated_at.
func NewMemStore(now func() time.Time) Store {
	return &memStore{rows: map[uuid.UUID]File{}, refs: map[uuid.UUID]bool{}, now: now}
}

// Reference marks id as referenced by another table (tests of ErrReferenced).
// s must come from NewMemStore; any other store is left alone.
func Reference(s Store, id uuid.UUID) {
	m, ok := s.(*memStore)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs[id] = true
}

func (s *memStore) check(f File) error {
	switch {
	case !f.Purpose.Valid(), f.Bucket == "", f.StorageKey == "", f.DeclaredMIME == "":
		return errCheck
	case f.Status != StatusUploading && f.Status != StatusActive && f.Status != StatusRejected:
		return errCheck // files_status_chk
	case f.Status != StatusUploading && (len(f.SHA256) != 32 || f.SizeBytes == nil):
		return errCheck // files_active_chk
	case (f.AttachedModule == nil) != (f.AttachedEntityID == nil):
		return errCheck // files_attached_chk
	case f.SizeBytes != nil && (*f.SizeBytes < 1 || *f.SizeBytes > 100*MB):
		return errCheck
	}
	switch f.ScanStatus {
	case ScanPending, ScanClean, ScanInfected, ScanSkipped:
	default:
		return errCheck
	}
	for _, o := range s.rows {
		if o.ID != f.ID && o.StorageKey == f.StorageKey {
			return errUnique // files_storage_key_uq
		}
	}
	return nil
}

func (s *memStore) Insert(_ context.Context, f File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.rows[f.ID]; dup {
		return errUnique
	}
	if err := s.check(f); err != nil {
		return err
	}
	s.rows[f.ID] = f.clone()
	return nil
}

func (s *memStore) Get(_ context.Context, id uuid.UUID) (File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.rows[id]
	if !ok {
		return File{}, ErrNotFound
	}
	return f.clone(), nil
}

func (s *memStore) Lock(ctx context.Context, id uuid.UUID) (File, error) { return s.Get(ctx, id) }

func (s *memStore) Update(_ context.Context, f File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.rows[f.ID]
	if !ok {
		return ErrNotFound
	}
	if err := s.check(f); err != nil {
		return err
	}
	f.CreatedAt = cur.CreatedAt
	f.UpdatedAt = s.now()
	s.rows[f.ID] = f.clone()
	return nil
}

func (s *memStore) Delete(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs[id] {
		return ErrReferenced
	}
	delete(s.rows, id)
	return nil
}

func (s *memStore) list(match func(File) bool, after uuid.UUID, limit int) []File {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []File
	for _, id := range slices.SortedFunc(maps.Keys(s.rows), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }) {
		f := s.rows[id]
		if bytes.Compare(id[:], after[:]) > 0 && match(f) {
			out = append(out, f.clone())
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out
}

func (s *memStore) Orphans(_ context.Context, before time.Time, after uuid.UUID, limit int) ([]File, error) {
	return s.list(func(f File) bool { return f.AttachedAt == nil && f.CreatedAt.Before(before) }, after, limit), nil
}

func (s *memStore) PendingScans(_ context.Context, before time.Time, after uuid.UUID, limit int) ([]File, error) {
	return s.list(func(f File) bool {
		return f.Status == StatusActive && f.ScanStatus == ScanPending && f.UpdatedAt.Before(before)
	}, after, limit), nil
}

func (s *memStore) SkippedScans(_ context.Context, after uuid.UUID, limit int) ([]File, error) {
	return s.list(func(f File) bool {
		return f.Status == StatusActive && f.ScanStatus == ScanSkipped
	}, after, limit), nil
}

func (s *memStore) UnattachedOf(_ context.Context, userID uuid.UUID) ([]File, error) {
	return s.list(func(f File) bool {
		return f.AttachedAt == nil && f.OwnerUserID != nil && *f.OwnerUserID == userID
	}, uuid.Nil(), 0), nil
}

// Constraint violations of the in-memory store, as db.Translate reports
// them for PostgreSQL (CHECK → 422, unique → 409).
var (
	errCheck  = problem.ErrValidation
	errUnique = problem.ErrConflict
)
