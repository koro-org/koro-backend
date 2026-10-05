package events_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"
	"uuid"

	"koro/internal/events"
)

const qrPayload = "KORO1:N4Q7ZK2M5XWB3T6Y8C1D9F0GHA"

func darkPixel(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return r < 0x8000 && g < 0x8000 && b < 0x8000
}

// TestQR_PNGShape: a square black-and-white PNG of at least 300 pixels with a
// white quiet zone of 4 modules and the top-left finder pattern right after
// it (R-003 §2.5.4); the same payload renders the same bytes.
func TestQR_PNGShape(t *testing.T) {
	t.Parallel()
	r := events.NewQRRenderer()
	b, err := r.PNG(qrPayload)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != bounds.Dy() || bounds.Dx() < events.QRMinSize {
		t.Fatalf("size %v", bounds)
	}
	// 32 alphanumeric characters at level M fit version 2 (25 modules; byte
	// mode would need version 3): 25 + 2×4 quiet modules, 10 px each.
	if bounds.Dx() != (25+2*events.QRQuietZone)*10 {
		t.Fatalf("side %d px: not version 2-M alphanumeric", bounds.Dx())
	}
	if len(b) > 10*1024 {
		t.Fatalf("PNG is %d bytes; a ticket QR should stay small (R-003 §2.5.6: 5-10 KB)", len(b))
	}
	// The first dark pixel on the diagonal is the finder's corner: the
	// quiet zone before it is exactly 4 modules of the scale.
	first := -1
	for i := range bounds.Dx() {
		if darkPixel(img.At(i, i)) {
			first = i
			break
		}
	}
	if first <= 0 {
		t.Fatal("no dark module on the diagonal")
	}
	scale := first / events.QRQuietZone
	if scale*events.QRQuietZone != first || scale < 1 {
		t.Fatalf("quiet zone %d px is not 4 modules", first)
	}
	// A finder pattern is 7 dark modules wide on its top row.
	for x := first; x < first+7*scale; x++ {
		if !darkPixel(img.At(x, first)) {
			t.Fatalf("finder row broken at x=%d", x)
		}
	}
	for _, p := range []image.Point{{0, 0}, {bounds.Dx() - 1, bounds.Dy() - 1}, {bounds.Dx() - 1, 0}} {
		if darkPixel(img.At(p.X, p.Y)) {
			t.Fatalf("corner %v is not white", p)
		}
	}
	again, err := r.PNG(qrPayload)
	if err != nil || !bytes.Equal(again, b) {
		t.Fatalf("not deterministic: %v", err)
	}
	other, err := r.PNG("KORO1:AAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err != nil || bytes.Equal(other, b) {
		t.Fatalf("another payload renders the same image: %v", err)
	}
}

func TestQR_PNGRejectsBadPayload(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"", strings.Repeat("A", events.QRMaxPayload+1)} {
		if _, err := events.NewQRRenderer().PNG(p); !errors.Is(err, events.ErrQRPayload) {
			t.Fatalf("PNG(%d bytes) = %v", len(p), err)
		}
	}
}

// TestQR_DeterministicUnderConcurrency: the same payload renders the same
// bytes however many renders run at once (a mask chosen by goroutine
// completion order made 05's first library render about 1.5% of ticket
// payloads to two different images; BE-031 flaky mail test).
func TestQR_DeterministicUnderConcurrency(t *testing.T) {
	t.Parallel()
	r := events.NewQRRenderer()
	for i := range 200 {
		tok, err := events.TicketToken(testDeriver(), "t1", uuid.NewV7(), i+1)
		if err != nil {
			t.Fatal(err)
		}
		payload := events.TicketPayload(tok)
		want, err := r.PNG(payload)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		got := make([][]byte, 16)
		for j := range got {
			wg.Go(func() { got[j], _ = r.PNG(payload) })
		}
		wg.Wait()
		for j, b := range got {
			if !bytes.Equal(b, want) {
				t.Fatalf("payload %d: render %d differs", i, j)
			}
		}
	}
}
