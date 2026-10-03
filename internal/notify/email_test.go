package notify_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"slices"
	"strings"
	"testing"

	"koro/internal/notify"
	"koro/internal/platform/problem"
)

// leaf is one non-multipart MIME part with the containers above it.
type leaf struct {
	Type        string
	ContentID   string
	Disposition string
	Filename    string
	Body        string
	Path        []string // enclosing multipart types, outermost first
}

type parsedMIME struct {
	Header mail.Header
	Type   string // top-level media type
	Leaves []leaf
}

func parseMIME(t *testing.T, raw []byte) parsedMIME {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse Content-Type: %v", err)
	}
	out := parsedMIME{Header: msg.Header, Type: mt}
	var walk func(mt string, params map[string]string, body io.Reader, hdr map[string][]string, path []string)
	walk = func(mt string, params map[string]string, body io.Reader, hdr map[string][]string, path []string) {
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if errors.Is(err, io.EOF) {
					return
				}
				if err != nil {
					t.Fatalf("read part: %v", err)
				}
				pmt, pparams, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
				if err != nil {
					t.Fatalf("part Content-Type: %v", err)
				}
				walk(pmt, pparams, p, p.Header, append(append([]string(nil), path...), mt))
			}
		}
		b, _ := io.ReadAll(body)
		get := func(k string) string {
			if v := hdr[k]; len(v) > 0 {
				return v[0]
			}
			return ""
		}
		disp, dparams, _ := mime.ParseMediaType(get("Content-Disposition"))
		out.Leaves = append(out.Leaves, leaf{
			Type: mt, ContentID: get("Content-Id"), Disposition: disp, Filename: dparams["filename"],
			Body: string(b), Path: path,
		})
	}
	walk(mt, params, msg.Body, msg.Header, nil)
	return out
}

func (p parsedMIME) find(typ string) (leaf, bool) {
	for _, l := range p.Leaves {
		if l.Type == typ {
			return l, true
		}
	}
	return leaf{}, false
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.Black)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const sampleICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:PUBLISH\r\nBEGIN:VEVENT\r\nUID:e1.r1@koro\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func messageWithQRAndICS(t *testing.T, key string) notify.Message {
	t.Helper()
	m := testMessage(key)
	m.Data = map[string]any{"name": "Ayşe", "qr_cid": "qr.png"}
	m.Inline = []notify.InlineAttachment{{CID: "qr.png", ContentType: "image/png", Data: tinyPNG(t)}}
	m.Attachments = []notify.Attachment{{Filename: "invite.ics", ContentType: "text/calendar; method=PUBLISH", Data: []byte(sampleICS)}}
	return m
}

