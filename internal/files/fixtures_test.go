package files_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	stdpng "image/png"
	"os"
	"strings"
	"testing"
)

// Personal data planted in the fixtures; none of it may survive the
// metadata removal (BE-059).
const (
	secretAuthor  = "Ayse Yilmaz"
	secretCreator = "KoroTestWriter 1.0"
	secretDevice  = "KoroTestPhone"
	secretApp     = "secret-app-data"
	secretComment = "taken at home"
)

// gpsLatitude is 41° 0' 45.23" N, as EXIF rationals (big endian).
var gpsLatitude = []uint32{41, 1, 0, 1, 4523, 100}

// tiffEntry is one IFD entry of the EXIF fixture.
type tiffEntry struct {
	tag, typ uint16
	count    uint32
	value    []byte // inline when ≤ 4 bytes, else stored after the IFD
}

// exifSegment builds an APP1 "Exif" segment (big-endian TIFF) with IFD0
// Make + Orientation + a GPS IFD (latitude ref and latitude).
func exifSegment(orientation uint16) []byte {
	be := binary.BigEndian
	short := func(v uint16) []byte { b := make([]byte, 2); be.PutUint16(b, v); return b }
	long := func(v uint32) []byte { b := make([]byte, 4); be.PutUint32(b, v); return b }
	rationals := func(vs []uint32) []byte {
		var b []byte
		for _, v := range vs {
			b = append(b, long(v)...)
		}
		return b
	}
	// writeIFD lays out entries at off and returns the IFD and its data area.
	writeIFD := func(off uint32, entries []tiffEntry) []byte {
		size := uint32(2 + 12*len(entries) + 4)
		var ifd, data []byte
		ifd = append(ifd, short(uint16(len(entries)))...)
		for _, e := range entries {
			ifd = append(ifd, short(e.tag)...)
			ifd = append(ifd, short(e.typ)...)
			ifd = append(ifd, long(e.count)...)
			if len(e.value) <= 4 {
				v := make([]byte, 4)
				copy(v, e.value)
				ifd = append(ifd, v...)
				continue
			}
			ifd = append(ifd, long(off+size+uint32(len(data)))...)
			data = append(data, e.value...)
		}
		ifd = append(ifd, long(0)...)
		return append(ifd, data...)
	}
	make0 := append([]byte(secretDevice), 0)
	ifd0Entries := []tiffEntry{
		{0x010F, 2, uint32(len(make0)), make0}, // Make (ASCII)
		{0x0112, 3, 1, short(orientation)},     // Orientation (SHORT)
		{0x8825, 4, 1, nil},                    // GPSInfo IFD pointer (LONG), patched below
	}
	ifd0Size := 2 + 12*len(ifd0Entries) + 4 + len(make0)
	gpsOff := uint32(8 + ifd0Size)
	ifd0Entries[2].value = long(gpsOff)
	ifd0 := writeIFD(8, ifd0Entries)
	gps := writeIFD(gpsOff, []tiffEntry{
		{0x0001, 2, 2, []byte("N\x00")},        // GPSLatitudeRef
		{0x0002, 5, 3, rationals(gpsLatitude)}, // GPSLatitude
	})
	tiff := append([]byte("MM\x00\x2a"), long(8)...)
	tiff = append(append(tiff, ifd0...), gps...)
	payload := append([]byte("Exif\x00\x00"), tiff...)
	return append([]byte{0xFF, 0xE1}, append(short(uint16(len(payload)+2)), payload...)...)
}

// comSegment is a JPEG comment (COM) segment.
func comSegment(text string) []byte {
	n := len(text) + 2
	return append([]byte{0xFF, 0xFE, byte(n >> 8), byte(n)}, text...)
}

// photo is a w×h picture: the top-left quarter red, the rest blue.
func photo(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{0, 0, 0xFF, 0xFF}
			if x < w/2 && y < h/2 {
				c = color.RGBA{0xFF, 0, 0, 0xFF}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// jpegWithEXIF is a camera-like JPEG: EXIF (device, orientation, GPS) and
// a comment right after SOI.
func jpegWithEXIF(t testing.TB, w, h int, orientation uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, photo(w, h), &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	out := append([]byte{}, b[:2]...)
	out = append(out, exifSegment(orientation)...)
	out = append(out, comSegment(secretComment)...)
	return append(out, b[2:]...)
}

// pngChunk encodes one PNG chunk.
func pngChunk(typ string, data []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	b = append(b, typ...)
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

// pngWithText is a PNG with a tEXt author chunk and an eXIf chunk (GPS)
// after IHDR.
func pngWithText(t testing.TB, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := stdpng.Encode(&buf, photo(w, h)); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	const ihdrEnd = 8 + 4 + 4 + 13 + 4
	exif := exifSegment(1)[4+6:] // the TIFF block without APP1 header and "Exif\0\0"
	out := append([]byte{}, b[:ihdrEnd]...)
	out = append(out, pngChunk("tEXt", []byte("Author\x00"+secretAuthor))...)
	out = append(out, pngChunk("eXIf", exif)...)
	return append(out, b[ihdrEnd:]...)
}

// pngHeaderOnly is a PNG whose IHDR announces w×h pixels (a decompression
// bomb needs no more than its header to be refused).
func pngHeaderOnly(w, h uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8-bit RGBA
	out := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr)...)
	out = append(out, pngChunk("IDAT", []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01})...)
	return append(out, pngChunk("IEND", nil)...)
}

