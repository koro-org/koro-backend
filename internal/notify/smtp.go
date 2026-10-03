package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	mail "github.com/wneessen/go-mail"
)

// SMTP transport security modes, same values as SMTP_TLS.
const (
	SMTPTLSNone     = "none"
	SMTPTLSStartTLS = "starttls"
	SMTPTLSImplicit = "tls"
)

// SMTPConfig configures SMTPTransport. cmd/koro fills it from config.Notify.
type SMTPConfig struct {
	Host     string
	Port     int
	TLS      string // SMTPTLSNone (local Mailpit only), SMTPTLSStartTLS or SMTPTLSImplicit
	Username string // optional; needs TLS
	Password string
	// Timeout bounds the dial and one whole send.
	Timeout time.Duration
}

// SMTPTransport is the go-mail (R-002) EmailTransport. It opens one connection
// per message, which is safe for concurrent use; the volume of Koro (a few
// thousand mails a day) does not justify pooling.
type SMTPTransport struct {
	client  *mail.Client
	timeout time.Duration
}

// NewSMTPTransport validates c and prepares the client. It does not connect.
func NewSMTPTransport(c SMTPConfig) (*SMTPTransport, error) {
	if c.Timeout <= 0 {
		return nil, errors.New("notify: smtp: timeout must be positive")
	}
	opts := []mail.Option{mail.WithPort(c.Port), mail.WithTimeout(c.Timeout)}
	tlsConfig := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	switch c.TLS {
	case SMTPTLSNone:
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	case SMTPTLSStartTLS:
		// Mandatory, never opportunistic: no silent downgrade to clear text.
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory), mail.WithTLSConfig(tlsConfig))
	case SMTPTLSImplicit:
		opts = append(opts, mail.WithSSL(), mail.WithTLSConfig(tlsConfig))
	default:
		return nil, fmt.Errorf("notify: smtp: unknown TLS mode %q", c.TLS)
	}
	if c.Username != "" {
		if c.TLS == SMTPTLSNone {
			return nil, errors.New("notify: smtp: credentials need TLS (starttls or tls)")
		}
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthAutoDiscover), mail.WithUsername(c.Username), mail.WithPassword(c.Password))
	}
	client, err := mail.NewClient(c.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("notify: smtp client: %w", err)
	}
	return &SMTPTransport{client: client, timeout: c.Timeout}, nil
}

// Name implements EmailTransport.
func (*SMTPTransport) Name() string { return "smtp" }

// Send implements EmailTransport. The provider message ID is the Message-ID
// the transport set itself: plain SMTP returns none.
func (t *SMTPTransport) Send(ctx context.Context, e Email) (string, error) {
	msg, err := newMsg(e)
	if err != nil {
		// A message that cannot even be composed will never work.
		return "", Permanent(fmt.Errorf("compose message: %w", err))
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	if err := t.client.DialAndSendWithContext(ctx, msg); err != nil {
		return "", classifySMTPError(err)
	}
	return e.MessageID, nil
}

// classifySMTPError marks the failures a retry cannot fix: the server refused
// the recipient (RCPT TO) or the content (DATA) with a 5xx. Everything else,
// including a refused sender or login, is a configuration or network problem
// that a later attempt may survive.
func classifySMTPError(err error) error {
	var se *mail.SendError
	if errors.As(err, &se) && !se.IsTemp() &&
		(se.Reason == mail.ErrSMTPRcptTo || se.Reason == mail.ErrSMTPData || se.Reason == mail.ErrSMTPDataClose) {
		return Permanent(err)
	}
	return fmt.Errorf("smtp send: %w", err)
}

// newMsg builds the MIME message: text/plain and text/html alternatives, CID
// images as multipart/related parts of the HTML, attachments, and (commercial
// only) the RFC 8058 one-click List-Unsubscribe headers.
func newMsg(e Email) (*mail.Msg, error) {
	m := mail.NewMsg(mail.WithNoDefaultUserAgent())
	if err := m.FromFormat(e.FromName, e.FromAddress); err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	if err := m.To(e.To); err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	m.Subject(e.Subject)
	m.SetMessageIDWithValue(e.MessageID)
	if e.Date.IsZero() {
		m.SetDate()
	} else {
		m.SetDateWithValue(e.Date)
	}
	m.SetBodyString(mail.TypeTextPlain, e.Text)
	m.AddAlternativeString(mail.TypeTextHTML, e.HTML)

	for _, in := range e.Inline {
		err := m.EmbedReader(in.CID, bytes.NewReader(in.Data),
			mail.WithFileContentType(mail.ContentType(in.ContentType)),
			mail.WithFileContentID("<"+in.CID+">"))
		if err != nil {
			return nil, fmt.Errorf("inline %q: %w", in.CID, err)
		}
	}
	for _, a := range e.Attachments {
		err := m.AttachReader(a.Filename, bytes.NewReader(a.Data),
			mail.WithFileContentType(mail.ContentType(a.ContentType)))
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", a.Filename, err)
		}
	}
	if e.UnsubscribeURL != "" {
		if err := m.SetListUnsubscribeOneClick(e.UnsubscribeURL); err != nil {
			return nil, fmt.Errorf("list-unsubscribe: %w", err)
		}
	}
	return m, nil
}