func TestEmail_InlineCIDAndAttachment(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	mustSend(t, f, messageWithQRAndICS(t, "ticket:r1:v1"))

	sent := f.tr.Sent()
	if len(sent) != 1 {
		t.Fatalf("e-mails sent = %d, want 1", len(sent))
	}
	e := sent[0]
	if len(e.Inline) != 1 || e.Inline[0].CID != "qr.png" || e.Inline[0].ContentType != "image/png" || !bytes.HasPrefix(e.Inline[0].Data, []byte("\x89PNG")) {
		t.Fatalf("Inline = %+v, want the QR PNG as cid qr.png", e.Inline)
	}
	if len(e.Attachments) != 1 || e.Attachments[0].Filename != "invite.ics" || e.Attachments[0].ContentType != "text/calendar; method=PUBLISH" {
		t.Fatalf("Attachments = %+v, want invite.ics", e.Attachments)
	}
	if !strings.Contains(e.HTML, `<img src="cid:qr.png"`) {
		t.Fatalf("HTML does not reference the inline image:\n%s", e.HTML)
	}
	if e.FromAddress != "noreply@koro.test" || e.To != "ayse.yilmaz@ogrenci.edu.tr" || !e.Date.Equal(t0) ||
		!strings.HasSuffix(e.MessageID, "@koro.test") || e.UnsubscribeURL != "" {
		t.Fatalf("envelope = %+v", e)
	}

	// The wire format: multipart/related carries the HTML with the CID image,
	// the .ics rides as a separate attachment, the text alternative is there.
	raw, err := notify.BuildMIME(e)
	if err != nil {
		t.Fatalf("BuildMIME() error = %v", err)
	}
	p := parseMIME(t, raw)
	if p.Type != "multipart/mixed" {
		t.Fatalf("top-level type = %s, want multipart/mixed", p.Type)
	}
	img, ok := p.find("image/png")
	if !ok || img.ContentID != "<qr.png>" || img.Disposition != "inline" || !slices.Contains(img.Path, "multipart/related") {
		t.Fatalf("inline part = %+v, want Content-ID <qr.png>, disposition inline, inside multipart/related", img)
	}
	html, ok := p.find("text/html")
	if !ok || !slices.Contains(html.Path, "multipart/related") || !strings.Contains(html.Body, "cid:qr.png") {
		t.Fatalf("html part = %+v, want it inside the same multipart/related and referencing cid:qr.png", html)
	}
	if txt, ok := p.find("text/plain"); !ok || !strings.Contains(txt.Body, "Merhaba") {
		t.Fatalf("text/plain alternative missing or empty: %+v", txt)
	}
	ics, ok := p.find("text/calendar")
	if !ok || ics.Disposition != "attachment" || ics.Filename != "invite.ics" || slices.Contains(ics.Path, "multipart/related") {
		t.Fatalf("calendar part = %+v, want attachment invite.ics outside multipart/related", ics)
	}
	for k, want := range map[string]string{"Message-Id": "<" + e.MessageID + ">", "Mime-Version": "1.0"} {
		if got := p.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if got := p.Header.Get("Date"); !strings.HasPrefix(got, "Sat, 03 Oct 2026 09:00:00") {
		t.Errorf("Date header = %q, want the injected clock (Sat, 03 Oct 2026 09:00:00 …)", got)
	}
	if subj, err := new(mime.WordDecoder).DecodeHeader(p.Header.Get("Subject")); err != nil || subj != "Koro test iletisi" {
		t.Errorf("Subject = %q, %v", subj, err)
	}
}

func TestEmail_ListUnsubscribe_OnlyOnCommercial(t *testing.T) {
	t.Parallel()
	f := promoFixture(t, &fakeConsent{active: true})

	mustSend(t, f, testMessage("t1"))
	mustSend(t, f, commercialMessage("c1"))
	sent := f.tr.Sent()
	if len(sent) != 2 {
		t.Fatalf("sent %d e-mails, want 2", len(sent))
	}

	for _, tt := range []struct {
		name string
		e    notify.Email
		want bool
	}{{"transactional", sent[0], false}, {"commercial", sent[1], true}} {
		raw, err := notify.BuildMIME(tt.e)
		if err != nil {
			t.Fatalf("%s: BuildMIME() error = %v", tt.name, err)
		}
		h := parseMIME(t, raw).Header
		gotURL, gotPost := h.Get("List-Unsubscribe"), h.Get("List-Unsubscribe-Post")
		if tt.want {
			if gotURL != "<https://koro.test/unsubscribe/abc>" || gotPost != "List-Unsubscribe=One-Click" {
				t.Errorf("%s: List-Unsubscribe = %q, -Post = %q, want the RFC 8058 one-click pair", tt.name, gotURL, gotPost)
			}
		} else if gotURL != "" || gotPost != "" {
			t.Errorf("%s: carries List-Unsubscribe headers (%q, %q)", tt.name, gotURL, gotPost)
		}
	}
}

func TestEmail_AttachmentRules(t *testing.T) {
	t.Parallel()
	png := tinyPNG(t)
	big := bytes.Repeat([]byte("x"), 512<<10+1)
	tests := []struct {
		name    string
		mutate  func(m *notify.Message)
		wantLoc string
	}{
		{"cid with angle brackets", func(m *notify.Message) { m.Inline[0].CID = "<qr.png>" }, "message.inline[0].cid"},
		{"cid with at sign", func(m *notify.Message) { m.Inline[0].CID = "qr@koro" }, "message.inline[0].cid"},
		{"duplicate cid", func(m *notify.Message) {
			m.Inline = append(m.Inline, notify.InlineAttachment{CID: "qr.png", ContentType: "image/png", Data: png})
		}, "message.inline[1].cid"},
		{"svg inline", func(m *notify.Message) { m.Inline[0].ContentType = "image/svg+xml" }, "message.inline[0].content_type"},
		{"empty inline", func(m *notify.Message) { m.Inline[0].Data = nil }, "message.inline[0].data"},
		{"oversize inline", func(m *notify.Message) { m.Inline[0].Data = big }, "message.inline[0].data"},
		{"too many inline", func(m *notify.Message) {
			for i := range 5 {
				m.Inline = append(m.Inline, notify.InlineAttachment{CID: "i" + string(rune('a'+i)), ContentType: "image/png", Data: png})
			}
		}, "message.inline"},
		{"path in filename", func(m *notify.Message) { m.Attachments[0].Filename = "../etc/passwd" }, "message.attachments[0].filename"},
		{"slash in filename", func(m *notify.Message) { m.Attachments[0].Filename = "a/b.ics" }, "message.attachments[0].filename"},
		{"newline in filename", func(m *notify.Message) { m.Attachments[0].Filename = "a\r\nb.ics" }, "message.attachments[0].filename"},
		{"non-ASCII filename", func(m *notify.Message) { m.Attachments[0].Filename = "bilet-şölen.ics" }, "message.attachments[0].filename"},
		{"executable attachment", func(m *notify.Message) { m.Attachments[0].ContentType = "application/x-msdownload" }, "message.attachments[0].content_type"},
		{"calendar with other parameter", func(m *notify.Message) { m.Attachments[0].ContentType = "text/calendar; boundary=x" }, "message.attachments[0].content_type"},
		{"empty attachment", func(m *notify.Message) { m.Attachments[0].Data = nil }, "message.attachments[0].data"},
		{"oversize attachments", func(m *notify.Message) {
			m.Attachments = []notify.Attachment{
				{Filename: "a.pdf", ContentType: "application/pdf", Data: bytes.Repeat([]byte("x"), 6<<20)},
				{Filename: "b.pdf", ContentType: "application/pdf", Data: bytes.Repeat([]byte("x"), 6<<20)},
			}
		}, "message.attachments"},
		{"too many attachments", func(m *notify.Message) {
			for range 5 {
				m.Attachments = append(m.Attachments, notify.Attachment{Filename: "x.pdf", ContentType: "application/pdf", Data: []byte("x")})
			}
		}, "message.attachments"},
		{"inline not referenced by the template", func(m *notify.Message) {
			m.Inline[0].CID = "other.png"
			m.Data["qr_cid"] = "qr.png" // the HTML references qr.png, not other.png
		}, "message.inline[0].cid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			m := messageWithQRAndICS(t, "k1")
			tt.mutate(&m)
			_, err := f.svc.Send(workerCtx(t), m)
			var pe *problem.Error
			if !errors.Is(err, notify.ErrInvalidMessage) || !errors.As(err, &pe) || !hasLocation(pe.Fields(), tt.wantLoc) {
				t.Fatalf("Send() error = %v (fields %+v), want ErrInvalidMessage at %s", err, fieldsOf(err), tt.wantLoc)
			}
			if len(f.tr.Sent()) != 0 {
				t.Fatal("an invalid message was sent")
			}
		})
	}
}

