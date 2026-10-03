// Package notify is the notification core (01 §4.5): embedded, versioned
// message templates, an e-mail composer with CID inline images and
// attachments, the Sender contract with idempotent delivery (dedupe key), the
// SMTP adapter (go-mail) and the ports for consent, push and in-app delivery.
//
// What lives elsewhere: the delivery table, preferences, the suppression list,
// the provider webhook and the /me/notifications endpoints (BE-015); real
// push (BE-055); the ticket e-mail itself (BE-031).
//
// Rules the package enforces:
//
//   - Send runs only in the worker (01 §0.5): a request context is refused.
//   - notify never resolves a recipient (01 §5.3); the caller passes the address.
//   - Neither the address nor the template data is stored or logged: the
//     delivery record holds IDs, the template name and a blind index.
//   - Commercial messages need an active consent and carry List-Unsubscribe;
//     with no ConsentChecker wired they are rejected (safe default).
//
// Other modules add their messages as template files only, under
// templates/<module>/ (BE-001 R8); the file layout and the rules a template
// set must satisfy (both locales, a meta file with version and category) are
// described at the top of templates.go and checked at start by
// EmbeddedTemplates.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/platform/clock"
)

// DefaultLease is how long an unfinished attempt keeps its delivery before a
// repeated Send may take it over (a crashed worker). It must exceed the SMTP
// timeout; after it, the worst case is one duplicate e-mail, never a lost one.
const DefaultLease = 2 * time.Minute

// Sender is the cross-module contract of notify (01 §4.5): consumers define a
// port with this signature and cmd/koro/wire.go binds the service to it.
type Sender interface {
	// Send delivers m. It is idempotent per m.DedupeKey: a repeated Send
	// returns the delivery of the first one without sending again (a delivery
	// that failed is attempted again, one that finished is final).
	//
	// A nil error with a delivery ID means the message was handed to the
	// provider (status sent) or deliberately skipped (skipped_no_consent).
	// Errors are ErrInvalidMessage, ErrCommercialDisabled, ErrNotInWorker,
	// ErrDeliveryInFlight, ErrDeliveryRejected, ErrDeliveryFailed or a store
	// error; IsPermanent tells the worker handler whether to retry.
	//
	// Send must run in the worker (a request context is refused).
	Send(ctx context.Context, m Message) (deliveryID uuid.UUID, err error)
}

// Service is the notify service. BE-015 extends it with preferences and the
// inbox; today it is the Sender.
type Service interface {
	Sender
}

// Deps are the dependencies of the notify service. Store, Transport,
// Templates, Clock, Log and FromAddress are required (New panics otherwise,
// 05 §3); the rest has a safe default.
type Deps struct {
	Store     Store
	Transport EmailTransport
	Templates *Templates
	Clock     clock.Clock
	Log       *slog.Logger

	// FromAddress and FromName are the sender of every e-mail (SMTP_FROM).
	FromAddress string
	FromName    string

	// Consent checks commercial messages. nil → commercial messages are
	// rejected with ErrCommercialDisabled.
	Consent ConsentChecker
	// Push and InApp deliver those channels. nil → the no-op adapters.
	Push  PushSender
	InApp InAppSender
	// Recipients derives the recipient blind index. nil → none is stored.
	Recipients RecipientIndexer
	// Lease overrides DefaultLease (tests).
	Lease time.Duration
}

func (d *Deps) mustValid() {
	var missing []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil},
		{"Transport", d.Transport != nil},
		{"Templates", d.Templates != nil},
		{"Clock", d.Clock != nil},
		{"Log", d.Log != nil},
		{"FromAddress", validEmail(d.FromAddress)},
	} {
		if !c.ok {
			missing = append(missing, fmt.Errorf("notify.Deps.%s is missing or invalid", c.name))
		}
	}
	if err := errors.Join(missing...); err != nil {
		panic(err)
	}
	if d.Push == nil {
		d.Push = NoopPush{}
	}
	if d.InApp == nil {
		d.InApp = NoopInApp{}
	}
	if d.Lease <= 0 {
		d.Lease = DefaultLease
	}
}

// Module is what notify hands to the composition root. Outbox subscriptions
// and jobs (notify.purge) arrive with BE-015/BE-057.
type Module struct {
	Service Service
}

// New builds the notify module.
func New(d Deps) Module {
	d.mustValid()
	return Module{Service: &service{d: d}}
}
