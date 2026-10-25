package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Consent configures the consent module (internal/consent). Owner: BE-012,
// BE-055 (İYS client), BE-062 (unsubscribe links), BE-103 (İYS status
// hold), BE-105 (İYS adapter choice), BE-110 (Netgsm); only tickets of the consent module
// edit this file and the "# --- consent ---" block of .env.example
// (BE-001 R7).
type Consent struct {
	// IYSRequired refuses to start without a real İYS client (H-14: live
	// operation needs İYS). While false and CONSENT_IYS_PROVIDER is none,
	// grants of commercial purposes are refused (consent.iys_unavailable:
	// a consent İYS does not get within three business days is not valid)
	// and the submissions of older consents end as "skipped".
	IYSRequired bool `env:"CONSENT_IYS_REQUIRED" envDefault:"false"`

	// IYSProvider picks the İYS adapter (internal/platform/iys; BE-105;
	// the accepted values are IYSProviders, cmd/koro/wire_consent.go wires
	// one adapter per value):
	//   none      no adapter (default): commercial consents are not
	//             collected; withdrawals are "skipped".
	//   fake      the in-memory İYS (internal/platform/iys/iysfake):
	//             development only, refused in staging and prod.
	//   official  İYS's own API (internal/platform/iys/official) with the
	//             CONSENT_IYS_* settings below. Live only with direct API
	//             access (İLETİ+): a service provider with fewer than 250 000
	//             addresses reaches İYS through an authorised integrator.
	//   netgsm    the Netgsm integrator (internal/platform/iys/netgsm;
	//             BE-110) with the CONSENT_IYS_NETGSM_* settings below: the
	//             pilot's adapter (İLETİ-5 + Netgsm).
	IYSProvider string `env:"CONSENT_IYS_PROVIDER" envDefault:"none"`

	// İYS's own API (BE-055; CONSENT_IYS_PROVIDER=official): base URL,
	// username, password, İYS code and brand code go together.
	IYSBaseURL       string        `env:"CONSENT_IYS_BASE_URL"`
	IYSUsername      string        `env:"CONSENT_IYS_USERNAME"`
	IYSPassword      Secret        `env:"CONSENT_IYS_PASSWORD"`
	IYSCode          string        `env:"CONSENT_IYS_CODE"`
	IYSBrandCode     string        `env:"CONSENT_IYS_BRAND_CODE"`
	IYSRecipientType string        `env:"CONSENT_IYS_RECIPIENT_TYPE" envDefault:"BIREYSEL"`
	IYSTimeout       time.Duration `env:"CONSENT_IYS_TIMEOUT" envDefault:"10s"`

	// The Netgsm integrator (BE-110; CONSENT_IYS_PROVIDER=netgsm; Netgsm's
	// API documentation, NETIYS). CONSENT_IYS_RECIPIENT_TYPE and
	// CONSENT_IYS_TIMEOUT apply too.
	Netgsm IYSNetgsm

	// IYSSyncMaxAge is the optional send-time hold of BE-103 (T-312): > 0
	// holds commercial messages of an İYS channel (e-mail) while the last
	// complete İYS status reconciliation of that channel
	// (consent.iys_status_sync, hourly) is older than this or never ran, so
	// that a refusal given in İYS itself is in the ledger before a message
	// goes out. 0 (default) sends on the ledger alone. Needs an İYS adapter
	// that reads İYS (the official one does: BE-105). İYS's change list
	// hides its last hour, so a reconciliation covers İYS up to about an
	// hour before it ran: the hold is at least 3h.
	IYSSyncMaxAge time.Duration `env:"CONSENT_IYS_SYNC_MAX_AGE" envDefault:"0s"`

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

// IYSNetgsm configures the Netgsm İYS adapter (BE-110). Username,
// password, İYS code, brand code and webhook token go together.
type IYSNetgsm struct {
	// BaseURL of Netgsm's API (https://api.netgsm.com.tr).
	BaseURL string `env:"CONSENT_IYS_NETGSM_BASE_URL" envDefault:"https://api.netgsm.com.tr"`
	// Username is the Netgsm subscriber number (usercode, 850xxxxxxx);
	// Password the password of its "API user" sub-user.
	Username string `env:"CONSENT_IYS_NETGSM_USERNAME"`
	Password Secret `env:"CONSENT_IYS_NETGSM_PASSWORD"`
	// IYSCode and BrandCode are the service provider's and the brand's İYS
	// numbers (the webhook's iyscode and brandcode are checked against them).
	IYSCode   string `env:"CONSENT_IYS_NETGSM_IYS_CODE"`
	BrandCode string `env:"CONSENT_IYS_NETGSM_BRAND_CODE"`
	// WebhookToken is the secret of the webhook URL entered in Netgsm's
	// portal (https://<api>/api/v1/webhooks/iys/netgsm/<token>): Netgsm's
	// webhook carries no signature, the URL's secret is the delivery's
	// proof. At least 32 characters of [A-Za-z0-9_-].
	WebhookToken Secret `env:"CONSENT_IYS_NETGSM_WEBHOOK_TOKEN"`
	// WebhookAllowedIPs, when set, are the only addresses (IPs or CIDRs,
	// comma-separated) a delivery may come from (Netgsm's sending addresses;
	// 00 T-368). The API resolves the client address through
	// TRUSTED_PROXIES.
	WebhookAllowedIPs []string `env:"CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS" envSeparator:","`
	// RefIDIn places Koro's reference of an upload: header (Netgsm's
	// parameter table) or item (Netgsm's PHP package).
	RefIDIn string `env:"CONSENT_IYS_NETGSM_REFID_IN" envDefault:"header"`
	// RequestsPerMinute bounds the calls of one worker (Netgsm: 10 per
	// minute); StatusQueriesPerHour the single queries of /iys/search
	// (reconciliation, backup check).
	RequestsPerMinute    int `env:"CONSENT_IYS_NETGSM_REQUESTS_PER_MINUTE" envDefault:"8"`
	StatusQueriesPerHour int `env:"CONSENT_IYS_NETGSM_STATUS_QUERIES_PER_HOUR" envDefault:"240"`
	// RecheckPerHour is how many users with a grant in force the backup
	// check reads an hour (consent.iys_netgsm_recheck, every 10 minutes,
	// queries spaced at half the per-minute budget: a refusal whose webhook
	// was lost); 0 turns it off.
	RecheckPerHour int `env:"CONSENT_IYS_NETGSM_RECHECK_PER_HOUR" envDefault:"120"`
	// ResultWait is how long one delivery attempt waits for İYS's answer by
	// webhook; ResendAfter when an upload without an answer is sent again.
	ResultWait  time.Duration `env:"CONSENT_IYS_NETGSM_RESULT_WAIT" envDefault:"15s"`
	ResendAfter time.Duration `env:"CONSENT_IYS_NETGSM_RESEND_AFTER" envDefault:"1h"`
}

// enabled reports whether any account setting of Netgsm is present.
func (n IYSNetgsm) enabled() bool {
	return n.Username != "" || !n.Password.IsZero() || n.IYSCode != "" || n.BrandCode != "" || !n.WebhookToken.IsZero()
}

// AllowedPrefixes parses WebhookAllowedIPs (validated at load).
func (n IYSNetgsm) AllowedPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(n.WebhookAllowedIPs))
	for _, s := range n.WebhookAllowedIPs {
		if p, err := parsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

var (
	reIYSNumber    = regexp.MustCompile(`^[0-9]{1,12}$`)
	reWebhookToken = regexp.MustCompile(`^[A-Za-z0-9_-]{32,200}$`)
)

func (n *IYSNetgsm) validate(c Common, active bool) []error {
	var errs []error
	n.BaseURL = strings.TrimRight(strings.TrimSpace(n.BaseURL), "/")
	if active {
		if n.Username == "" || n.Password.IsZero() || n.IYSCode == "" || n.BrandCode == "" || n.WebhookToken.IsZero() {
			errs = append(errs, errors.New("CONSENT_IYS_PROVIDER=netgsm needs CONSENT_IYS_NETGSM_USERNAME, "+
				"CONSENT_IYS_NETGSM_PASSWORD, CONSENT_IYS_NETGSM_IYS_CODE, CONSENT_IYS_NETGSM_BRAND_CODE and "+
				"CONSENT_IYS_NETGSM_WEBHOOK_TOKEN"))
		}
	} else if n.enabled() {
		errs = append(errs, errors.New("the CONSENT_IYS_NETGSM_* settings need CONSENT_IYS_PROVIDER=netgsm"))
	}
	u, err := url.Parse(n.BaseURL)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "":
		errs = append(errs, errors.New("CONSENT_IYS_NETGSM_BASE_URL must be an http(s) URL without credentials or query"))
	case active && c.ProdLike() && u.Scheme != "https":
		errs = append(errs, fmt.Errorf("CONSENT_IYS_NETGSM_BASE_URL must use https in %s", c.Env))
	}
	if (n.IYSCode != "" && !reIYSNumber.MatchString(n.IYSCode)) || (n.BrandCode != "" && !reIYSNumber.MatchString(n.BrandCode)) {
		errs = append(errs, errors.New("CONSENT_IYS_NETGSM_IYS_CODE and CONSENT_IYS_NETGSM_BRAND_CODE must be İYS numbers"))
	}
	if !n.WebhookToken.IsZero() && !reWebhookToken.MatchString(n.WebhookToken.Reveal()) {
		errs = append(errs, errors.New("CONSENT_IYS_NETGSM_WEBHOOK_TOKEN must be 32–200 characters of [A-Za-z0-9_-]"))
	}
	for _, s := range n.WebhookAllowedIPs {
		if _, err := parsePrefix(s); err != nil {
			errs = append(errs, fmt.Errorf("CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS: %q is not an IP address or CIDR", s))
		}
	}
	if n.RefIDIn != "header" && n.RefIDIn != "item" {
		errs = append(errs, errors.New("CONSENT_IYS_NETGSM_REFID_IN must be header or item"))
	}
	switch {
	case n.RequestsPerMinute < 1 || n.RequestsPerMinute > 10:
		errs = append(errs, errors.New("CONSENT_IYS_NETGSM_REQUESTS_PER_MINUTE must be between 1 and 10 (Netgsm: 10 per minute)"))
	case n.StatusQueriesPerHour < 1 || n.StatusQueriesPerHour > 60*n.RequestsPerMinute:
		errs = append(errs, errors.New("CONSENT_IYS_NETGSM_STATUS_QUERIES_PER_HOUR must be between 1 and 60 × CONSENT_IYS_NETGSM_REQUESTS_PER_MINUTE"))
	case n.RecheckPerHour < 0 || n.RecheckPerHour > n.StatusQueriesPerHour:
		errs = append(errs, errors.New("CONSENT_IYS_NETGSM_RECHECK_PER_HOUR must be between 0 (off) and CONSENT_IYS_NETGSM_STATUS_QUERIES_PER_HOUR"))
	}
	if n.ResultWait < 0 || n.ResultWait > time.Minute {
		errs = append(errs, errors.New("CONSENT_IYS_NETGSM_RESULT_WAIT must be between 0s and 1m"))
	}
	if n.ResendAfter < 10*time.Minute || n.ResendAfter > 24*time.Hour {
		errs = append(errs, errors.New("CONSENT_IYS_NETGSM_RESEND_AFTER must be between 10m and 24h"))
	}
	return errs
}

