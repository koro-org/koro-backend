package events

// The QR code of a ticket (R-003 §2.5.4, 05 §0.1 item 9, "qr.Renderer"):
// the QRRenderer interface hides the library. It lives in the events
// package: a module may not import its own subpackage (internal/arch layer
// rule).
//
// The renderer must be deterministic: the same payload always gives the
// same image (the ticket mail, every resend and a retried task show one
// QR; the tests compare the bytes). yeqown/go-qrcode/v2, 05's first
// choice, is not: it scores the 8 mask patterns in goroutines and keeps the
// first lowest score in completion order, so a tie between masks picks a
// different mask from run to run (about 1.5% of ticket payloads rendered
// to two images under concurrency; BE-031 flaky test). NewQRRenderer is
// therefore 05's fallback skip2/go-qrcode, which scores the masks in order
// and keeps the lowest index on a tie, with a PNG writer of the standard
// library (the library's border is replaced by ours). Its internal
// consistency checks panic; PNG turns a panic into an error.
//
// The image follows R-003 §2.5.4: error correction level M, a quiet zone of
// 4 modules, black on white, at least 300×300 pixels. The payload is
// rendered as given and never logged or kept (a ticket payload is a bearer
// credential).

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"

	qrcode "github.com/skip2/go-qrcode"
)

// QR image rules (R-003 §2.5.4).
const (
	// QRMinSize is the smallest side of a rendered PNG in pixels.
	QRMinSize = 300
	// QRQuietZone is the white border in modules.
	QRQuietZone = 4
	// QRMaxPayload bounds what a ticket QR carries (KORO1: + 26 characters
	// today; the limit keeps the image small and scannable).
	QRMaxPayload = 128
)

// ErrQRPayload rejects an empty or oversized payload.
var ErrQRPayload = errors.New("events: QR payload must be 1-128 bytes")

// QRRenderer renders a QR payload as a PNG image.
type QRRenderer interface {
	// PNG returns the PNG of payload: error correction M, a quiet zone of
	// QRQuietZone modules, square, at least QRMinSize pixels; the same
	// payload always gives the same bytes.
	PNG(payload string) ([]byte, error)
}

// NewQRRenderer returns the deterministic skip2/go-qrcode renderer.
func NewQRRenderer() QRRenderer { return skip2QR{} }

type skip2QR struct{}

// PNG implements QRRenderer.
func (skip2QR) PNG(payload string) ([]byte, error) {
	if payload == "" || len(payload) > QRMaxPayload {
		return nil, ErrQRPayload
	}
	bits, err := qrModules(payload)
	if err != nil {
		return nil, err
	}
	return qrPNG(bits)
}

// qrModules encodes payload (error correction M, the smallest version) and
// returns its module matrix without a border; a panic of the library's
// consistency checks is an error (its text never carries the payload).
func qrModules(payload string) (bits [][]bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			bits, err = nil, fmt.Errorf("events: QR encode: %v", r)
		}
	}()
	code, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("events: QR encode: %w", err)
	}
	code.DisableBorder = true // the quiet zone is drawn below
	return code.Bitmap(), nil
}

var qrPalette = color.Palette{color.White, color.Black}

// qrPNG draws the module matrix as a 1-bit paletted PNG with the quiet
// zone, scaled to at least QRMinSize pixels.
func qrPNG(bits [][]bool) ([]byte, error) {
	n := len(bits)
	if n == 0 {
		return nil, errors.New("events: empty QR matrix")
	}
	modules := n + 2*QRQuietZone
	scale := (QRMinSize + modules - 1) / modules
	side := modules * scale
	img := image.NewPaletted(image.Rect(0, 0, side, side), qrPalette) // index 0: white
	for y, row := range bits {
		for x, dark := range row {
			if !dark {
				continue
			}
			x0, y0 := (x+QRQuietZone)*scale, (y+QRQuietZone)*scale
			for dy := range scale {
				off := img.PixOffset(x0, y0+dy)
				for dx := range scale {
					img.Pix[off+dx] = 1
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("events: QR render: %w", err)
	}
	return buf.Bytes(), nil
}
