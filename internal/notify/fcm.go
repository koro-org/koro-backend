package notify

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"koro/internal/platform/clock"
)

// FCM defaults (Firebase Cloud Messaging HTTP v1).
const (
	FCMBaseURL      = "https://fcm.googleapis.com"
	fcmScope        = "https://www.googleapis.com/auth/firebase.messaging"
	fcmJWTBearer    = "urn:ietf:params:oauth:grant-type:jwt-bearer" //nolint:gosec // G101: the OAuth grant type name, not a credential
	fcmAssertionTTL = time.Hour
	fcmTokenMargin  = 5 * time.Minute
	pushMaxBody     = 64 << 10
)

var reFCMProject = regexp.MustCompile(`^[a-z][a-z0-9-]{4,61}[a-z0-9]$`)

// FCMConfig configures the FCM adapter.
type FCMConfig struct {
	// ProjectID is the Firebase project (PUSH_FCM_PROJECT_ID).
	ProjectID string
	// CredentialsJSON is the service account key file
	// (PUSH_FCM_CREDENTIALS_FILE, a Docker secret).
	CredentialsJSON []byte
	// BaseURL overrides FCMBaseURL (tests).
	BaseURL string
	Timeout time.Duration
	Clock   clock.Clock
	// HTTPClient replaces the client built from Timeout (tests).
	HTTPClient *http.Client
}

// FCM sends through FCM HTTP v1 with an OAuth 2.0 access token obtained
// from a service-account JWT (RS256), cached until five minutes before it
// expires and fetched again on a 401.
type FCM struct {
	project  string
	base     string
	email    string
	keyID    string
	tokenURI string
	key      *rsa.PrivateKey
	client   *http.Client
	clk      clock.Clock

	mu     sync.Mutex
	access string
	expiry time.Time
}

type serviceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

// NewFCM parses the service account and validates the settings. Errors
// never repeat the credentials.
func NewFCM(c FCMConfig) (*FCM, error) {
	if !reFCMProject.MatchString(c.ProjectID) {
		return nil, errors.New("notify: PUSH_FCM_PROJECT_ID must be a Firebase project ID")
	}
	if c.Clock == nil {
		return nil, errors.New("notify: FCM needs a clock")
	}
	var sa serviceAccount
	if err := json.Unmarshal(c.CredentialsJSON, &sa); err != nil {
		return nil, errors.New("notify: PUSH_FCM_CREDENTIALS is not a JSON service account key")
	}
	if sa.Type != "service_account" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("notify: PUSH_FCM_CREDENTIALS must be a service_account key with client_email and private_key")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	if u, err := url.Parse(sa.TokenURI); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("notify: the service account token_uri must be an https URL")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, errors.New("notify: the service account private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*rsa.PrivateKey)
	if err != nil || !ok {
		return nil, errors.New("notify: the service account private_key must be a PKCS#8 RSA key")
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = FCMBaseURL
	}
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("notify: the FCM base URL must be https")
	}
	client := c.HTTPClient
	if client == nil {
		client = pushHTTPClient(c.Timeout)
	}
	return &FCM{
		project: c.ProjectID, base: base, email: sa.ClientEmail, keyID: sa.PrivateKeyID, tokenURI: sa.TokenURI,
		key: key, client: client, clk: c.Clock,
	}, nil
}

// pushHTTPClient is the outbound client of the push providers (05 §10.6):
// explicit timeouts, no redirects, HTTP/2 (APNs requires it).
func pushHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{ForceAttemptHTTP2: true, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout, IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 8},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (*FCM) name() string { return "fcm" }

// accessToken returns the cached token or exchanges a new assertion.
func (f *FCM) accessToken(ctx context.Context, force bool) (string, error) {
	now := f.clk.Now()
	f.mu.Lock()
	if !force && f.access != "" && now.Before(f.expiry.Add(-fcmTokenMargin)) {
		tok := f.access
		f.mu.Unlock()
		return tok, nil
	}
	f.mu.Unlock()

	assertion, err := f.assertion(now)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {fcmJWTBearer}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("fcm: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fcm: token exchange: %s", transportText(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, pushMaxBody))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &e)
		return "", fmt.Errorf("fcm: token exchange: HTTP %d %s", resp.StatusCode, providerText(e.Error+" "+e.Desc))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(raw, &out) != nil || out.AccessToken == "" || out.ExpiresIn <= 0 {
		return "", errors.New("fcm: token exchange: malformed answer")
	}
	f.mu.Lock()
	f.access, f.expiry = out.AccessToken, now.Add(time.Duration(out.ExpiresIn)*time.Second)
	f.mu.Unlock()
	return out.AccessToken, nil
}