func fieldsOf(err error) []problem.FieldError {
	var pe *problem.Error
	if errors.As(err, &pe) {
		return pe.Fields()
	}
	return nil
}

func TestEmail_ValidContentTypeParametersAndPDF(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	m := messageWithQRAndICS(t, "k1")
	m.Attachments = append(m.Attachments,
		notify.Attachment{Filename: "bilet.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7")},
		notify.Attachment{Filename: "cancel.ics", ContentType: "text/calendar; method=CANCEL; charset=UTF-8", Data: []byte(sampleICS)},
	)
	mustSend(t, f, m)
	raw, err := notify.BuildMIME(f.tr.Sent()[0])
	if err != nil {
		t.Fatal(err)
	}
	p := parseMIME(t, raw)
	pdf, ok := p.find("application/pdf")
	if !ok || pdf.Filename != "bilet.pdf" || pdf.Disposition != "attachment" {
		t.Fatalf("pdf part = %+v", pdf)
	}
	if len(p.Leaves) != 6 { // plain, html, png, 3 attachments
		t.Fatalf("leaves = %d, want 6: %+v", len(p.Leaves), p.Leaves)
	}
}

func TestEmail_BuildersCopyCallerBuffers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	m := messageWithQRAndICS(t, "k1")
	qr := m.Inline[0].Data
	first := qr[0]
	mustSend(t, f, m)
	qr[0] ^= 0xff // the caller reuses its buffer after Send returns
	if got := f.tr.Sent()[0].Inline[0].Data[0]; got != first {
		t.Fatalf("Email.Inline aliases the caller's buffer (first byte %x, want %x)", got, first)
	}
}
