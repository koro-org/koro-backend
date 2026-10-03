package main

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/files/clamavtest"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/privacy"
	"koro/internal/platform/storage"
)

// testBuckets are the buckets of the in-memory object store of wiring and
// matrix tests.
var testBuckets = files.Buckets{Files: "koro-files-test", Quarantine: "koro-quarantine-test"}

// useMemStore installs the in-memory files store and object storage used by
// wiring and authz matrix tests (05 §13.2). First owner: BE-008. Only
// tickets of the files module edit this file (BE-001 R4). The stores use
// the real clock: these tests do not depend on file timestamps.
func (w *filesWiring) useMemStore() {
	clk := clock.Real()
	w.store = files.NewMemStore(clk.Now)
	w.objects = storage.NewMem(clk)
	w.tx = db.NopTx{}
	w.buckets = &testBuckets
	w.cfg = &config.Files{UploadURLTTL: storage.MaxPutTTL}
}

// TestWire_FilesTasksAndPrivacy pins what the files module registers (01 §6.2,
// §7): the scan subscription, both jobs, the delete retry task and the KVKK
// provider.
func TestWire_FilesTasksAndPrivacy(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "files.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	want := []string{"files.orphan_cleanup 40 4 * * * low", "files.rescan_pending */10 * * * * low",
		"files.rescan_skipped 5-59/10 * * * * low"}
	if !slices.Equal(jobs, want) {
		t.Fatalf("files jobs %q, want %q", jobs, want)
	}
	subs := c.infra.subs.For(domainevent.TopicFileUploaded)
	if len(subs) != 1 || subs[0].Name != files.ScanTaskName {
		t.Fatalf("file.uploaded subscriptions %+v", subs)
	}
	if subs := c.infra.subs.For(domainevent.TopicFileRejected); len(subs) != 1 || subs[0].Name != files.NotifyRejectedTaskName {
		t.Fatalf("file.rejected subscriptions %+v", subs)
	}
	mux, err := c.workerMux(obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(mux.Types(), files.DeleteObjectTaskName) {
		t.Fatalf("worker task types %v lack %s", mux.Types(), files.DeleteObjectTaskName)
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "files" }) {
		t.Fatal("files privacy provider not registered")
	}
}

// TestWire_FilesRefusesScanRequiredWithoutScanner: FILES_SCAN_REQUIRED=true
// with the no-op scanner stops the process at start.
func TestWire_FilesRefusesScanRequiredWithoutScanner(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.files.cfg = &config.Files{ScanRequired: true, UploadURLTTL: storage.MaxPutTTL}
	if err := c.build(); err == nil || !strings.Contains(err.Error(), "FILES_SCAN_REQUIRED") {
		t.Fatalf("build() error = %v", err)
	}
}

// TestWire_FilesBuildsClamAVScanner: FILES_SCANNER=clamav wires the ClamAV
// adapter, which satisfies FILES_SCAN_REQUIRED=true; the optional readyz
// check pings clamd.
func TestWire_FilesBuildsClamAVScanner(t *testing.T) {
	t.Parallel()
	clamd := clamavtest.NewFake(t)
	for _, ready := range []bool{false, true} {
		c := newTestContainer(t)
		c.files.cfg = &config.Files{
			ScanRequired: true, UploadURLTTL: storage.MaxPutTTL, Scanner: config.ScannerClamAV,
			ClamAVAddr: clamd.Addr, ClamAVTimeout: 5 * time.Second, ClamAVMaxConns: 2, ClamAVMaxBytes: 25 << 20,
			ClamAVReadyCheck: ready, RescanBatch: 50,
		}
		if err := c.build(); err != nil {
			t.Fatalf("build with ClamAV: %v", err)
		}
		i := slices.IndexFunc(c.readyChecks, func(ch httpx.Check) bool { return ch.Name == "clamav" })
		if (i >= 0) != ready {
			t.Fatalf("FILES_CLAMAV_READY_CHECK=%v: clamav readyz check present = %v", ready, i >= 0)
		}
		if ready {
			if err := c.readyChecks[i].Run(t.Context()); err != nil {
				t.Fatalf("clamav readyz check: %v", err)
			}
		}
	}

	// A limit below twice the largest upload stops the process.
	c := newTestContainer(t)
	c.files.cfg = &config.Files{ScanRequired: true, UploadURLTTL: storage.MaxPutTTL, Scanner: config.ScannerClamAV,
		ClamAVAddr: clamd.Addr, ClamAVMaxBytes: 10 << 20}
	if err := c.build(); err == nil || !strings.Contains(err.Error(), "FILES_CLAMAV_MAX_BYTES") {
		t.Fatalf("build() error = %v", err)
	}
}

// TestWire_FilesNotifiesUploaderOfRejectedFile: the notify adapter puts a
// fixed in-app notice (file ID only) into the uploader's inbox, once per
// file however often file.rejected is delivered.
func TestWire_FilesNotifiesUploaderOfRejectedFile(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	user, infected, broken := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	n := filesNotifier{c: c}
	ctx := workerContext(t)
	for range 2 {
		for _, r := range []files.RejectionNotice{
			{FileID: infected, UserID: user, Purpose: files.PurposeCV, Reason: files.RejectInfected},
			{FileID: broken, UserID: user, Purpose: files.PurposeAvatar, Reason: files.RejectUnprocessable},
		} {
			if err := n.NotifyFileRejected(ctx, r); err != nil {
				t.Fatalf("NotifyFileRejected(%s): %v", r.Reason, err)
			}
		}
	}
	p, err := c.notify.svc.ListMine(t.Context(), authz.Actor{Kind: authz.KindUser, UserID: user}, notify.InAppQuery{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, it := range p.Items {
		kinds[it.Kind] = it.Data["file_id"]
	}
	if len(p.Items) != 2 || kinds["files.file_infected"] != infected.String() || kinds["files.file_unprocessable"] != broken.String() {
		t.Fatalf("inbox = %+v", p.Items)
	}
}