// readTestdata returns a fixture file of testdata/.
func readTestdata(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// webpWithEXIF wraps the VP8 bitstream of a simple lossy WebP in a VP8X
// container with an EXIF chunk (what phones write).
func webpWithEXIF(t testing.TB) []byte {
	t.Helper()
	simple := readTestdata(t, "lossy.webp")
	if string(simple[12:16]) != "VP8 " {
		t.Fatalf("fixture is not a simple lossy WebP: %q", simple[12:16])
	}
	vp8 := simple[12:] // the "VP8 " chunk (header + data + padding)
	// The VP8 frame header carries the size: 14 bits each after the start code.
	frame := simple[20:]
	w := int(binary.LittleEndian.Uint16(frame[6:8]) & 0x3FFF)
	h := int(binary.LittleEndian.Uint16(frame[8:10]) & 0x3FFF)
	le24 := func(v int) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16)} }
	vp8x := []byte{0x08, 0, 0, 0} // EXIF flag
	vp8x = append(vp8x, le24(w-1)...)
	vp8x = append(vp8x, le24(h-1)...)
	chunk := func(typ string, data []byte) []byte {
		b := append([]byte(typ), binary.LittleEndian.AppendUint32(nil, uint32(len(data)))...)
		b = append(b, data...)
		if len(data)%2 == 1 {
			b = append(b, 0)
		}
		return b
	}
	body := append([]byte("WEBP"), chunk("VP8X", vp8x)...)
	body = append(body, vp8...)
	body = append(body, chunk("EXIF", exifSegment(1)[4+6:])...)
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...)
}

// buildPDF writes a classic PDF (xref table) from object bodies; object i
// is objs[i-1]. trailer is the trailer dictionary content without /Size.
func buildPDF(objs []string, trailer string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d %s >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, trailer, xref)
	return b.Bytes()
}

// stream is a PDF stream object body.
func stream(dict string, data []byte) string {
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(data), data)
}

// xmp is an XMP packet naming the author and the location.
func xmp(who string) []byte {
	return []byte(`<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?><x:xmpmeta xmlns:x="adobe:ns:meta/">` +
		`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:exif="http://ns.adobe.com/exif/1.0/"><dc:creator>` + who + `</dc:creator><exif:GPSLatitude>41,0.75N</exif:GPSLatitude>` +
		`</rdf:Description></rdf:RDF></x:xmpmeta><?xpacket end="w"?>`)
}

// cvPDF is a one-page CV as office software writes it: an info dictionary
// (author, creator, title), document and image XMP, application data
// (/PieceInfo) and an embedded photo with EXIF GPS.
func cvPDF(t testing.TB) []byte {
	t.Helper()
	img := jpegWithEXIF(t, 16, 16, 1)
	content := []byte("q 100 0 0 100 50 50 cm /Im1 Do Q BT /F1 12 Tf 20 20 Td (Curriculum Vitae) Tj ET")
	return buildPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R /Metadata 6 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R " +
			"/Resources << /XObject << /Im1 7 0 R >> /Font << /F1 9 0 R >> >> " +
			"/PieceInfo << /KoroApp << /LastModified (D:20260101000000Z) /Private (" + secretApp + ") >> >> >>",
		stream("", content),
		"<< /Author (" + secretAuthor + ") /Creator (" + secretCreator + ") /Title (Ozgecmis " + secretAuthor + ") " +
			"/Producer (KoroTestPDF) /CreationDate (D:20260101000000Z) >>",
		stream("/Type /Metadata /Subtype /XML", xmp(secretAuthor)),
		stream("/Type /XObject /Subtype /Image /Width 16 /Height 16 /ColorSpace /DeviceRGB /BitsPerComponent 8 "+
			"/Filter /DCTDecode /Metadata 8 0 R", img),
		stream("/Type /Metadata /Subtype /XML", xmp(secretDevice)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}, "/Root 1 0 R /Info 5 0 R")
}

// minimalPDF is a valid one-page PDF without metadata.
func minimalPDF() []byte {
	return buildPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] >>",
	}, "/Root 1 0 R")
}

// containsAny reports which of the needles occur in haystack.
func containsAny(haystack []byte, needles ...string) []string {
	var found []string
	for _, n := range needles {
		if bytes.Contains(haystack, []byte(n)) || strings.Contains(strings.ToLower(string(haystack)), strings.ToLower(n)) {
			found = append(found, n)
		}
	}
	return found
}