// Values of CONSENT_IYS_PROVIDER.
const (
	IYSProviderNone     = "none"
	IYSProviderFake     = "fake"
	IYSProviderOfficial = "official"
	IYSProviderNetgsm   = "netgsm"
)

// IYSProviders returns the accepted values of CONSENT_IYS_PROVIDER (none
// has no adapter; cmd/koro wires every other one).
func IYSProviders() []string {
	return []string{IYSProviderNone, IYSProviderFake, IYSProviderOfficial, IYSProviderNetgsm}
}

// Bounds of CONSENT_UNSUBSCRIBE_TTL and CONSENT_UNSUBSCRIBE_BASE_URL, and
// of CONSENT_IYS_SYNC_MAX_AGE when set (the job runs hourly and İYS's
// change list hides its last hour: below three hours one late run would
// already hold every message; above a week the hold protects nothing).
const (
	minIYSSyncMaxAge      = 3 * time.Hour
	maxIYSSyncMaxAge      = 7 * 24 * time.Hour
	minUnsubscribeTTL     = 24 * time.Hour
	maxUnsubscribeTTL     = 400 * 24 * time.Hour
	maxUnsubscribeBaseURL = 300
)

// IYSEnabled reports whether any setting of İYS's own API is present.
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
	switch {
	case !slices.Contains(IYSProviders(), s.IYSProvider):
		errs = append(errs, fmt.Errorf("CONSENT_IYS_PROVIDER must be one of %s (got %q)", strings.Join(IYSProviders(), ", "), s.IYSProvider))
	case s.IYSProvider == IYSProviderFake && c.ProdLike():
		errs = append(errs, fmt.Errorf("CONSENT_IYS_PROVIDER=fake is a development adapter and is not allowed in %s", c.Env))
	case s.IYSProvider == IYSProviderOfficial && !s.IYSEnabled():
		errs = append(errs, errors.New("CONSENT_IYS_PROVIDER=official needs CONSENT_IYS_BASE_URL, CONSENT_IYS_USERNAME, "+
			"CONSENT_IYS_PASSWORD, CONSENT_IYS_CODE and CONSENT_IYS_BRAND_CODE"))
	case s.IYSProvider != IYSProviderOfficial && s.IYSEnabled():
		errs = append(errs, errors.New("CONSENT_IYS_BASE_URL and the other settings of İYS's own API need CONSENT_IYS_PROVIDER=official"))
	case s.IYSRequired && s.IYSProvider == IYSProviderNone:
		errs = append(errs, errors.New("CONSENT_IYS_REQUIRED=true needs an İYS adapter (CONSENT_IYS_PROVIDER)"))
	}
	errs = append(errs, s.Netgsm.validate(c, s.IYSProvider == IYSProviderNetgsm)...)
	if s.IYSRecipientType != "BIREYSEL" && s.IYSRecipientType != "TACIR" {
		errs = append(errs, errors.New("CONSENT_IYS_RECIPIENT_TYPE must be BIREYSEL or TACIR"))
	}
	if s.IYSTimeout <= 0 || s.IYSTimeout > time.Minute {
		errs = append(errs, errors.New("CONSENT_IYS_TIMEOUT must be positive and at most 1m"))
	}
	if s.IYSSyncMaxAge != 0 && (s.IYSSyncMaxAge < minIYSSyncMaxAge || s.IYSSyncMaxAge > maxIYSSyncMaxAge) {
		errs = append(errs, errors.New("CONSENT_IYS_SYNC_MAX_AGE must be 0 (off) or between 3h and 168h"))
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
