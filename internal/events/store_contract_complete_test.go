package events_test

import (
	"testing"
	"time"

	"koro/internal/events"
)

func TestStore_CompleteContract_Mem(t *testing.T) {
	t.Parallel()
	completeContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// completeContract is CompleteEnded (T-264): published events that ended
// by the cutoff become completed, oldest end first and at most limit; a
// draft, a cancelled or an upcoming event is left alone.
func completeContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	t.Run("complete ended: published and ended only, oldest first, limited", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		org, by := w.org(t), w.user(t)
		insert := func(title string, at time.Time, status events.Status) events.Event {
			t.Helper()
			e := row(org, by, title, at)
			e.Status = status
			if status == events.StatusDraft {
				e.PublishedAt = nil
			}
			if status == events.StatusCancelled {
				e.CancelledAt = new(t0)
			}
			got, ok, err := s.InsertEvent(ctx, e)
			if err != nil || !ok {
				t.Fatalf("InsertEvent(%s): %v %v", title, ok, err)
			}
			return got
		}
		cutoff := start.Add(3 * time.Hour)
		older := insert("Eski Etkinlik", start.Add(-time.Hour), events.StatusPublished)
		newer := insert("Yeni Biten", start, events.StatusPublished)
		insert("Taslak Biten", start, events.StatusDraft)
		insert("İptal Biten", start, events.StatusCancelled)
		upcoming := insert("Gelecek Etkinlik", cutoff, events.StatusPublished)

		got, err := s.CompleteEnded(ctx, cutoff, 1)
		if err != nil || len(got) != 1 || got[0].ID != older.ID || got[0].Status != events.StatusCompleted {
			t.Fatalf("first batch = %+v, %v", got, err)
		}
		got, err = s.CompleteEnded(ctx, cutoff, 10)
		if err != nil || len(got) != 1 || got[0].ID != newer.ID {
			t.Fatalf("second batch = %+v, %v", got, err)
		}
		if got, err = s.CompleteEnded(ctx, cutoff, 10); err != nil || len(got) != 0 {
			t.Fatalf("third batch = %+v, %v", got, err)
		}
		for _, id := range []events.Event{older, newer} {
			if e, err := s.GetEvent(ctx, id.ID); err != nil || e.Status != events.StatusCompleted {
				t.Fatalf("GetEvent = %+v, %v", e.Status, err)
			}
		}
		if e, _ := s.GetEvent(ctx, upcoming.ID); e.Status != events.StatusPublished {
			t.Fatalf("upcoming event = %s", e.Status)
		}
	})
}
