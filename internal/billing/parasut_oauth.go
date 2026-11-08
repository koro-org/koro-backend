package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"koro/internal/platform/obs"
)

// Paraşüt's OAuth 2.0 token endpoint (devhub.parasut.com "OAuth2 ile Kimlik
// Doğrulama" and its AI rules file, read 2026-11-08; BE-053):
//
//	POST https://id.parasut.com/realms/parasut/protocol/openid-connect/token
//	     Content-Type: application/x-www-form-urlencoded
//	     grant_type=refresh_token&client_id=…&client_secret=…&refresh_token=…
//	→ {"access_token":"…","token_type":"Bearer","expires_in":7200,
//	   "refresh_token":"…","refresh_expires_in":2592000,"scope":"openid"}
//
// The refresh "usually" returns a new refresh token, which replaces the
// old one (an answer without one keeps the current); refreshes must not
// run in parallel with the same refresh token (ProviderTokens' row lock).
// The staging realm is id-staging.parasut.com. The first token pair comes
// from the authorization-code flow with PKCE (not in Koro yet: an
// administrator enters it, adminPutIntegration).
//
// Rules: an OAuth error answer (400/401: invalid_grant, invalid_client …)
// is ErrRefreshRefused carrying the error code only (never
// error_description, never a token); 429, 5xx and transport errors are
// transient. Redirects are never followed.
const (
	// DefaultParasutTokenURL is Paraşüt's production token endpoint.
	DefaultParasutTokenURL = "https://id.parasut.com/realms/parasut/protocol/openid-connect/token" //nolint:gosec // a public endpoint, not a credential
	// DefaultParasutTimeout bounds one request to Paraşüt.
	DefaultParasutTimeout = 10 * time.Second

	maxTokenAnswer = 64 << 10
)

// reOAuthCode is an OAuth error code worth repeating (RFC 6749 §5.2 shape).
var reOAuthCode = regexp.MustCompile(`^[a-z_]{1,40}$`)

// ParasutOAuthConfig configures ParasutOAuth.
type ParasutOAuthConfig struct {
	TokenURL     string        // DefaultParasutTokenURL when empty
	ClientID     string        // the API client of Koro's Paraşüt application
	ClientSecret string        // its secret ("" for a public client)
	Timeout      time.Duration // DefaultParasutTimeout when ≤ 0
	HTTPClient   *http.Client  // replaces the client built from Timeout (tests)
	Log          *slog.Logger  // one debug line per call (HTTP status); nil discards
}

// ParasutOAuth is the TokenRefresher of Paraşüt.
type ParasutOAuth struct {
	url, id, secret string
	http            *http.Client
	log             *slog.Logger
}

var _ TokenRefresher = (*ParasutOAuth)(nil)

// NewParasutOAuth checks c.
func NewParasutOAuth(c ParasutOAuthConfig) (*ParasutOAuth, error) {
	raw := strings.TrimSpace(c.TokenURL)
	if raw == "" {
		raw = DefaultParasutTokenURL
	}
	if err := checkAPIURL(raw); err != nil {
		return nil, fmt.Errorf("billing parasut: BILLING_PARASUT_TOKEN_URL %w", err)
	}
	if strings.TrimSpace(c.ClientID) == "" {
		return nil, errors.New("billing parasut: BILLING_PARASUT_CLIENT_ID is required")
	}
	if c.Log == nil {
		c.Log = obs.Discard()
	}
	return &ParasutOAuth{url: raw, id: c.ClientID, secret: c.ClientSecret, http: httpClient(c.HTTPClient, c.Timeout), log: c.Log}, nil
}

// Refresh implements TokenRefresher.
func (o *ParasutOAuth) Refresh(ctx context.Context, refreshToken string) (TokenGrant, error) {
	if refreshToken == "" {
		return TokenGrant{}, fmt.Errorf("%w: empty refresh token", ErrRefreshRefused)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {o.id}, "refresh_token": {refreshToken}}
	if o.secret != "" {
		form.Set("client_secret", o.secret)
	}
	body := []byte(form.Encode())
	defer clear(body) // holds the refresh token and the client secret
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(body))
	if err != nil {
		return TokenGrant{}, errors.New("billing parasut: token: cannot build the request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return TokenGrant{}, fmt.Errorf("billing parasut: token: %s", transportError(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxTokenAnswer))
	defer clear(raw) // holds the new tokens
	o.log.LogAttrs(ctx, slog.LevelDebug, "parasut token call", slog.Int("status", resp.StatusCode))
	switch {
	case resp.StatusCode/100 == 2:
		var a struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
		}
		if json.Unmarshal(raw, &a) != nil || a.AccessToken == "" {
			return TokenGrant{}, errors.New("billing parasut: token: malformed answer")
		}
		return TokenGrant{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken,
			ExpiresIn: time.Duration(max(a.ExpiresIn, 0)) * time.Second}, nil
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		var a struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &a)
		code := "?"
		if reOAuthCode.MatchString(a.Error) {
			code = a.Error
		}
		return TokenGrant{}, fmt.Errorf("%w: Paraşüt answered HTTP %d (%s)", ErrRefreshRefused, resp.StatusCode, code)
	case resp.StatusCode == http.StatusTooManyRequests:
		return TokenGrant{}, errors.New("billing parasut: token: HTTP 429 (rate limit)")
	}
	return TokenGrant{}, fmt.Errorf("billing parasut: token: HTTP %d", resp.StatusCode)
}

// httpClient returns given, or a client bounded by timeout that never
// follows a redirect (the bearer token would follow it).
func httpClient(given *http.Client, timeout time.Duration) *http.Client {
	if given != nil {
		return given
	}
	if timeout <= 0 {
		timeout = DefaultParasutTimeout
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// checkAPIURL accepts an http(s) URL without credentials, query or fragment.
func checkAPIURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" {
		return errors.New("must be an http(s) URL without credentials, query or fragment")
	}
	return nil
}

// transportError is the text of a failed request without the URL (which
// may carry a query) and scrubbed.
func transportError(err error) string {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	return safeText(err.Error())
}

// safeText scrubs personal data, credentials and control characters from
// a provider's or a transport's text and bounds it to maxErrorText runes.
func safeText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, obs.Scrub(s))
	s = reLongDigits.ReplaceAllString(s, "***")
	if r := []rune(s); len(r) > maxErrorText {
		s = string(r[:maxErrorText])
	}
	return strings.TrimSpace(s)
}

// reLongDigits masks tax and national numbers (ten or more digits) that a
// provider's message may repeat.
var reLongDigits = regexp.MustCompile(`\d{10,}`)

// maxErrorText bounds a provider's text in an error (invoices.last_error
// takes 2000).
const maxErrorText = 300
