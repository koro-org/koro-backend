package files_test

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/storage"
)

func TestCreateUpload_PurposeAllowList(t *testing.T) {
	t.Parallel()
	org := authztest.ID("club_a")
	tests := []struct {
		name    string
		persona string
		req     files.UploadRequest
		want    error // nil = allowed
	}{
		{"cv pdf", "student", cvRequest(100), nil},
		{"avatar webp", "student", files.UploadRequest{Purpose: files.PurposeAvatar, Filename: "a.webp", ContentType: files.MIMEWebP, SizeBytes: 10}, nil},
		{"org logo of own club", "club_admin", files.UploadRequest{Purpose: files.PurposeOrgLogo, Filename: "l.png", ContentType: files.MIMEPNG, SizeBytes: 10, OrgID: &org}, nil},
		{"cv as image", "student", files.UploadRequest{Purpose: files.PurposeCV, Filename: "cv.png", ContentType: files.MIMEPNG, SizeBytes: 10}, files.ErrInvalidUpload},
		{"svg avatar", "student", files.UploadRequest{Purpose: files.PurposeAvatar, Filename: "a.svg", ContentType: "image/svg+xml", SizeBytes: 10}, files.ErrInvalidUpload},
		{"html document", "student", files.UploadRequest{Purpose: files.PurposeEduDocument, Filename: "x.html", ContentType: "text/html", SizeBytes: 10}, files.ErrInvalidUpload},
		{"docx cv", "student", files.UploadRequest{Purpose: files.PurposeCV, Filename: "cv.docx", ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", SizeBytes: 10}, files.ErrInvalidUpload},
		{"unknown purpose", "student", files.UploadRequest{Purpose: "selfie", Filename: "s.png", ContentType: files.MIMEPNG, SizeBytes: 10}, files.ErrInvalidUpload},
		{"event export is server-generated", "club_admin", files.UploadRequest{Purpose: files.PurposeEventExport, Filename: "e.csv", ContentType: files.MIMECSV, SizeBytes: 10, OrgID: &org}, files.ErrInvalidUpload},
		{"dsr export is server-generated", "student", files.UploadRequest{Purpose: files.PurposeDSRExport, Filename: "d.zip", ContentType: files.MIMEZIP, SizeBytes: 10}, files.ErrInvalidUpload},
		{"org purpose without org", "club_admin", files.UploadRequest{Purpose: files.PurposeOrgLogo, Filename: "l.png", ContentType: files.MIMEPNG, SizeBytes: 10}, files.ErrInvalidUpload},
		{"personal purpose with org", "student", files.UploadRequest{Purpose: files.PurposeCV, Filename: "cv.pdf", ContentType: files.MIMEPDF, SizeBytes: 10, OrgID: &org}, files.ErrInvalidUpload},
		{"control character in name", "student", files.UploadRequest{Purpose: files.PurposeCV, Filename: "cv\x00.pdf", ContentType: files.MIMEPDF, SizeBytes: 10}, files.ErrInvalidUpload},
		{"path-only name", "student", files.UploadRequest{Purpose: files.PurposeCV, Filename: "../", ContentType: files.MIMEPDF, SizeBytes: 10}, files.ErrInvalidUpload},
		// k30: the purpose decides who may upload (403), the org who sees it (404).
		{"unverified user cv", "user_unverified", cvRequest(100), authz.ErrForbidden},
		{"receipt by a student", "student", files.UploadRequest{Purpose: files.PurposeTransferReceipt, Filename: "r.pdf", ContentType: files.MIMEPDF, SizeBytes: 10}, authz.ErrForbidden},
		{"logo of a foreign club", "other_club_admin", files.UploadRequest{Purpose: files.PurposeOrgLogo, Filename: "l.png", ContentType: files.MIMEPNG, SizeBytes: 10, OrgID: &org}, authz.ErrNotFound},
		// BE-093: the seller's delivery evidence (photo or PDF, org required; k30: the markers of its lines).
		{"delivery evidence photo", "club_admin", files.UploadRequest{Purpose: files.PurposeDeliveryEvidence, Filename: "stant.webp", ContentType: files.MIMEWebP, SizeBytes: 10, OrgID: &org}, nil},
		{"delivery evidence pdf", "club_admin", files.UploadRequest{Purpose: files.PurposeDeliveryEvidence, Filename: "teslim.pdf", ContentType: files.MIMEPDF, SizeBytes: 10, OrgID: &org}, nil},
		{"delivery evidence without org", "club_admin", files.UploadRequest{Purpose: files.PurposeDeliveryEvidence, Filename: "teslim.pdf", ContentType: files.MIMEPDF, SizeBytes: 10}, files.ErrInvalidUpload},
		{"delivery evidence as csv", "club_admin", files.UploadRequest{Purpose: files.PurposeDeliveryEvidence, Filename: "teslim.csv", ContentType: files.MIMECSV, SizeBytes: 10, OrgID: &org}, files.ErrInvalidUpload},
		{"delivery evidence by a club member", "club_member", files.UploadRequest{Purpose: files.PurposeDeliveryEvidence, Filename: "teslim.pdf", ContentType: files.MIMEPDF, SizeBytes: 10, OrgID: &org}, authz.ErrForbidden},
		{"delivery evidence for a foreign club", "other_club_admin", files.UploadRequest{Purpose: files.PurposeDeliveryEvidence, Filename: "teslim.pdf", ContentType: files.MIMEPDF, SizeBytes: 10, OrgID: &org}, authz.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			up, err := e.svc.CreateUpload(t.Context(), actor(t, tt.persona), tt.req)
			if tt.want != nil {
				wantErr(t, err, tt.want)
				if e.mem.Len() != 0 {
					t.Fatal("a rejected upload left an object behind")
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateUpload: %v", err)
			}
			f := e.row(t, up.FileID)
			if f.Status != files.StatusUploading || f.ScanStatus != files.ScanPending || f.Bucket != buckets.Quarantine {
				t.Fatalf("new row = %+v", f)
			}
			if strings.Contains(f.StorageKey, "CV") || !strings.HasPrefix(f.StorageKey, string(tt.req.Purpose)+"/2026/10/") ||
				!strings.HasSuffix(f.StorageKey, up.FileID.String()) {
				t.Fatalf("storage key %q must be <purpose>/<yyyy>/<mm>/<id> without the original name", f.StorageKey)
			}
			if up.UploadHeaders["Content-Type"] != tt.req.ContentType || !strings.Contains(up.UploadURL, f.StorageKey) {
				t.Fatalf("upload = %+v", up)
			}
			if got := up.ExpiresAt.Sub(e.clk.Now()); got != storage.MaxPutTTL {
				t.Fatalf("upload URL lifetime = %s, want 10m (04 §1.14)", got)
			}
		})
	}
}

func TestCreateUpload_SizeLimitPerPurpose(t *testing.T) {
	t.Parallel()
	req := func(p files.Purpose, mime string, size int64) files.UploadRequest {
		return files.UploadRequest{Purpose: p, Filename: "f", ContentType: mime, SizeBytes: size}
	}
	tests := []struct {
		name string
		req  files.UploadRequest
		want error
	}{
		{"avatar at 5 MB", req(files.PurposeAvatar, files.MIMEJPEG, 5*files.MB), nil},
		{"avatar over 5 MB", req(files.PurposeAvatar, files.MIMEJPEG, 5*files.MB+1), files.ErrTooLarge},
		{"cv at 10 MB", req(files.PurposeCV, files.MIMEPDF, 10*files.MB), nil},
		{"cv over 10 MB", req(files.PurposeCV, files.MIMEPDF, 10*files.MB+1), files.ErrTooLarge},
		{"edu document over 10 MB", req(files.PurposeEduDocument, files.MIMEPNG, 10*files.MB+1), files.ErrTooLarge},
		{"empty file", req(files.PurposeCV, files.MIMEPDF, 0), files.ErrInvalidUpload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := newEnv(t).svc.CreateUpload(t.Context(), actor(t, "student"), tt.req)
			if tt.want == nil && err != nil {
				t.Fatalf("CreateUpload: %v", err)
			}
			if tt.want != nil {
				wantErr(t, err, tt.want)
			}
		})
	}
	// The browser upload limit is 10 MB for every purpose (rapor §8.2).
	for _, p := range files.UploadPurposes() {
		if p.MaxBytes() > files.MaxUploadBytes || p.MaxBytes() <= 0 {
			t.Errorf("%s allows %d bytes", p, p.MaxBytes())
		}
	}
}

func TestCreateUpload_IdempotencyKeyReplays(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := idempotencyCtx(t, "k-upload-1")
	a := actor(t, "student")
	first, err := e.svc.CreateUpload(ctx, a, cvRequest(10))
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.CreateUpload(ctx, a, cvRequest(10))
	if err != nil || second.FileID != first.FileID {
		t.Fatalf("replay = %v, %v; want the first upload %s", second.FileID, err, first.FileID)
	}
}

func TestCompleteUpload_ActivatesAndQueuesScan(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	up := e.upload(t, "student", cvRequest(len(pdf)), pdf)
	m, err := e.svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, digest(pdf))
	if err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	if m.Status != files.StatusActive || m.ScanStatus != files.ScanPending || m.MIME != files.MIMEPDF || m.SizeBytes != int64(len(pdf)) {
		t.Fatalf("meta = %+v", m)
	}
	if got := e.out.Topics(); len(got) != 1 || got[0] != domainevent.TopicFileUploaded {
		t.Fatalf("events = %v, want [file.uploaded]", got)
	}
	// A retry with the same checksum is idempotent; another checksum conflicts.
	if _, err := e.svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, digest(pdf)); err != nil {
		t.Fatalf("repeated CompleteUpload: %v", err)
	}
	_, err = e.svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, digest(png))
	wantErr(t, err, files.ErrNotUploading)
	if len(e.out.Events()) != 1 {
		t.Fatal("a repeated completion queued the scan again")
	}
	// Only the uploader sees the file (03 §3.6).
	_, err = e.svc.CompleteUpload(t.Context(), actor(t, "student_b"), up.FileID, digest(pdf))
	wantErr(t, err, authz.ErrNotFound)
}

