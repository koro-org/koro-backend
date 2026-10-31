package files_test

import (
	"context"
	"errors"
	"maps"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/problem/problemtest"
)

// TestFileMeta_State (BE-154, T-393): what an owning module shows of an
// attached file, under the rule of PresignGet. A file taken out of use
// after its upload (status rejected, BE-059; scan infected, BE-048) is
// rejected with its reason; an active file is clean once scanned clean, or
// skipped while no scanner is required; anything else is pending.
func TestFileMeta_State(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status   files.Status
		scan     files.ScanStatus
		required bool
		want     files.State
		reason   files.RejectReason
	}{
		{files.StatusActive, files.ScanClean, true, files.StateClean, ""},
		{files.StatusActive, files.ScanSkipped, false, files.StateClean, ""},
		{files.StatusActive, files.ScanSkipped, true, files.StatePending, ""},
		{files.StatusActive, files.ScanPending, false, files.StatePending, ""},
		{files.StatusUploading, files.ScanPending, false, files.StatePending, ""},
		{files.StatusActive, files.ScanInfected, false, files.StateRejected, files.RejectInfected},
		{files.StatusRejected, files.ScanPending, false, files.StateRejected, files.RejectUnprocessable},
		{files.StatusRejected, files.ScanInfected, true, files.StateRejected, files.RejectInfected},
	} {
		m := files.FileMeta{Status: c.status, ScanStatus: c.scan}
		if got := m.State(c.required); got != c.want || m.Rejected() != (c.want == files.StateRejected) || m.RejectReason() != c.reason {
			t.Errorf("%s/%s required=%v: state %s, rejected %v, reason %q; want %s, %q", c.status, c.scan, c.required,
				got, m.Rejected(), m.RejectReason(), c.want, c.reason)
		}
		if err := files.CheckReplace(m); (err == nil) != (c.want == files.StateRejected) {
			t.Errorf("%s/%s: CheckReplace = %v", c.status, c.scan, err)
		}
	}
	problemtest.WantProblem(t, files.CheckReplace(files.FileMeta{Status: files.StatusActive, ScanStatus: files.ScanClean}),
		409, "files.not_rejected")
}

// TestFiles_States_OneRead (BE-154): the states of a list of files in one
// store read — clean (served), pending (uploading; or an object still in
// quarantine), rejected (infected, unprocessable); a missing file has no
// entry and no IDs read nothing. IDs and StateOf are the owning module's
// helpers (each set file once; "" without a file).
func TestFiles_States_OneRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	insert := func(status files.Status, scan files.ScanStatus, bucket string) uuid.UUID {
		t.Helper()
		id, user, size := uuid.NewV7(), actor(t, "club_admin").UserID, int64(42)
		if err := e.store.Insert(t.Context(), files.File{ID: id, Purpose: files.PurposeDeliveryEvidence, UploadedBy: &user,
			OwnerUserID: &user, Bucket: bucket, StorageKey: "k/" + id.String(), DeclaredMIME: files.MIMEPDF, SizeBytes: &size,
			SHA256: make([]byte, 32), Status: status, ScanStatus: scan}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	clean := insert(files.StatusActive, files.ScanSkipped, buckets.Files)
	scanned := insert(files.StatusActive, files.ScanClean, buckets.Files)
	quarantined := insert(files.StatusActive, files.ScanClean, buckets.Quarantine)
	uploading := insert(files.StatusUploading, files.ScanPending, buckets.Quarantine)
	infected := insert(files.StatusActive, files.ScanInfected, buckets.Files)
	unprocessable := insert(files.StatusRejected, files.ScanPending, buckets.Quarantine)
	missing := uuid.NewV7()
	ids := files.IDs(&clean, &scanned, nil, &quarantined, &uploading, &infected, &unprocessable, &missing, &clean)
	if len(ids) != 7 {
		t.Fatalf("IDs = %v; want 7 (nil and the repeat left out)", ids)
	}
	got, err := e.svc.States(t.Context(), ids)
	want := map[uuid.UUID]files.State{clean: files.StateClean, scanned: files.StateClean, quarantined: files.StatePending,
		uploading: files.StatePending, infected: files.StateRejected, unprocessable: files.StateRejected}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("States = %v, %v; want %v", got, err, want)
	}
	if files.StateOf(got, &infected) != files.StateRejected || files.StateOf(got, nil) != "" || files.StateOf(got, &missing) != "" {
		t.Fatal("StateOf")
	}
	if got, err := e.svc.States(t.Context(), nil); err != nil || len(got) != 0 {
		t.Fatalf("States(nil) = %v, %v", got, err)
	}
}

