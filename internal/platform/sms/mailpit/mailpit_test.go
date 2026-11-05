package mailpit_test

import (
	"io"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"koro/internal/platform/sms"
	"koro/internal/platform/sms/mailpit"
	"koro/internal/platform/sms/smscontract"
)

// box is a minimal SMTP server in Mailpit's place (enough of RFC 5321 for
// go-mail, no TLS, no login): it keeps the accepted messages and answers
// the queued outcome on the next connection.
type box struct {
	ln net.Listener

	mu       sync.Mutex
	next     []smscontract.Outcome
	accepted []accepted
}

type accepted struct {
	rcpt string
	data string
}

func startBox(t *testing.T) *box {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &box{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(c)
		}
	}()
	return b
}

func (b *box) addr() string { return b.ln.Addr().String() }

func (b *box) queue(o smscontract.Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next = append(b.next, o)
}

func (b *box) serve(c net.Conn) {
	defer c.Close()
	b.mu.Lock()
	var o smscontract.Outcome
	if len(b.next) > 0 {
		o, b.next = b.next[0], b.next[1:]
	}
	b.mu.Unlock()
	if o == smscontract.Down {
		return // Mailpit is not running: the connection closes before the greeting
	}
	tc := textproto.NewConn(c)
	_ = tc.PrintfLine("220 mailpit ESMTP")
	var rcpt string
	for {
		line, err := tc.ReadLine()
		if err != nil {
			return
		}
		switch strings.ToUpper(strings.Fields(line + " x")[0]) {
		case "EHLO":
			_ = tc.PrintfLine("250-mailpit")
			_ = tc.PrintfLine("250 8BITMIME")
		case "HELO", "MAIL", "RSET", "NOOP":
			_ = tc.PrintfLine("250 OK")
		case "RCPT":
			switch o {
			case smscontract.BadNumber:
				_ = tc.PrintfLine("550 5.1.1 <%s> unknown", strings.TrimPrefix(line, "RCPT TO:"))
			case smscontract.Throttled:
				_ = tc.PrintfLine("451 4.7.1 try again later")
			default:
				rcpt = strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), "<> ")
				_ = tc.PrintfLine("250 OK")
			}
		case "DATA":
			_ = tc.PrintfLine("354 go ahead")
			raw, err := tc.ReadDotBytes()
			if err != nil {
				return
			}
			if o == smscontract.Refused {
				_ = tc.PrintfLine("554 5.6.0 message refused")
				continue
			}
			b.mu.Lock()
			b.accepted = append(b.accepted, accepted{rcpt: rcpt, data: string(raw)})
			b.mu.Unlock()
			_ = tc.PrintfLine("250 2.0.0 queued")
		case "QUIT":
			_ = tc.PrintfLine("221 bye")
			return
		default:
			_ = tc.PrintfLine("502 not implemented")
		}
	}
}

// mails returns the accepted e-mails, parsed.
func (b *box) mails(t *testing.T) []parsed {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]parsed, 0, len(b.accepted))
	for _, a := range b.accepted {
		m, err := mail.ReadMessage(strings.NewReader(a.data))
		if err != nil {
			t.Fatalf("an unreadable e-mail: %v", err)
		}
		body := m.Body
		if strings.EqualFold(m.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			body = quotedprintable.NewReader(body)
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, parsed{rcpt: a.rcpt, header: m.Header, body: strings.ReplaceAll(string(raw), "\r\n", "\n")})
	}
	return out
}

type parsed struct {
	rcpt   string
	header mail.Header
	body   string
}

// sms rebuilds the SMS of an e-mail: the number from the recipient, the
// text after the body's header lines.
func (p parsed) sms() sms.Message {
	_, text, _ := strings.Cut(p.body, "\n\n")
	local, _, _ := strings.Cut(p.rcpt, "@")
	return sms.Message{To: "+" + local, Text: strings.TrimSuffix(text, "\n")}
}

func newSender(t *testing.T, b *box) *mailpit.Sender {
	t.Helper()
	s, err := mailpit.New(mailpit.Config{Addr: b.addr(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMailpit_Contract: the development SMS box passes the SMS contract
// over an SMTP double in Mailpit's place.
func TestMailpit_Contract(t *testing.T) {
	t.Parallel()
	smscontract.Run(t, func(t *testing.T) smscontract.Harness {
		b := startBox(t)
		return smscontract.Harness{
			Adapter: newSender(t, b),
			Delivered: func() []sms.Message {
				var out []sms.Message
				for _, m := range b.mails(t) {
					out = append(out, m.sms())
				}
				return out
			},
			Next: b.queue,
		}
	})
}

// TestMailpit_CodeOnlyInTheBody: the e-mail goes to the number's address
// of the reserved test domain; the subject names the reference and never
// the text or the code; the body holds the number and the text.
func TestMailpit_CodeOnlyInTheBody(t *testing.T) {
	t.Parallel()
	b := startBox(t)
	s := newSender(t, b)
	rc, err := s.Send(t.Context(), sms.Message{To: smscontract.Number, Text: smscontract.Text, Ref: smscontract.Ref})
	if err != nil || !strings.HasPrefix(rc.ID, "mailpit-") || s.Name() != "mailpit" {
		t.Fatalf("Send = %+v, %v (name %q)", rc, err, s.Name())
	}
	got := b.mails(t)
	if len(got) != 1 {
		t.Fatalf("%d e-mails", len(got))
	}
	m := got[0]
	subject := m.header.Get("Subject")
	if m.rcpt != "905550000001@sms.koro.test" || subject != "Koro SMS ("+smscontract.Ref+")" ||
		!strings.Contains(m.header.Get("From"), mailpit.From) {
		t.Fatalf("recipient %q, subject %q, from %q", m.rcpt, subject, m.header.Get("From"))
	}
	if strings.Contains(subject, "482915") || strings.Contains(subject, smscontract.Number) {
		t.Fatalf("the subject %q carries the code or the number", subject)
	}
	if !strings.Contains(m.body, "To: "+smscontract.Number) || !strings.Contains(m.body, smscontract.Text) {
		t.Fatalf("body %q", m.body)
	}
	if !strings.HasPrefix(m.header.Get("Content-Type"), "text/plain") {
		t.Fatalf("content type %q, want plain text", m.header.Get("Content-Type"))
	}
}

// TestMailpit_NewRefusesBadConfig: an address without a port, a bad port
// and a missing timeout are refused before any connection.
func TestMailpit_NewRefusesBadConfig(t *testing.T) {
	t.Parallel()
	for _, c := range []mailpit.Config{
		{Addr: "localhost", Timeout: time.Second},
		{Addr: ":1025", Timeout: time.Second},
		{Addr: "localhost:" + strconv.Itoa(70000), Timeout: time.Second},
		{Addr: "localhost:1025"},
	} {
		if _, err := mailpit.New(c); err == nil {
			t.Errorf("New(%+v) accepted", c)
		}
	}
}
