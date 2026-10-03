package main

import (
	"slices"
	"strings"
	"testing"

	"koro/internal/files"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
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
	want := []string{"files.orphan_cleanup 40 4 * * * low", "files.rescan_pending */10 * * * * low"}
	if !slices.Equal(jobs, want) {
		t.Fatalf("files jobs %q, want %q", jobs, want)
	}
	subs := c.infra.subs.For(domainevent.TopicFileUploaded)
	if len(subs) != 1 || subs[0].Name != files.ScanTaskName {
		t.Fatalf("file.uploaded subscriptions %+v", subs)
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
