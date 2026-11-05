package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/problem"
)

// MaxWebPushSubscriptions is the per-user cap of browser subscriptions
// (BE-173). A new browser beyond it replaces the least recently seen one.
const MaxWebPushSubscriptions = 10

// WebPushSubscriptionInput is the body of registerMyWebPushSubscription:
// PushSubscription.endpoint and the decoded keys (p256dh, auth) of
// PushSubscription.toJSON().
type WebPushSubscriptionInput struct {
	Endpoint string
	P256DH   []byte
	Auth     []byte
}

// WebPushSettings is what a browser needs before it subscribes
// (getMyWebPushConfig).
type WebPushSettings struct {
	// Enabled is false when no VAPID key is configured: the web push
	// channel is off and subscribing is 503.
	Enabled bool
	// VAPIDPublicKey is the applicationServerKey (base64url of the
	// uncompressed P-256 point); "" when disabled.
	VAPIDPublicKey string
}

func webPushResource(owner, id uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeWebPushSubscription, ID: id, OwnerUserID: &owner}
}

// MyWebPushSettings implements Service.
func (s *service) MyWebPushSettings(ctx context.Context, a authz.Actor) (WebPushSettings, error) {
	if err := authz.Can(ctx, a, authz.NotifyPushTokenManage, webPushResource(a.UserID, uuid.Nil())); err != nil {
		return WebPushSettings{}, err
	}
	return WebPushSettings{Enabled: s.d.WebPushKey != "", VAPIDPublicKey: s.d.WebPushKey}, nil
}

// RegisterWebPushSubscription implements Service. The endpoint must name a
// known push service (ValidWebPushEndpoint: the SSRF guard) and the keys
// must be a P-256 point and a 16-byte secret. The subscription is bound to
// the actor's session; the same endpoint registered again — after signing
// in to another account too — moves the one row (endpoint_sha256 is
// unique). Beyond MaxWebPushSubscriptions the least recently seen browser
// of the user is dropped. Neither the endpoint nor the keys are ever logged
// or audited.
func (s *service) RegisterWebPushSubscription(ctx context.Context, a authz.Actor, in WebPushSubscriptionInput) (uuid.UUID, error) {
	if err := authz.Can(ctx, a, authz.NotifyPushTokenManage, webPushResource(a.UserID, uuid.Nil())); err != nil {
		return uuid.Nil(), err
	}
	if s.d.WebPushKey == "" {
		return uuid.Nil(), ErrWebPushUnavailable
	}
	var fe []problem.FieldError
	if !ValidWebPushEndpoint(in.Endpoint) {
		fe = append(fe, problem.FieldError{Location: "body.endpoint",
			Message: "must be the https endpoint of a known push service (FCM, Mozilla, Apple, Microsoft)"})
	}
	if _, err := ecdh.P256().NewPublicKey(in.P256DH); err != nil {
		fe = append(fe, problem.FieldError{Location: "body.keys.p256dh", Message: "must be an uncompressed P-256 public key (65 bytes, base64url)"})
	}
	if len(in.Auth) != WebPushAuthLen {
		fe = append(fe, problem.FieldError{Location: "body.keys.auth", Message: "must be 16 bytes, base64url"})
	}
	if len(fe) > 0 {
		return uuid.Nil(), problem.ErrValidation.WithFields(fe...)
	}
	sum := sha256.Sum256([]byte(in.Endpoint))
	now := s.d.Clock.Now()
	sub := WebPushSubscription{ID: uuid.NewV7(), UserID: a.UserID, Endpoint: in.Endpoint, EndpointSHA256: sum[:],
		P256DH: in.P256DH, Auth: in.Auth, CreatedAt: now, LastSeenAt: now}
	if a.SessionID != uuid.Nil() {
		sid := a.SessionID
		sub.SessionID = &sid
	}
	var stored WebPushSubscription
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if stored, err = s.d.Store.UpsertWebPushSubscription(ctx, sub, MaxWebPushSubscriptions); err != nil {
			return err
		}
		id := stored.ID
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifyPushTokenManage), Outcome: audit.Success,
			ResourceType: authz.TypeWebPushSubscription, ResourceID: &id, Metadata: map[string]any{"op": "register"},
		}.By(a))
	})
	if err != nil {
		return uuid.Nil(), err
	}
	return stored.ID, nil
}

// DeleteWebPushSubscription implements Service: the row is deleted;
// another user's subscription is not found.
func (s *service) DeleteWebPushSubscription(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	if err := authz.Can(ctx, a, authz.NotifyPushTokenManage, webPushResource(a.UserID, id)); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.DeleteWebPushSubscription(ctx, a.UserID, id); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifyPushTokenManage), Outcome: audit.Success,
			ResourceType: authz.TypeWebPushSubscription, ResourceID: &id, Metadata: map[string]any{"op": "delete"},
		}.By(a))
	})
}
