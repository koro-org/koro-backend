package notify

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
)

// Consumer-side ports (05 §2.1 ports.go): the narrow interfaces notify needs
// from other modules, the platform or later tickets. The composition root
// adapts the providers to them (cmd/koro/wire_notify.go).

// ConsentChecker answers which users have a commercial consent in force. It
// is the consumer-side port of consent.Checker.FilterActive (01 §4.3; 06
// H-43 orchestrator note): unscoped grants only, so a per-registration
// consent never unlocks marketing.
//
// Deps.Consent == nil is the safe default: commercial messages are rejected
// with ErrCommercialDisabled.
type ConsentChecker interface {
	// FilterActive returns those of userIDs whose latest consent for purpose
	// (consent_purposes.code, see PurposeCommercialEmail) is a grant.
	FilterActive(ctx context.Context, userIDs []uuid.UUID, purpose string) ([]uuid.UUID, error)
}

// PushMessage is what a push adapter delivers: the fixed text of the
// template and opaque IDs only, never personal data (02 §6.3).
type PushMessage struct {
	DeliveryID uuid.UUID
	UserID     uuid.UUID
	Kind       string // template ID
	Title      string
	Body       string
	LinkPath   string
	Data       map[string]string // opaque IDs
}

// PushSender delivers push notifications. The real adapter (FCM/APNs over
// Store.ActivePushTokens) arrives with BE-055; until then NoopPush is used.
type PushSender interface {
	// Name is stored as the delivery's provider.
	Name() string
	// SendPush delivers m to the devices of m.UserID and returns the provider
	// message ID ("" if there is none).
	SendPush(ctx context.Context, m PushMessage) (providerMessageID string, err error)
}

// RecipientIndexer derives the blind index of an e-mail address (HMAC of the
// normalised address, 02 §6.1, §6.2): notification_deliveries.recipient_bidx
// and email_suppressions.email_bidx. The composition root adapts
// platform/crypto.Indexer. Required.
type RecipientIndexer interface {
	// Index receives the address trimmed and lower-cased and returns 32 bytes.
	Index(email string) []byte
}

// AuditLogger is what notify needs from audit (01 §4.1): preference and push
// token changes, suppression removal and the break-glass reads are written
// in the caller's transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// WebhookInbox stores provider webhooks for processing in the worker
// (platform/inbox; 05 §14.4). *inbox.Inbox implements it.
type WebhookInbox interface {
	// Receive stores the delivery under (provider, eventKey) and enqueues its
	// processing; isNew=false for a delivery stored before.
	Receive(ctx context.Context, provider, eventKey string, sigValid bool, headers map[string]string, payload []byte) (isNew bool, err error)
}

// AuditFeed reads the chained audit log in chain order (06 H-40, H-43: the
// admin grant and break-glass notifications are derived from audit
// records). The composition root passes audit's store, whose Chained and
// HeadAt have exactly these signatures; notify never writes through it.
type AuditFeed interface {
	// Chained returns up to limit chained rows with chain_seq > after, in
	// chain_seq order.
	Chained(ctx context.Context, after int64, limit int) ([]audit.Record, error)
	// HeadAt returns the newest chained row with chained_at < before.
	HeadAt(ctx context.Context, before time.Time) (audit.Link, error)
}

// CursorStore keeps the position of the audit alert job between runs.
type CursorStore interface {
	// Load returns the stored position; ok=false when there is none.
	Load(ctx context.Context, name string) (pos int64, ok bool, err error)
	// Save stores the position.
	Save(ctx context.Context, name string, pos int64) error
}

// PlatformAdmins lists the users holding an effective platform_admin role
// (06 H-33: status active or revoke_pending). identity provides it
// (BE-016); nil → the admin alerts of 06 H-40 cannot be addressed and are
// skipped with a warning.
type PlatformAdmins interface {
	PlatformAdmins(ctx context.Context) ([]uuid.UUID, error)
}
