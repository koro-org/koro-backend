package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Consent configures the consent module (internal/consent). Owner: BE-012,
// BE-055 (İYS client), BE-062 (unsubscribe links); only tickets of the consent module edit this file and
// the "# --- consent ---" block of .env.example (BE-001 R7).
type Consent struct {
	// IYSRequired refuses to start without a real İYS client (H-14: live
	// operation needs İYS). While false and no CONSENT_IYS_BASE_URL is set,
	// submissions of commercial consents end as "skipped".
	IYSRequired bool `env:"CONSENT_IYS_REQUIRED" envDefault:"false"`

	// İYS API (BE-055). All of base URL, username, password, İYS code and
	// brand code enable the real client; none leaves the no-op client.
	IYSBaseURL       string        `env:"CONSENT_IYS_BASE_URL"`
	IYSUsername      string        `env:"CONSENT_IYS_USERNAME"`
	IYSPassword      Secret        `env:"CONSENT_IYS_PASSWORD"`
	IYSCode          string        `env:"CONSENT_IYS_CODE"`
	IYSBrandCode     string        `env:"CONSENT_IYS_BRAND_CODE"`
	IYSRecipientType string        `env:"CONSENT_IYS_RECIPIENT_TYPE" envDefault:"BIREYSEL"`
	IYSTimeout       time.Duration `env:"CONSENT_IYS_TIMEOUT" envDefault:"10s"`

	// UnsubscribeBaseURL is where the one-click unsubscribe links of
	// commercial e-mail point (BE-062; 6563 ret, RFC 8058): the link is
	// <base>/<token>. The API's own endpoint
	// (https://<api>/api/v1/unsubscribe-links) or a web page that renders
	// GET and forwards POST to it. Empty: no link is minted, so commercial
	// dispatches cannot use e-mail (push only).
	UnsubscribeBaseURL string `env:"CONSENT_UNSUBSCRIBE_BASE_URL"`
	// UnsubscribeTTL is how long an unsubscribe link stays valid.
	UnsubscribeTTL time.Duration `env:"CONSENT_UNSUBSCRIBE_TTL" envDefault:"8760h"`
}

// Bounds of CONSENT_UNSUBSCRIBE_TTL and CONSENT_UNSUBSCRIBE_BASE_URL.
const (
	minUnsubscribeTTL     = 24 * time.Hour
	maxUnsubscribeTTL     = 400 * 24 * time.Hour
	maxUnsubscribeBaseURL = 300
)

// IYSEnabled reports whether any İYS API setting is present.
func (s Consent) IYSEnabled() bool {
	return s.IYSBaseURL != "" || s.IYSUsername != "" || !s.IYSPassword.IsZero() || s.IYSCode != "" || s.IYSBrandCode != ""
}

func (s *Consent) validate(c Common) error {
	var errs []error
	s.IYSBaseURL = strings.TrimSpace(s.IYSBaseURL)
	if s.IYSEnabled() {
		if s.IYSBaseURL == "" || s.IYSUsername == "" || s.IYSPassword.IsZero() || s.IYSCode == "" || s.IYSBrandCode == "" {
			errs = append(errs, errors.New("CONSENT_IYS_BASE_URL, CONSENT_IYS_USERNAME, CONSENT_IYS_PASSWORD, "+
				"CONSENT_IYS_CODE and CONSENT_IYS_BRAND_CODE must be set together"))
		}
		u, err := url.Parse(s.IYSBaseURL)
		switch {
		case s.IYSBaseURL == "":
		case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
			errs = append(errs, errors.New("CONSENT_IYS_BASE_URL must be an http(s) URL"))
		case c.ProdLike() && u.Scheme != "https":
			errs = append(errs, fmt.Errorf("CONSENT_IYS_BASE_URL must use https in %s", c.Env))
		}
	}
	if s.IYSRequired && !s.IYSEnabled() {
		errs = append(errs, errors.New("CONSENT_IYS_REQUIRED=true needs the İYS API settings (CONSENT_IYS_BASE_URL, ...)"))
	}
	if s.IYSRecipientType != "BIREYSEL" && s.IYSRecipientType != "TACIR" {
		errs = append(errs, errors.New("CONSENT_IYS_RECIPIENT_TYPE must be BIREYSEL or TACIR"))
	}
	if s.IYSTimeout <= 0 || s.IYSTimeout > time.Minute {
		errs = append(errs, errors.New("CONSENT_IYS_TIMEOUT must be positive and at most 1m"))
	}
	s.UnsubscribeBaseURL = strings.TrimRight(strings.TrimSpace(s.UnsubscribeBaseURL), "/")
	if s.UnsubscribeBaseURL != "" {
		u, err := url.Parse(s.UnsubscribeBaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
			u.Fragment != "" || strings.Contains(s.UnsubscribeBaseURL, "#") || len(s.UnsubscribeBaseURL) > maxUnsubscribeBaseURL {
			errs = append(errs, fmt.Errorf("CONSENT_UNSUBSCRIBE_BASE_URL must be an absolute https URL without user info, "+
				"query or fragment, at most %d characters (got %q)", maxUnsubscribeBaseURL, s.UnsubscribeBaseURL))
		}
	}
	if s.UnsubscribeTTL < minUnsubscribeTTL || s.UnsubscribeTTL > maxUnsubscribeTTL {
		errs = append(errs, errors.New("CONSENT_UNSUBSCRIBE_TTL must be between 24h and 9600h"))
	}
	return errors.Join(errs...)
}
