// Package mailpit is the development SMS box (SMS_PROVIDER=mailpit;
// BE-178): every SMS becomes a plain-text e-mail to the local Mailpit
// (deploy/compose.dev.yaml, SMTP without TLS or login), so a developer of
// the web or mobile client reads the one-time code in Mailpit's inbox
// instead of a phone. Configuration refuses it in staging and prod
// (config.ProviderMailpit is a development adapter).
//
// The e-mail goes from From to <number digits>@ToDomain (the number of the
// SMS, so Mailpit's list shows whose code it is); the subject carries only
// the reference, never the text (a code in a subject ends up in
// notifications and list views); the body holds the number and the text.
//
// It follows the rules of internal/platform/sms (smscontract pins them):
// a message sms.CheckMessage refuses is never handed over; Mailpit's
// refusal of the recipient is sms.ErrInvalidNumber, of the content
// sms.ErrRejected, a temporary SMTP reply (4xx) sms.ErrThrottled, anything
// else (Mailpit is not running) transient. Its errors never hold the
// number, the text or the SMTP server's text (go-mail's errors repeat the
// recipient), and it never logs.
package mailpit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"uuid"

	mail "github.com/wneessen/go-mail"

	"koro/internal/platform/sms"
)

// Name is the adapter's name (SMS_PROVIDER).
const Name = "mailpit"

// Addresses of the e-mails (reserved .test domain: never routable).
const (
	From     = "sms@koro.test"
	ToDomain = "sms.koro.test"
)

// Config configures Sender.
type Config struct {
	// Addr is Mailpit's SMTP address, host:port.
	Addr string
	// Timeout bounds the dial and one whole send.
	Timeout time.Duration
}

// Sender hands SMS to Mailpit; safe for concurrent use (one connection
// per message, as notify's SMTP transport).
type Sender struct {
	client  *mail.Client
	timeout time.Duration
}

// New validates c and prepares the client. It does not connect.
func New(c Config) (*Sender, error) {
	host, p, err := net.SplitHostPort(c.Addr)
	port, perr := strconv.Atoi(p)
	switch {
	case err != nil || host == "" || perr != nil || port < 1 || port > 65535:
		return nil, errors.New("sms/mailpit: the address must be host:port")
	case c.Timeout <= 0:
		return nil, errors.New("sms/mailpit: the timeout must be positive")
	}
	client, err := mail.NewClient(host, mail.WithPort(port), mail.WithTimeout(c.Timeout), mail.WithTLSPolicy(mail.NoTLS))
	if err != nil {
		return nil, fmt.Errorf("sms/mailpit: client: %w", err)
	}
	return &Sender{client: client, timeout: c.Timeout}, nil
}

// Name names the adapter.
func (*Sender) Name() string { return Name }

// Recipient is the e-mail address an SMS to number (E.164) goes to.
func Recipient(number string) string { return strings.TrimPrefix(number, "+") + "@" + ToDomain }

// Subject is the e-mail subject of an SMS with reference ref: no text, no
// code.
func Subject(ref string) string { return "Koro SMS (" + ref + ")" }

// Send implements the consumers' sender port.
func (s *Sender) Send(ctx context.Context, m sms.Message) (sms.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return sms.Receipt{}, err
	}
	if err := sms.CheckMessage(m); err != nil {
		return sms.Receipt{}, err
	}
	id := uuid.NewV7().String()
	msg := mail.NewMsg(mail.WithNoDefaultUserAgent())
	if err := msg.From(From); err != nil {
		return sms.Receipt{}, fmt.Errorf("%w: sms/mailpit: compose the sender", sms.ErrRejected)
	}
	if err := msg.To(Recipient(m.To)); err != nil {
		return sms.Receipt{}, fmt.Errorf("sms/mailpit: compose: %w", errInvalidNumber)
	}
	msg.Subject(Subject(m.Ref))
	msg.SetMessageIDWithValue(id + "@" + ToDomain)
	msg.SetDate()
	msg.SetBodyString(mail.TypeTextPlain, "To: "+m.To+"\nRef: "+m.Ref+"\n\n"+m.Text+"\n")

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.client.DialAndSendWithContext(ctx, msg); err != nil {
		return sms.Receipt{}, classify(ctx, err)
	}
	return sms.Receipt{ID: "mailpit-" + id}, nil
}

var errInvalidNumber = fmt.Errorf("%w: %w", sms.ErrRejected, sms.ErrInvalidNumber)

// classify maps a go-mail error of a send under ctx to the sms errors.
// The texts are fixed: go-mail's own (and the server's) repeat the
// recipient.
func classify(ctx context.Context, err error) error {
	var se *mail.SendError
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("sms/mailpit: send stopped: %w", ctx.Err())
	case !errors.As(err, &se):
		return errors.New("sms/mailpit: Mailpit is unreachable (is the dev stack up?)")
	case se.IsTemp():
		return fmt.Errorf("sms/mailpit: %w (SMTP %d)", sms.ErrThrottled, se.ErrorCode())
	case se.Reason == mail.ErrSMTPRcptTo:
		return fmt.Errorf("sms/mailpit: the recipient was refused (SMTP %d): %w", se.ErrorCode(), errInvalidNumber)
	case se.Reason == mail.ErrSMTPMailFrom || se.Reason == mail.ErrSMTPData || se.Reason == mail.ErrSMTPDataClose:
		return fmt.Errorf("sms/mailpit: %w (SMTP %d)", sms.ErrRejected, se.ErrorCode())
	}
	return fmt.Errorf("sms/mailpit: send failed (SMTP %d)", se.ErrorCode())
}
