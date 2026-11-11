package ingest

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// errCheck mirrors a CHECK violation (the GORM store gets 422 from
// db.Translate); errConflict a unique violation (409).
var (
	errCheck    = problem.ErrValidation.Wrap(errors.New("ingest: check constraint violated"))
	errConflict = problem.ErrConflict.Wrap(errors.New("ingest: unique constraint violated"))
)

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It enforces the CHECK and unique constraints of 000187 that
// a test could otherwise meet only in PostgreSQL. Every method is atomic
// under one mutex; it has no transactions (db.NopTx).
type MemStore struct {
	mu      sync.Mutex
	now     func() time.Time
	sources map[uuid.UUID]Source
	runs    map[uuid.UUID]Run
	clubs   map[uuid.UUID]ImportedClub
	events  map[uuid.UUID]ImportedEvent
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory Store; now stamps the rows.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{now: now, sources: map[uuid.UUID]Source{}, runs: map[uuid.UUID]Run{},
		clubs: map[uuid.UUID]ImportedClub{}, events: map[uuid.UUID]ImportedEvent{}}
}

func (s *MemStore) stamp() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// ---- sources

func checkSource(r Source) error {
	switch {
	case !r.Kind.Valid(), !r.Adapter.Valid(), r.URL == "", len(r.URL) > 2048, r.Host == "",
		len([]rune(r.Label)) < 1, len([]rune(r.Label)) > 120,
		r.IntervalSeconds != 0 && (r.IntervalSeconds < 3600 || r.IntervalSeconds > 2592000),
		r.Enabled && r.PausedReason != nil, r.ParentID != nil && r.Kind != KindEvents,
		r.ContentHash != nil && len(r.ContentHash) != 32, r.FailCount < 0, r.ZeroYieldRuns < 0:
		return errCheck
	}
	return nil
}

// InsertSource implements Store.
func (s *MemStore) InsertSource(_ context.Context, r Source) (Source, bool, error) {
	if err := checkSource(r); err != nil {
		return Source{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.sources {
		if x.URL == r.URL {
			return x, false, nil
		}
	}
	if r.ParentID != nil {
		if _, ok := s.sources[*r.ParentID]; !ok {
			return Source{}, false, errConflict
		}
	}
	r.CreatedAt, r.UpdatedAt = s.stamp(), s.stamp()
	s.sources[r.ID] = r
	return r, true, nil
}

// GetSource implements Store.
func (s *MemStore) GetSource(_ context.Context, id uuid.UUID) (Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.sources[id]
	if !ok {
		return Source{}, errNotFound
	}
	return r, nil
}

// LockSource implements Store.
func (s *MemStore) LockSource(ctx context.Context, id uuid.UUID) (Source, error) {
	return s.GetSource(ctx, id)
}

// ListSources implements Store.
func (s *MemStore) ListSources(_ context.Context, f SourceFilter, after *uuid.UUID, limit int) ([]Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Source
	for _, r := range s.sources {
		switch {
		case f.UniversityID != nil && r.UniversityID != *f.UniversityID,
			f.ClubOrgID != nil && (r.ClubOrgID == nil || *r.ClubOrgID != *f.ClubOrgID),
			f.Kind != "" && r.Kind != f.Kind, f.Enabled != nil && r.Enabled != *f.Enabled,
			after != nil && r.ID.Compare(*after) <= 0:
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Source) int { return a.ID.Compare(b.ID) })
	return head(out, limit), nil
}

// DueSources implements Store.
func (s *MemStore) DueSources(_ context.Context, now time.Time, limit int) ([]Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Source
	for _, r := range s.sources {
		if r.Enabled && r.NextRunAt != nil && !r.NextRunAt.After(now) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Source) int {
		return cmp.Or(a.NextRunAt.Compare(*b.NextRunAt), a.ID.Compare(b.ID))
	})
	return head(out, limit), nil
}

// HostLastFetched implements Store.
func (s *MemStore) HostLastFetched(_ context.Context, hosts []string) (map[string]time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]time.Time{}
	for _, r := range s.sources {
		if r.LastFetchedAt == nil || !slices.Contains(hosts, r.Host) {
			continue
		}
		if t, ok := out[r.Host]; !ok || r.LastFetchedAt.After(t) {
			out[r.Host] = *r.LastFetchedAt
		}
	}
	return out, nil
}

