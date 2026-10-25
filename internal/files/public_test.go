package files_test

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
)

const publicBase = "https://media.koro.example/public"

var (
	publicMedia = files.PublicMedia{Bucket: "public-test", BaseURL: publicBase}
	// publicURLRe is the URL shape koro-web relies on: base + content-addressed key.
	publicURLRe = regexp.MustCompile(`^` + regexp.QuoteMeta(publicBase) +
		`/org_logo/[0-9a-f-]{36}-[0-9a-f]{32}\.png$`)
)

func withPublic(d *files.Deps) { d.Public = publicMedia }

// logo uploads and completes an org_logo of club_a as its club_admin.
func (e *env) logo(t *testing.T, content []byte) uuid.UUID {
	t.Helper()
	org := authztest.ID("club_a")
	req := files.UploadRequest{Purpose: files.PurposeOrgLogo, Filename: "logo.png", ContentType: files.MIMEPNG,
		SizeBytes: int64(len(content)), OrgID: &org}
	up := e.upload(t, "club_admin", req, content)
	if _, err := e.svc.CompleteUpload(t.Context(), actor(t, "club_admin"), up.FileID, digest(content)); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	return up.FileID
}

func (e *env) publishedEvents(t *testing.T) []domainevent.FilePublished {
	t.Helper()
	var out []domainevent.FilePublished
	for _, ev := range e.out.Events() {
		if ev.Topic != domainevent.TopicFilePublished {
			continue
		}
		p, err := domainevent.Decode[domainevent.FilePublished](ev)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// publicObject returns the public copy behind url.
func (e *env) publicObject(t *testing.T, url string) (data []byte, cacheControl string, ok bool) {
	t.Helper()
	key, found := strings.CutPrefix(url, publicBase+"/")
	if !found {
		t.Fatalf("public URL %q is not under %s", url, publicBase)
	}
	info, err := e.mem.Stat(t.Context(), publicMedia.Bucket, key)
	if err != nil {
		return nil, "", false
	}
	data, _, _, ok = e.mem.Object(publicMedia.Bucket, key)
	return data, info.CacheControl, ok
}

// TestPublicMedia_CopiedAfterCleanScan: a cleared org logo gets one public
// copy — the rewritten bytes, immutable Cache-Control, a content-addressed
// URL — audited as the publish job; an unattached file writes no event.
func TestPublicMedia_CopiedAfterCleanScan(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withPublic, func(d *files.Deps) { d.Scanner = &fakeScanner{verdict: files.ScanVerdict{Status: files.ScanClean}} })
	id := e.logo(t, pngWithText(t, 4, 3))
	if u, err := e.svc.PublicURL(t.Context(), id); err == nil || u != "" {
		t.Fatalf("PublicURL before the scan = %q, %v; want ErrNotClean", u, err)
	}
	if err := e.scan(t, id); err != nil {
		t.Fatal(err)
	}
	u, err := e.svc.PublicURL(t.Context(), id)
	if err != nil || !publicURLRe.MatchString(u) || !strings.Contains(u, id.String()) {
		t.Fatalf("PublicURL = %q, %v", u, err)
	}
	f := e.row(t, id)
	main, _, _, _ := e.mem.Object(buckets.Files, f.StorageKey)
	pub, cc, ok := e.publicObject(t, u)
	if !ok || !bytes.Equal(pub, main) || cc != files.PublicCacheControl {
		t.Fatalf("public copy = %d bytes (main %d), Cache-Control %q, found %v", len(pub), len(main), cc, ok)
	}
	if !strings.Contains(u, hexPrefix(f.SHA256)) {
		t.Fatalf("URL %q does not carry the content hash of the stored bytes", u)
	}
	again, _ := e.svc.PublicURL(t.Context(), id)
	if again != u {
		t.Fatalf("PublicURL changed: %q then %q", u, again)
	}
	recs := e.events(t)
	last := recs[len(recs)-1]
	if last.Action != "files.file.publish" || last.SystemJob != files.PublishPublicJobName {
		t.Fatalf("last audit = %s by %s, want files.file.publish by the publish job", last.Action, last.SystemJob)
	}
	if evs := e.publishedEvents(t); len(evs) != 0 {
		t.Fatalf("file.published for an unattached file: %+v", evs)
	}
}

func hexPrefix(sum []byte) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	for _, c := range sum[:16] {
		b.WriteByte(digits[c>>4])
		b.WriteByte(digits[c&0xf])
	}
	return b.String()
}