func TestCompleteUpload_MagicByteMismatch_422(t *testing.T) {
	t.Parallel()
	for name, content := range map[string][]byte{
		"png declared as pdf": png,
		"svg declared as pdf": []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"html polyglot":       []byte("<html>%PDF-1.7</html>"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			up := e.upload(t, "student", cvRequest(len(content)), content)
			_, err := e.svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, digest(content))
			wantErr(t, err, files.ErrContentMismatch)
			if pe := problemStatus(err); pe != 422 {
				t.Fatalf("status = %d, want 422", pe)
			}
			f := e.row(t, up.FileID)
			if f.Status != files.StatusUploading || e.mem.Len() != 0 || len(e.out.Events()) != 0 {
				t.Fatalf("after a rejected upload: status %s, %d objects, %d events", f.Status, e.mem.Len(), len(e.out.Events()))
			}
		})
	}
	// The detected type must also equal the declared one (an image purpose).
	e := newEnv(t)
	req := files.UploadRequest{Purpose: files.PurposeAvatar, Filename: "a.jpg", ContentType: files.MIMEJPEG, SizeBytes: int64(len(png))}
	up := e.upload(t, "student", req, png)
	_, err := e.svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, digest(png))
	wantErr(t, err, files.ErrContentMismatch)
}

