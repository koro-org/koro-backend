package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"uuid"

	"koro/internal/platform/clock"
)

// PushTokens is what the push transport needs from the store.
type PushTokens interface {
	ActivePushTokens(ctx context.Context, userID uuid.UUID) ([]PushToken, error)
	RevokePushToken(ctx context.Context, id uuid.UUID, at time.Time) (bool, error)
}

// pushProvider delivers one push to one device. Errors: errTokenInvalid
// (the token is dead: revoke it), ErrPermanent (refused, a retry cannot
// help), anything else is transient. Errors never contain the device token
// or a credential.
type pushProvider interface {
	name() string
	send(ctx context.Context, deviceToken string, m PushMessage) (messageID string, err error)
}

// errTokenInvalid marks a device token the provider reports unregistered or
// malformed.
var errTokenInvalid = errors.New("push: device token is invalid")

// WebPushSubscriptions is what the push transport needs from the store for
// browsers (BE-173).
type WebPushSubscriptions interface {
	WebPushSubscriptionsOf(ctx context.Context, userID uuid.UUID) ([]WebPushSubscription, error)
	DropWebPushSubscription(ctx context.Context, id uuid.UUID) (bool, error)
}

// PushTransportConfig configures PushTransport. At least one provider.
type PushTransportConfig struct {
	// Tokens are the device tokens of FCM and APNs (required with either).
	Tokens PushTokens
	FCM    *FCM  // android, web (and ios when APNs is not configured)
	APNs   *APNs // ios
	// WebPush sends to the browsers of Subscriptions (required with it;
	// BE-173).
	WebPush       *WebPush
	Subscriptions WebPushSubscriptions
	Clock         clock.Clock
	Log           *slog.Logger
}

// PushTransport is the real PushSender (BE-055, BE-173): it sends a message
// to every reachable surface of the user the message allows — iOS devices
// over APNs, Android devices and FCM web tokens over FCM HTTP v1 (iOS over
// FCM when only FCM is configured: the app then registers FCM tokens), and
// browsers subscribed over the Push API with Koro's VAPID key (RFC 8030,
// 8291, 8292). A target the provider reports invalid is dropped (a device
// token is revoked, push_tokens.revoked_at; a web push subscription is
// deleted) and never tried again. The payload is the template's fixed
// text, the in-app link and opaque IDs (02 §6.3: no personal data); the
// delivery ID is the collapse key, so a retried delivery replaces the
// earlier notification instead of stacking.
//
// Outcome: success when at least one target took the message or no target
// failed transiently (no target, all dropped); an error (retried by the
// worker) only when every attempt failed and one of them was transient.
type PushTransport struct {
	c PushTransportConfig
}

var _ PushSender = (*PushTransport)(nil)

// NewPushTransport validates c.
func NewPushTransport(c PushTransportConfig) (*PushTransport, error) {
	if c.Clock == nil || c.Log == nil {
		return nil, errors.New("notify: push transport needs Clock and Log")
	}
	if c.FCM == nil && c.APNs == nil && c.WebPush == nil {
		return nil, errors.New("notify: push transport needs FCM, APNs or web push")
	}
	if (c.FCM != nil || c.APNs != nil) && c.Tokens == nil {
		return nil, errors.New("notify: push transport needs Tokens for FCM and APNs")
	}
	if c.WebPush != nil && c.Subscriptions == nil {
		return nil, errors.New("notify: push transport needs Subscriptions for web push")
	}
	return &PushTransport{c: c}, nil
}

// Name implements PushSender.
func (*PushTransport) Name() string { return "push" }

// WebPushKey is the VAPID public key browsers subscribe with; "" when web
// push is not configured.
func (p *PushTransport) WebPushKey() string {
	if p.c.WebPush == nil {
		return ""
	}
	return p.c.WebPush.PublicKey()
}

func (p *PushTransport) provider(pl PushPlatform) pushProvider {
	switch {
	case pl == PushIOS && p.c.APNs != nil:
		return p.c.APNs
	case p.c.FCM != nil:
		return p.c.FCM
	default:
		return nil
	}
}

// tokenChannel is the preference channel of a device token: an FCM web
// token lives in a browser.
func tokenChannel(pl PushPlatform) Channel {
	if pl == PushWeb {
		return ChannelWebPush
	}
	return ChannelPush
}

// pushTarget is one surface of the user: a device token or a web push
// subscription, with the adapter that reaches it and the store call that
// drops it when the provider reports it invalid.
type pushTarget struct {
	idKey    string // log attribute naming id
	id       uuid.UUID
	provider string // "" = no configured provider serves it (skipped)
	send     func(ctx context.Context, m PushMessage) (string, error)
	drop     func(ctx context.Context) (bool, error)
}

