package files_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	stdpng "image/png"
	"strings"
	"testing"
	"uuid"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"

	"koro/internal/audit"
	"koro/internal/files"
)

// jpegMarkers lists the markers of a JPEG's header segments up to SOS.
func jpegMarkers(t *testing.T, b []byte) []byte {
	t.Helper()
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xD8 {
		t.Fatal("not a JPEG")
	}
	var out []byte
	for i := 2; i+4 <= len(b); {
		m := b[i+1]
		out = append(out, m)
		if m == 0xDA {
			return out
		}
		i += 2 + int(binary.BigEndian.Uint16(b[i+2:]))
	}
	t.Fatal("JPEG without start of scan")
	return nil
}

// pngChunks lists the chunk types of a PNG.
func pngChunks(t *testing.T, b []byte) []string {
	t.Helper()
	var out []string
	for i := 8; i+8 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[i:]))
		out = append(out, string(b[i+4:i+8]))
		i += 12 + n
	}
	return out
}

// gpsBytes is the GPS latitude of the fixtures as stored in EXIF.
func gpsBytes() []byte {
	var b []byte
	for _, v := range gpsLatitude {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}

// noSecrets fails if any planted personal datum survived in data.
func noSecrets(t *testing.T, what string, data []byte) {
	t.Helper()
	if found := containsAny(data, secretAuthor, secretCreator, secretDevice, secretApp, secretComment, "Exif\x00\x00", "xmpmeta"); len(found) > 0 {
		t.Errorf("%s still contains %q", what, found)
	}
	if bytes.Contains(data, gpsBytes()) {
		t.Errorf("%s still contains the GPS latitude", what)
	}
}

func reddish(c [4]uint32) bool { return c[0] > 0xB000 && c[2] < 0x5000 }
func bluish(c [4]uint32) bool  { return c[2] > 0xB000 && c[0] < 0x5000 }
func rgba(img image.Image, x, y int) [4]uint32 {
	r, g, b, a := img.At(x, y).RGBA()
	return [4]uint32{r, g, b, a}
}

// TestFiles_StripsEXIFGPS: a phone photo's EXIF (device, GPS, orientation),
// comments, PNG text/eXIf chunks and WebP EXIF never reach the main
// bucket; the photo stays upright (05 §14.7, T-138; BE-059).
func TestFiles_StripsEXIFGPS(t *testing.T) {
	t.Parallel()
	avatar := func(mime string) files.UploadRequest {
		return files.UploadRequest{Purpose: files.PurposeAvatar, Filename: "IMG_0001", ContentType: mime}
	}

	t.Run("jpeg", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		in := jpegWithEXIF(t, 64, 32, 6) // stored landscape, shown rotated 90° clockwise
		if !bytes.Contains(in, gpsBytes()) || !bytes.Contains(in, []byte(secretDevice)) {
			t.Fatal("fixture has no GPS/device EXIF")
		}
		f, out := e.scanned(t, avatar(files.MIMEJPEG), in)
		if f.Status != files.StatusActive || f.Bucket != buckets.Files || out == nil {
			t.Fatalf("after scan: %+v (object %d bytes)", f, len(out))
		}
		noSecrets(t, "main object", out)
		for _, m := range jpegMarkers(t, out) {
			if (m >= 0xE1 && m <= 0xEF) || m == 0xFE {
				t.Errorf("rewritten JPEG keeps segment 0x%X", m)
			}
		}
		img, err := jpeg.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		// Orientation 6 is applied to the pixels: 64×32 becomes 32×64 and
		// the red top-left quarter moves to the top-right.
		if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 64 {
			t.Fatalf("rewritten size %v, want 32×64 (orientation applied)", b)
		}
		if !reddish(rgba(img, 24, 8)) || !bluish(rgba(img, 8, 8)) || !bluish(rgba(img, 24, 48)) {
			t.Errorf("orientation not applied: (24,8)=%v (8,8)=%v", rgba(img, 24, 8), rgba(img, 8, 8))
		}
		if !bytes.Equal(f.SHA256, digest(out)) || *f.SizeBytes != int64(len(out)) || *f.DetectedMIME != files.MIMEJPEG {
			t.Error("row does not describe the rewritten object")
		}
		if _, _, _, ok := e.mem.Object(buckets.Quarantine, f.StorageKey); ok {
			t.Error("the original (with GPS) is left in quarantine")
		}
	})

	t.Run("png", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		in := pngWithText(t, 40, 20)
		f, out := e.scanned(t, avatar(files.MIMEPNG), in)
		if f.Status != files.StatusActive || out == nil {
			t.Fatalf("after scan: %+v", f)
		}
		noSecrets(t, "main object", out)
		for _, c := range pngChunks(t, out) {
			if c != "IHDR" && c != "IDAT" && c != "IEND" {
				t.Errorf("rewritten PNG keeps chunk %s", c)
			}
		}
		img, err := stdpng.Decode(bytes.NewReader(out))
		if err != nil || img.Bounds().Dx() != 40 || !reddish(rgba(img, 1, 1)) {
			t.Fatalf("rewritten PNG: %v", err)
		}
	})

	t.Run("lossy webp becomes jpeg", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		in := webpWithEXIF(t)
		f, out := e.scanned(t, avatar(files.MIMEWebP), in)
		if f.Status != files.StatusActive || *f.DetectedMIME != files.MIMEJPEG || files.DetectMIME(out) != files.MIMEJPEG {
			t.Fatalf("after scan: status %s, type %v", f.Status, *f.DetectedMIME)
		}
		noSecrets(t, "main object", out)
		_, ctype, disp, _ := e.mem.Object(buckets.Files, f.StorageKey)
		if ctype != files.MIMEJPEG || !strings.Contains(disp, "IMG_0001.jpg") {
			t.Errorf("stored as %q / %q, want image/jpeg and a .jpg name", ctype, disp)
		}
		if m, _ := e.svc.Get(t.Context(), f.ID); m.MIME != files.MIMEJPEG {
			t.Errorf("FileMeta.MIME = %s", m.MIME)
		}
	})

	t.Run("lossless webp with alpha becomes png", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		f, out := e.scanned(t, avatar(files.MIMEWebP), readTestdata(t, "lossless-alpha.webp"))
		if f.Status != files.StatusActive || *f.DetectedMIME != files.MIMEPNG || files.DetectMIME(out) != files.MIMEPNG {
			t.Fatalf("after scan: status %s, type %v", f.Status, *f.DetectedMIME)
		}
		img, err := stdpng.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		if _, opaque := img.(interface{ Opaque() bool }); opaque && img.(interface{ Opaque() bool }).Opaque() {
			t.Error("transparency lost")
		}
	})
}

