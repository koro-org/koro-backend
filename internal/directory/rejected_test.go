package directory_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// TestDirectory_FileRejected_DropsReference (T-176, T-222): file.rejected
// for claim evidence attached to directory drops the claim's file reference
// and deletes the file in one transaction (the claim stays for the
// reviewer, whose view reads meanwhile); the system job audits it as a
// failed evidence submission. The subscription is registered by Subscribe
// (wired since BE-151). A second delivery changes nothing more; another
// module's file is not the claim's.
func TestDirectory_FileRejected_DropsReference(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	ctx := t.Context()
	s := w.student(t, w.uni, false)
	file := uuid.NewV7()
	w.files.put(files.FileMeta{ID: file, Purpose: files.PurposeClubClaimEvidence, UploadedBy: &s.UserID,
		Status: files.StatusActive, ScanStatus: files.ScanSkipped})
	v, err := w.svc.CreateClubClaim(ctx, s, w.club.Club.OrgID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceFileID: &file})
	if err != nil {
		t.Fatal(err)
	}
	// The scan rejects the file (unprocessable): until the subscription has
	// run, the reviewer's view still reads, without the evidence.
	m, _ := w.files.Get(ctx, file)
	m.Status = files.StatusRejected
	w.files.put(m)
	if got, err := w.svc.AdminGetClubClaim(ctx, pad(t), v.Claim.ID); err != nil || got.EvidenceState != directory.EvidenceNotClean ||
		got.Evidence != nil {
		t.Fatalf("admin view of a rejected file = %+v, %v", got, err)
	}
	r := outbox.NewRegistry()
	w.mod.Subscribe(r)
	var sub *outbox.Subscription
	for _, x := range r.For(domainevent.TopicFileRejected) {
		if x.Name == directory.RemoveRejectedEvidenceTaskName {
			sub = &x
		}
	}
	if sub == nil {
		t.Fatalf("file.rejected has no %s subscription", directory.RemoveRejectedEvidenceTaskName)
	}
	deliver := func(id uuid.UUID, module string) {
		t.Helper()
		entity := v.Claim.ID
		env, err := domainevent.New(domainevent.FileRejected{FileID: id, UploaderID: &s.UserID,
			Purpose: string(files.PurposeClubClaimEvidence), Reason: "infected", AttachedModule: module,
			AttachedEntityID: &entity}, "file", id)
		if err != nil {
			t.Fatal(err)
		}
		if err := sub.Handle(ctx, env); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 { // at-least-once delivery
		deliver(file, "directory")
	}
	c, err := w.store.GetClaim(ctx, v.Claim.ID)
	if err != nil || c.EvidenceFileID != nil || c.Status != directory.ClaimPending {
		t.Fatalf("claim after the rejection = %+v, %v", c, err)
	}
	if !slices.Contains(w.removed.ids, file) {
		t.Fatal("the rejected evidence was not deleted")
	}
	var got []audit.Event
	for _, ev := range w.audit.Of(authz.DirectoryClubClaimCreate) {
		if ev.Metadata["operation"] == "evidence_rejected_by_scan" {
			got = append(got, ev)
		}
	}
	if len(got) != 1 || got[0].Outcome != audit.Failure || got[0].SystemJob != directory.RemoveRejectedEvidenceTaskName ||
		got[0].Metadata["file_id"] != file.String() || got[0].Metadata[audit.MetaSubjectUserID] != s.UserID.String() {
		t.Fatalf("system audit = %+v", got)
	}
	other := uuid.NewV7()
	deliver(other, "students")
	if slices.Contains(w.removed.ids, other) {
		t.Fatal("another module's file was deleted")
	}
}
