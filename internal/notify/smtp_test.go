package notify_test

import (
	"errors"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"koro/internal/notify"
)

// fakeSMTP is a minimal SMTP server: enough of RFC 5321 for go-mail, with
// scripted replies. It records the commands and the DATA of every message.
type fakeSMTP struct {
	ln net.Listener

	mu        sync.Mutex
	rcptReply string // reply to RCPT TO
	dataReply string // reply after DATA
	silent    bool   // accept the connection but never greet
	commands  []string
	data      []string
}

func startFakeSMTP(t *testing.T, mod func(*fakeSMTP)) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{ln: ln, rcptReply: "250 OK", dataReply: "250 2.0.0 queued"}
	if mod != nil {
		mod(s)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *fakeSMTP) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	if s.silent {
		time.Sleep(5 * time.Second)
		return
	}
	tc := textproto.NewConn(c)
	_ = tc.PrintfLine("220 fake ESMTP")
	for {
		line, err := tc.ReadLine()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, line)
		rcpt, data := s.rcptReply, s.dataReply
		s.mu.Unlock()

		switch cmd := strings.ToUpper(strings.Fields(line + " x")[0]); cmd {
		case "EHLO":
			_ = tc.PrintfLine("250-fake")
			_ = tc.PrintfLine("250-8BITMIME")
			_ = tc.PrintfLine("250 SMTPUTF8")
		case "HELO", "MAIL", "RSET", "NOOP":
			_ = tc.PrintfLine("250 OK")
		case "RCPT":
			_ = tc.PrintfLine("%s", rcpt)
		case "DATA":
			_ = tc.PrintfLine("354 go ahead")
			b, err := tc.ReadDotBytes()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.data = append(s.data, string(b))
			s.mu.Unlock()
			_ = tc.PrintfLine("%s", data)
		case "QUIT":
			_ = tc.PrintfLine("221 bye")
			return
		default:
			_ = tc.PrintfLine("502 not implemented")
		}
	}
}

func (s *fakeSMTP) received() (commands, data []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...), append([]string(nil), s.data...)
}

func sampleEmail() notify.Email {
	return notify.Email{
		MessageID: "01a1@koro.test", FromAddress: "noreply@koro.test", FromName: "Koro",
		To: "ayse.yilmaz@ogrenci.edu.tr", Date: t0, Subject: "Merhaba", Text: "metin", HTML: "<p>metin</p>",
	}
}