func TestCompleteUpload_ChecksumMismatch_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	up := e.upload(t, "student", cvRequest(len(pdf)), pdf)
	_, err := e.svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, digest([]byte("something else")))
	wantErr(t, err, files.ErrChecksumMismatch)
	if problemStatus(err) != 422 {
		t.Fatalf("status = %d, want 422", problemStatus(err))
	}
	if f := e.row(t, up.FileID); f.Status != files.StatusUploading || e.mem.Len() != 0 {
		t.Fatalf("status %s with %d objects; want uploading and the object deleted", f.Status, e.mem.Len())
	}
	_, err = e.svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, []byte{1, 2, 3})
	wantErr(t, err, files.ErrChecksumMismatch)
}

func TestCompleteUpload_SizeAndObjectChecks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := actor(t, "student")

	up := e.upload(t, "student", cvRequest(len(pdf)), nil)
	_, err := e.svc.CompleteUpload(t.Context(), a, up.FileID, digest(pdf))
	wantErr(t, err, files.ErrObjectMissing)

	bigger := append(bytes.Clone(pdf), ' ')
	up = e.upload(t, "student", cvRequest(len(pdf)), bigger)
	_, err = e.svc.CompleteUpload(t.Context(), a, up.FileID, digest(bigger))
	wantErr(t, err, files.ErrSizeMismatch)

	// The object exceeds the purpose limit whatever was declared.
	huge := append(bytes.Clone(pdf), make([]byte, 10*files.MB)...)
	up = e.upload(t, "student", cvRequest(len(pdf)), huge)
	_, err = e.svc.CompleteUpload(t.Context(), a, up.FileID, digest(huge))
	wantErr(t, err, files.ErrTooLarge)
	if e.mem.Len() != 0 {
		t.Fatalf("%d rejected objects left in quarantine", e.mem.Len())
	}
}

