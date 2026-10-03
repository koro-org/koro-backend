package files_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/files/clamavtest"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// BE-048: the ClamAV scanner in the upload pipeline, infected files, the
// FILES_SCAN_REQUIRED gate and the rescan of skipped files.

// recordingNotifier records the notices of files.notify_rejected.
type recordingNotifier struct {
	mu  sync.Mutex
	got []files.RejectionNotice
}

func (n *recordingNotifier) NotifyFileRejected(_ context.Context, r files.RejectionNotice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, r)
	return nil
}

func (n *recordingNotifier) notices() []files.RejectionNotice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.got)
}

// rejectedEvents returns the file.rejected events written so far.
func (e *env) rejectedEvents(t *testing.T) []domainevent.FileRejected {
	t.Helper()
	var out []domainevent.FileRejected
	for _, ev := range e.out.Events() {
		if ev.Topic != domainevent.TopicFileRejected {
			continue
		}
		p, err := domainevent.Decode[domainevent.FileRejected](ev)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// deliverRejected hands every file.rejected event to files.notify_rejected.
func (e *env) deliverRejected(t *testing.T) {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	subs := r.For(domainevent.TopicFileRejected)
	if len(subs) != 1 || subs[0].Name != files.NotifyRejectedTaskName {
		t.Fatalf("file.rejected subscriptions = %+v", subs)
	}
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicFileRejected {
			if err := subs[0].Handle(t.Context(), ev); err != nil {
				t.Fatalf("notify_rejected: %v", err)
			}
		}
	}
}

// switchScanner starts as the no-op scanner (not real, "skipped") and can
// be switched to a real one with a verdict per file.
type switchScanner struct {
	real    atomic.Bool
	mu      sync.Mutex
	verdict func(id uuid.UUID) (files.ScanVerdict, error)
	scanned []uuid.UUID
}

func (s *switchScanner) Name() string { return "switch" }
func (s *switchScanner) Real() bool   { return s.real.Load() }
func (s *switchScanner) Scan(ctx context.Context, obj files.ScanObject) (files.ScanVerdict, error) {
	if !s.real.Load() {
		return files.ScanVerdict{Status: files.ScanSkipped}, nil
	}
	rc, err := obj.Open(ctx)
	if err != nil {
		return files.ScanVerdict{}, err
	}
	_ = rc.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanned = append(s.scanned, obj.FileID)
	return s.verdict(obj.FileID)
}

func (s *switchScanner) goReal(verdict func(uuid.UUID) (files.ScanVerdict, error)) {
	s.mu.Lock()
	s.verdict = verdict
	s.mu.Unlock()
	s.real.Store(true)
}

// hookScanner returns a fixed verdict and runs after (if set) first.
type hookScanner struct {
	verdict files.ScanVerdict
	after   func()
}

func (s *hookScanner) Name() string { return "hook" }
func (s *hookScanner) Real() bool   { return true }
func (s *hookScanner) Scan(context.Context, files.ScanObject) (files.ScanVerdict, error) {
	if s.after != nil {
		s.after()
	}
	return s.verdict, nil
}

func clean(uuid.UUID) (files.ScanVerdict, error) {
	return files.ScanVerdict{Status: files.ScanClean}, nil
}

// TestInfected_DeletedAndOwnerNotified: an infected upload loses its
// objects and its row, the verdict is audited, file.rejected (O) is written
// and the uploader is notified; an attached file stays "infected" (never
// served) for its owning module, which gets the owner in the event.
func TestInfected_DeletedAndOwnerNotified(t *testing.T) {
	t.Parallel()
	student := actor(t, "student")
	eicar := files.ScanVerdict{Status: files.ScanInfected, Signature: "Win.Test.EICAR_HDB-1"}
	infected := func() files.Scanner { return &fakeScanner{verdict: eicar} } // one per test: it records what it read

	t.Run("unattached: objects and row deleted, uploader notified", func(t *testing.T) {
		t.Parallel()
		n := &recordingNotifier{}
		e := newEnv(t, func(d *files.Deps) { d.Scanner, d.Notifier = infected(), n })
		id := e.active(t, cvRequest(len(pdf)), pdf)
		key := e.row(t, id).StorageKey
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.Get(t.Context(), id); !errors.Is(err, files.ErrNotFound) {
			t.Fatalf("infected row: %v, want deleted", err)
		}
		for _, b := range []string{buckets.Quarantine, buckets.Files} {
			if _, _, _, ok := e.mem.Object(b, key); ok {
				t.Fatalf("infected object kept in %s", b)
			}
		}
		recs := e.events(t)
		if len(recs) != 4 {
			t.Fatalf("%d audit events, want create, complete, failed verification, delete", len(recs))
		}
		verdict, del := recs[2], recs[3]
		if verdict.Action != string(authz.FilesUploadComplete) || verdict.Outcome != audit.Failure ||
			verdict.SystemJob != files.ScanTaskName || !bytes.Contains(verdict.Metadata, []byte(`"signature":"Win.Test.EICAR_HDB-1"`)) ||
			!bytes.Contains(verdict.Metadata, []byte(`"reason":"infected"`)) || !bytes.Contains(verdict.Metadata, []byte(`"error_code":"files.not_clean"`)) {
			t.Fatalf("verdict audit = %s %s %s %s", verdict.Action, verdict.Outcome, verdict.SystemJob, verdict.Metadata)
		}
		if del.Action != "files.file.delete" || del.Outcome != audit.Success || del.SystemJob != files.ScanTaskName ||
			!bytes.Contains(del.Metadata, []byte(`"attached":false`)) {
			t.Fatalf("delete audit = %s %s %s %s", del.Action, del.Outcome, del.SystemJob, del.Metadata)
		}
		evs := e.rejectedEvents(t)
		uid := student.UserID
		if len(evs) != 1 || evs[0].FileID != id || evs[0].UploaderID == nil || *evs[0].UploaderID != uid ||
			evs[0].Reason != "infected" || evs[0].Purpose != "cv" || evs[0].AttachedModule != "" || evs[0].AttachedEntityID != nil {
			t.Fatalf("file.rejected = %+v", evs)
		}
		e.deliverRejected(t)
		e.deliverRejected(t) // at least once: the adapter dedupes per file
		got := n.notices()
		want := files.RejectionNotice{FileID: id, UserID: uid, Purpose: files.PurposeCV, Reason: files.RejectInfected}
		if len(got) != 2 || got[0] != want {
			t.Fatalf("notices = %+v, want %+v", got, want)
		}
		// A second delivery of file.uploaded changes nothing.
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		if len(e.events(t)) != 4 || len(e.rejectedEvents(t)) != 1 {
			t.Fatal("a repeated scan wrote again")
		}
	})

	t.Run("attached: row stays infected for the owning module", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, func(d *files.Deps) { d.Scanner = infected() })
		id := e.active(t, cvRequest(len(pdf)), pdf)
		owner := files.Owner{Module: "students", EntityID: uuid.NewV7(), Purpose: files.PurposeCV}
		if err := e.svc.Attach(t.Context(), student, id, owner); err != nil { // attached while pending
			t.Fatal(err)
		}
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		f := e.row(t, id)
		if f.ScanStatus != files.ScanInfected || f.ScannedAt == nil || e.mem.Len() != 0 {
			t.Fatalf("attached infected row = %+v, %d objects", f, e.mem.Len())
		}
		_, err := e.svc.PresignGet(t.Context(), id, 0)
		wantErr(t, err, files.ErrNotClean)
		err = e.svc.Attach(t.Context(), student, id, owner)
		wantErr(t, err, files.ErrNotClean)
		evs := e.rejectedEvents(t)
		if len(evs) != 1 || evs[0].AttachedModule != "students" || evs[0].AttachedEntityID == nil || *evs[0].AttachedEntityID != owner.EntityID {
			t.Fatalf("file.rejected = %+v, want the owner", evs)
		}
		// The owning module then deletes it.
		if err := e.svc.Delete(t.Context(), authz.SystemActor("students.cv_detach"), id); err != nil {
			t.Fatal(err)
		}
		e.deliverRejected(t) // no notifier wired: logged, not an error
	})

	t.Run("referenced row stays for orphan cleanup", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, func(d *files.Deps) { d.Scanner = infected() })
		id := e.active(t, cvRequest(len(pdf)), pdf)
		files.Reference(e.store, id)
		if err := e.scan(t, id); err != nil {
			t.Fatalf("scan: %v (a row that cannot be deleted is not a task failure)", err)
		}
		if f := e.row(t, id); f.ScanStatus != files.ScanInfected || e.mem.Len() != 0 {
			t.Fatalf("referenced infected row = %+v, %d objects", f, e.mem.Len())
		}
	})

	t.Run("object deletion failure retries before any verdict", func(t *testing.T) {
		t.Parallel()
		var e *env
		failOnce := &hookScanner{verdict: eicar}
		e = newEnv(t, func(d *files.Deps) { d.Scanner = failOnce })
		failOnce.after = func() { e.mem.FailNext(errors.New("s3 down")) } // hits the deletion of the malware
		id := e.active(t, cvRequest(len(pdf)), pdf)
		if err := e.scan(t, id); err == nil {
			t.Fatal("a failed delete of malware must fail the task")
		}
		if f := e.row(t, id); f.ScanStatus != files.ScanPending || len(e.rejectedEvents(t)) != 0 {
			t.Fatalf("after a failed delete: %+v", f)
		}
		failOnce.after = nil
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.Get(t.Context(), id); !errors.Is(err, files.ErrNotFound) {
			t.Fatalf("retried scan: %v", err)
		}
	})

	t.Run("unprocessable content notifies too (T-163)", func(t *testing.T) {
		t.Parallel()
		n := &recordingNotifier{}
		e := newEnv(t, func(d *files.Deps) { d.Notifier = n })
		bad := []byte("%PDF-1.7\nthis is not a pdf")
		id := e.active(t, cvRequest(len(bad)), bad)
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		if f := e.row(t, id); f.Status != files.StatusRejected {
			t.Fatalf("row = %+v, want rejected", f)
		}
		e.deliverRejected(t)
		if got := n.notices(); len(got) != 1 || got[0].Reason != files.RejectUnprocessable || got[0].FileID != id {
			t.Fatalf("notices = %+v", got)
		}
	})
}