// TestPublicMedia_AttachedBeforeScanWritesFilePublished: the usual flow —
// the org saves the logo right after the upload, the scan clears it later —
// writes file.published with the owner, so the web refreshes the page.
func TestPublicMedia_AttachedBeforeScanWritesFilePublished(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withPublic, func(d *files.Deps) { d.Scanner = &fakeScanner{verdict: files.ScanVerdict{Status: files.ScanClean}} })
	id := e.logo(t, pngWithText(t, 4, 3))
	org := authztest.ID("club_a")
	if err := e.svc.Attach(t.Context(), actor(t, "club_admin"), id, files.Owner{Module: "orgs", EntityID: org, Purpose: files.PurposeOrgLogo}); err != nil {
		t.Fatal(err)
	}
	if err := e.scan(t, id); err != nil {
		t.Fatal(err)
	}
	evs := e.publishedEvents(t)
	if len(evs) != 1 || evs[0].FileID != id || evs[0].Purpose != "org_logo" || evs[0].AttachedModule != "orgs" || evs[0].AttachedEntityID != org {
		t.Fatalf("file.published = %+v", evs)
	}
	// A repeated scan delivery or a backfill run copies nothing again.
	if err := e.scan(t, id); err != nil {
		t.Fatal(err)
	}
	if err := e.job(t, files.PublishPublicJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.publishedEvents(t)); n != 1 {
		t.Fatalf("%d file.published events, want 1", n)
	}
}

// TestPublicMedia_NeverForUnclearedOrPrivateFiles: no public copy for a
// pending scan, a private purpose, a skipped scan while
// FILES_SCAN_REQUIRED=true, or with public media off.
func TestPublicMedia_NeverForUnclearedOrPrivateFiles(t *testing.T) {
	t.Parallel()
	t.Run("private purpose", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, withPublic)
		f, _ := e.scanned(t, cvRequest(len(pdf)), pdf)
		if f.PublicKey != nil {
			t.Fatalf("a CV got a public copy %q", *f.PublicKey)
		}
		if _, err := e.svc.PublicURL(t.Context(), f.ID); !errors.Is(err, files.ErrPurposeMismatch) {
			t.Fatalf("PublicURL(cv) = %v, want ErrPurposeMismatch", err)
		}
	})
	t.Run("scan still pending", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, withPublic)
		id := e.logo(t, pngWithText(t, 4, 3))
		if err := e.job(t, files.PublishPublicJobName).Handle(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f := e.row(t, id); f.PublicKey != nil {
			t.Fatalf("an unscanned logo got a public copy")
		}
		if _, err := e.svc.PublicURL(t.Context(), id); !errors.Is(err, files.ErrNotClean) {
			t.Fatalf("PublicURL(pending) = %v, want ErrNotClean", err)
		}
	})
	t.Run("skipped while the scan is required", func(t *testing.T) {
		t.Parallel()
		sc := &fakeScanner{verdict: files.ScanVerdict{Status: files.ScanSkipped}}
		e := newEnv(t, withPublic, func(d *files.Deps) { d.Scanner, d.ScanRequired = sc, true })
		id := e.logo(t, pngWithText(t, 4, 3))
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		if f := e.row(t, id); f.ScanStatus != files.ScanSkipped || f.PublicKey != nil {
			t.Fatalf("row = %s, public key %v; want skipped without a copy", f.ScanStatus, f.PublicKey)
		}
	})
	t.Run("public media off", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.logo(t, pngWithText(t, 4, 3))
		if err := e.scan(t, id); err != nil {
			t.Fatal(err)
		}
		if u, err := e.svc.PublicURL(t.Context(), id); u != "" || err != nil {
			t.Fatalf("PublicURL with public media off = %q, %v; want \"\", nil", u, err)
		}
		if e.row(t, id).PublicKey != nil {
			t.Fatal("public copy recorded with public media off")
		}
	})
}

