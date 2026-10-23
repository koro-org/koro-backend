package events_test

import (
	"maps"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
)

func TestStore_MineContract_Mem(t *testing.T) {
	t.Parallel()
	mineStoreContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// mineStoreContract is the contract of ListStaffEvents and StaffRolesOf
// (BE-095) both stores pass: published events that have not ended, of the
// given orgs or staffed by the user, by starts_at then id with a keyset;
// another user's staff rows, drafts, cancelled and ended events never.
func mineStoreContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	t.Run("staff events and roles", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		managed, other, by, me, someone := w.org(t), w.org(t), w.user(t), w.user(t), w.user(t)
		put := func(e events.Event) events.Event {
			t.Helper()
			got, ok, err := s.InsertEvent(ctx, e)
			if err != nil || !ok {
				t.Fatalf("InsertEvent: ok=%v err=%v", ok, err)
			}
			return got
		}
		staff := func(e events.Event, user uuid.UUID, role events.StaffRole) {
			t.Helper()
			if _, err := s.InsertStaff(ctx, events.Staff{EventID: e.ID, UserID: user, OrgID: e.OrgID, Role: role, InvitedBy: by}); err != nil {
				t.Fatal(err)
			}
		}
		byOrg := put(row(managed, by, "Kulüp Söyleşisi", start.Add(2*time.Hour)))
		mine := put(row(other, by, "Görevli Olduğum", start))
		staff(mine, me, events.StaffScanner)
		notMine := put(row(other, by, "Başkasının Kapısı", start.Add(time.Hour)))
		staff(notMine, someone, events.StaffManager)
		draft := row(managed, by, "Taslak", start)
		draft.Status, draft.PublishedAt = events.StatusDraft, nil
		draft = put(draft)
		staff(draft, me, events.StaffManager)
		ended := put(row(managed, by, "Bitmiş", start.Add(-3*time.Hour)))
		staff(ended, me, events.StaffScanner)
		cancelled := row(other, by, "İptal", start)
		cancelled.Status, cancelled.CancelledAt = events.StatusCancelled, new(t0)
		cancelled = put(cancelled)
		staff(cancelled, me, events.StaffScanner)

		ids := func(f events.StaffEventFilter, after *events.After, limit int) []uuid.UUID {
			t.Helper()
			rows, err := s.ListStaffEvents(ctx, f, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			out := make([]uuid.UUID, len(rows))
			for i, r := range rows {
				out[i] = r.ID
			}
			return out
		}
		f := events.StaffEventFilter{UserID: me, OrgIDs: []uuid.UUID{managed}, EndsAfter: start.Add(-time.Hour)}
		if got := ids(f, nil, 10); !slices.Equal(got, []uuid.UUID{mine.ID, byOrg.ID}) {
			t.Fatalf("door list = %v, want %v", got, []uuid.UUID{mine.ID, byOrg.ID})
		}
		if got := ids(f, nil, 1); !slices.Equal(got, []uuid.UUID{mine.ID}) {
			t.Fatalf("first page = %v", got)
		}
		if got := ids(f, &events.After{At: mine.StartsAt, ID: mine.ID}, 1); !slices.Equal(got, []uuid.UUID{byOrg.ID}) {
			t.Fatalf("second page = %v", got)
		}
		// Without a managed org: the staff assignments only.
		if got := ids(events.StaffEventFilter{UserID: me, EndsAfter: f.EndsAfter}, nil, 10); !slices.Equal(got, []uuid.UUID{mine.ID}) {
			t.Fatalf("staff only = %v", got)
		}
		// Another user's assignment is never the caller's.
		if got := ids(events.StaffEventFilter{UserID: someone, EndsAfter: f.EndsAfter}, nil, 10); !slices.Equal(got, []uuid.UUID{notMine.ID}) {
			t.Fatalf("someone's door list = %v", got)
		}
		// An event that has ended by EndsAfter leaves the list.
		if got := ids(events.StaffEventFilter{UserID: me, OrgIDs: f.OrgIDs, EndsAfter: start.Add(90 * time.Minute)}, nil, 10); !slices.Equal(got, []uuid.UUID{byOrg.ID}) {
			t.Fatalf("after the first end = %v", got)
		}
		roles, err := s.StaffRolesOf(ctx, me, []uuid.UUID{mine.ID, byOrg.ID, notMine.ID, draft.ID})
		want := map[uuid.UUID]events.StaffRole{mine.ID: events.StaffScanner, draft.ID: events.StaffManager}
		if err != nil || !maps.Equal(roles, want) {
			t.Fatalf("StaffRolesOf = %v, %v; want %v", roles, err, want)
		}
		if roles, err := s.StaffRolesOf(ctx, me, nil); err != nil || len(roles) != 0 {
			t.Fatalf("StaffRolesOf(none) = %v, %v", roles, err)
		}
	})
}
