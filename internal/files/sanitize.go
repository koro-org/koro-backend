package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/webp"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// Metadata removal (05 §14.7 "metadata temizliği", T-138; BE-059). Every
// browser upload is rewritten before it leaves the quarantine bucket, so no
// EXIF/GPS, XMP, IPTC, ICC, comment or document-info field of the uploader's
// device or software reaches the main bucket:
//
//   - JPEG and PNG are decoded and encoded again: only the pixels survive.
//     The EXIF orientation of a JPEG is applied to the pixels first (the tag
//     itself is dropped, so a phone photo keeps its upright position).
//   - WebP has no encoder in the Go standard library or golang.org/x/image:
//     a lossy, opaque WebP becomes a JPEG, any other WebP (alpha, lossless)
//     a PNG. The detected MIME type (and the download extension) follow.
//   - PDF is parsed and written again with pdfcpu (Apache-2.0): the
//     document information dictionary is replaced by a fresh one (Producer
//     and dates only), every /Metadata (XMP) and /PieceInfo entry is
//     removed, and APPn/COM segments (EXIF/XMP of embedded photos) are cut
//     out of DCT (JPEG) image streams. Encrypted PDFs are refused.
//
// Decoding is bounded: the header is read first and an image larger than
// MaxImageSide or MaxImagePixels is refused before any pixel buffer is
// allocated (decompression bomb); pdfcpu runs with explicit resource limits;
// at most sanitizeSlots rewrites run at once per process. A decoder panic is
// recovered. Whatever cannot be rewritten is unprocessable: the file is
// rejected and never reaches the main bucket.
const (
	// MaxImagePixels is the largest image (width × height) that is decoded:
	// 40 MP covers phone cameras (12–24 MP default, 48 MP high-resolution
	// mode is refused) while bounding a decode to a few hundred MB.
	MaxImagePixels = 40_000_000
	// MaxImageSide is the longest accepted image side in pixels.
	MaxImageSide = 16_384
	// jpegQuality is the quality of re-encoded JPEGs.
	jpegQuality = 90
	// sanitizeSlots bounds concurrent rewrites per process (memory).
	sanitizeSlots = 2
	// maxGrowth bounds the rewritten size relative to the purpose limit
	// (a WebP that becomes a PNG grows).
	maxGrowth = 2
)

// pdfLimits are the pdfcpu resource limits of a PDF rewrite: uploads are
// at most 10 MB, so the defaults (512 MB streams, 10 M objects) are cut down.
var pdfLimits = model.ResourceLimits{
	MaxInputBytes:        MaxUploadBytes,
	MaxObjectBytes:       16 * MB,
	MaxStreamBytes:       MaxUploadBytes,
	MaxDecodeBytes:       128 * MB,
	MaxImagePixels:       MaxImagePixels,
	MaxImageBytes:        256 * MB,
	MaxObjectCount:       500_000,
	MaxObjectStreamCount: 100_000,
	MaxObjectStreamFirst: 4 * MB,
	MaxXRefEntries:       500_000,
	MaxRecursionDepth:    100,
}

// unprocessable is a permanent sanitising failure: the content cannot be
// rewritten (corrupt, too large, unsupported). Reason is a short code for
// logs; it never contains file content.
type unprocessable struct {
	Reason string
	err    error
}

func (u *unprocessable) Error() string {
	if u.err != nil {
		return "files: unprocessable content (" + u.Reason + "): " + u.err.Error()
	}
	return "files: unprocessable content (" + u.Reason + ")"
}

func (u *unprocessable) Unwrap() error { return u.err }

func refuse(reason string, err error) error { return &unprocessable{Reason: reason, err: err} }

// sanitized is the rewritten content and its (possibly converted) type.
type sanitized struct {
	data []byte
	mime string
}

// sanitizer rewrites uploads; slots bounds concurrent rewrites.
type sanitizer struct{ slots chan struct{} }

func newSanitizer() *sanitizer { return &sanitizer{slots: make(chan struct{}, sanitizeSlots)} }