// TestScanRequired_SkippedNotDownloadable: with FILES_SCAN_REQUIRED=true a
// file the no-op scanner skipped is not served until a real scan clears
// it; with false it is. The no-op scanner itself cannot be combined with
// the flag (the process does not start).
func TestScanRequired_SkippedNotDownloadable(t *testing.T) {
	t.Parallel()
	_, err := files.New(files.Deps{ScanRequired: true, Scanner: files.NoopScanner{}})
	if err == nil {
		t.Fatal("FILES_SCAN_REQUIRED=true with the no-op scanner must not start")
	}

	for _, required := range []bool{false, true} {
		sc := &switchScanner{}
		e := newEnv(t, func(d *files.Deps) { d.Scanner = sc })
		id := e.active(t, cvRequest(len(pdf)), pdf)
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		if f := e.row(t, id); f.ScanStatus != files.ScanSkipped || f.Bucket != buckets.Files {
			t.Fatalf("legacy upload = %+v", f)
		}
		// ClamAV arrives; the same rows are served by a module configured
		// with or without the flag.
		sc.goReal(clean)
		strict := e.withScanRequired(t, required, sc)
		_, err := strict.svc.PresignGet(t.Context(), id, time.Minute)
		switch {
		case required && !errors.Is(err, files.ErrNotClean):
			t.Fatalf("FILES_SCAN_REQUIRED=true: skipped file served (err %v)", err)
		case !required && err != nil:
			t.Fatalf("FILES_SCAN_REQUIRED=false: skipped file refused: %v", err)
		}
		if required && problemStatus(err) != 409 {
			t.Fatalf("status %d, want 409 files.not_clean", problemStatus(err))
		}
		// Once rescanned clean it is served either way.
		if err := strict.job(t, files.RescanSkippedJobName).Handle(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f := e.row(t, id); f.ScanStatus != files.ScanClean {
			t.Fatalf("after the rescan: %+v", f)
		}
		if _, err := strict.svc.PresignGet(t.Context(), id, time.Minute); err != nil {
			t.Fatalf("clean file refused: %v", err)
		}
	}
}

// withScanRequired returns a second files module over the stores of e,
// with its own scanner and FILES_SCAN_REQUIRED.
func (e *env) withScanRequired(t *testing.T, required bool, sc files.Scanner) *env {
	t.Helper()
	return newEnv(t, func(d *files.Deps) {
		d.Store, d.Objects, d.Outbox, d.Scanner, d.ScanRequired = e.store, e.mem, e.out, sc, required
	})
}

// TestRescanSkipped_Batches: once a real scanner is wired,
// files.rescan_skipped scans the skipped files FILES_RESCAN_BATCH at a time
// in ID order; a file that fails is passed over and retried on the next
// round, a streak of failures (scanner down) stops the run without losing
// its place, an infected one is taken out of use. Without a real scanner
// the job does nothing.
func TestRescanSkipped_Batches(t *testing.T) {
	t.Parallel()
	sc := &switchScanner{}
	e := newEnv(t, func(d *files.Deps) { d.Scanner, d.RescanBatch = sc, 2 })
	var ids []uuid.UUID
	for range 5 {
		id := e.active(t, cvRequest(len(pdf)), pdf)
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	run := func() {
		t.Helper()
		if err := e.job(t, files.RescanSkippedJobName).Handle(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	statuses := func() []files.ScanStatus {
		var out []files.ScanStatus
		for _, id := range ids {
			f, err := e.store.Get(t.Context(), id)
			if errors.Is(err, files.ErrNotFound) {
				out = append(out, "deleted")
				continue
			}
			out = append(out, f.ScanStatus)
		}
		return out
	}
	want := func(s ...files.ScanStatus) {
		t.Helper()
		if got := statuses(); !slices.Equal(got, s) {
			t.Fatalf("scan statuses = %v, want %v", got, s)
		}
	}
	const sk, cl, inf = files.ScanSkipped, files.ScanClean, files.ScanInfected

	run() // no real scanner: nothing happens
	want(sk, sk, sk, sk, sk)
	if len(sc.scanned) != 0 {
		t.Fatal("the no-op scanner rescanned")
	}

	failing := ids[1]
	sc.goReal(func(id uuid.UUID) (files.ScanVerdict, error) {
		switch id {
		case failing:
			return files.ScanVerdict{}, errors.New("clamav: i/o timeout")
		case ids[3]:
			return files.ScanVerdict{Status: files.ScanInfected, Signature: "Koro.Test"}, nil
		}
		return files.ScanVerdict{Status: files.ScanClean}, nil
	})
	run() // ids[0] clean, ids[1] fails and is passed over
	want(cl, sk, sk, sk, sk)
	run() // ids[2] clean, ids[3] infected
	want(cl, sk, cl, "deleted", sk)
	run() // ids[4]; the end of the table: the next run starts over
	want(cl, sk, cl, "deleted", cl)
	if evs := e.rejectedEvents(t); len(evs) != 1 || evs[0].FileID != ids[3] || evs[0].Reason != "infected" {
		t.Fatalf("file.rejected = %+v", evs)
	}

	// Scanner down: three failures in a row stop the run before them.
	more := make([]uuid.UUID, 0, 3)
	sc.real.Store(false)
	for range 3 {
		id := e.active(t, cvRequest(len(pdf)), pdf)
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		more = append(more, id)
	}
	e2 := newEnv(t, func(d *files.Deps) {
		d.Store, d.Objects, d.Outbox, d.Scanner, d.RescanBatch = e.store, e.mem, e.out, sc, 10
	})
	down := true
	sc.goReal(func(uuid.UUID) (files.ScanVerdict, error) {
		if down {
			return files.ScanVerdict{}, errors.New("clamav: connect: connection refused")
		}
		return files.ScanVerdict{Status: files.ScanClean}, nil
	})
	if err := e2.job(t, files.RescanSkippedJobName).Handle(t.Context()); err != nil {
		t.Fatalf("a stopped run is not a job failure: %v", err)
	}
	for _, id := range append([]uuid.UUID{failing}, more...) {
		if f := e.row(t, id); f.ScanStatus != sk {
			t.Fatalf("scanner down: %s became %s (fail closed: never clean)", id, f.ScanStatus)
		}
	}
	down = false
	if err := e2.job(t, files.RescanSkippedJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range append([]uuid.UUID{failing}, more...) {
		if f := e.row(t, id); f.ScanStatus != cl {
			t.Fatalf("scanner back: %s is %s, want clean", id, f.ScanStatus)
		}
	}
}

// TestClamAV_Timeout_RetriesLater runs the ClamAV adapter in the pipeline
// against a clamd that hangs on the first scan: the scan times out, the
// file stays pending in quarantine (no verdict, never clean) and the task
// fails for a retry; files.rescan_pending enqueues it again and the next
// scan clears it.
func TestClamAV_Timeout_RetriesLater(t *testing.T) {
	t.Parallel()
	clamd := clamavtest.NewFake(t)
	clamd.Stall = func(n int) bool { return n == 1 }
	sc, err := files.NewClamAV(files.ClamAVConfig{Addr: clamd.Addr, Timeout: 300 * time.Millisecond, Clock: clock.Real()})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(d *files.Deps) { d.Scanner, d.ScanRequired = sc, true })
	id := e.active(t, cvRequest(len(pdf)), pdf)
	before := e.row(t, id)

	start := time.Now()
	if err := e.scan(t, id); err == nil {
		t.Fatal("a scanner timeout must fail the task (retry), never pass")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the timeout took %s", took)
	}
	f := e.row(t, id)
	if f.ScanStatus != files.ScanPending || f.Bucket != buckets.Quarantine || f.ScannedAt != nil {
		t.Fatalf("after the timeout: %+v", f)
	}
	if _, _, _, ok := e.mem.Object(buckets.Quarantine, before.StorageKey); !ok {
		t.Fatal("the quarantined upload is gone")
	}
	if _, err := e.svc.PresignGet(t.Context(), id, 0); !errors.Is(err, files.ErrNotClean) {
		t.Fatalf("PresignGet after a timeout = %v, want files.not_clean", err)
	}
	if len(e.rejectedEvents(t)) != 0 || len(e.events(t)) != 2 {
		t.Fatal("a timeout wrote a verdict")
	}

	// Later: files.rescan_pending re-enqueues the file, the scan succeeds.
	e.clk.Advance(files.RescanAfter + time.Minute)
	n := len(e.out.Events())
	if err := e.job(t, files.RescanPendingJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	evs := e.out.Events()[n:]
	if len(evs) != 1 || evs[0].Topic != domainevent.TopicFileUploaded || evs[0].AggregateID != id {
		t.Fatalf("re-enqueued %+v", evs)
	}
	if err := e.subscription(t).Handle(t.Context(), evs[0]); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if f := e.row(t, id); f.ScanStatus != files.ScanClean || f.Bucket != buckets.Files {
		t.Fatalf("after the retry: %+v", f)
	}
	if _, err := e.svc.PresignGet(t.Context(), id, 0); err != nil {
		t.Fatalf("clean file refused: %v", err)
	}
	if clamd.Scans() != 2 {
		t.Fatalf("clamd saw %d scans, want 2", clamd.Scans())
	}
}

// TestClamAV_PipelineFindsEICAR: the adapter in the pipeline against the
// fake clamd: an upload carrying the EICAR string is infected and deleted.
func TestClamAV_PipelineFindsEICAR(t *testing.T) {
	t.Parallel()
	clamd := clamavtest.NewFake(t)
	sc, err := files.NewClamAV(files.ClamAVConfig{Addr: clamd.Addr, Clock: clock.Real()})
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(d *files.Deps) { d.Scanner = sc })
	body := slices.Concat(pdf, []byte("% "), clamavtest.EICAR, []byte("\n"))
	id := e.active(t, cvRequest(len(body)), body)
	if err := e.scan(t, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Get(t.Context(), id); !errors.Is(err, files.ErrNotFound) || e.mem.Len() != 0 {
		t.Fatalf("EICAR upload kept: %v, %d objects", err, e.mem.Len())
	}
	if evs := e.rejectedEvents(t); len(evs) != 1 || evs[0].Reason != "infected" {
		t.Fatalf("file.rejected = %+v", evs)
	}
	if recs := e.events(t); !bytes.Contains(recs[2].Metadata, []byte(clamavtest.FakeSignature)) ||
		!bytes.Contains(recs[2].Metadata, []byte(`"scanner":"clamav"`)) {
		t.Fatalf("verdict audit %s", recs[2].Metadata)
	}
}

// TestNew_ScannerSizeLimitMustCoverUploads: a scanner that cannot take the
// largest rewritten upload stops the process at start.
func TestNew_ScannerSizeLimitMustCoverUploads(t *testing.T) {
	t.Parallel()
	small, err := files.NewClamAV(files.ClamAVConfig{Addr: "127.0.0.1:3310", MaxBytes: 10 << 20, Clock: clock.Real()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = files.New(files.Deps{Scanner: small})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("FILES_CLAMAV_MAX_BYTES")) {
		t.Fatalf("New() error = %v", err)
	}
	if files.MaxScanBytes() != 20<<20 {
		t.Fatalf("MaxScanBytes = %d, want twice the 10 MB upload limit", files.MaxScanBytes())
	}
}