// pdfText collects every string, name and decoded stream of a parsed PDF.
func pdfText(t *testing.T, pc *model.Context) (text []byte, metadataKeys int) {
	t.Helper()
	var walk func(o types.Object, depth int)
	walk = func(o types.Object, depth int) {
		if depth > 50 {
			return
		}
		switch v := o.(type) {
		case types.StringLiteral:
			s, err := types.StringLiteralToString(v)
			if err != nil {
				s = string(v)
			}
			text = append(text, s...)
		case types.HexLiteral:
			b, _ := v.Bytes()
			text = append(text, b...)
		case types.Dict:
			for k, x := range v {
				if k == "Metadata" || k == "PieceInfo" {
					metadataKeys++
				}
				text = append(text, k...)
				walk(x, depth+1)
			}
		case types.Array:
			for _, x := range v {
				walk(x, depth+1)
			}
		case types.StreamDict:
			walk(v.Dict, depth+1)
			if len(v.FilterPipeline) == 1 && v.FilterPipeline[0].Name == "DCTDecode" {
				text = append(text, v.Raw...)
				return
			}
			if err := v.Decode(); err == nil {
				text = append(text, v.Content...)
			} else {
				text = append(text, v.Raw...)
			}
		}
	}
	for _, e := range pc.Table {
		if e != nil && !e.Free && e.Object != nil {
			walk(e.Object, 0)
		}
	}
	return text, metadataKeys
}

// TestFiles_StripsPDFMetadata: the info dictionary (Author, Creator,
// Title), XMP streams, /PieceInfo and the EXIF of an embedded photo are
// gone from the stored CV; the page and its image survive (BE-059).
func TestFiles_StripsPDFMetadata(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := cvPDF(t)
	if found := containsAny(in, secretAuthor, secretCreator, secretDevice, secretApp, "xmpmeta"); len(found) != 5 {
		t.Fatalf("fixture lacks metadata: %v", found)
	}
	f, out := e.scanned(t, cvRequest(0), in)
	if f.Status != files.StatusActive || f.Bucket != buckets.Files || files.DetectMIME(out) != files.MIMEPDF {
		t.Fatalf("after scan: %+v", f)
	}
	noSecrets(t, "raw PDF", out)

	conf := model.NewStatelessConfiguration()
	conf.Offline = true
	pc, err := api.ReadContext(t.Context(), bytes.NewReader(out), conf)
	if err != nil {
		t.Fatalf("rewritten PDF does not parse: %v", err)
	}
	if err := api.ValidateContext(t.Context(), pc); err != nil {
		t.Fatalf("rewritten PDF is invalid: %v", err)
	}
	if pc.PageCount != 1 {
		t.Fatalf("pages = %d, want 1", pc.PageCount)
	}
	text, keys := pdfText(t, pc)
	if keys != 0 {
		t.Errorf("%d /Metadata or /PieceInfo entries left", keys)
	}
	noSecrets(t, "parsed PDF objects", text)
	if !bytes.Contains(text, []byte("Curriculum Vitae")) {
		t.Error("page content lost")
	}
	if pc.Info != nil {
		info, err := pc.DereferenceDict(*pc.Info)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"Author", "Creator", "Title", "Subject", "Keywords"} {
			if _, ok := info.Find(k); ok {
				t.Errorf("info dictionary keeps /%s", k)
			}
		}
	}
	// The embedded photo is still a decodable JPEG, without EXIF.
	var photos int
	for _, entry := range pc.Table {
		sd, ok := entry.Object.(types.StreamDict)
		if !ok || len(sd.FilterPipeline) != 1 || sd.FilterPipeline[0].Name != "DCTDecode" {
			continue
		}
		photos++
		if _, err := jpeg.Decode(bytes.NewReader(sd.Raw)); err != nil {
			t.Errorf("embedded photo broken: %v", err)
		}
		for _, m := range jpegMarkers(t, sd.Raw) {
			if m == 0xE1 || m == 0xFE {
				t.Errorf("embedded photo keeps segment 0x%X", m)
			}
		}
	}
	if photos != 1 {
		t.Errorf("embedded photos = %d, want 1", photos)
	}
}

