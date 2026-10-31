package main

import (
	"bytes"
	"crypto/sha256"
	"image"
	"image/png"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/files/clamavtest"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/storage"
	"koro/internal/platform/webcache"
	"koro/internal/students"
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
	w.cfg = &config.Files{UploadURLTTL: storage.MaxPutTTL, Scanner: config.ScannerNoop}
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
		"files.rescan_skipped 5-59/10 * * * * low", "files.publish_public 2-59/10 * * * * low"}
	if !slices.Equal(jobs, want) {
		t.Fatalf("files jobs %q, want %q", jobs, want)
	}
	subs := c.infra.subs.For(domainevent.TopicFileUploaded)
	if len(subs) != 1 || subs[0].Name != files.ScanTaskName {
		t.Fatalf("file.uploaded subscriptions %+v", subs)
	}
	// file.rejected has several subscribers: files notifies the uploader and
	// every owning module drops its reference (01 §6.2, T-176, T-222; the
	// first owner is students, BE-025). Pin files' own one.
	if subs := c.infra.subs.For(domainevent.TopicFileRejected); !slices.ContainsFunc(subs, func(s outbox.Subscription) bool {
		return s.Name == files.NotifyRejectedTaskName
	}) {
		t.Fatalf("file.rejected subscriptions %+v lack %s", subs, files.NotifyRejectedTaskName)
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

// TestWire_FileRejectedOwners (T-176, BE-151): every browser upload purpose
// is classified for file.rejected. The owning module of a personal or
// public content file drops its reference and deletes the file (one
// subscription each); a financial or legal record keeps its reference (no
// subscriber: the row is the record and its audit trail, the file answers
// 409 and the uploader re-uploads through the record's own flow, T-393).
// files notifies the uploader and webcache revalidates a public page. A new
// purpose fails here until it is classified; server-generated purposes
// (event_export, dsr_export) are never scanned, so never rejected.
func TestWire_FileRejectedOwners(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	const keeps = "" // a financial or legal record: the reference stays
	owner := map[files.Purpose]string{
		files.PurposeAvatar:                students.DropRejectedFileTaskName,
		files.PurposeCV:                    students.DropRejectedFileTaskName,
		files.PurposeEduDocument:           identity.RemoveRejectedDocumentTaskName,
		files.PurposeOrgLogo:               orgs.DropRejectedLogoTaskName,
		files.PurposeEventCover:            events.DropRejectedCoverTaskName,
		files.PurposePostMedia:             feed.DropRejectedMediaTaskName,
		files.PurposeClubClaimEvidence:     directory.RemoveRejectedEvidenceTaskName,
		files.PurposePartyDocument:         keeps,
		files.PurposeBankAccountEvidence:   keeps,
		files.PurposeTransferReceipt:       keeps,
		files.PurposePayoutReceipt:         keeps,
		files.PurposeRefundReceipt:         keeps,
		files.PurposeInvoicePDF:            keeps,
		files.PurposeDeliveryEvidence:      keeps,
		files.PurposeRefundRequestEvidence: keeps,
	}
	want := []string{files.NotifyRejectedTaskName, webcache.SubFileRejected}
	for _, p := range files.UploadPurposes() {
		name, ok := owner[p]
		switch {
		case !ok:
			t.Errorf("purpose %s is not classified for file.rejected (T-176)", p)
		case name != keeps && !slices.Contains(want, name):
			want = append(want, name)
		}
	}
	var got []string
	for _, s := range c.infra.subs.For(domainevent.TopicFileRejected) {
		got = append(got, s.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("file.rejected subscriptions %q, want %q", got, want)
	}
}

// TestWire_FilesRefusesScanRequiredWithoutScanner: FILES_SCAN_REQUIRED=true
// with the no-op scanner stops the process at start.
func TestWire_FilesRefusesScanRequiredWithoutScanner(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.files.cfg = &config.Files{ScanRequired: true, UploadURLTTL: storage.MaxPutTTL, Scanner: config.ScannerNoop}
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

// TestWire_FilesPublicMedia: S3_BUCKET_PUBLIC + S3_PUBLIC_BASE_URL reach the
// files module (BE-120): a scanned org logo gets its public URL under the
// configured base, and the copy lands in the public bucket.
func TestWire_FilesPublicMedia(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.infra.platform.Storage.BucketPublic = "koro-public-test"
	c.infra.platform.Storage.PublicBaseURL = "https://media.koro.test"
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	content := buf.Bytes()
	admin, ok := authztest.Get("club_admin")
	if !ok {
		t.Fatal("no club_admin persona")
	}
	org := authztest.ID("club_a")
	up, err := c.files.svc.CreateUpload(t.Context(), admin.Actor, files.UploadRequest{
		Purpose: files.PurposeOrgLogo, Filename: "logo.png", ContentType: files.MIMEPNG, SizeBytes: int64(len(content)), OrgID: &org,
	})
	if err != nil {
		t.Fatal(err)
	}
	mem := c.files.objects.(*storage.Mem)
	sum := sha256.Sum256(content)
	u, err := url.Parse(up.UploadURL)
	if err != nil {
		t.Fatal(err)
	}
	bucket, objKey, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	mem.Upload(bucket, objKey, content, files.MIMEPNG)
	if _, err := c.files.svc.CompleteUpload(t.Context(), admin.Actor, up.FileID, sum[:]); err != nil {
		t.Fatal(err)
	}
	ev, err := domainevent.New(domainevent.FileUploaded{FileID: up.FileID}, "file", up.FileID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range c.infra.subs.For(domainevent.TopicFileUploaded) {
		if err := s.Handle(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
	pub, err := c.files.svc.PublicURL(t.Context(), up.FileID)
	if err != nil || !strings.HasPrefix(pub, "https://media.koro.test/org_logo/"+up.FileID.String()+"-") {
		t.Fatalf("PublicURL = %q, %v", pub, err)
	}
	if _, _, _, ok := mem.Object("koro-public-test", strings.TrimPrefix(pub, "https://media.koro.test/")); !ok {
		t.Fatal("no copy in the public bucket")
	}
}
