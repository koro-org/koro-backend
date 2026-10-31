package orgs_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// rejectFiles is the logo files port that records deletions.
type rejectFiles struct {
	logoFiles
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
// to orgs' subscription, as the worker would.
func (e *env) rejectFile(t *testing.T, file uuid.UUID, purpose, module string, entity uuid.UUID) {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	env, err := domainevent.New(domainevent.FileRejected{FileID: file, Purpose: purpose, Reason: "unprocessable",
		AttachedModule: module, AttachedEntityID: &entity}, "file", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.For(domainevent.TopicFileRejected) {
		if s.Name == orgs.DropRejectedLogoTaskName {
			if err := s.Handle(t.Context(), env); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
			return
		}
	}
	t.Fatalf("file.rejected has no %s subscription", orgs.DropRejectedLogoTaskName)
}

// TestOrgs_FileRejected_DropsReference (T-176): file.rejected for the logo
// attached to an org clears logo_file_id and deletes the file in one
// transaction; the ETag moves and no org.updated is written (webcache's own
// file.rejected subscription revalidates the org page, BE-120). A second
// delivery changes nothing more; another module's file and a logo replaced
// meanwhile are not the org's reference (only a stale file goes).
func TestOrgs_FileRejected_DropsReference(t *testing.T) {
	t.Parallel()
	rf := &rejectFiles{}
	e := newEnv(t, func(d *orgs.Deps) { d.Files = rf })
	ctx := t.Context()
	club := e.club(t, "Robotik Kulübü", "open", false)
	logo := e.withLogo(t, club, orgs.StatusActive)
	before, err := e.store.GetOrg(ctx, club)
	if err != nil {
		t.Fatal(err)
	}
	topics := len(e.topics())

	for range 2 { // at-least-once delivery
		e.rejectFile(t, logo, string(files.PurposeOrgLogo), "orgs", club)
	}
	after, err := e.store.GetOrg(ctx, club)
	if err != nil {
		t.Fatal(err)
	}
	if after.LogoFileID != nil || after.UpdatedAt.Equal(before.UpdatedAt) || !rf.wasDeleted(logo) {
		t.Fatalf("org after the rejection: logo %v, updated %v → %v, deleted %v",
			after.LogoFileID, before.UpdatedAt, after.UpdatedAt, rf.deleted)
	}
	if got := e.topics(); len(got) != topics {
		t.Fatalf("events written by the rejection: %v (webcache revalidates on file.rejected)", got[topics:])
	}
	au := e.audit.Of(authz.Action("orgs.org.drop_rejected_logo"))
	if len(au) != 1 || au[0].SystemJob != orgs.DropRejectedLogoTaskName || au[0].OrgID == nil || *au[0].OrgID != club ||
		au[0].Metadata["file_id"] != logo.String() || au[0].Metadata["reason"] != "unprocessable" {
		t.Fatalf("system audit = %+v", au)
	}

	// Not the org's reference: another module's file, a replaced logo.
	current := e.withLogo(t, club, orgs.StatusActive)
	e.rejectFile(t, current, string(files.PurposeOrgLogo), "events", club)
	old := uuid.NewV7()
	e.rejectFile(t, old, string(files.PurposeOrgLogo), "orgs", club)
	o, err := e.store.GetOrg(ctx, club)
	if err != nil {
		t.Fatal(err)
	}
	if o.LogoFileID == nil || *o.LogoFileID != current || rf.wasDeleted(current) || !rf.wasDeleted(old) {
		t.Fatalf("logo %v, deleted %v", o.LogoFileID, rf.deleted)
	}
	if len(e.audit.Of(authz.Action("orgs.org.drop_rejected_logo"))) != 1 {
		t.Fatal("a rejection that dropped nothing was audited")
	}
}