// TestFiles_Evidence_StateAndUploader (BE-159): Evidence is States with
// the uploader of each file (nil for a generated one), in the same read; a
// missing file has no entry.
func TestFiles_Evidence_StateAndUploader(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	user, size := actor(t, "club_admin").UserID, int64(42)
	insert := func(by *uuid.UUID, scan files.ScanStatus) uuid.UUID {
		t.Helper()
		id := uuid.NewV7()
		if err := e.store.Insert(t.Context(), files.File{ID: id, Purpose: files.PurposeDeliveryEvidence, UploadedBy: by,
			OwnerUserID: by, Bucket: buckets.Files, StorageKey: "k/" + id.String(), DeclaredMIME: files.MIMEPDF, SizeBytes: &size,
			SHA256: make([]byte, 32), Status: files.StatusActive, ScanStatus: scan}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	uploaded, generated, missing := insert(&user, files.ScanInfected), insert(nil, files.ScanSkipped), uuid.NewV7()
	ids := []uuid.UUID{uploaded, generated, missing}
	got, err := e.svc.Evidence(t.Context(), ids)
	if err != nil || len(got) != 2 {
		t.Fatalf("Evidence = %v, %v; want two entries", got, err)
	}
	if u := got[uploaded]; u.State != files.StateRejected || u.UploadedBy == nil || *u.UploadedBy != user {
		t.Fatalf("uploaded = %+v", u)
	}
	if g := got[generated]; g.State != files.StateClean || g.UploadedBy != nil {
		t.Fatalf("generated = %+v", g)
	}
	states, err := e.svc.States(t.Context(), ids)
	if err != nil || len(states) != 2 || states[uploaded] != got[uploaded].State || states[generated] != got[generated].State {
		t.Fatalf("States = %v, %v; want the states of Evidence", states, err)
	}
	if got, err := e.svc.Evidence(t.Context(), nil); err != nil || len(got) != 0 {
		t.Fatalf("Evidence(nil) = %v, %v", got, err)
	}
}

// fakeReplacer records Attach; get answers Get.
type fakeReplacer struct {
	get      map[uuid.UUID]files.FileMeta
	attachEr error
	attached []files.Owner
}

func (f *fakeReplacer) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	m, ok := f.get[id]
	if !ok {
		return files.FileMeta{}, files.ErrNotFound
	}
	return m, nil
}

func (f *fakeReplacer) Attach(_ context.Context, _ authz.Actor, _ uuid.UUID, owner files.Owner) error {
	if f.attachEr != nil {
		return f.attachEr
	}
	f.attached = append(f.attached, owner)
	return nil
}

// TestReplace_OnlyARejectedFile (BE-154): the files half of a replacement
// attaches the new file to the record only when the record's file was
// rejected (409 files.not_rejected otherwise, nothing attached) and returns
// the audit metadata (both IDs and the reason); the errors of Get and
// Attach pass through.
func TestReplace_OnlyARejectedFile(t *testing.T) {
	t.Parallel()
	a := actor(t, "finance_admin")
	owner := files.Owner{Module: "ledger", EntityID: uuid.NewV7(), Purpose: files.PurposePayoutReceipt}
	ok, bad, next := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	f := &fakeReplacer{get: map[uuid.UUID]files.FileMeta{
		ok:  {ID: ok, Status: files.StatusRejected, ScanStatus: files.ScanPending},
		bad: {ID: bad, Status: files.StatusActive, ScanStatus: files.ScanClean},
	}}
	_, err := files.Replace(t.Context(), f, a, bad, next, owner)
	problemtest.WantErr(t, err, files.ErrNotRejected)
	if _, err := files.Replace(t.Context(), f, a, uuid.NewV7(), next, owner); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("missing current file: %v", err)
	}
	if len(f.attached) != 0 {
		t.Fatalf("attached %v without a rejected file", f.attached)
	}
	meta, err := files.Replace(t.Context(), f, a, ok, next, owner)
	if err != nil || len(f.attached) != 1 || f.attached[0] != owner {
		t.Fatalf("Replace = %v; attached %v", err, f.attached)
	}
	want := map[string]any{"operation": "replace_rejected_file", "replaced_file_id": ok.String(),
		"replaced_reason": "unprocessable", "file_id": next.String()}
	if !maps.Equal(meta, want) {
		t.Fatalf("meta = %v, want %v", meta, want)
	}
	f.attachEr = files.ErrAlreadyAttached
	_, err = files.Replace(t.Context(), f, a, ok, next, owner)
	problemtest.WantErr(t, err, files.ErrAlreadyAttached)
}