// targets lists the surfaces of m.UserID that m allows.
func (p *PushTransport) targets(ctx context.Context, m PushMessage) ([]pushTarget, error) {
	var out []pushTarget
	if p.c.Tokens != nil && (m.allows(ChannelPush) || m.allows(ChannelWebPush)) {
		tokens, err := p.c.Tokens.ActivePushTokens(ctx, m.UserID)
		if err != nil {
			return nil, err
		}
		for _, t := range tokens {
			if !m.allows(tokenChannel(t.Platform)) {
				continue
			}
			tg := pushTarget{idKey: "push_token_id", id: t.ID,
				drop: func(ctx context.Context) (bool, error) { return p.c.Tokens.RevokePushToken(ctx, t.ID, p.c.Clock.Now()) }}
			if prov := p.provider(t.Platform); prov != nil {
				tg.provider = prov.name()
				tg.send = func(ctx context.Context, m PushMessage) (string, error) { return prov.send(ctx, t.Token, m) }
			}
			out = append(out, tg)
		}
	}
	if p.c.WebPush != nil && m.allows(ChannelWebPush) {
		subs, err := p.c.Subscriptions.WebPushSubscriptionsOf(ctx, m.UserID)
		if err != nil {
			return nil, err
		}
		for _, s := range subs {
			out = append(out, pushTarget{idKey: "web_push_subscription_id", id: s.ID, provider: p.c.WebPush.name(),
				send: func(ctx context.Context, m PushMessage) (string, error) { return p.c.WebPush.send(ctx, s, m) },
				drop: func(ctx context.Context) (bool, error) { return p.c.Subscriptions.DropWebPushSubscription(ctx, s.ID) }})
		}
	}
	return out, nil
}

// SendPush implements PushSender.
func (p *PushTransport) SendPush(ctx context.Context, m PushMessage) (string, error) {
	targets, err := p.targets(ctx, m)
	if err != nil {
		return "", err
	}
	var (
		firstID                        string
		sent, dropped, skipped, failed int
		transient                      error
		permanent                      error
	)
	for _, t := range targets {
		if t.send == nil {
			skipped++
			continue
		}
		id, err := t.send(ctx, m)
		switch {
		case err == nil:
			sent++
			if firstID == "" {
				firstID = id
			}
		case errors.Is(err, errTokenInvalid):
			// The outcome is recorded even if the context ends meanwhile.
			if ok, derr := t.drop(context.WithoutCancel(ctx)); derr != nil {
				failed++
				transient = derr
			} else if ok {
				dropped++
			}
		case errors.Is(err, ErrPermanent):
			failed++
			permanent = err
			p.c.Log.LogAttrs(ctx, slog.LevelWarn, "push refused", slog.String("delivery_id", m.DeliveryID.String()),
				slog.String(t.idKey, t.id.String()), slog.String("provider", t.provider), slog.String("error", err.Error()))
		default:
			failed++
			transient = err
		}
	}
	p.c.Log.LogAttrs(ctx, slog.LevelInfo, "push sent", slog.String("delivery_id", m.DeliveryID.String()),
		slog.Int("devices", len(targets)), slog.Int("sent", sent), slog.Int("revoked", dropped),
		slog.Int("skipped", skipped), slog.Int("failed", failed))
	switch {
	case sent > 0:
		return firstID, nil
	case transient != nil:
		return "", fmt.Errorf("push: no device reached: %w", transient)
	case permanent != nil:
		return "", permanent
	default:
		return "", nil // no (valid) device: nothing to deliver
	}
}

// ---------------------------------------------------------------- helpers

// maxProviderError bounds a provider error text kept in errors.
const maxProviderError = 300

// providerText keeps a provider error text short, on one line.
func providerText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return truncate(s, maxProviderError)
}

// transportText reduces an HTTP client error to its cause: url.Error would
// repeat the request URL, which holds the device token for APNs.
func transportText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return providerText(err.Error())
}

// pushData is the custom data of a push: the opaque IDs of the message plus
// the in-app link, the template and the delivery ID.
func pushData(m PushMessage) map[string]string {
	out := make(map[string]string, len(m.Data)+3)
	for k, v := range m.Data {
		out[k] = v
	}
	out["kind"], out["delivery_id"] = m.Kind, m.DeliveryID.String()
	if m.LinkPath != "" {
		out["link"] = m.LinkPath
	}
	return out
}
