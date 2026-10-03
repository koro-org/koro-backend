package notify

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"koro/internal/platform/clock"
)

// APNs endpoints and token lifetime. Apple accepts a provider token for one
// hour and refuses refreshes more often than every 20 minutes
// (TooManyProviderTokenUpdates): Koro signs a new one after 50 minutes.
const (
	APNsProductionURL = "https://api.push.apple.com"
	APNsSandboxURL    = "https://api.sandbox.push.apple.com"
	APNsTokenTTL      = 50 * time.Minute
	apnsExpiry        = 24 * time.Hour
)

var (
	reAPNsID     = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	reAPNsTopic  = regexp.MustCompile(`^[A-Za-z0-9.-]{1,155}$`)
	reAPNsDevice = regexp.MustCompile(`^[0-9a-fA-F]{64,200}$`)
)

// APNsConfig configures the APNs adapter (token-based authentication).
type APNsConfig struct {
	// KeyPEM is the .p8 signing key (PUSH_APNS_KEY_FILE, a Docker secret).
	KeyPEM []byte
	// KeyID and TeamID are the 10-character IDs of the key and the team.
	KeyID, TeamID string
	// Topic is the app's bundle ID (apns-topic).
	Topic string
	// Sandbox selects the development endpoint.
	Sandbox bool
	// BaseURL overrides the endpoint (tests).
	BaseURL string
	Timeout time.Duration
	Clock   clock.Clock
	// HTTPClient replaces the client built from Timeout (tests).
	HTTPClient *http.Client
}

// APNs sends to Apple over HTTP/2 with an ES256 provider token that is
// reused for APNsTokenTTL, then rotated; an ExpiredProviderToken answer
// rotates it at once and the push is retried once.
type APNs struct {
	key    *ecdsa.PrivateKey
	keyID  string
	teamID string
	topic  string
	base   string
	client *http.Client
	clk    clock.Clock

	mu       sync.Mutex
	token    string
	issuedAt time.Time
}

// NewAPNs parses the key and validates the settings. Errors never repeat
// the key.
func NewAPNs(c APNsConfig) (*APNs, error) {
	if !reAPNsID.MatchString(c.KeyID) || !reAPNsID.MatchString(c.TeamID) {
		return nil, errors.New("notify: PUSH_APNS_KEY_ID and PUSH_APNS_TEAM_ID must be 10-character Apple IDs")
	}
	if !reAPNsTopic.MatchString(c.Topic) {
		return nil, errors.New("notify: PUSH_APNS_TOPIC must be the app's bundle ID")
	}
	if c.Clock == nil {
		return nil, errors.New("notify: APNs needs a clock")
	}
	block, _ := pem.Decode(c.KeyPEM)
	if block == nil {
		return nil, errors.New("notify: PUSH_APNS_KEY is not a PEM (.p8) key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("notify: PUSH_APNS_KEY must be a PKCS#8 P-256 key (.p8)")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	switch {
	case base != "":
	case c.Sandbox:
		base = APNsSandboxURL
	default:
		base = APNsProductionURL
	}
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("notify: the APNs base URL must be https")
	}
	client := c.HTTPClient
	if client == nil {
		client = pushHTTPClient(c.Timeout)
	}
	return &APNs{key: key, keyID: c.KeyID, teamID: c.TeamID, topic: c.Topic, base: base, client: client, clk: c.Clock}, nil
}

func (*APNs) name() string { return "apns" }

// providerToken returns the cached token, or a new one when it is older
// than APNsTokenTTL or force is set.
func (a *APNs) providerToken(force bool) (string, error) {
	now := a.clk.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !force && a.token != "" && now.Sub(a.issuedAt) < APNsTokenTTL && !now.Before(a.issuedAt) {
		return a.token, nil
	}
	input, err := jwtInput(map[string]string{"alg": "ES256", "kid": a.keyID}, map[string]any{"iss": a.teamID, "iat": now.Unix()})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(input))
	der, err := ecdsa.SignASN1(rand.Reader, a.key, sum[:])
	if err != nil {
		return "", fmt.Errorf("apns: sign: %w", err)
	}
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		return "", fmt.Errorf("apns: sign: %w", err)
	}
	raw := make([]byte, 64) // JWS ES256: R ‖ S, 32 bytes each
	sig.R.FillBytes(raw[:32])
	sig.S.FillBytes(raw[32:])
	a.token, a.issuedAt = input+"."+base64.RawURLEncoding.EncodeToString(raw), now
	return a.token, nil
}

type apnsPayload struct {
	APS  apnsAPS           `json:"aps"`
	Data map[string]string `json:"koro"`
}

type apnsAPS struct {
	Alert apnsAlert `json:"alert"`
	Sound string    `json:"sound"`
}

type apnsAlert struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// send implements pushProvider.
func (a *APNs) send(ctx context.Context, deviceToken string, m PushMessage) (string, error) {
	if !reAPNsDevice.MatchString(deviceToken) {
		return "", fmt.Errorf("%w: not an APNs device token", errTokenInvalid)
	}
	body, err := json.Marshal(apnsPayload{APS: apnsAPS{Alert: apnsAlert{Title: m.Title, Body: m.Body}, Sound: "default"}, Data: pushData(m)})
	if err != nil {
		return "", fmt.Errorf("apns: encode: %w", err)
	}
	for attempt := 0; ; attempt++ {
		tok, err := a.providerToken(attempt > 0)
		if err != nil {
			return "", err
		}
		id, reason, err := a.post(ctx, tok, deviceToken, m, body)
		if reason == "ExpiredProviderToken" && attempt == 0 {
			continue
		}
		return id, err
	}
}

func (a *APNs) post(ctx context.Context, providerToken, deviceToken string, m PushMessage, body []byte) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/3/device/"+deviceToken, bytes.NewReader(body))
	if err != nil {
		return "", "", errors.New("apns: cannot build the request")
	}
	req.Header.Set("Authorization", "bearer "+providerToken)
	req.Header.Set("apns-topic", a.topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("apns-collapse-id", m.DeliveryID.String())
	req.Header.Set("apns-expiration", strconv.FormatInt(a.clk.Now().Add(apnsExpiry).Unix(), 10))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("apns: %s", transportText(err)) // never the URL: it holds the device token
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, pushMaxBody))
	if resp.StatusCode == http.StatusOK {
		return resp.Header.Get("apns-id"), "", nil
	}
	var e struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &e)
	reason := providerText(e.Reason)
	err = fmt.Errorf("apns: HTTP %d %s", resp.StatusCode, reason)
	switch {
	case resp.StatusCode == http.StatusGone, reason == "BadDeviceToken", reason == "DeviceTokenNotForTopic", reason == "Unregistered":
		return "", reason, fmt.Errorf("%w: %w", errTokenInvalid, err)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500, reason == "ExpiredProviderToken":
		return "", reason, err
	default: // InvalidProviderToken, BadTopic, PayloadTooLarge, ...
		return "", reason, Permanent(err)
	}
}
