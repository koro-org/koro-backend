package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Consent configures the consent module (internal/consent). Owner: BE-012,
// BE-055 (İYS client); only tickets of the consent module edit this file and
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
}

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
	return errors.Join(errs...)
}