// UpdateSource implements Store.
func (s *MemStore) UpdateSource(_ context.Context, r Source) error {
	if err := checkSource(r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.sources[r.ID]
	if !ok {
		return errNotFound
	}
	// Only the mutable columns change.
	old.IntervalSeconds, old.Enabled, old.PausedReason, old.NextRunAt = r.IntervalSeconds, r.Enabled, r.PausedReason, r.NextRunAt
	old.ETag, old.LastModified, old.ContentHash, old.FailCount = r.ETag, r.LastModified, r.ContentHash, r.FailCount
	old.ZeroYieldRuns, old.LastError, old.LastStatus, old.LastYield = r.ZeroYieldRuns, r.LastError, r.LastStatus, r.LastYield
	old.LastFetchedAt, old.LastOKAt, old.UpdatedAt = r.LastFetchedAt, r.LastOKAt, s.stamp()
	s.sources[r.ID] = old
	return nil
}

// PauseClubSources implements Store.
func (s *MemStore) PauseClubSources(_ context.Context, clubOrgID uuid.UUID, reason PausedReason) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.sources {
		if r.Enabled && r.ClubOrgID != nil && *r.ClubOrgID == clubOrgID {
			r.Enabled, r.PausedReason, r.UpdatedAt = false, &reason, s.stamp()
			s.sources[id] = r
			n++
		}
	}
	return n, nil
}

// DeleteFetchedChildren implements Store.
func (s *MemStore) DeleteFetchedChildren(_ context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.sources {
		if n >= limit {
			break
		}
		if r.ParentID != nil && r.IntervalSeconds == 0 && r.LastFetchedAt != nil && r.LastFetchedAt.Before(before) {
			s.deleteSourceLocked(id)
			n++
		}
	}
	return n, nil
}

// deleteSourceLocked applies the FKs: runs cascade, the rows that name the
// source keep NULL, children cascade.
func (s *MemStore) deleteSourceLocked(id uuid.UUID) {
	delete(s.sources, id)
	for rid, r := range s.runs {
		if r.SourceID == id {
			delete(s.runs, rid)
		}
	}
	for eid, e := range s.events {
		if e.SourceID != nil && *e.SourceID == id {
			e.SourceID = nil
			s.events[eid] = e
		}
	}
	for cid, c := range s.clubs {
		if c.SourceID != nil && *c.SourceID == id {
			c.SourceID = nil
			s.clubs[cid] = c
		}
	}
	for cid, c := range s.sources {
		if c.ParentID != nil && *c.ParentID == id {
			s.deleteSourceLocked(cid)
		}
	}
}

// ---- runs

// InsertRun implements Store.
func (s *MemStore) InsertRun(_ context.Context, r Run) error {
	if r.FinishedAt.Before(r.StartedAt) || r.Outcome == "" {
		return errCheck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sources[r.SourceID]; !ok {
		return errConflict
	}
	s.runs[r.ID] = r
	return nil
}

// ListRuns implements Store.
func (s *MemStore) ListRuns(_ context.Context, sourceID uuid.UUID, limit int) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Run
	for _, r := range s.runs {
		if r.SourceID == sourceID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Run) int { return cmp.Or(b.StartedAt.Compare(a.StartedAt), b.ID.Compare(a.ID)) })
	return head(out, limit), nil
}

// DeleteRunsBefore implements Store.
func (s *MemStore) DeleteRunsBefore(_ context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, r := range s.runs {
		if n < limit && r.StartedAt.Before(before) {
			delete(s.runs, id)
			n++
		}
	}
	return n, nil
}

// ---- imported clubs

// GetImportedClub implements Store.
func (s *MemStore) GetImportedClub(_ context.Context, clubOrgID uuid.UUID) (ImportedClub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clubs[clubOrgID]
	if !ok {
		return ImportedClub{}, errNotFound
	}
	return c, nil
}