func newTransport(t *testing.T, s *fakeSMTP, mod func(*notify.SMTPConfig)) *notify.SMTPTransport {
	t.Helper()
	cfg := notify.SMTPConfig{Host: "127.0.0.1", Port: s.port(), TLS: notify.SMTPTLSNone, Timeout: 5 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	tr, err := notify.NewSMTPTransport(cfg)
	if err != nil {
		t.Fatalf("NewSMTPTransport() error = %v", err)
	}
	return tr
}

func TestSMTPTransport_SendsTheMessage(t *testing.T) {
	t.Parallel()
	srv := startFakeSMTP(t, nil)
	tr := newTransport(t, srv, nil)

	id, err := tr.Send(t.Context(), sampleEmail())
	if err != nil || id != "01a1@koro.test" || tr.Name() != "smtp" {
		t.Fatalf("Send() = (%q, %v), name %q; want the Message-ID as provider ID", id, err, tr.Name())
	}
	cmds, data := srv.received()
	if !containsPrefix(cmds, "MAIL FROM:<NOREPLY@KORO.TEST>") || !containsLine(cmds, "RCPT TO:<ayse.yilmaz@ogrenci.edu.tr>") {
		t.Fatalf("envelope commands = %v", cmds)
	}
	if len(data) != 1 || !strings.Contains(data[0], "Message-ID: <01a1@koro.test>") || !strings.Contains(data[0], "Subject: Merhaba") {
		t.Fatalf("DATA = %q", data)
	}
	if !containsLine(cmds, "QUIT") {
		t.Fatalf("connection was not closed with QUIT: %v", cmds)
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

func TestSMTPTransport_ErrorClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		server        func(*fakeSMTP)
		wantPermanent bool
	}{
		{"recipient refused for good (550)", func(s *fakeSMTP) { s.rcptReply = "550 5.1.1 <ayse.yilmaz@ogrenci.edu.tr>: user unknown" }, true},
		{"content refused for good (554)", func(s *fakeSMTP) { s.dataReply = "554 5.7.1 message rejected as spam" }, true},
		{"recipient deferred (451)", func(s *fakeSMTP) { s.rcptReply = "451 4.3.0 try again later" }, false},
		{"mailbox full deferred (452)", func(s *fakeSMTP) { s.dataReply = "452 4.2.2 mailbox full" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := startFakeSMTP(t, tt.server)
			tr := newTransport(t, srv, nil)
			_, err := tr.Send(t.Context(), sampleEmail())
			if err == nil {
				t.Fatal("Send() succeeded")
			}
			if got := errors.Is(err, notify.ErrPermanent); got != tt.wantPermanent {
				t.Fatalf("errors.Is(err, ErrPermanent) = %v, want %v (err: %v)", got, tt.wantPermanent, err)
			}
		})
	}
}

func TestSMTPTransport_Unreachable_IsTransient(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // nothing listens there now
	tr, err := notify.NewSMTPTransport(notify.SMTPConfig{Host: "127.0.0.1", Port: port, TLS: notify.SMTPTLSNone, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Send(t.Context(), sampleEmail()); err == nil || errors.Is(err, notify.ErrPermanent) {
		t.Fatalf("Send() error = %v, want a transient error", err)
	}
}

func TestSMTPTransport_Timeout(t *testing.T) {
	t.Parallel()
	srv := startFakeSMTP(t, func(s *fakeSMTP) { s.silent = true })
	tr := newTransport(t, srv, func(c *notify.SMTPConfig) { c.Timeout = 300 * time.Millisecond })

	start := time.Now()
	_, err := tr.Send(t.Context(), sampleEmail())
	if err == nil || errors.Is(err, notify.ErrPermanent) {
		t.Fatalf("Send() error = %v, want a transient timeout", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Send() took %s, the timeout is 300ms", took)
	}
}

func TestSMTPTransport_StartTLSIsMandatory_NoDowngrade(t *testing.T) {
	t.Parallel()
	srv := startFakeSMTP(t, nil) // does not offer STARTTLS
	tr := newTransport(t, srv, func(c *notify.SMTPConfig) { c.TLS = notify.SMTPTLSStartTLS })

	_, err := tr.Send(t.Context(), sampleEmail())
	if err == nil || errors.Is(err, notify.ErrPermanent) {
		t.Fatalf("Send() error = %v, want a transient failure", err)
	}
	if cmds, data := srv.received(); len(data) != 0 || containsPrefix(cmds, "MAIL") {
		t.Fatalf("the message went out in clear text (commands %v, data %d)", cmds, len(data))
	}
}

func containsPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(strings.ToUpper(l), prefix) {
			return true
		}
	}
	return false
}

func TestSMTPTransport_UncomposableMessage_IsPermanent(t *testing.T) {
	t.Parallel()
	srv := startFakeSMTP(t, nil)
	tr := newTransport(t, srv, nil)
	e := sampleEmail()
	e.To = "not an address"
	if _, err := tr.Send(t.Context(), e); !errors.Is(err, notify.ErrPermanent) {
		t.Fatalf("Send() error = %v, want ErrPermanent", err)
	}
	if cmds, _ := srv.received(); len(cmds) != 0 {
		t.Fatalf("a message that cannot be composed touched the server: %v", cmds)
	}
}

func TestNewSMTPTransport_RejectsBadConfig(t *testing.T) {
	t.Parallel()
	ok := notify.SMTPConfig{Host: "smtp.koro.test", Port: 587, TLS: notify.SMTPTLSStartTLS, Timeout: time.Second}
	tests := []struct {
		name string
		mod  func(c *notify.SMTPConfig)
	}{
		{"unknown TLS mode", func(c *notify.SMTPConfig) { c.TLS = "opportunistic" }},
		{"empty TLS mode", func(c *notify.SMTPConfig) { c.TLS = "" }},
		{"no host", func(c *notify.SMTPConfig) { c.Host = "" }},
		{"port zero", func(c *notify.SMTPConfig) { c.Port = 0 }},
		{"zero timeout", func(c *notify.SMTPConfig) { c.Timeout = 0 }},
		{"credentials in clear text", func(c *notify.SMTPConfig) { c.TLS, c.Username, c.Password = notify.SMTPTLSNone, "u", "p" }},
	}
	if _, err := notify.NewSMTPTransport(ok); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := ok
			tt.mod(&c)
			if _, err := notify.NewSMTPTransport(c); err == nil {
				t.Fatal("NewSMTPTransport() accepted the config")
			}
		})
	}
	creds := ok
	creds.Username, creds.Password = "user", "pass"
	if _, err := notify.NewSMTPTransport(creds); err != nil {
		t.Fatalf("credentials with STARTTLS rejected: %v", err)
	}
}

func TestPermanent_WrapsAndIsRecognised(t *testing.T) {
	t.Parallel()
	cause := errors.New("550 nope")
	err := notify.Permanent(cause)
	if !errors.Is(err, notify.ErrPermanent) || !errors.Is(err, cause) {
		t.Fatalf("Permanent(err) = %v, want it to match ErrPermanent and the cause", err)
	}
	if errors.Is(cause, notify.ErrPermanent) {
		t.Fatal("the plain cause matches ErrPermanent")
	}
}