// assertion is the RS256 service-account JWT of the token exchange.
func (f *FCM) assertion(now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if f.keyID != "" {
		header["kid"] = f.keyID
	}
	claims := map[string]any{
		"iss": f.email, "scope": fcmScope, "aud": f.tokenURI,
		"iat": now.Unix(), "exp": now.Add(fcmAssertionTTL).Unix(),
	}
	input, err := jwtInput(header, claims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("fcm: sign assertion: %w", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// jwtInput is base64url(header) "." base64url(claims).
func jwtInput(header map[string]string, claims map[string]any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c), nil
}

type fcmMessage struct {
	Message fcmBody `json:"message"`
}

type fcmBody struct {
	Token        string            `json:"token"`
	Notification fcmNotification   `json:"notification"`
	Data         map[string]string `json:"data,omitempty"`
	Android      fcmAndroid        `json:"android"`
	APNs         *fcmAPNs          `json:"apns,omitempty"`
}

type fcmNotification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type fcmAndroid struct {
	CollapseKey string `json:"collapse_key"`
	Priority    string `json:"priority"`
	TTL         string `json:"ttl"`
}

type fcmAPNs struct {
	Headers map[string]string `json:"headers"`
}

// send implements pushProvider.
func (f *FCM) send(ctx context.Context, deviceToken string, m PushMessage) (string, error) {
	body, err := json.Marshal(fcmMessage{Message: fcmBody{
		Token:        deviceToken,
		Notification: fcmNotification{Title: m.Title, Body: m.Body},
		Data:         pushData(m),
		Android:      fcmAndroid{CollapseKey: m.DeliveryID.String(), Priority: "high", TTL: "86400s"},
		APNs:         &fcmAPNs{Headers: map[string]string{"apns-collapse-id": m.DeliveryID.String()}},
	}})
	if err != nil {
		return "", fmt.Errorf("fcm: encode: %w", err)
	}
	for attempt := 0; ; attempt++ {
		tok, err := f.accessToken(ctx, attempt > 0)
		if err != nil {
			return "", err
		}
		id, status, err := f.post(ctx, tok, body)
		if status == http.StatusUnauthorized && attempt == 0 {
			continue // the access token was revoked or expired early: fetch a new one once
		}
		return id, err
	}
}

func (f *FCM) post(ctx context.Context, accessToken string, body []byte) (string, int, error) {
	u := f.base + "/v1/projects/" + f.project + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("fcm: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("fcm: %s", transportText(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, pushMaxBody))
	if resp.StatusCode == http.StatusOK {
		var ok struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &ok)
		return ok.Name, resp.StatusCode, nil
	}
	return "", resp.StatusCode, fcmError(resp.StatusCode, raw)
}

// fcmError maps an FCM v1 error (FcmError.errorCode, google.rpc status).
func fcmError(status int, raw []byte) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	code := e.Error.Status
	for _, d := range e.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
		}
	}
	err := fmt.Errorf("fcm: HTTP %d %s: %s", status, code, providerText(e.Error.Message))
	switch {
	case code == "UNREGISTERED" || code == "SENDER_ID_MISMATCH" || status == http.StatusNotFound,
		code == "INVALID_ARGUMENT" && strings.Contains(strings.ToLower(e.Error.Message), "registration token"):
		return fmt.Errorf("%w: %w", errTokenInvalid, err)
	case status == http.StatusTooManyRequests || status >= 500 || status == http.StatusUnauthorized:
		return err
	default: // INVALID_ARGUMENT (payload), PERMISSION_DENIED, THIRD_PARTY_AUTH_ERROR
		return Permanent(err)
	}
}