// FindImportedClub implements Store.
func (s *MemStore) FindImportedClub(_ context.Context, universityID uuid.UUID, nameKey string) (ImportedClub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clubs {
		if c.UniversityID == universityID && c.NameKey == nameKey {
			return c, nil
		}
	}
	return ImportedClub{}, errNotFound
}

// UpsertImportedClub implements Store.
func (s *MemStore) UpsertImportedClub(_ context.Context, c ImportedClub) error {
	if c.NameKey == "" || len([]rune(c.NameKey)) > 200 {
		return errCheck
	}
	if c.ContactEmail != nil {
		if _, ok := emailDomain(*c.ContactEmail); !ok {
			return errCheck
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, x := range s.clubs {
		if id != c.ClubOrgID && x.UniversityID == c.UniversityID && x.NameKey == c.NameKey {
			return errConflict
		}
	}
	old, ok := s.clubs[c.ClubOrgID]
	if !ok {
		c.CreatedAt, c.UpdatedAt, c.MissingSince = s.stamp(), s.stamp(), nil
		s.clubs[c.ClubOrgID] = c
		return nil
	}
	old.SourceID, old.NameKey, old.SourceCategory, old.LastSeenAt, old.MissingSince = c.SourceID, c.NameKey,
		c.SourceCategory, c.LastSeenAt, nil
	if c.ContactEmail != nil {
		old.ContactEmail = c.ContactEmail
	}
	old.UpdatedAt = s.stamp()
	s.clubs[c.ClubOrgID] = old
	return nil
}

// MarkMissingClubs implements Store.
func (s *MemStore) MarkMissingClubs(_ context.Context, sourceID uuid.UUID, seenBefore, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, c := range s.clubs {
		if c.SourceID != nil && *c.SourceID == sourceID && c.LastSeenAt.Before(seenBefore) && c.MissingSince == nil {
			c.MissingSince = &now
			s.clubs[id] = c
			n++
		}
	}
	return n, nil
}

// ListImportedClubs implements Store.
func (s *MemStore) ListImportedClubs(_ context.Context, f ImportedClubFilter, after *uuid.UUID, limit int) ([]ImportedClub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ImportedClub
	for _, c := range s.clubs {
		switch {
		case f.UniversityID != nil && c.UniversityID != *f.UniversityID,
			f.NeedsReview != nil && c.NeedsReview != *f.NeedsReview,
			after != nil && c.ClubOrgID.Compare(*after) <= 0:
			continue
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b ImportedClub) int { return a.ClubOrgID.Compare(b.ClubOrgID) })
	return head(out, limit), nil
}

// MarkClubReviewed implements Store.
func (s *MemStore) MarkClubReviewed(_ context.Context, clubOrgID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clubs[clubOrgID]
	if !ok {
		return errNotFound
	}
	c.NeedsReview, c.UpdatedAt = false, s.stamp()
	s.clubs[clubOrgID] = c
	return nil
}

// ---- imported events

func checkEvent(e ImportedEvent) error {
	n := len([]rune(e.Title))
	switch {
	case n < 3 || n > 200, !e.Status.Valid(), !e.Category.Valid(), len(e.Fingerprint) != 32,
		e.EndsAt != nil && e.EndsAt.Before(e.StartsAt), !e.ExpiresAt.After(e.StartsAt),
		e.Confidence < 0 || e.Confidence > 1, len([]rune(e.Evidence)) < 1 || len([]rune(e.Evidence)) > 500,
		e.LocationText != nil && (len([]rune(*e.LocationText)) < 1 || len([]rune(*e.LocationText)) > 200),
		(e.Status == StatusHidden) != (e.HiddenReason != nil), e.Status != StatusPromoted && e.PromotedEventID != nil,
		len(e.SourceURL) > 2048 || e.SourceURL == "":
		return errCheck
	}
	return nil
}

// EventByFingerprint implements Store.
func (s *MemStore) EventByFingerprint(_ context.Context, fp []byte) (ImportedEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if bytes.Equal(e.Fingerprint, fp) {
			return e, nil
		}
	}
	return ImportedEvent{}, errNotFound
}

// InsertEvent implements Store.
func (s *MemStore) InsertEvent(_ context.Context, e ImportedEvent) error {
	if err := checkEvent(e); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.events {
		if bytes.Equal(x.Fingerprint, e.Fingerprint) {
			return errConflict
		}
	}
	e.CreatedAt, e.UpdatedAt = s.stamp(), s.stamp()
	s.events[e.ID] = e
	return nil
}

// RefreshEvent implements Store.
func (s *MemStore) RefreshEvent(_ context.Context, e ImportedEvent) error {
	if err := checkEvent(e); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.events[e.ID]
	if !ok {
		return errNotFound
	}
	old.EndsAt, old.LocationText, old.Category, old.ExpiresAt, old.LastSeenAt = e.EndsAt, e.LocationText, e.Category,
		e.ExpiresAt, e.LastSeenAt
	old.UpdatedAt = s.stamp()
	s.events[e.ID] = old
	return nil
}

// GetEvent implements Store.
func (s *MemStore) GetEvent(_ context.Context, id uuid.UUID) (ImportedEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return ImportedEvent{}, errNotFound
	}
	return e, nil
}