// TestPublicMedia_DeletedWithTheFile: deleting the row (owner module, orphan
// purge) removes the public copy too.
func TestPublicMedia_DeletedWithTheFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withPublic)
	id := e.logo(t, pngWithText(t, 4, 3))
	if err := e.scan(t, id); err != nil { // no-op scanner: skipped, served while the scan is not required
		t.Fatal(err)
	}
	u, err := e.svc.PublicURL(t.Context(), id)
	if err != nil || u == "" {
		t.Fatalf("PublicURL = %q, %v", u, err)
	}
	if err := e.svc.Delete(t.Context(), authz.SystemActor("test"), id); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.publicObject(t, u); ok {
		t.Fatal("public copy kept after the file was deleted")
	}
}

// TestPublicMedia_InfectedRescanRemovesCopy: a logo published while the
// no-op scanner skipped it loses its public copy when the real scanner
// finds malware, and the row no longer carries the key.
func TestPublicMedia_InfectedRescanRemovesCopy(t *testing.T) {
	t.Parallel()
	sc := &switchScanner{}
	e := newEnv(t, withPublic, func(d *files.Deps) { d.Scanner = sc })
	id := e.logo(t, pngWithText(t, 4, 3))
	if err := e.svc.Attach(t.Context(), actor(t, "club_admin"), id,
		files.Owner{Module: "orgs", EntityID: authztest.ID("club_a"), Purpose: files.PurposeOrgLogo}); err != nil {
		t.Fatal(err)
	}
	if err := e.scan(t, id); err != nil {
		t.Fatal(err)
	}
	u, _ := e.svc.PublicURL(t.Context(), id)
	if _, _, ok := e.publicObject(t, u); !ok {
		t.Fatalf("no public copy at %q after the skipped scan", u)
	}
	sc.goReal(func(uuid.UUID) (files.ScanVerdict, error) {
		return files.ScanVerdict{Status: files.ScanInfected, Signature: "Win.Test.EICAR_HDB-1"}, nil
	})
	if err := e.job(t, files.RescanSkippedJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.publicObject(t, u); ok {
		t.Fatal("infected logo still public")
	}
	if f := e.row(t, id); f.ScanStatus != files.ScanInfected || f.PublicKey != nil {
		t.Fatalf("row = %s, public key %v", f.ScanStatus, f.PublicKey)
	}
	if _, err := e.svc.PublicURL(t.Context(), id); !errors.Is(err, files.ErrNotClean) {
		t.Fatalf("PublicURL(infected) = %v, want ErrNotClean", err)
	}
}

// TestPublicMedia_BackfillJob: files cleared while public media was off
// (or before BE-120) get their copy from files.publish_public; an attached
// one writes file.published.
func TestPublicMedia_BackfillJob(t *testing.T) {
	t.Parallel()
	var d files.Deps
	e := newEnv(t, func(x *files.Deps) { d = *x })
	id := e.logo(t, pngWithText(t, 4, 3))
	if err := e.scan(t, id); err != nil {
		t.Fatal(err)
	}
	org := authztest.ID("club_a")
	if err := e.svc.Attach(t.Context(), actor(t, "club_admin"), id, files.Owner{Module: "orgs", EntityID: org, Purpose: files.PurposeOrgLogo}); err != nil {
		t.Fatal(err)
	}
	if e.row(t, id).PublicKey != nil {
		t.Fatal("copied with public media off")
	}
	d.Public = publicMedia // the operator turns public media on
	mod, err := files.New(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range mod.Jobs() {
		if j.Name == files.PublishPublicJobName {
			if err := j.Handle(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	u, err := mod.Service.PublicURL(t.Context(), id)
	if err != nil || !publicURLRe.MatchString(u) {
		t.Fatalf("PublicURL after the backfill = %q, %v", u, err)
	}
	if evs := e.publishedEvents(t); len(evs) != 1 || evs[0].AttachedEntityID != org {
		t.Fatalf("file.published = %+v", evs)
	}
}

func TestPublicMedia_DepsValidation(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]files.PublicMedia{
		"bucket without base URL": {Bucket: "public-test"},
		"base URL without bucket": {BaseURL: publicBase},
		"trailing slash":          {Bucket: "public-test", BaseURL: publicBase + "/"},
		"the files bucket":        {Bucket: buckets.Files, BaseURL: publicBase},
		"the quarantine bucket":   {Bucket: buckets.Quarantine, BaseURL: publicBase},
	} {
		var d files.Deps
		newEnv(t, func(x *files.Deps) { d = *x })
		d.Public = p
		if _, err := files.New(d); err == nil {
			t.Errorf("%s: files.New accepted %+v", name, p)
		}
	}
}