func TestScan_NoopSkipsAndMovesOutOfQuarantine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.active(t, cvRequest(len(pdf)), pdf)
	before := e.row(t, id)
	if err := e.scan(t, id); err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := e.row(t, id)
	if f.ScanStatus != files.ScanSkipped || f.Bucket != buckets.Files || f.ScannedAt == nil {
		t.Fatalf("after scan: %+v", f)
	}
	if _, _, _, ok := e.mem.Object(buckets.Quarantine, before.StorageKey); ok {
		t.Fatal("quarantine copy not deleted")
	}
	// The main bucket holds the rewritten PDF (BE-059) and the row
	// describes exactly those bytes.
	data, ctype, disp, ok := e.mem.Object(buckets.Files, f.StorageKey)
	if !ok || files.DetectMIME(data) != files.MIMEPDF || ctype != files.MIMEPDF || !strings.HasPrefix(disp, "attachment") {
		t.Fatalf("main object: ok=%v type=%q disposition=%q", ok, ctype, disp)
	}
	if !bytes.Equal(f.SHA256, digest(data)) || *f.SizeBytes != int64(len(data)) {
		t.Fatalf("row checksum/size do not describe the stored object")
	}
	// Idempotent: a second delivery changes nothing.
	if err := e.scan(t, id); err != nil {
		t.Fatal(err)
	}
	// A deleted file is ignored.
	if err := e.scan(t, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
}

func TestScan_VerdictsAndErrors(t *testing.T) {
	t.Parallel()
	t.Run("clean", func(t *testing.T) {
		t.Parallel()
		sc := &fakeScanner{verdict: files.ScanVerdict{Status: files.ScanClean}}
		e := newEnv(t, func(d *files.Deps) { d.Scanner = sc })
		id := e.active(t, cvRequest(len(pdf)), pdf)
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		if f := e.row(t, id); f.ScanStatus != files.ScanClean || f.Bucket != buckets.Files || !bytes.Equal(sc.read, pdf) {
			t.Fatalf("clean scan: %+v, scanner read %d bytes", f, len(sc.read))
		}
	})
	t.Run("infected", func(t *testing.T) {
		t.Parallel()
		sc := &fakeScanner{verdict: files.ScanVerdict{Status: files.ScanInfected, Signature: "Eicar-Test-Signature"}}
		e := newEnv(t, func(d *files.Deps) { d.Scanner = sc })
		id := e.active(t, cvRequest(len(pdf)), pdf)
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		// BE-048: the object and the (unattached) row are gone; the
		// verdict is in the audit log (TestInfected_DeletedAndOwnerNotified).
		if _, err := e.store.Get(t.Context(), id); !errors.Is(err, files.ErrNotFound) || e.mem.Len() != 0 {
			t.Fatalf("infected: row lookup %v with %d objects", err, e.mem.Len())
		}
		_, err := e.svc.PresignGet(t.Context(), id, 0)
		wantErr(t, err, files.ErrNotFound)
		err = e.svc.Attach(t.Context(), actor(t, "student"), id, files.Owner{Module: "students", EntityID: uuid.NewV7(), Purpose: files.PurposeCV})
		wantErr(t, err, files.ErrNotFound)
	})
	t.Run("scanner error keeps the file pending", func(t *testing.T) {
		t.Parallel()
		sc := &fakeScanner{err: errors.New("clamd: i/o timeout")}
		e := newEnv(t, func(d *files.Deps) { d.Scanner = sc })
		id := e.active(t, cvRequest(len(pdf)), pdf)
		if err := e.scan(t, id); err == nil {
			t.Fatal("a scanner error must fail the task (retry)")
		}
		if f := e.row(t, id); f.ScanStatus != files.ScanPending || f.Bucket != buckets.Quarantine {
			t.Fatalf("after a scanner error: %+v", f)
		}
	})
}

