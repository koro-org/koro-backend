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

// PushTransportConfig configures PushTransport. At least one provider.
type PushTransportConfig struct {
	Tokens PushTokens
	FCM    *FCM  // android, web (and ios when APNs is not configured)
	APNs   *APNs // ios
	Clock  clock.Clock
	Log    *slog.Logger
}

// PushTransport is the real PushSender (BE-055): it sends a message to every
// active device of the user, iOS over APNs and Android/web over FCM HTTP v1
// (iOS over FCM when only FCM is configured: the app then registers FCM
// tokens). A token the provider reports invalid is revoked
// (push_tokens.revoked_at) and never tried again. The payload is the
// template's fixed text, the in-app link and opaque IDs (02 §6.3: no
// personal data); the delivery ID is the collapse key, so a retried
// delivery replaces the earlier notification on the device instead of
// stacking.
//
// Outcome: success when at least one device took the message or no device
// failed transiently (no device, all revoked); an error (retried by the
// worker) only when every attempt failed and one of them was transient.
type PushTransport struct {
	c PushTransportConfig
}

var _ PushSender = (*PushTransport)(nil)

// NewPushTransport validates c.
func NewPushTransport(c PushTransportConfig) (*PushTransport, error) {
	if c.Tokens == nil || c.Clock == nil || c.Log == nil {
		return nil, errors.New("notify: push transport needs Tokens, Clock and Log")
	}
	if c.FCM == nil && c.APNs == nil {
		return nil, errors.New("notify: push transport needs FCM or APNs")
	}
	return &PushTransport{c: c}, nil
}

// Name implements PushSender.
func (*PushTransport) Name() string { return "push" }

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

// SendPush implements PushSender.
func (p *PushTransport) SendPush(ctx context.Context, m PushMessage) (string, error) {
	tokens, err := p.c.Tokens.ActivePushTokens(ctx, m.UserID)
	if err != nil {
		return "", err
	}
	var (
		firstID                        string
		sent, revoked, skipped, failed int
		transient                      error
		permanent                      error
	)
	for _, t := range tokens {
		prov := p.provider(t.Platform)
		if prov == nil {
			skipped++
			continue
		}
		id, err := prov.send(ctx, t.Token, m)
		switch {
		case err == nil:
			sent++
			if firstID == "" {
				firstID = id
			}
		case errors.Is(err, errTokenInvalid):
			// The outcome is recorded even if the context ends meanwhile.
			if ok, rerr := p.c.Tokens.RevokePushToken(context.WithoutCancel(ctx), t.ID, p.c.Clock.Now()); rerr != nil {
				failed++
				transient = rerr
			} else if ok {
				revoked++
			}
		case errors.Is(err, ErrPermanent):
			failed++
			permanent = err
			p.c.Log.LogAttrs(ctx, slog.LevelWarn, "push refused", slog.String("delivery_id", m.DeliveryID.String()),
				slog.String("push_token_id", t.ID.String()), slog.String("provider", prov.name()), slog.String("error", err.Error()))
		default:
			failed++
			transient = err
		}
	}
	p.c.Log.LogAttrs(ctx, slog.LevelInfo, "push sent", slog.String("delivery_id", m.DeliveryID.String()),
		slog.Int("devices", len(tokens)), slog.Int("sent", sent), slog.Int("revoked", revoked),
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
