// Package notify is the notification module (01 §4.5; BE-005, BE-015):
// embedded, versioned message templates, an e-mail composer with CID inline
// images and attachments, the Sender contract with idempotent delivery
// (dedupe key) over notification_deliveries, the SMTP adapter (go-mail), the
// suppression list fed by the provider webhooks (SES over SNS, Resend over
// svix), user preferences, the in-app inbox, push token registration and the
// alerts derived from the audit log (06 H-40, H-43).
//
// Push (BE-055): PushTransport sends over FCM HTTP v1 (Android, web) and
// APNs (iOS, token-based) and revokes the device tokens the providers
// report invalid; without provider settings NoopPush records the delivery.
//
// What lives elsewhere: the retention job notify.purge (BE-057); the ticket
// e-mail itself (BE-031).
//
// Rules the package enforces:
//
//   - Send runs only in the worker (01 §0.5): a request context is refused.
//   - notify never resolves a recipient (01 §5.3); the caller passes the address.
//   - Neither the address nor the template data is stored or logged: the
//     delivery record holds IDs, the template name and a blind index.
//   - Informational and commercial messages are separate (Law 6563):
//     templates declare their category and the footer follows it; a
//     commercial message needs a consent in force (consent.Checker
//     FilterActive) and carries List-Unsubscribe; with no ConsentChecker
//     wired it is rejected (safe default). Preferences never govern
//     commercial messages, and consent never governs transactional ones.
//   - A suppressed address (hard bounce, complaint) receives no e-mail: the
//     delivery is recorded as suppressed.
//   - in_app and push texts are fixed per template and carry opaque IDs
//     only: no personal data reaches an in-app notification or a push
//     payload.
//   - Provider webhooks are verified (signature, topic allow-list), stored in
//     platform/inbox under the provider's event ID (deduplication) and
//     processed in the worker.
//
// Other modules add their messages as template files only, under
// templates/<module>/ (BE-001 R8); the file layout and the rules a template
// set must satisfy are described at the top of templates.go and checked at
// start by EmbeddedTemplates.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/inbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// DefaultLease is how long an unfinished attempt keeps its delivery before a
// repeated Send may take it over (a crashed worker). It must exceed the SMTP
// timeout; after it, the worst case is one duplicate e-mail, never a lost one.
const DefaultLease = 2 * time.Minute

// Sender is the cross-module contract of notify (01 §4.5): consumers define a
// port with this signature and the composition root binds the service to it.
type Sender interface {
	// Send delivers m. It is idempotent per m.DedupeKey: a repeated Send
	// returns the delivery of the first one without sending again (a delivery
	// that failed is attempted again, one that finished is final).
	//
	// A nil error with a delivery ID means the message was handed to the
	// provider (status sent) or deliberately not sent (suppressed,
	// skipped_no_consent, skipped_preference). Errors are ErrInvalidMessage,
	// ErrCommercialDisabled, ErrNotInWorker, ErrDeliveryInFlight,
	// ErrDeliveryRejected, ErrDeliveryFailed or a store error; IsPermanent
	// tells the worker handler whether to retry.
	//
	// Send must run in the worker (a request context is refused).
	Send(ctx context.Context, m Message) (deliveryID uuid.UUID, err error)
}

// Preferences answers whether a user wants a category on a channel (01 §4.5).
type Preferences interface {
	// Allowed reports whether userID receives category on ch: the stored
	// choice, else DefaultEnabled; security is always allowed.
	Allowed(ctx context.Context, userID uuid.UUID, category PrefCategory, ch Channel) (bool, error)
}

// Access is the break-glass context of a support or admin read (03 §6.3):
// the X-Koro-Access-Reason and X-Koro-Ticket-Ref headers.
type Access struct {
	Reason    string
	TicketRef string
}