func TestRescanPending_ReenqueuesStuckScans(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	stuck := e.active(t, cvRequest(len(pdf)), pdf)
	e.clk.Advance(files.RescanAfter + time.Minute)
	fresh := e.active(t, cvRequest(len(pdf)), pdf)
	before := len(e.out.Events())
	if err := e.job(t, files.RescanPendingJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	evs := e.out.Events()[before:]
	if len(evs) != 1 || evs[0].AggregateID != stuck {
		t.Fatalf("re-enqueued %d events (fresh %s, stuck %s): %+v", len(evs), fresh, stuck, evs)
	}
}

func TestPresignGet_PendingScan_Rejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.active(t, cvRequest(len(pdf)), pdf)
	_, err := e.svc.PresignGet(t.Context(), id, time.Minute)
	wantErr(t, err, files.ErrNotClean)
	if problemStatus(err) != 409 {
		t.Fatalf("status = %d, want 409 files.not_clean (04 §1.14)", problemStatus(err))
	}
	up := e.upload(t, "student", cvRequest(len(pdf)), pdf)
	_, err = e.svc.PresignGet(t.Context(), up.FileID, time.Minute)
	wantErr(t, err, files.ErrNotActive)
}

func TestPresignGet_SkippedAllowedWhenScanNotRequired(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	req := cvRequest(len(pdf))
	req.Filename = "Çağla Işık CV.exe"
	up := e.upload(t, "student", req, pdf)
	if _, err := e.svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, digest(pdf)); err != nil {
		t.Fatal(err)
	}
	if err := e.scan(t, up.FileID); err != nil {
		t.Fatal(err)
	}
	d, err := e.svc.PresignGet(t.Context(), up.FileID, 0)
	if err != nil {
		t.Fatalf("PresignGet(skipped, scan not required): %v", err)
	}
	if got := d.ExpiresAt.Sub(e.clk.Now()); got != storage.MaxGetTTL {
		t.Fatalf("default lifetime = %s, want 5m", got)
	}
	u, err := url.Parse(d.URL)
	if err != nil {
		t.Fatal(err)
	}
	disp := u.Query().Get("response-content-disposition")
	if !strings.HasPrefix(disp, "attachment;") || !strings.Contains(disp, ".pdf") || strings.Contains(disp, ".exe") ||
		u.Query().Get("response-content-type") != files.MIMEPDF {
		t.Fatalf("download URL %s: disposition %q", d.URL, disp)
	}
	for _, ttl := range []time.Duration{storage.MaxGetTTL + time.Second, time.Hour, -time.Second} {
		if _, err := e.svc.PresignGet(t.Context(), up.FileID, ttl); !errors.Is(err, storage.ErrInvalidTTL) {
			t.Fatalf("PresignGet(ttl %s) error = %v, want ErrInvalidTTL", ttl, err)
		}
	}

	// FILES_SCAN_REQUIRED=true: skipped is no longer enough. The module
	// refuses the flag with the no-op scanner, so a real scanner is wired
	// that did not run on this file.
	strict := newEnv(t, func(d *files.Deps) {
		d.ScanRequired, d.Scanner = true, &fakeScanner{verdict: files.ScanVerdict{Status: files.ScanSkipped}}
	})
	id := strict.active(t, cvRequest(len(pdf)), pdf)
	if err := strict.scan(t, id); err != nil {
		t.Fatal(err)
	}
	_, err = strict.svc.PresignGet(t.Context(), id, time.Minute)
	wantErr(t, err, files.ErrNotClean)
}

