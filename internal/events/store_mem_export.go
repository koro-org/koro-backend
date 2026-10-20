package events

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"
)

// checkExport applies the CHECK constraints of event_data_exports (02
// §11.9, migration 000126).
func checkExport(x Export) error {
	n := utf8.RuneCountInString(x.Reason)
	switch {
	case n < 10 || n > 500, len(x.Fields) < 1 || len(x.Fields) > 40, !x.Format.Valid():
		return errCheck
	case x.Status != ExportQueued && x.Status != ExportReady && x.Status != ExportFailed && x.Status != ExportPurged:
		return errCheck
	case x.RowCount != nil && *x.RowCount < 0, x.DownloadCount < 0:
		return errCheck
	case x.FileSHA256 != nil && len(x.FileSHA256) != 32:
		return errCheck
	case x.Status == ExportReady && x.FileID == nil:
		return errCheck
	case (x.Status == ExportPurged) != (x.PurgedAt != nil), x.Status == ExportPurged && x.FileID != nil:
		return errCheck
	}
	return nil
}

func (s *MemStore) exportOf(orgID, eventID, id uuid.UUID) (Export, bool) {
	x, ok := s.exports[id]
	return x, ok && x.OrgID == orgID && x.EventID == eventID
}

// InsertExport implements Store (event_id and org_id from the events row).
func (s *MemStore) InsertExport(_ context.Context, x Export) (Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[x.EventID]
	if !ok || e.OrgID != x.OrgID {
		return Export{}, ErrNotFound
	}
	x.EventID, x.OrgID = e.ID, e.OrgID
	x.Fields = slices.Clone(x.Fields)
	if err := checkExport(x); err != nil {
		return Export{}, err
	}
	if _, dup := s.exports[x.ID]; dup {
		return Export{}, errCheck
	}
	x.CreatedAt = s.stamp(time.Time{})
	x.UpdatedAt = x.CreatedAt
	s.exports[x.ID] = x
	return x, nil
}

// GetExport implements Store.
func (s *MemStore) GetExport(_ context.Context, orgID, eventID, id uuid.UUID) (Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.exportOf(orgID, eventID, id)
	if !ok {
		return Export{}, ErrNotFound
	}
	return x, nil
}

// LockExport implements Store (no locks in memory).
func (s *MemStore) LockExport(ctx context.Context, orgID, eventID, id uuid.UUID) (Export, error) {
	return s.GetExport(ctx, orgID, eventID, id)
}

// UpdateExport implements Store.
func (s *MemStore) UpdateExport(_ context.Context, x Export) (Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.exportOf(x.OrgID, x.EventID, x.ID)
	if !ok {
		return Export{}, ErrNotFound
	}
	cur.Status, cur.RowCount, cur.FileID, cur.FileSHA256, cur.PurgedAt = x.Status, x.RowCount, x.FileID, x.FileSHA256, x.PurgedAt
	if err := checkExport(cur); err != nil {
		return Export{}, err
	}
	cur.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.exports[cur.ID] = cur
	return cur, nil
}

// RecordDownload implements Store.
func (s *MemStore) RecordDownload(_ context.Context, orgID, eventID, id uuid.UUID, at time.Time) (Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.exportOf(orgID, eventID, id)
	if !ok || x.Status != ExportReady {
		return Export{}, ErrNotFound
	}
	x.DownloadCount++
	if x.FirstDownloadedAt == nil {
		t := at
		x.FirstDownloadedAt = &t
	}
	x.UpdatedAt = s.stamp(x.UpdatedAt)
	s.exports[id] = x
	return x, nil
}

// ListExports implements Store.
func (s *MemStore) ListExports(_ context.Context, orgID, eventID uuid.UUID, after *After, limit int) ([]Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Export
	for _, x := range s.exports {
		if x.OrgID != orgID || x.EventID != eventID {
			continue
		}
		if after != nil && cmp.Or(x.CreatedAt.Compare(after.At), bytes.Compare(x.ID[:], after.ID[:])) >= 0 {
			continue
		}
		out = append(out, x)
	}
	slices.SortFunc(out, func(a, b Export) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), bytes.Compare(b.ID[:], a.ID[:]))
	})
	return out[:min(len(out), max(limit, 0))], nil
}

// exportsByID returns the rows of s.exports matching keep after after, by id.
func (s *MemStore) exportsByID(after uuid.UUID, limit int, keep func(Export) bool) []Export {
	var out []Export
	for _, x := range s.exports {
		if bytes.Compare(x.ID[:], after[:]) > 0 && keep(x) {
			out = append(out, x)
		}
	}
	slices.SortFunc(out, func(a, b Export) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	return out[:min(len(out), max(limit, 0))]
}

// ExportsDue implements Store.
func (s *MemStore) ExportsDue(_ context.Context, now time.Time, after uuid.UUID, limit int) ([]Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exportsByID(after, limit, func(x Export) bool { return x.Status == ExportReady && !x.ExpiresAt.After(now) }), nil
}

// QueuedExports implements Store.
func (s *MemStore) QueuedExports(_ context.Context, before time.Time, after uuid.UUID, limit int) ([]Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exportsByID(after, limit, func(x Export) bool { return x.Status == ExportQueued && x.CreatedAt.Before(before) }), nil
}

// ExportsOfUser implements Store.
func (s *MemStore) ExportsOfUser(_ context.Context, userID uuid.UUID) ([]Export, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Export
	for _, x := range s.exports {
		if x.RequestedBy == userID {
			out = append(out, x)
		}
	}
	slices.SortFunc(out, func(a, b Export) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID.String(), b.ID.String()))
	})
	return out, nil
}

// ClearExportIPs implements Store.
func (s *MemStore) ClearExportIPs(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, x := range s.exports {
		if x.RequestedBy == userID && x.IP != nil {
			x.IP = nil
			x.UpdatedAt = s.stamp(x.UpdatedAt)
			s.exports[id] = x
			n++
		}
	}
	return n, nil
}

// PutExport stores x as given (matrix and wiring fixtures: a ready export
// with its file, timestamps included). The event must exist in x.OrgID.
func (s *MemStore) PutExport(x Export) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.events[x.EventID]; !ok || e.OrgID != x.OrgID {
		return ErrNotFound
	}
	if err := checkExport(x); err != nil {
		return err
	}
	s.exports[x.ID] = x
	return nil
}
