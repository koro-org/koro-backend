package files

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// TestOrient_AllEXIFOrientations: each EXIF orientation maps the marked
// stored corner to the displayed top-left (CIPA DC-008 tag 0x0112).
func TestOrient_AllEXIFOrientations(t *testing.T) {
	t.Parallel()
	// A 3×2 image, every pixel distinct: value = 10*y + x.
	src := image.NewGray(image.Rect(0, 0, 3, 2))
	for y := range 2 {
		for x := range 3 {
			src.SetGray(x, y, color.Gray{Y: uint8(10*y + x)})
		}
	}
	// want[o] is the displayed image row by row.
	want := map[int][][]uint8{
		1: {{0, 1, 2}, {10, 11, 12}},
		2: {{2, 1, 0}, {12, 11, 10}},
		3: {{12, 11, 10}, {2, 1, 0}},
		4: {{10, 11, 12}, {0, 1, 2}},
		5: {{0, 10}, {1, 11}, {2, 12}},
		6: {{10, 0}, {11, 1}, {12, 2}},
		7: {{12, 2}, {11, 1}, {10, 0}},
		8: {{2, 12}, {1, 11}, {0, 10}},
	}
	for o, rows := range want {
		img := orient(src, o)
		if b := img.Bounds(); b.Dy() != len(rows) || b.Dx() != len(rows[0]) {
			t.Errorf("orientation %d: size %v", o, b)
			continue
		}
		for y, row := range rows {
			for x, v := range row {
				if g := color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y; g != v {
					t.Errorf("orientation %d: (%d,%d) = %d, want %d", o, x, y, g, v)
				}
			}
		}
	}
}

// TestJPEGOrientation_Malformed: truncated or hostile EXIF never panics
// and falls back to "no rotation".
func TestJPEGOrientation_Malformed(t *testing.T) {
	t.Parallel()
	exif := func(tiff string) []byte {
		seg := append([]byte("Exif\x00\x00"), tiff...)
		n := len(seg) + 2
		return append([]byte{0xFF, 0xD8, 0xFF, 0xE1, byte(n >> 8), byte(n)}, seg...)
	}
	for name, b := range map[string][]byte{
		"empty":               nil,
		"not a jpeg":          []byte("GIF89a"),
		"soi only":            {0xFF, 0xD8},
		"segment past end":    {0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF, 'E'},
		"zero length":         {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x00},
		"bad byte order":      exif("XX\x00\x2a\x00\x00\x00\x08"),
		"ifd offset too far":  exif("MM\x00\x2a\xff\xff\xff\xf0"),
		"ifd count too large": exif("MM\x00\x2a\x00\x00\x00\x08\xff\xff"),
		"orientation 99":      exif("MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x63\x00\x00"),
		"orientation as long": exif("MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x04\x00\x00\x00\x01\x00\x00\x00\x06"),
	} {
		if got := jpegOrientation(b); got != 1 {
			t.Errorf("%s: orientation %d, want 1", name, got)
		}
	}
	le := exif("II\x2a\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x08\x00\x00\x00")
	if got := jpegOrientation(le); got != 8 {
		t.Errorf("little-endian orientation 8: got %d", got)
	}
}

// TestStripJPEGSegments_KeepsJFIFAndAdobe: APP0 and APP14 decide how the
// pixels are read and stay; EXIF, XMP, IPTC, ICC and comments go.
func TestStripJPEGSegments_KeepsJFIFAndAdobe(t *testing.T) {
	t.Parallel()
	seg := func(m byte, body string) []byte {
		n := len(body) + 2
		return append([]byte{0xFF, m, byte(n >> 8), byte(n)}, body...)
	}
	var in []byte
	in = append(in, 0xFF, 0xD8)
	in = append(in, seg(0xE0, "JFIF\x00")...)
	in = append(in, seg(0xE1, "Exif\x00\x00gps")...)
	in = append(in, seg(0xE2, "ICC_PROFILE")...)
	in = append(in, seg(0xED, "Photoshop 3.0")...)
	in = append(in, seg(0xEE, "Adobe")...)
	in = append(in, seg(0xFE, "comment")...)
	in = append(in, seg(0xDB, "quant")...)
	in = append(in, 0xFF, 0xDA, 0x00, 0x02, 0x12, 0x34, 0xFF, 0xD9)
	out, changed := stripJPEGSegments(in)
	if !changed {
		t.Fatal("nothing stripped")
	}
	for _, gone := range []string{"Exif", "ICC_PROFILE", "Photoshop", "comment"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Errorf("%s kept", gone)
		}
	}
	for _, kept := range []string{"JFIF", "Adobe", "quant", "\xFF\xDA\x00\x02\x12\x34\xFF\xD9"} {
		if !bytes.Contains(out, []byte(kept)) {
			t.Errorf("%s dropped", kept)
		}
	}
	if _, changed := stripJPEGSegments([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0xFF}); changed {
		t.Error("a truncated header was rewritten")
	}
}

// TestSanitize_LimitsAndCancellation: the output limit and a cancelled
// context are honoured; an unknown type is refused.
func TestSanitize_LimitsAndCancellation(t *testing.T) {
	t.Parallel()
	z := newSanitizer()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if _, err := z.Sanitize(t.Context(), MIMEPNG, buf.Bytes(), 10); err == nil {
		t.Error("output above the limit accepted")
	} else if r, ok := isUnprocessable(err); !ok || r != "output_too_large" {
		t.Errorf("error = %v", err)
	}
	if _, err := z.Sanitize(t.Context(), MIMECSV, []byte("a;b"), MB); err == nil {
		t.Error("unsupported type accepted")
	}
	// All slots busy and the context ends: a transient error, not a refusal.
	for range sanitizeSlots {
		z.slots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := z.Sanitize(ctx, MIMEPNG, buf.Bytes(), MB)
	if _, refused := isUnprocessable(err); err == nil || refused {
		t.Errorf("cancelled sanitize = %v, want the context error", err)
	}
}