func TestNew_ScanRequiredNeedsRealScanner(t *testing.T) {
	t.Parallel()
	_, err := files.New(files.Deps{ScanRequired: true})
	if err == nil || !strings.Contains(err.Error(), "FILES_SCAN_REQUIRED") || !strings.Contains(err.Error(), "Store") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestDeleteMyFile_Attached_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := actor(t, "student")
	id := e.active(t, cvRequest(len(pdf)), pdf)
	if err := e.svc.Attach(t.Context(), actor(t, "student"), id, files.Owner{Module: "students", EntityID: uuid.NewV7(), Purpose: files.PurposeCV}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	err := e.svc.DeleteOwn(t.Context(), a, id)
	wantErr(t, err, files.ErrAttached)
	if problemStatus(err) != 409 {
		t.Fatalf("status = %d, want 409", problemStatus(err))
	}
	if e.mem.Len() != 1 {
		t.Fatal("the attached file's object was deleted")
	}

	// An unattached file: only its uploader deletes it, objects included.
	other := e.active(t, cvRequest(len(pdf)), pdf)
	wantErr(t, e.svc.DeleteOwn(t.Context(), actor(t, "student_b"), other), authz.ErrNotFound)
	if err := e.svc.DeleteOwn(t.Context(), a, other); err != nil {
		t.Fatalf("DeleteOwn: %v", err)
	}
	wantErr(t, e.svc.DeleteOwn(t.Context(), a, other), files.ErrNotFound)
	if e.mem.Len() != 1 {
		t.Fatalf("%d objects left, want only the attached one", e.mem.Len())
	}
}

func TestAttach_Rules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	owner := files.Owner{Module: "students", EntityID: uuid.NewV7(), Purpose: files.PurposeCV}
	up := e.upload(t, "student", cvRequest(len(pdf)), pdf)
	wantErr(t, e.svc.Attach(t.Context(), actor(t, "student"), up.FileID, owner), files.ErrNotActive)

	id := e.active(t, cvRequest(len(pdf)), pdf)
	wrong := owner
	wrong.Purpose = files.PurposeAvatar
	wantErr(t, e.svc.Attach(t.Context(), actor(t, "student"), id, wrong), files.ErrPurposeMismatch)
	if err := e.svc.Attach(t.Context(), actor(t, "student"), id, owner); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Attach(t.Context(), actor(t, "student"), id, owner); err != nil {
		t.Fatalf("re-attaching to the same owner: %v", err)
	}
	another := owner
	another.EntityID = uuid.NewV7()
	wantErr(t, e.svc.Attach(t.Context(), actor(t, "student"), id, another), files.ErrAlreadyAttached)
	if m, _ := e.svc.Get(t.Context(), id); m.AttachedModule != "students" || m.AttachedEntityID != owner.EntityID {
		t.Fatalf("meta = %+v", m)
	}
	if err := e.svc.Attach(t.Context(), actor(t, "student"), id, files.Owner{Module: "Bad Module", EntityID: owner.EntityID, Purpose: files.PurposeCV}); err == nil {
		t.Fatal("invalid owner accepted")
	}
	wantErr(t, e.svc.Attach(t.Context(), actor(t, "student"), uuid.NewV7(), owner), files.ErrNotFound)
}

func TestDelete_RemovesObjectsAndRetriesFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.active(t, cvRequest(len(pdf)), pdf)
	if err := e.scan(t, id); err != nil {
		t.Fatal(err)
	}
	e.mem.FailNext(errors.New("storage down"))
	if err := e.svc.Delete(t.Context(), actor(t, "student"), id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := e.store.Get(t.Context(), id); !errors.Is(err, files.ErrNotFound) {
		t.Fatal("row not deleted")
	}
	tasks := e.q.Tasks()
	if len(tasks) != 1 || tasks[0].Type != files.DeleteObjectTaskName {
		t.Fatalf("retry tasks = %+v", tasks)
	}
	if err := e.svc.Delete(t.Context(), actor(t, "student"), id); err != nil {
		t.Fatalf("Delete of a missing file: %v", err)
	}
	// The retry task removes the object.
	var task func() error
	for _, tk := range e.mod.Tasks() {
		if tk.Name == files.DeleteObjectTaskName {
			task = func() error { return tk.Handle(t.Context(), tasks[0].Payload) }
		}
	}
	if err := task(); err != nil {
		t.Fatal(err)
	}
	if e.mem.Len() != 0 {
		t.Fatalf("%d objects left", e.mem.Len())
	}

	// A file another table still references is not deleted.
	ref := e.active(t, cvRequest(len(pdf)), pdf)
	files.Reference(e.store, ref)
	wantErr(t, e.svc.Delete(t.Context(), actor(t, "student"), ref), files.ErrReferenced)
	if e.mem.Len() != 1 {
		t.Fatal("the referenced file's object was deleted")
	}
}

