package files_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
)

// TestFiles_WritesAudit: every write of a file row is in the audit log
// with its actor, resource and purpose, never the file name (BE-059; 01
// §4.1): upload start and completion (also a failed verification), attach,
// delete by the uploader and by the owning module, the orphan purge and
// the KVKK erasure.
func TestFiles_WritesAudit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	student := actor(t, "student")
	ctx := t.Context()

	up := e.upload(t, "student", cvRequest(len(pdf)), pdf)
	if _, err := e.svc.CompleteUpload(ctx, student, up.FileID, digest(pdf)); err != nil {
		t.Fatal(err)
	}
	bad := e.upload(t, "student", cvRequest(len(png)), png) // PNG declared as PDF
	_, err := e.svc.CompleteUpload(ctx, student, bad.FileID, digest(png))
	wantErr(t, err, files.ErrContentMismatch)
	owner := files.Owner{Module: "students", EntityID: uuid.NewV7(), Purpose: files.PurposeCV}
	if err := e.svc.Attach(ctx, student, up.FileID, owner); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Attach(ctx, student, up.FileID, owner); err != nil { // no-op: not audited again
		t.Fatal(err)
	}
	own := e.active(t, cvRequest(len(pdf)), pdf)
	if err := e.svc.DeleteOwn(ctx, student, own); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(ctx, authz.SystemActor("students.cv_purge"), up.FileID); err != nil {
		t.Fatal(err)
	}
	orphan := e.active(t, cvRequest(len(pdf)), pdf)
	e.clk.Advance(files.DefaultOrphanRetention + time.Minute)
	if err := e.job(t, files.OrphanCleanupJobName).Handle(ctx); err != nil {
		t.Fatal(err)
	}
	erased := e.active(t, cvRequest(len(pdf)), pdf)
	if _, err := e.mod.Privacy.Erase(ctx, authztest.ID("student")); err != nil {
		t.Fatal(err)
	}

	uid := student.UserID
	want := []struct {
		action  string
		outcome audit.Outcome
		file    uuid.UUID
		user    bool   // actor is the student
		job     string // else this system job
		meta    string // a metadata fragment
	}{
		{"files.upload.create", audit.Success, up.FileID, true, "", `"mime":"application/pdf"`},
		{"files.upload.complete", audit.Success, up.FileID, true, "", `"size_bytes":`},
		{"files.upload.create", audit.Success, bad.FileID, true, "", `"purpose":"cv"`},
		{"files.upload.complete", audit.Failure, bad.FileID, true, "", `"error_code":"files.content_mismatch"`},
		{"files.file.attach", audit.Success, up.FileID, true, "", `"owner_module":"students"`},
		{"files.upload.create", audit.Success, own, true, "", `"purpose":"cv"`},
		{"files.upload.complete", audit.Success, own, true, "", `"purpose":"cv"`},
		{"files.file.delete_own", audit.Success, own, true, "", `"attached":false`},
		{"files.file.delete", audit.Success, up.FileID, false, "students.cv_purge", `"attached":true`},
		{"files.upload.create", audit.Success, orphan, true, "", `"purpose":"cv"`},
		{"files.upload.complete", audit.Success, orphan, true, "", `"purpose":"cv"`},
		{"files.orphan.purge", audit.Success, bad.FileID, false, files.OrphanCleanupJobName, `"status":"uploading"`},
		{"files.orphan.purge", audit.Success, orphan, false, files.OrphanCleanupJobName, `"status":"active"`},
		{"files.upload.create", audit.Success, erased, true, "", `"purpose":"cv"`},
		{"files.upload.complete", audit.Success, erased, true, "", `"purpose":"cv"`},
		{"files.file.delete", audit.Success, erased, false, "files.privacy_erase", `"purpose":"cv"`},
	}
	got := e.events(t)
	if len(got) != len(want) {
		for _, r := range got {
			t.Logf("%s %s %v %s", r.Action, r.Outcome, r.ResourceID, r.Metadata)
		}
		t.Fatalf("%d audit events, want %d", len(got), len(want))
	}
	for i, w := range want {
		r := got[i]
		switch {
		case r.Action != w.action || r.Outcome != w.outcome:
			t.Errorf("event %d = %s/%s, want %s/%s", i, r.Action, r.Outcome, w.action, w.outcome)
		case r.ResourceType != authz.TypeFile || r.ResourceID == nil || *r.ResourceID != w.file:
			t.Errorf("event %d (%s): resource %s %v, want file %s", i, w.action, r.ResourceType, r.ResourceID, w.file)
		case w.user && (r.ActorKind != "user" || r.ActorUserID == nil || *r.ActorUserID != uid):
			t.Errorf("event %d (%s): actor %s %v, want the student", i, w.action, r.ActorKind, r.ActorUserID)
		case !w.user && (r.ActorKind != "system" || r.SystemJob != w.job):
			t.Errorf("event %d (%s): actor %s %q, want system %q", i, w.action, r.ActorKind, r.SystemJob, w.job)
		case !bytes.Contains(r.Metadata, []byte(w.meta)):
			t.Errorf("event %d (%s): metadata %s lacks %s", i, w.action, r.Metadata, w.meta)
		case bytes.Contains(r.Metadata, []byte("CV.pdf")):
			t.Errorf("event %d (%s): metadata carries the file name", i, w.action)
		}
		var m map[string]any
		if err := json.Unmarshal(r.Metadata, &m); err != nil || m["purpose"] != "cv" {
			t.Errorf("event %d (%s): metadata %s without purpose", i, w.action, r.Metadata)
		}
	}

	// A module call needs a real actor: the audit log has no anonymous writes.
	wantErrAny(t, e.svc.Attach(ctx, authz.Actor{}, uuid.NewV7(), owner))
	wantErrAny(t, e.svc.Delete(ctx, authz.Anonymous(), uuid.NewV7()))
}

func wantErrAny(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
}
