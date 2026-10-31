package feed

import (
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"
)

// The in-memory store knows no dispatches or contact requests: every
// removed post counts as unreferenced here (the GORM store checks both).
// Its reports are the GORM store's content_reports (droppedDue).

func (s *MemStore) removedDue(cutoff time.Time) []Post {
	var out []Post
	for _, p := range s.posts {
		if p.Status == StatusRemoved && p.RemovedAt != nil && p.RemovedAt.Before(cutoff) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Post) int { return cmp.Or(a.RemovedAt.Compare(*b.RemovedAt), a.ID.Compare(b.ID)) })
	return out
}

// RemovedPostsDue implements Store.
func (s *MemStore) RemovedPostsDue(_ context.Context, cutoff time.Time, limit int) ([]Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.removedDue(cutoff)
	return out[:min(len(out), max(limit, 0))], nil
}

// CountRemovedPostsDue implements Store.
func (s *MemStore) CountRemovedPostsDue(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.removedDue(cutoff)), nil
}

// PurgePosts implements Store (the cascade of 02 §12.3–12.6).
func (s *MemStore) PurgePosts(_ context.Context, ids []uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, ok := s.posts[id]; !ok {
			continue
		}
		delete(s.posts, id)
		delete(s.media, id)
		s.dropPostRows(id)
		for _, m := range []map[[2]uuid.UUID]time.Time{s.hides, s.likes, s.saves} {
			for k := range m {
				if k[1] == id {
					delete(m, k)
				}
			}
		}
		n++
	}
	return n, nil
}

// droppedDue is droppedDueSQL of the GORM store: dropped before cutoff,
// no report on the post open or in review.
func (s *MemStore) droppedDue(cutoff time.Time) []DroppedMedia {
	open := map[uuid.UUID]bool{}
	for _, r := range s.reports {
		if r.TargetType == TargetPost && r.Status.Unresolved() {
			open[r.TargetID] = true
		}
	}
	var out []DroppedMedia
	for _, m := range s.dropped {
		if m.DroppedAt.Before(cutoff) && !open[m.PostID] {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, compareDropped)
	return out
}

// DroppedMediaDue implements Store.
func (s *MemStore) DroppedMediaDue(_ context.Context, cutoff time.Time, limit int) ([]DroppedMedia, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.droppedDue(cutoff)
	return out[:min(len(out), max(limit, 0))], nil
}

// CountDroppedMediaDue implements Store.
func (s *MemStore) CountDroppedMediaDue(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.droppedDue(cutoff)), nil
}

// PurgeResolvedReports implements Store.
func (s *MemStore) PurgeResolvedReports(_ context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Report
	for _, r := range s.reports {
		if !r.Status.Unresolved() && r.ResolvedAt != nil && r.ResolvedAt.Before(cutoff) {
			due = append(due, r)
		}
	}
	if dryRun {
		return len(due), nil
	}
	slices.SortFunc(due, func(a, b Report) int { return cmp.Or(a.ResolvedAt.Compare(*b.ResolvedAt), a.ID.Compare(b.ID)) })
	due = due[:min(len(due), max(limit, 0))]
	for _, r := range due {
		delete(s.reports, r.ID)
		for i := range s.actions {
			if a := s.actions[i].ReportID; a != nil && *a == r.ID {
				s.actions[i].ReportID = nil
			}
		}
	}
	return len(due), nil
}

// PurgeModerationActions implements Store (the approval check of the
// database function is kvkk's dry-run rule here).
func (s *MemStore) PurgeModerationActions(_ context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []ModerationAction
	for _, a := range s.actions {
		if a.CreatedAt.Before(cutoff) {
			due = append(due, a)
		}
	}
	if dryRun {
		return len(due), nil
	}
	slices.SortFunc(due, func(a, b ModerationAction) int { return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), a.ID.Compare(b.ID)) })
	gone := map[uuid.UUID]bool{}
	for _, a := range due[:min(len(due), max(limit, 0))] {
		gone[a.ID] = true
	}
	s.actions = slices.DeleteFunc(s.actions, func(a ModerationAction) bool { return gone[a.ID] })
	return len(gone), nil
}