// Sanitize returns content of type mime without metadata, at most limit
// bytes long. A permanent failure is an *unprocessable error; a context
// error is returned as is (the task is retried).
func (z *sanitizer) Sanitize(ctx context.Context, mime string, content []byte, limit int64) (sanitized, error) {
	select {
	case z.slots <- struct{}{}:
		defer func() { <-z.slots }()
	case <-ctx.Done():
		return sanitized{}, ctx.Err()
	}
	out, err := sanitize(ctx, mime, content)
	switch {
	case err != nil && ctx.Err() != nil:
		return sanitized{}, ctx.Err()
	case err != nil:
		return sanitized{}, err
	case len(out.data) == 0:
		return sanitized{}, refuse("empty_output", nil)
	case int64(len(out.data)) > limit:
		return sanitized{}, refuse("output_too_large", nil)
	case DetectMIME(out.data) != out.mime:
		return sanitized{}, refuse("output_type", nil)
	}
	return out, nil
}

// sanitize dispatches on the verified MIME type and turns a decoder panic
// into an unprocessable error.
func sanitize(ctx context.Context, mime string, content []byte) (out sanitized, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = sanitized{}, refuse("panic", fmt.Errorf("%v", r))
		}
	}()
	switch mime {
	case MIMEJPEG:
		return sanitizeJPEG(content)
	case MIMEPNG:
		return sanitizePNG(content)
	case MIMEWebP:
		return sanitizeWebP(content)
	case MIMEPDF:
		return sanitizePDF(ctx, content)
	}
	return sanitized{}, refuse("unsupported_type", nil)
}

// ---------------------------------------------------------------- images

// checkSize refuses an image whose header announces too many pixels.
func checkSize(cfg image.Config) error {
	w, h := int64(cfg.Width), int64(cfg.Height)
	if w <= 0 || h <= 0 || w > MaxImageSide || h > MaxImageSide || w*h > MaxImagePixels {
		return refuse("image_too_large", fmt.Errorf("%d×%d pixels", w, h))
	}
	return nil
}

func encodeJPEG(img image.Image) (sanitized, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return sanitized{}, refuse("encode", err)
	}
	return sanitized{data: buf.Bytes(), mime: MIMEJPEG}, nil
}

func encodePNG(img image.Image) (sanitized, error) {
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&buf, img); err != nil {
		return sanitized{}, refuse("encode", err)
	}
	return sanitized{data: buf.Bytes(), mime: MIMEPNG}, nil
}

func sanitizeJPEG(content []byte) (sanitized, error) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return sanitized{}, refuse("decode", err)
	}
	if err := checkSize(cfg); err != nil {
		return sanitized{}, err
	}
	img, err := jpeg.Decode(bytes.NewReader(content))
	if err != nil {
		return sanitized{}, refuse("decode", err)
	}
	return encodeJPEG(orient(img, jpegOrientation(content)))
}

func sanitizePNG(content []byte) (sanitized, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return sanitized{}, refuse("decode", err)
	}
	if err := checkSize(cfg); err != nil {
		return sanitized{}, err
	}
	img, err := png.Decode(bytes.NewReader(content))
	if err != nil {
		return sanitized{}, refuse("decode", err)
	}
	return encodePNG(img)
}

// sanitizeWebP converts a WebP: lossy and opaque (*image.YCbCr) → JPEG,
// anything else (alpha, lossless) → PNG.
func sanitizeWebP(content []byte) (sanitized, error) {
	cfg, err := webp.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return sanitized{}, refuse("decode", err)
	}
	if err := checkSize(cfg); err != nil {
		return sanitized{}, err
	}
	img, err := webp.Decode(bytes.NewReader(content))
	if err != nil {
		return sanitized{}, refuse("decode", err)
	}
	if _, lossyOpaque := img.(*image.YCbCr); lossyOpaque {
		return encodeJPEG(img)
	}
	return encodePNG(img)
}

// jpegOrientation returns the EXIF orientation (1–8) of a JPEG, 1 when it
// has none or the EXIF block is malformed. Every offset is bounds-checked.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF: // fill byte
			i++
			continue
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7): // no length
			i += 2
			continue
		case marker == 0xDA || marker == 0xD9: // start of scan / end: no metadata after this
			return 1
		}
		n := int(b[i+2])<<8 | int(b[i+3])
		if n < 2 || i+2+n > len(b) {
			return 1
		}
		if seg := b[i+4 : i+2+n]; marker == 0xE1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return tiffOrientation(seg[6:])
		}
		i += 2 + n
	}
	return 1
}

