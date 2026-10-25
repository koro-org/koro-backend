package notify

import (
	"time"
	"uuid"
)

// Channel is the delivery channel of a message (notification_deliveries.channel).
type Channel string

// Delivery channels (02 §6.1).
const (
	ChannelEmail Channel = "email"
	ChannelPush  Channel = "push"
	ChannelInApp Channel = "in_app"
)

func (c Channel) valid() bool {
	return c == ChannelEmail || c == ChannelPush || c == ChannelInApp
}

// Category separates informational from commercial messages
// (notification_deliveries.category). The separation is a legal one (Law
// 6563): a transactional message must not promote goods or services, and a
// commercial message needs the recipient's prior consent and an opt-out.
type Category string

// Message categories (02 §6.1).
const (
	// CategoryTransactional is an informational message the user needs or
	// asked for (verification, ticket, registration status, security).
	CategoryTransactional Category = "transactional"
	// CategoryCommercial is a commercial electronic message. It is sent only
	// with an active consent, carries List-Unsubscribe and the commercial
	// footer.
	CategoryCommercial Category = "commercial"
)

func (c Category) valid() bool {
	return c == CategoryTransactional || c == CategoryCommercial
}

// Consent purposes (consent_purposes.code, 02 §4.1) checked before a
// commercial message is delivered.
const (
	PurposeCommercialEmail = "commercial_email"
	PurposeCommercialPush  = "commercial_push"
)

// Status is the delivery status (notification_deliveries.status, 02 §6.1).
type Status string

// Delivery statuses. Send writes queued, sent, failed, suppressed (the
// address is on the suppression list), skipped_no_consent (commercial without
// consent) and skipped_preference (the user switched the category off); the
// provider webhook moves sent e-mails to delivered, bounced or complained.
const (
	StatusQueued            Status = "queued"
	StatusSent              Status = "sent"
	StatusDelivered         Status = "delivered"
	StatusBounced           Status = "bounced"
	StatusComplained        Status = "complained"
	StatusFailed            Status = "failed"
	StatusSuppressed        Status = "suppressed"
	StatusSkippedNoConsent  Status = "skipped_no_consent"
	StatusSkippedPreference Status = "skipped_preference"
)

// Message is one notification to send (01 §4.5).
//
// Neither the recipient address nor Data is ever stored or logged by notify:
// the delivery record holds only IDs, the template name and a recipient blind
// index.
type Message struct {
	// UserID, when set, resolves preferences, consent and push tokens. It is
	// required for push and in_app messages and for commercial messages.
	UserID *uuid.UUID
	// ToEmail is required on the email channel. notify does not resolve
	// recipients (01 §5.3): the caller reads the address from identity.
	ToEmail  string
	Channel  Channel
	Category Category
	// Template is "<module>.<name>" (02 §6.1 CHECK), for example
	// "notify.test_message"; the files live in templates/<module>/<name>.*.
	Template string
	// Locale is the preferred language ("tr", "en", "tr-TR"). Empty or
	// unsupported falls back to the default locale (tr).
	Locale string
	// Data feeds the e-mail template as {{.Data.<key>}}; a key the template
	// reads but Data lacks is an error, never an empty string in a mail.
	//
	// On the in_app and push channels Data is not rendered: it travels with
	// the notification (in_app_notifications.data, the push payload) and may
	// hold only opaque IDs (UUID values under snake_case keys), never personal
	// data. The visible text of those channels is fixed per template.
	Data map[string]any
	// Inline are CID images (QR PNG). Each one must be referenced from the
	// HTML as src="cid:<CID>".
	Inline []InlineAttachment
	// Attachments are .ics and PDF files.
	Attachments []Attachment
	// DedupeKey makes Send idempotent, for example
	// "ticket:<registration_id>:v<ticket_version>". At most 200 characters and
	// never an address or other personal data: it is stored in clear.
	DedupeKey string
	// UnsubscribeURL is the HTTPS endpoint of the RFC 8058 one-click
	// unsubscribe. Required for commercial e-mail, ignored otherwise.
	UnsubscribeURL string

	// LinkPath is the in-app deep link of an in_app or push message, one
	// of the catalog forms (links.go: built only by the route builders,
	// such as MySecurityEventsLink). Ignored on the email channel.
	LinkPath Link
	// SourceOrgID names the org an in_app message comes from (optional).
	SourceOrgID *uuid.UUID
}

// InlineAttachment is an image embedded in the HTML part (multipart/related).
type InlineAttachment struct {
	CID         string // referenced as cid:<CID>; no angle brackets
	ContentType string // image/png, image/jpeg or image/gif
	Data        []byte
}

// Attachment is a file attached to the message.
type Attachment struct {
	Filename    string // a plain file name, no path
	ContentType string // text/calendar or application/pdf
	Data        []byte
}

// Delivery is the record of one notification (notification_deliveries). It
// carries no address and no template data.
type Delivery struct {
	ID     uuid.UUID
	UserID *uuid.UUID
	// RecipientIndex is the blind index (HMAC) of the normalised address for
	// recipients without a user; nil when no RecipientIndexer is wired.
	RecipientIndex    []byte
	Channel           Channel
	Category          Category
	Template          string
	TemplateVersion   int
	DedupeKey         string
	Provider          string // "smtp", "mem", "noop", ...
	ProviderMessageID string
	Status            Status
	Attempts          int
	LastError         string // scrubbed provider error, at most 2000 characters
	SentAt            *time.Time
	DeliveredAt       *time.Time // set by the provider webhook (delivered)
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