// LockEvent implements Store.
func (s *MemStore) LockEvent(ctx context.Context, id uuid.UUID) (ImportedEvent, error) {
	return s.GetEvent(ctx, id)
}

// SetEventStatus implements Store.
func (s *MemStore) SetEventStatus(_ context.Context, id uuid.UUID, st Status, reason *HiddenReason, promoted *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return errNotFound
	}
	e.Status, e.HiddenReason, e.PromotedEventID = st, reason, promoted
	if err := checkEvent(e); err != nil {
		return err
	}
	e.UpdatedAt = s.stamp()
	s.events[id] = e
	return nil
}

// DeleteEvent implements Store.
func (s *MemStore) DeleteEvent(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.events, id)
	return nil
}

// matchEvent applies f to one event.
func matchEvent(e ImportedEvent, f EventFilter) bool {
	switch {
	case len(f.Statuses) > 0 && !slices.Contains(f.Statuses, e.Status),
		f.UniversityID != nil && e.UniversityID != *f.UniversityID,
		f.ClubOrgID != nil && (e.ClubOrgID == nil || *e.ClubOrgID != *f.ClubOrgID),
		f.SourceID != nil && (e.SourceID == nil || *e.SourceID != *f.SourceID),
		f.From != nil && e.StartsAt.Before(*f.From), f.To != nil && !e.StartsAt.Before(*f.To),
		f.EndsAfter != nil && !e.End().After(*f.EndsAfter):
		return false
	}
	return true
}

// ListEvents implements Store.
func (s *MemStore) ListEvents(_ context.Context, f EventFilter, after *EventAfter, limit int) ([]ImportedEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ImportedEvent
	for _, e := range s.events {
		if !matchEvent(e, f) {
			continue
		}
		if after != nil && cmp.Or(e.StartsAt.Compare(after.StartsAt), e.ID.Compare(after.ID)) <= 0 {
			continue
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b ImportedEvent) int { return cmp.Or(a.StartsAt.Compare(b.StartsAt), a.ID.Compare(b.ID)) })
	return head(out, limit), nil
}

// CountClubEvents implements Store.
func (s *MemStore) CountClubEvents(_ context.Context, clubOrgID uuid.UUID, st Status, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.ClubOrgID != nil && *e.ClubOrgID == clubOrgID && e.Status == st && e.End().After(now) {
			n++
		}
	}
	return n, nil
}

// DeleteExpiredEvents implements Store.
func (s *MemStore) DeleteExpiredEvents(_ context.Context, now time.Time, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, e := range s.events {
		if n < limit && !e.ExpiresAt.After(now) {
			delete(s.events, id)
			n++
		}
	}
	return n, nil
}

// head returns at most limit rows.
func head[T any](rows []T, limit int) []T {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}