// TestFiles_RejectsUnprocessableContent: content whose metadata cannot be
// removed is rejected: it never reaches the main bucket, the quarantined
// object is deleted, and the file can be neither attached nor served
// (05 §14.7 "başarısızsa sil"; BE-059).
func TestFiles_RejectsUnprocessableContent(t *testing.T) {
	t.Parallel()
	corruptJPEG := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0xAB}, 64)...)
	tests := []struct {
		name    string
		req     files.UploadRequest
		content []byte
		swap    []byte // replaces the quarantined object after the completion
	}{
		{"corrupt jpeg", files.UploadRequest{Purpose: files.PurposeAvatar, Filename: "a.jpg", ContentType: files.MIMEJPEG}, corruptJPEG, nil},
		{"png decompression bomb", files.UploadRequest{Purpose: files.PurposeAvatar, Filename: "a.png", ContentType: files.MIMEPNG}, pngHeaderOnly(100_000, 100_000), nil},
		{"png wider than the side limit", files.UploadRequest{Purpose: files.PurposeAvatar, Filename: "a.png", ContentType: files.MIMEPNG}, pngHeaderOnly(files.MaxImageSide+1, 1), nil},
		{"broken pdf", cvRequest(0), []byte("%PDF-1.7\nthis is not a pdf at all\n%%EOF\n"), nil},
		{"object changed after completion", cvRequest(0), pdf, cvPDF(t)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			tt.req.SizeBytes = int64(len(tt.content))
			id := e.active(t, tt.req, tt.content)
			f := e.row(t, id)
			if tt.swap != nil {
				e.mem.Upload(f.Bucket, f.StorageKey, tt.swap, tt.req.ContentType)
			}
			if err := e.scan(t, id); err != nil {
				t.Fatalf("a rejection is final, the task must succeed: %v", err)
			}
			f = e.row(t, id)
			if f.Status != files.StatusRejected || f.Bucket != buckets.Quarantine {
				t.Fatalf("after scan: status %s bucket %s", f.Status, f.Bucket)
			}
			if e.mem.Len() != 0 {
				t.Fatalf("%d objects left (main bucket or quarantine)", e.mem.Len())
			}
			_, err := e.svc.PresignGet(t.Context(), id, 0)
			wantErr(t, err, files.ErrRejected)
			wantErr(t, e.svc.Attach(t.Context(), actor(t, "student"), id,
				files.Owner{Module: "students", EntityID: uuid.NewV7(), Purpose: tt.req.Purpose}), files.ErrRejected)
			_, err = e.svc.CompleteUpload(t.Context(), actor(t, "student"), id, digest(tt.content))
			wantErr(t, err, files.ErrRejected)
			if err := e.scan(t, id); err != nil { // a redelivery changes nothing
				t.Fatal(err)
			}
			evs := e.events(t)
			last := evs[len(evs)-1]
			if last.Action != "files.upload.complete" || last.Outcome != audit.Failure || last.SystemJob != files.ScanTaskName ||
				!bytes.Contains(last.Metadata, []byte(`"files.rejected"`)) {
				t.Fatalf("rejection audit = %+v (%s)", last, last.Metadata)
			}
		})
	}

	t.Run("storage error is retried, not rejected", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.active(t, cvRequest(len(pdf)), pdf)
		e.mem.FailNext(errors.New("connection reset"))
		if err := e.scan(t, id); err == nil {
			t.Fatal("a storage failure must fail the task (retry)")
		}
		if f := e.row(t, id); f.Status != files.StatusActive || f.ScanStatus != files.ScanPending {
			t.Fatalf("after a storage error: %+v", f)
		}
		if err := e.scan(t, id); err != nil {
			t.Fatalf("retry: %v", err)
		}
	})
}
