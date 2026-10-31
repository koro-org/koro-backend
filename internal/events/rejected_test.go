package events_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// rejectFiles is the cover files port that records deletions.
type rejectFiles struct {
	coverFiles
	mu      sync.Mutex
	deleted []uuid.UUID
}

func (f *rejectFiles) Delete(_ context.Context, _ authz.Actor, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *rejectFiles) wasDeleted(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.deleted, id)
}

// rejectFile delivers file.rejected of file (attached to module's entity)
// to events' subscription, as the worker would.
func (e *env) rejectFile(t *testing.T, file uuid.UUID, purpose, module string, entity uuid.UUID) {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	env, err := domainevent.New(domainevent.FileRejected{FileID: file, Purpose: purpose, Reason: "infected",
		AttachedModule: module, AttachedEntityID: &entity}, "file", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.For(domainevent.TopicFileRejected) {
		if s.Name == events.DropRejectedCoverTaskName {
			if err := s.Handle(t.Context(), env); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
			return
		}
	}
	t.Fatalf("file.rejected has no %s subscription", events.DropRejectedCoverTaskName)
}

// TestEvents_FileRejected_DropsReference (T-176): file.rejected for the
// cover attached to an event clears cover_file_id and deletes the file in
// one transaction, whatever the event's status; the ETag moves and no
// event.updated is written (webcache's own file.rejected subscription
// revalidates the event pages, BE-120). A second delivery changes nothing
// more; another module's file, a cover replaced meanwhile and a
// registration export (never scanned) are not the event's cover.
func TestEvents_FileRejected_DropsReference(t *testing.T) {
	t.Parallel()
	rf := &rejectFiles{coverFiles: coverFiles{org: map[uuid.UUID]uuid.UUID{}}}
	e := newEnv(t, func(d *events.Deps) { d.Files = rf })
	ctx := t.Context()
	cover := uuid.NewV7()
	rf.org[cover] = orgID("club_a")
	in := input("Go Atölyesi")
	in.CoverFileID = &cover
	v := e.create(t, "club_admin", "club_a", in)
	if err := e.mem.SetFormVersion(v.Event.ID, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PublishEvent(ctx, persona(t, "club_admin"), v.Event.ID); err != nil {
		t.Fatal(err)
	}
	before, err := e.mem.GetEvent(ctx, v.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	topics := len(e.out.Topics())

	for range 2 { // at-least-once delivery
		e.rejectFile(t, cover, string(files.PurposeEventCover), "events", v.Event.ID)
	}
	after, err := e.mem.GetEvent(ctx, v.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CoverFileID != nil || after.UpdatedAt.Equal(before.UpdatedAt) || after.Status != events.StatusPublished ||
		!rf.wasDeleted(cover) {
		t.Fatalf("event after the rejection: cover %v, updated %v → %v, status %s, deleted %v",
			after.CoverFileID, before.UpdatedAt, after.UpdatedAt, after.Status, rf.deleted)
	}
	if got := e.out.Topics(); len(got) != topics {
		t.Fatalf("events written by the rejection: %v (webcache revalidates on file.rejected)", got[topics:])
	}
	au := e.audit.Of(authz.Action("events.event.drop_rejected_cover"))
	if len(au) != 1 || au[0].SystemJob != events.DropRejectedCoverTaskName || au[0].OrgID == nil ||
		*au[0].OrgID != orgID("club_a") || au[0].Metadata["file_id"] != cover.String() || au[0].Metadata["status"] != "published" {
		t.Fatalf("system audit = %+v", au)
	}
	if g, err := e.svc.GetEvent(ctx, authz.Anonymous(), v.Event.ID); err != nil || g.CoverURL != "" || g.CoverPublicURL != "" {
		t.Fatalf("public event: cover_url %q, cover_public_url %q, %v", g.CoverURL, g.CoverPublicURL, err)
	}

	// Not the event's cover: another module's file, a replaced cover, an
	// export.
	current := uuid.NewV7()
	cur := after
	cur.CoverFileID = &current
	if _, err := e.mem.UpdateEvent(ctx, cur); err != nil {
		t.Fatal(err)
	}
	e.rejectFile(t, current, string(files.PurposeEventCover), "orgs", v.Event.ID)
	old := uuid.NewV7()
	e.rejectFile(t, old, string(files.PurposeEventCover), "events", v.Event.ID)
	export := uuid.NewV7()
	e.rejectFile(t, export, string(files.PurposeEventExport), "events", v.Event.ID)
	got, err := e.mem.GetEvent(ctx, v.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CoverFileID == nil || *got.CoverFileID != current || rf.wasDeleted(current) || !rf.wasDeleted(old) ||
		rf.wasDeleted(export) {
		t.Fatalf("cover %v, deleted %v", got.CoverFileID, rf.deleted)
	}
	if len(e.audit.Of(authz.Action("events.event.drop_rejected_cover"))) != 1 {
		t.Fatal("a rejection that dropped nothing was audited")
	}
}