// Service is the notify service: the cross-module contracts plus the HTTP use
// cases (04 §2.19, §2.0). Every method that takes an actor calls authz.Can
// first (05 §5.3).
type Service interface {
	Sender
	Preferences

	// ListMine is listMyNotifications (notify.in_app.list_own).
	ListMine(ctx context.Context, a authz.Actor, q InAppQuery) (InAppPage, error)
	// MarkRead is markMyNotificationRead (notify.in_app.mark_read).
	MarkRead(ctx context.Context, a authz.Actor, id uuid.UUID) error
	// MarkAllRead is markAllMyNotificationsRead (notify.in_app.mark_read).
	MarkAllRead(ctx context.Context, a authz.Actor) error
	// MyPreferences is getMyNotificationPreferences: every (category,
	// channel) pair, defaults included (notify.preference.read_own).
	MyPreferences(ctx context.Context, a authz.Actor) ([]Preference, error)
	// PutMyPreferences is putMyNotificationPreferences
	// (notify.preference.update_own); switching security off is 422.
	PutMyPreferences(ctx context.Context, a authz.Actor, items []Preference) ([]Preference, error)
	// RegisterPushToken is registerMyPushToken (notify.push_token.manage).
	RegisterPushToken(ctx context.Context, a authz.Actor, in PushTokenInput) (uuid.UUID, error)
	// DeletePushToken is deleteMyPushToken (notify.push_token.manage).
	DeletePushToken(ctx context.Context, a authz.Actor, id uuid.UUID) error
	// AdminListSuppressions is adminQueryEmailSuppressions
	// (notify.suppression.manage, B).
	AdminListSuppressions(ctx context.Context, a authz.Actor, access Access, q SuppressionQuery) (SuppressionPage, error)
	// AdminDeleteSuppression is adminDeleteEmailSuppression
	// (notify.suppression.manage, B).
	AdminDeleteSuppression(ctx context.Context, a authz.Actor, access Access, id string) error
	// AdminListUserDeliveries is adminListUserDeliveries (notify.delivery.list,
	// B): IDs, channel, template and status only, never content or data.
	AdminListUserDeliveries(ctx context.Context, a authz.Actor, access Access, userID uuid.UUID, q DeliveryQuery) (DeliveryPage, error)
	// ReceiveEmailWebhook is receiveEmailWebhook
	// (notify.provider_webhook.receive): verifies the provider signature and
	// stores the delivery in the webhook inbox. ErrUnauthenticated (401) for
	// a bad signature.
	ReceiveEmailWebhook(ctx context.Context, provider string, headers map[string]string, body []byte) error
}

// Deps are the dependencies of the notify module. Store, Tx, Transport,
// Templates, Clock, Log, FromAddress, Recipients and Audit are required (New
// panics otherwise, 05 §3); the rest has a safe default.
type Deps struct {
	Store     Store
	Tx        db.TxManager
	Transport EmailTransport
	Templates *Templates
	Clock     clock.Clock
	Log       *slog.Logger

	// FromAddress and FromName are the sender of every e-mail (SMTP_FROM).
	FromAddress string
	FromName    string

	// Recipients derives the blind index of addresses (delivery records and
	// the suppression list).
	Recipients RecipientIndexer
	// Audit records the user and admin changes and the break-glass reads.
	Audit AuditLogger

	// Consent checks commercial messages. nil → commercial messages are
	// rejected with ErrCommercialDisabled.
	Consent ConsentChecker
	// Push delivers the push channel (PushTransport). nil → NoopPush.
	Push PushSender

	// Inbox stores provider webhooks. nil → receiveEmailWebhook answers 503.
	Inbox WebhookInbox
	// Webhooks are the provider verification settings; a provider without
	// its settings rejects every delivery (401).
	Webhooks WebhookConfig

	// AuditFeed, Cursor and Admins feed the audit alert job (06 H-40, H-43).
	// AuditFeed nil → no job.
	AuditFeed AuditFeed
	Cursor    CursorStore
	Admins    PlatformAdmins

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
		{"Tx", d.Tx != nil},
		{"Transport", d.Transport != nil},
		{"Templates", d.Templates != nil},
		{"Clock", d.Clock != nil},
		{"Log", d.Log != nil},
		{"FromAddress", validEmail(d.FromAddress)},
		{"Recipients", d.Recipients != nil},
		{"Audit", d.Audit != nil},
		{"Cursor", d.AuditFeed == nil || d.Cursor != nil},
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
	if d.Lease <= 0 {
		d.Lease = DefaultLease
	}
}

// Module is what notify hands to the composition root.
type Module struct {
	Service Service
	// Jobs are the scheduled jobs: notify.send_audit_alerts when an
	// AuditFeed is wired. The notify.purge row of 01 §7 is the Sweeper,
	// run by kvkk.retention_sweep (BE-057).
	Jobs []queue.Job
	// Webhook is the platform/inbox processor of provider "email".
	Webhook inbox.Processor
	// Privacy is the KVKK provider (01 §3.3).
	Privacy privacy.Provider
	// Sweeper ends the retention of deliveries, suppressions, push tokens
	// and in-app notifications (01 §3.3, 02 §24.2; BE-057).
	Sweeper privacy.RetentionSweeper
}

// New builds the notify module.
func New(d Deps) Module {
	d.mustValid()
	s := &service{d: d}
	m := Module{Service: s, Webhook: s.webhookProcessor(), Privacy: privacyProvider{s: s}, Sweeper: sweeper{s: s}}
	if d.AuditFeed != nil {
		m.Jobs = append(m.Jobs, s.auditAlertJob())
	}
	return m
}
