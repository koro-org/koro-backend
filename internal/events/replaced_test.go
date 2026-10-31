package events_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/domainevent"
)

// TestEvents_ReplacedCover_DeletesFile (BE-155, T-376): updateEvent deletes
// the cover it replaces or clears, in its transaction, and a deleted draft
// takes its cover file with it; an unchanged cover and a patch without one
// delete nothing. The published event's page is revalidated by the
// event.updated the change writes anyway (BE-106).
func TestEvents_ReplacedCover_DeletesFile(t *testing.T) {
	t.Parallel()
	rf := &rejectFiles{coverFiles: coverFiles{org: map[uuid.UUID]uuid.UUID{}}}
	e := newEnv(t, func(d *events.Deps) { d.Files = rf })
	ctx := t.Context()
	admin := persona(t, "club_admin")
	c1, c2, c3 := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	for _, c := range []uuid.UUID{c1, c2, c3} {
		rf.org[c] = orgID("club_a")
	}
	in := input("Go Atölyesi")
	in.CoverFileID = &c1
	v := e.create(t, "club_admin", "club_a", in)
	if err := e.mem.SetFormVersion(v.Event.ID, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PublishEvent(ctx, admin, v.Event.ID); err != nil {
		t.Fatal(err)
	}
	etag := func() string {
		t.Helper()
		cur, err := e.svc.GetEvent(ctx, admin, v.Event.ID)
		if err != nil {
			t.Fatal(err)
		}
		return cur.ETag
	}

	// The same cover again and a patch without one keep the file.
	title := "Go Atölyesi 2"
	for _, p := range []events.EventPatch{{CoverFileID: &c1}, {Title: &title}} {
		if _, err := e.svc.UpdateEvent(ctx, admin, v.Event.ID, etag(), p); err != nil {
			t.Fatal(err)
		}
	}
	if len(rf.deleted) != 0 {
		t.Fatalf("deleted without a cover change: %v", rf.deleted)
	}
	topics := len(e.out.Topics())
	if _, err := e.svc.UpdateEvent(ctx, admin, v.Event.ID, etag(), events.EventPatch{CoverFileID: &c2}); err != nil {
		t.Fatal(err)
	}
	if !rf.wasDeleted(c1) || rf.wasDeleted(c2) {
		t.Fatalf("replaced cover: deleted %v", rf.deleted)
	}
	if got := e.out.Topics()[topics:]; !slices.Contains(got, domainevent.TopicEventUpdated) {
		t.Fatalf("a replaced cover of a published event wrote %v, want event.updated", got)
	}
	got, err := e.svc.UpdateEvent(ctx, admin, v.Event.ID, etag(), events.EventPatch{Clear: []string{"cover_file_id"}})
	if err != nil || got.Event.CoverFileID != nil || !rf.wasDeleted(c2) {
		t.Fatalf("cleared cover: %+v, %v, deleted %v", got.Event.CoverFileID, err, rf.deleted)
	}

	// A deleted draft takes its cover with it.
	in = input("Taslak Etkinlik")
	in.CoverFileID = &c3
	draft := e.create(t, "club_admin", "club_a", in)
	if err := e.svc.DeleteEventDraft(ctx, admin, draft.Event.ID); err != nil {
		t.Fatal(err)
	}
	if !rf.wasDeleted(c3) || len(rf.deleted) != 3 {
		t.Fatalf("deleted draft: deleted %v", rf.deleted)
	}
}