func TestPutGenerated_StoresCleanFileInMainBucket(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	user := authztest.ID("student")
	csv := []byte("ad;soyad\n")
	id, err := e.svc.PutGenerated(t.Context(), files.GeneratedFile{
		Purpose: files.PurposeEventExport, OrgID: &user, Name: "katilimcilar.csv", ContentType: files.MIMECSV, Content: csv,
	})
	if err != nil {
		t.Fatalf("PutGenerated: %v", err)
	}
	f := e.row(t, id)
	if f.Status != files.StatusActive || f.ScanStatus != files.ScanClean || f.Bucket != buckets.Files || !bytes.Equal(f.SHA256, digest(csv)) {
		t.Fatalf("generated row = %+v", f)
	}
	if _, err := e.svc.PresignGet(t.Context(), id, time.Minute); err != nil {
		t.Fatalf("PresignGet(generated): %v", err)
	}
	_, err = e.svc.PutGenerated(t.Context(), files.GeneratedFile{Purpose: files.PurposeEventExport, Name: "x", ContentType: "text/html", Content: csv})
	wantErr(t, err, files.ErrInvalidUpload)
	_, err = e.svc.PutGenerated(t.Context(), files.GeneratedFile{Purpose: files.PurposeAvatar, Name: "x", ContentType: files.MIMEPNG, Content: make([]byte, 5*files.MB+1)})
	wantErr(t, err, files.ErrTooLarge)
}

func TestOrphanCleanup_DeletesOldUnattachedFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	orphan := e.active(t, cvRequest(len(pdf)), pdf)
	pending := e.upload(t, "student", cvRequest(len(pdf)), pdf).FileID
	kept := e.active(t, cvRequest(len(pdf)), pdf)
	if err := e.svc.Attach(t.Context(), actor(t, "student"), kept, files.Owner{Module: "students", EntityID: uuid.NewV7(), Purpose: files.PurposeCV}); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(files.DefaultOrphanRetention + time.Minute)
	young := e.active(t, cvRequest(len(pdf)), pdf)
	if err := e.job(t, files.OrphanCleanupJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{orphan, pending} {
		if _, err := e.store.Get(t.Context(), id); !errors.Is(err, files.ErrNotFound) {
			t.Errorf("orphan %s not deleted", id)
		}
	}
	for _, id := range []uuid.UUID{kept, young} {
		e.row(t, id)
	}
	if e.mem.Len() != 2 {
		t.Fatalf("%d objects left, want the 2 kept files", e.mem.Len())
	}
}

func TestPrivacy_ExportAndEraseUnattachedFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	user := authztest.ID("student")
	loose := e.active(t, cvRequest(len(pdf)), pdf)
	kept := e.active(t, cvRequest(len(pdf)), pdf)
	if err := e.svc.Attach(t.Context(), actor(t, "student"), kept, files.Owner{Module: "students", EntityID: uuid.NewV7(), Purpose: files.PurposeCV}); err != nil {
		t.Fatal(err)
	}
	p := e.mod.Privacy
	if p.Name() != "files" {
		t.Fatalf("provider name %q", p.Name())
	}
	sec, err := p.Export(t.Context(), user)
	if err != nil || len(sec.Items) != 1 || sec.Items[0]["file_id"] != loose.String() || sec.Items[0]["name"] != "CV.pdf" {
		t.Fatalf("Export = %+v, %v", sec, err)
	}
	res, err := p.Erase(t.Context(), user)
	if err != nil || res.Deleted != 1 {
		t.Fatalf("Erase = %+v, %v", res, err)
	}
	if res, err := p.Erase(t.Context(), user); err != nil || res.Deleted != 0 {
		t.Fatalf("second Erase = %+v, %v (must be idempotent)", res, err)
	}
	e.row(t, kept)
}

// TestRead_ServableOnly: the owner module reads a file's content only under
// the download rule (BE-042: the invoice PDF e-mail) and within its limit.
func TestRead_ServableOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pending := e.active(t, cvRequest(len(pdf)), pdf)
	_, _, err := e.svc.Read(t.Context(), pending, files.MaxUploadBytes)
	wantErr(t, err, files.ErrNotClean)
	if err := e.scan(t, pending); err != nil {
		t.Fatal(err)
	}
	b, meta, err := e.svc.Read(t.Context(), pending, files.MaxUploadBytes)
	if err != nil || !strings.HasPrefix(string(b), "%PDF-") || int64(len(b)) != meta.SizeBytes || meta.ID != pending ||
		meta.MIME != files.MIMEPDF {
		t.Fatalf("Read = %d bytes, %+v, %v", len(b), meta, err)
	}
	_, _, err = e.svc.Read(t.Context(), pending, int64(len(b)-1))
	wantErr(t, err, files.ErrTooLarge)
	_, _, err = e.svc.Read(t.Context(), uuid.NewV7(), files.MaxUploadBytes)
	wantErr(t, err, files.ErrNotFound)
}