// tiffOrientation reads tag 0x0112 (Orientation, SHORT) of IFD0.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	u16 := func(p []byte) int { return int(p[0])<<8 | int(p[1]) }
	u32 := func(p []byte) uint64 {
		return uint64(p[0])<<24 | uint64(p[1])<<16 | uint64(p[2])<<8 | uint64(p[3])
	}
	switch string(t[:2]) {
	case "MM":
	case "II": // little endian
		u16 = func(p []byte) int { return int(p[1])<<8 | int(p[0]) }
		u32 = func(p []byte) uint64 {
			return uint64(p[3])<<24 | uint64(p[2])<<16 | uint64(p[1])<<8 | uint64(p[0])
		}
	default:
		return 1
	}
	if u16(t[2:]) != 42 {
		return 1
	}
	off := u32(t[4:])
	if off < 8 || off+2 > uint64(len(t)) {
		return 1
	}
	ifd := t[off:]
	for k, n := 0, u16(ifd); k < n; k++ {
		e := 2 + 12*k
		if e+12 > len(ifd) {
			return 1
		}
		if u16(ifd[e:]) == 0x0112 && u16(ifd[e+2:]) == 3 { // Orientation, SHORT
			if v := u16(ifd[e+8:]); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient applies EXIF orientation o (2–8) to img; 1 (or unknown) returns
// img unchanged.
func orient(img image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 { // 90° rotations swap the sides
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	at := rgbaAt(img)
	for y := range h {
		for x := range w {
			var dx, dy int
			switch o {
			case 2: // mirror horizontal
				dx, dy = w-1-x, y
			case 3: // rotate 180°
				dx, dy = w-1-x, h-1-y
			case 4: // mirror vertical
				dx, dy = x, h-1-y
			case 5: // transpose
				dx, dy = y, x
			case 6: // rotate 90° clockwise
				dx, dy = h-1-y, x
			case 7: // transverse
				dx, dy = h-1-y, w-1-x
			case 8: // rotate 90° counter-clockwise
				dx, dy = y, w-1-x
			}
			p := dst.PixOffset(dx, dy)
			dst.Pix[p], dst.Pix[p+1], dst.Pix[p+2], dst.Pix[p+3] = at(b.Min.X+x, b.Min.Y+y)
		}
	}
	return dst
}

// rgbaAt returns a pixel reader of img with a fast path for the YCbCr
// images the JPEG decoder returns.
func rgbaAt(img image.Image) func(x, y int) (r, g, b, a uint8) {
	if yc, ok := img.(*image.YCbCr); ok {
		return func(x, y int) (uint8, uint8, uint8, uint8) {
			c := yc.YCbCrAt(x, y)
			r, g, b := color.YCbCrToRGB(c.Y, c.Cb, c.Cr)
			return r, g, b, 0xFF
		}
	}
	return func(x, y int) (uint8, uint8, uint8, uint8) {
		c, _ := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA) // RGBAModel always returns color.RGBA
		return c.R, c.G, c.B, c.A
	}
}

// ---------------------------------------------------------------- PDF

// pdfConfig is the pdfcpu configuration of a rewrite: no configuration
// directory, no network (certificate revocation), bounded resources.
func pdfConfig() *model.Configuration {
	conf := model.NewStatelessConfiguration()
	conf.Offline = true
	conf.ValidationMode = model.ValidationRelaxed
	conf.Limits = pdfLimits
	return conf
}

func sanitizePDF(ctx context.Context, content []byte) (sanitized, error) {
	pc, err := api.ReadContext(ctx, bytes.NewReader(content), pdfConfig())
	if err != nil {
		return sanitized{}, refuse("pdf_read", err)
	}
	if pc.Encrypt != nil || pc.EncKey != nil {
		return sanitized{}, refuse("pdf_encrypted", nil)
	}
	if err := api.ValidateContext(ctx, pc); err != nil {
		return sanitized{}, refuse("pdf_invalid", err)
	}
	if err := stripPDF(pc); err != nil {
		return sanitized{}, err
	}
	var buf bytes.Buffer
	if err := api.WriteContext(ctx, pc, &buf); err != nil {
		return sanitized{}, refuse("pdf_write", err)
	}
	return sanitized{data: buf.Bytes(), mime: MIMEPDF}, nil
}

// pdfMetadataKeys are dictionary entries that carry metadata (ISO 32000
// §14.3.2 /Metadata XMP streams, §14.5 /PieceInfo application data).
var pdfMetadataKeys = []string{"Metadata", "PieceInfo"}

// stripPDF removes the document information dictionary and every metadata
// entry of every object; DCT image streams lose their APPn/COM segments.
// Unreferenced objects (the old info dictionary, XMP streams) are not
// written by pdfcpu.
func stripPDF(pc *model.Context) error {
	pc.Info = nil // the writer creates a fresh one: Producer + dates only
	for _, e := range pc.Table {
		if e == nil || e.Free || e.Object == nil {
			continue
		}
		obj, err := stripObject(e.Object, 0)
		if err != nil {
			return err
		}
		e.Object = obj
	}
	return stripDict(pc.RootDict, 0)
}

func stripObject(o types.Object, depth int) (types.Object, error) {
	if depth > pdfLimits.MaxRecursionDepth {
		return nil, refuse("pdf_depth", nil)
	}
	switch v := o.(type) {
	case types.Dict:
		return v, stripDict(v, depth)
	case types.Array:
		for i, x := range v {
			y, err := stripObject(x, depth+1)
			if err != nil {
				return nil, err
			}
			v[i] = y
		}
		return v, nil
	case types.StreamDict:
		if err := stripDict(v.Dict, depth); err != nil {
			return nil, err
		}
		if len(v.FilterPipeline) == 1 && v.FilterPipeline[0].Name == "DCTDecode" && v.Raw != nil {
			if raw, changed := stripJPEGSegments(v.Raw); changed {
				n := int64(len(raw))
				v.Raw, v.StreamLength, v.StreamLengthObjNr = raw, &n, nil
				v.Update("Length", types.Integer(n))
			}
		}
		return v, nil
	}
	return o, nil
}

func stripDict(d types.Dict, depth int) error {
	if d == nil {
		return nil
	}
	for _, k := range pdfMetadataKeys {
		d.Delete(k)
	}
	for k, x := range d {
		y, err := stripObject(x, depth+1)
		if err != nil {
			return err
		}
		d[k] = y
	}
	return nil
}

// stripJPEGSegments drops the APP1–APP13, APP15 and COM segments (EXIF,
// XMP, IPTC, comments) of a JPEG without touching the compressed image
// data. APP0 (JFIF) and APP14 (Adobe colour transform) are kept: they
// decide how the pixels are interpreted. A malformed header returns b
// unchanged (the image is then written as it was; pdfcpu validated it).
func stripJPEGSegments(b []byte) ([]byte, bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return b, false
	}
	out := make([]byte, 0, len(b))
	out = append(out, 0xFF, 0xD8)
	changed := false
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return b, false
		}
		hdr := b[i : i+4]
		marker := hdr[1]
		if marker == 0xDA { // start of scan: copy the rest verbatim
			return append(out, b[i:]...), changed
		}
		if marker == 0xFF || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0xD9 {
			return b, false // fill bytes or stand-alone markers before SOS: leave it alone
		}
		n := int(hdr[2])<<8 | int(hdr[3])
		if n < 2 || i+2+n > len(b) {
			return b, false
		}
		if (marker >= 0xE1 && marker <= 0xED) || marker == 0xEF || marker == 0xFE {
			changed = true
		} else {
			out = append(out, b[i:i+2+n]...)
		}
		i += 2 + n
	}
	return b, false
}

// isUnprocessable reports whether err is a permanent sanitising failure and
// returns its reason code.
func isUnprocessable(err error) (string, bool) {
	if u, ok := errors.AsType[*unprocessable](err); ok {
		return u.Reason, true
	}
	return "", false
}
