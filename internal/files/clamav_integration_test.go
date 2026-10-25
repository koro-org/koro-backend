//go:build integration

package files_test

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"koro/internal/audit"
	"koro/internal/files"
	"koro/internal/files/clamavtest"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// TestFiles_ClamAV_EICARUploadTakenOut runs the BE-048 pipeline against
// PostgreSQL, SeaweedFS and a real clamd: a browser upload of a PDF that
// carries the EICAR string is infected — both objects and the row are
// deleted, the verdict and the deletion are in audit_events (same
// transaction), file.rejected is written and the uploader notified — while
// a clean CV is cleared and served with FILES_SCAN_REQUIRED=true.
func TestFiles_ClamAV_EICARUploadTakenOut(t *testing.T) {
	t.Parallel()
	sc, err := files.NewClamAV(files.ClamAVConfig{Addr: clamavtest.Start(t), Timeout: 30 * time.Second, Clock: clock.Real()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sc.Close)
	n := &recordingNotifier{}
	e := newRealEnv(t, func(d *files.Deps) { d.Scanner, d.ScanRequired, d.Notifier = sc, true, n })
	svc := e.mod.Service
	student := actor(t, "student")

	body := slices.Concat(pdf, []byte("% "), clamavtest.EICAR, []byte("\n"))
	up := e.browserUpload(t, body)
	if _, err := svc.CompleteUpload(t.Context(), student, up.FileID, digest(body)); err != nil {
		t.Fatal(err)
	}
	row, _ := e.store.Get(t.Context(), up.FileID)
	e.scan(t, up.FileID)
	if _, err := e.store.Get(t.Context(), up.FileID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("infected row: %v, want deleted", err)
	}
	if e.exists(t, e.b.Quarantine, row.StorageKey) || e.exists(t, e.b.Files, row.StorageKey) {
		t.Fatal("infected object kept")
	}
	recs, err := e.audit.List(t.Context(), audit.Filter{ResourceType: "file", ResourceID: &up.FileID}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 || recs[0].Action != "files.file.delete" || recs[1].Action != "files.upload.complete" ||
		recs[1].Outcome != audit.Failure || !bytes.Contains(recs[1].Metadata, []byte(clamavtest.BodySignature)) {
		for _, r := range recs {
			t.Logf("%s %s %s", r.Action, r.Outcome, r.Metadata)
		}
		t.Fatal("audit of the infected upload")
	}
	var rejected []domainevent.Envelope
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicFileRejected {
			rejected = append(rejected, ev)
		}
	}
	if len(rejected) != 1 || rejected[0].AggregateID != up.FileID {
		t.Fatalf("file.rejected events = %+v", rejected)
	}
	reg := outbox.NewRegistry()
	e.mod.Subscribe(reg)
	if err := reg.For(domainevent.TopicFileRejected)[0].Handle(t.Context(), rejected[0]); err != nil {
		t.Fatal(err)
	}
	if got := n.notices(); len(got) != 1 || got[0].UserID != student.UserID || got[0].Reason != files.RejectInfected {
		t.Fatalf("notices = %+v", got)
	}

	// A clean CV passes the same scanner and is served.
	cv := cvPDF(t)
	ok := e.browserUpload(t, cv)
	if _, err := svc.CompleteUpload(t.Context(), student, ok.FileID, digest(cv)); err != nil {
		t.Fatal(err)
	}
	e.scan(t, ok.FileID)
	if f, _ := e.store.Get(t.Context(), ok.FileID); f.ScanStatus != files.ScanClean || f.Bucket != e.b.Files {
		t.Fatalf("clean CV = %+v", f)
	}
	if _, err := svc.PresignGet(t.Context(), ok.FileID, time.Minute); err != nil {
		t.Fatalf("clean CV not served: %v", err)
	}
}

// TestClamAV_EICAR_Infected talks INSTREAM to a real clamd: the EICAR test
// file is infected, a clean document is clean, both over one pooled
// session; a stream over clamd's StreamMaxLength is refused (no verdict).
func TestClamAV_EICAR_Infected(t *testing.T) {
	t.Parallel()
	addr := clamavtest.Start(t)
	s := newClamAV(t, files.ClamAVConfig{Addr: addr, Timeout: 30 * time.Second, MaxBytes: 30 << 20})
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	pings := s.Dials() // Ping uses its own connection, outside the pool

	v, err := s.Scan(t.Context(), scanObject(clamavtest.EICAR))
	// Both test signatures match the bare file; clamd names the first hit.
	if err != nil || v.Status != files.ScanInfected || !strings.HasPrefix(v.Signature, "Koro.Test.EICAR") ||
		!strings.HasSuffix(v.Signature, ".UNOFFICIAL") {
		t.Fatalf("Scan(EICAR) = %+v, %v; want infected by a Koro.Test.EICAR signature", v, err)
	}
	inPDF := slices.Concat(clamDoc, []byte("% "), clamavtest.EICAR, []byte("\n"))
	v, err = s.Scan(t.Context(), scanObject(inPDF))
	if err != nil || v.Status != files.ScanInfected || !strings.HasPrefix(v.Signature, clamavtest.BodySignature) {
		t.Fatalf("Scan(EICAR in a PDF) = %+v, %v; want infected by %s", v, err, clamavtest.BodySignature)
	}
	big := bytes.Repeat([]byte("Koro clean test content. "), 200000) // 5 MB, many chunks
	for _, b := range [][]byte{clamDoc, big} {
		v, err := s.Scan(t.Context(), scanObject(b))
		if err != nil || v.Status != files.ScanClean {
			t.Fatalf("Scan(clean %d bytes) = %+v, %v", len(b), v, err)
		}
	}
	if got := s.Dials() - pings; got != 1 {
		t.Fatalf("%d connections for 4 sequential scans, want one pooled session", got)
	}

	over := bytes.Repeat([]byte{'k'}, clamavtest.StreamMaxLength+1<<20)
	v, err = s.Scan(t.Context(), scanObject(over))
	if !errors.Is(err, files.ErrClamAVTooLarge) || v.Status != "" {
		t.Fatalf("Scan(over StreamMaxLength) = %+v, %v; want ErrTooLarge", v, err)
	}
	// The pool recovers from the session clamd closed.
	if v, err := s.Scan(t.Context(), scanObject(clamavtest.EICAR)); err != nil || v.Status != files.ScanInfected {
		t.Fatalf("Scan after the limit error = %+v, %v", v, err)
	}
}

// TestClamAV_ContractSuite_Clamd runs the Scanner contract suite (BE-113)
// against a real clamd (the shared container of this package).
func TestClamAV_ContractSuite_Clamd(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	s := newClamAV(t, files.ClamAVConfig{Addr: clamavtest.Start(t), Timeout: 30 * time.Second})
	scannerContract(t, s, scannerFixture{Infected: clamavtest.EICAR})
}
