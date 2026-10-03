package notify

import (
	"context"
	"uuid"
)

// Consumer-side ports (05 §2.1 ports.go): the narrow interfaces notify needs
// from other modules or from later tickets. The composition root adapts the
// providers to them (cmd/koro/wire_notify.go); notify imports none of those
// modules.

// ConsentChecker answers whether a user has an active consent. It is the
// consumer-side port of consent.Checker.HasActive (01 §4.3); BE-012 provides
// the service and cmd/koro/wire_notify.go adapts it.
//
// Deps.Consent == nil is the safe default: commercial messages are rejected
// with ErrCommercialDisabled.
type ConsentChecker interface {
	// HasActive reports whether userID's latest consent record for purpose
	// (consent_purposes.code, see PurposeCommercialEmail) is a grant.
	HasActive(ctx context.Context, userID uuid.UUID, purpose string) (bool, error)
}

// PushSender delivers push notifications. The real adapter (FCM/APNs and the
// push_tokens lookup) arrives with BE-055; until then the no-op is used.
type PushSender interface {
	// Name is stored as the delivery's provider.
	Name() string
	// SendPush delivers m to the devices of m.UserID and returns the provider
	// message ID ("" if there is none).
	SendPush(ctx context.Context, m Message) (providerMessageID string, err error)
}

// InAppSender writes in-app notifications. The real adapter (the
// in_app_notifications table) arrives with BE-015; until then the no-op is
// used.
type InAppSender interface {
	// Name is stored as the delivery's provider.
	Name() string
	// SendInApp stores m for m.UserID and returns the notification ID ("" if
	// there is none).
	SendInApp(ctx context.Context, m Message) (providerMessageID string, err error)
}

// RecipientIndexer derives the recipient blind index of notification_deliveries
// (HMAC of the normalised address, 02 §6.1), so that deliveries to recipients
// without a user can be found without storing the address. BE-015 adapts
// platform/crypto.BlindIndex; nil leaves Delivery.RecipientIndex empty.
type RecipientIndexer interface {
	// Index receives the address trimmed and lower-cased.
	Index(email string) []byte
}
