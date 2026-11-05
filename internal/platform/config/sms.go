package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SMS configures the SMS port (internal/platform/sms; BE-172): which
// adapter sends identity's one-time codes (phone verification, the SMS
// second step of sign-in, the SMS password reset). Owner: BE-172; only
// tickets of the SMS port edit this file and the "# --- sms ---" block of
// .env.example (BE-001 R7).
type SMS struct {
	// Provider selects the adapter (SMS_PROVIDER; cmd/koro/wire_identity.go
	// wires one per value of SMSProviders):
	//   none    no adapter (default): every SMS feature answers 503
	//           identity.sms_unavailable; e-mail stays the only channel.
	//   mem     in process memory, nothing leaves (development and tests;
	//           refused in staging and prod).
	//   netgsm  Netgsm's OTP SMS API with the SMS_NETGSM_* settings below
	//           (the same account pattern as the Netgsm İYS adapter).
	//   mailpit the development SMS box (BE-178): every SMS becomes a
	//           plain-text e-mail to the local Mailpit at SMS_MAILPIT_ADDR,
	//           so a developer reads the code in Mailpit's inbox; refused in
	//           staging and prod like mem.
	Provider string `env:"SMS_PROVIDER" envDefault:"none"`
	// Timeout bounds one provider request.
	Timeout time.Duration `env:"SMS_TIMEOUT" envDefault:"10s"`

	Netgsm SMSNetgsm
	// MailpitAddr is the SMTP address (host:port, no TLS, no login) of the
	// local Mailpit (SMS_PROVIDER=mailpit; deploy/compose.dev.yaml).
	MailpitAddr string `env:"SMS_MAILPIT_ADDR" envDefault:"localhost:1025"`
}

// SMSNetgsm configures the Netgsm SMS adapter (SMS_PROVIDER=netgsm):
// username, password and header go together.
type SMSNetgsm struct {
	// BaseURL of Netgsm's API.
	BaseURL string `env:"SMS_NETGSM_BASE_URL" envDefault:"https://api.netgsm.com.tr"`
	// Username is the Netgsm subscriber number (usercode); Password the
	// password of its "API user" sub-user (the İYS adapter may use the same
	// account: CONSENT_IYS_NETGSM_USERNAME / _PASSWORD).
	Username string `env:"SMS_NETGSM_USERNAME"`
	Password Secret `env:"SMS_NETGSM_PASSWORD"`
	// Header is the sender name approved on the account (msgheader).
	Header string `env:"SMS_NETGSM_HEADER"`
}

// Values of SMS_PROVIDER.
const (
	SMSProviderNone   = "none"
	SMSProviderMem    = ProviderMem
	SMSProviderNetgsm = "netgsm"
	// SMSProviderMailpit is the development SMS box (BE-178).
	SMSProviderMailpit = ProviderMailpit
)

// SMSProviders returns the accepted values of SMS_PROVIDER (none has no
// adapter; cmd/koro wires every one).
func SMSProviders() []string {
	return []string{SMSProviderNone, SMSProviderMem, SMSProviderNetgsm, SMSProviderMailpit}
}

// Enabled reports whether an adapter sends SMS.
func (s SMS) Enabled() bool { return s.Provider != SMSProviderNone }

// enabled reports whether any account setting of Netgsm is present.
func (n SMSNetgsm) enabled() bool { return n.Username != "" || !n.Password.IsZero() || n.Header != "" }

var reSMSHeader = regexp.MustCompile(`^[A-Za-z0-9 .\-]{2,11}$`)

func (s *SMS) validate(c Common) error {
	var errs []error
	if err := checkProvider(c, "SMS_PROVIDER", &s.Provider, SMSProviders()); err != nil {
		errs = append(errs, err)
	}
	if s.Timeout <= 0 || s.Timeout > time.Minute {
		errs = append(errs, errors.New("SMS_TIMEOUT must be positive and at most 1m"))
	}
	n := &s.Netgsm
	active := s.Provider == SMSProviderNetgsm
	switch {
	case active && (n.Username == "" || n.Password.IsZero() || n.Header == ""):
		errs = append(errs, errors.New("SMS_PROVIDER=netgsm needs SMS_NETGSM_USERNAME, SMS_NETGSM_PASSWORD and SMS_NETGSM_HEADER"))
	case !active && n.enabled():
		errs = append(errs, errors.New("the SMS_NETGSM_* settings need SMS_PROVIDER=netgsm"))
	}
	if n.Header != "" && !reSMSHeader.MatchString(n.Header) {
		errs = append(errs, errors.New("SMS_NETGSM_HEADER must be the approved sender name (2-11 letters, digits, spaces, dots or dashes)"))
	}
	if err := checkAPIBaseURL(c, "SMS_NETGSM_BASE_URL", &n.BaseURL, active); err != nil {
		errs = append(errs, err)
	}
	s.MailpitAddr = strings.TrimSpace(s.MailpitAddr)
	if s.Provider == SMSProviderMailpit {
		host, port, err := net.SplitHostPort(s.MailpitAddr)
		if p, perr := strconv.Atoi(port); err != nil || host == "" || perr != nil || p < 1 || p > 65535 {
			errs = append(errs, errors.New("SMS_MAILPIT_ADDR must be host:port (Mailpit's SMTP, e.g. localhost:1025)"))
		}
	}
	return errors.Join(errs...)
}

// checkAPIBaseURL normalises *raw (trimmed, no trailing slash) and checks
// that it is an http(s) URL without credentials or query; an active
// adapter must use https in staging and prod. Shared by the Netgsm
// adapters' settings (SMS, consent's İYS; BE-172).
func checkAPIBaseURL(c Common, key string, raw *string, active bool) error {
	*raw = strings.TrimRight(strings.TrimSpace(*raw), "/")
	u, err := url.Parse(*raw)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "":
		return fmt.Errorf("%s must be an http(s) URL without credentials or query", key)
	case active && c.ProdLike() && u.Scheme != "https":
		return fmt.Errorf("%s must use https in %s", key, c.Env)
	}
	return nil
}
