package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Email is a fully composed e-mail, independent of the provider: SMTP today,
// an HTTP API (SES, Resend) tomorrow. Inline and Attachments are carried from
// the start (R-003 §2.5.6) because some providers need raw MIME for them.
type Email struct {
	// MessageID is the Message-ID without angle brackets, "<delivery id>@<domain>".
	// It is stable across retries of one delivery.
	MessageID   string
	FromAddress string
	FromName    string
	To          string
	Date        time.Time // Date header; the service sets it from the injected clock
	Subject     string    // one line
	Text        string    // text/plain alternative
	HTML        string    // text/html; refers to Inline parts as cid:<CID>
	Inline      []InlineAttachment
	Attachments []Attachment
	// UnsubscribeURL, when set, adds RFC 8058 one-click List-Unsubscribe
	// headers. It is set for commercial messages only.
	UnsubscribeURL string
}

// EmailTransport hands a composed e-mail to a provider.
type EmailTransport interface {
	// Name identifies the provider in the delivery record ("smtp", "mem").
	Name() string
	// Send delivers e and returns the provider's message ID. An error that
	// wraps ErrPermanent means the provider refused the message for good;
	// every other error is transient. Error text may contain the recipient
	// address: the caller scrubs it before logging or storing.
	Send(ctx context.Context, e Email) (providerMessageID string, err error)
}

// ErrPermanent marks a transport error that a retry cannot fix (SMTP 5xx on
// the recipient or the content). Wrap with Permanent.
var ErrPermanent = errors.New("permanent delivery failure")

// Permanent marks err as a permanent provider failure; errors.Is(…, ErrPermanent)
// holds for the result and the cause stays reachable with errors.Unwrap.
func Permanent(err error) error {
	return fmt.Errorf("%w: %w", ErrPermanent, err)
}

// MemTransport is an in-memory EmailTransport for tests and wiring tests: it
// records what would be sent and can be told to fail.
type MemTransport struct {
	mu   sync.Mutex
	sent []Email
	err  error
}

// NewMemTransport returns a transport that records every e-mail.
func NewMemTransport() *MemTransport { return &MemTransport{} }

// Name implements EmailTransport.
func (*MemTransport) Name() string { return "mem" }

// Send implements EmailTransport. It returns the Message-ID as provider ID.
func (t *MemTransport) Send(ctx context.Context, e Email) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("mem transport: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return "", t.err
	}
	t.sent = append(t.sent, e)
	return e.MessageID, nil
}

// FailWith makes every following Send return err (nil restores success).
func (t *MemTransport) FailWith(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.err = err
}

// Sent returns a copy of the recorded e-mails in send order.
func (t *MemTransport) Sent() []Email {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.sent)
}
