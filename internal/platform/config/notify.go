package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// SMTP transport security modes (SMTP_TLS).
const (
	SMTPTLSNone     = "none"     // plain text; local development (Mailpit) only
	SMTPTLSStartTLS = "starttls" // upgrade is mandatory; the send fails if the server cannot
	SMTPTLSImplicit = "tls"      // implicit TLS (usually port 465)
)

// Notify configures the notify module (internal/notify): the SMTP relay, the
// sender identity of e-mails, the provider webhook secrets and the push
// providers. Owner: BE-005, BE-015, BE-055 (push), BE-113 (adapter selection). Only tickets of the notify
// module edit this file and the "# --- notify ---" block of .env.example
// (BE-001 R7).
//
// The defaults point at the local Mailpit (deploy/compose.dev.yaml). They are
// rejected in staging and prod (see validate), so a forgotten variable fails
// the process at start instead of failing every send later (05 §12.2).
type Notify struct {
	// EmailProvider selects the notify.EmailTransport adapter
	// (NOTIFY_EMAIL_PROVIDER, BE-113): "smtp" (the SMTP_* relay) or "mem"
	// (kept in process memory, nothing leaves; development only, refused in
	// staging and prod). SMTP_FROM and SMTP_FROM_NAME are the sender of
	// every adapter; the other SMTP_* settings are checked only for smtp.
	EmailProvider string `env:"NOTIFY_EMAIL_PROVIDER" envDefault:"smtp"`

	SMTPHost     string        `env:"SMTP_HOST" envDefault:"localhost"`
	SMTPPort     int           `env:"SMTP_PORT" envDefault:"1025"`
	SMTPTLS      string        `env:"SMTP_TLS" envDefault:"none"` // none | starttls | tls
	SMTPUsername string        `env:"SMTP_USERNAME"`
	SMTPPassword Secret        `env:"SMTP_PASSWORD"`
	SMTPFrom     string        `env:"SMTP_FROM" envDefault:"noreply@koro.test"`
	SMTPFromName string        `env:"SMTP_FROM_NAME" envDefault:"Koro"`
	SMTPTimeout  time.Duration `env:"SMTP_TIMEOUT" envDefault:"15s"` // dial + one send

	// EmailWebhookResendSecret is the svix signing secret of the Resend
	// webhook ("whsec_<base64>"); empty → /webhooks/email/resend answers 401
	// (BE-015).
	EmailWebhookResendSecret Secret `env:"EMAIL_WEBHOOK_RESEND_SECRET"`
	// EmailWebhookSESTopicARNs lists the SNS topics (comma separated) allowed
	// to post SES notifications; empty → /webhooks/email/ses answers 401.
	EmailWebhookSESTopicARNs string `env:"EMAIL_WEBHOOK_SES_TOPIC_ARNS"`

	// Push (BE-055). PushProvider selects the notify.PushSender adapter
	// (NOTIFY_PUSH_PROVIDER, BE-113): "auto" (default: every provider whose
	// settings are complete; neither → the no-op adapter, the behaviour
	// before BE-113), a comma-separated list of "fcm" and "apns" (each
	// listed provider must be configured; an unlisted one stays off), or
	// "noop" (development only, refused in staging and prod). PushRequired
	// refuses to start without at least one provider.
	PushProvider string `env:"NOTIFY_PUSH_PROVIDER" envDefault:"auto"`
	PushRequired bool   `env:"PUSH_REQUIRED" envDefault:"false"`
	// PushFCMProjectID and PushFCMCredentials (service account JSON,
	// PUSH_FCM_CREDENTIALS_FILE) enable FCM HTTP v1 (Android, web).
	PushFCMProjectID   string `env:"PUSH_FCM_PROJECT_ID"`
	PushFCMCredentials Secret `env:"PUSH_FCM_CREDENTIALS"`
	// PushAPNsKey (.p8, PUSH_APNS_KEY_FILE), key ID, team ID and topic
	// (bundle ID) enable APNs (iOS). PushAPNsSandbox selects the development
	// endpoint (not allowed in prod).
	PushAPNsKey     Secret `env:"PUSH_APNS_KEY"`
	PushAPNsKeyID   string `env:"PUSH_APNS_KEY_ID"`
	PushAPNsTeamID  string `env:"PUSH_APNS_TEAM_ID"`
	PushAPNsTopic   string `env:"PUSH_APNS_TOPIC"`
	PushAPNsSandbox bool   `env:"PUSH_APNS_SANDBOX" envDefault:"false"`
	// PushTimeout bounds one provider request.
	PushTimeout time.Duration `env:"PUSH_TIMEOUT" envDefault:"10s"`
}

// E-mail adapters (NOTIFY_EMAIL_PROVIDER).
const (
	EmailProviderSMTP = "smtp"
	EmailProviderMem  = ProviderMem
)

// EmailProviders lists the accepted NOTIFY_EMAIL_PROVIDER values; cmd/koro
// has one constructor per name (wire_notify.go).
func EmailProviders() []string { return []string{EmailProviderSMTP, EmailProviderMem} }

// Push adapters (NOTIFY_PUSH_PROVIDER). PushProviderAuto and PushProviderNoop
// stand alone; FCM and APNs may be listed together ("fcm,apns").
const (
	PushProviderAuto = "auto"
	PushProviderNoop = ProviderNoop
	PushProviderFCM  = "fcm"
	PushProviderAPNs = "apns"
)

// PushProviders lists the push providers a NOTIFY_PUSH_PROVIDER list may
// name; cmd/koro has one constructor per name (wire_notify.go).
func PushProviders() []string { return []string{PushProviderFCM, PushProviderAPNs} }

// ActivePushProviders resolves NOTIFY_PUSH_PROVIDER (validated) to the push
// providers to build, in PushProviders order; empty means the no-op adapter.
func (n Notify) ActivePushProviders() []string {
	var out []string
	for _, p := range PushProviders() {
		switch {
		case n.PushProvider == PushProviderAuto:
			if (p == PushProviderFCM && n.FCMEnabled()) || (p == PushProviderAPNs && n.APNsEnabled()) {
				out = append(out, p)
			}
		case slices.Contains(splitList(n.PushProvider), p):
			out = append(out, p)
		}
	}
	return out
}

// splitList splits a comma-separated value into trimmed, non-empty items.
func splitList(v string) []string {
	var out []string
	for item := range strings.SplitSeq(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// FCMEnabled reports whether FCM settings are present.
func (n Notify) FCMEnabled() bool { return n.PushFCMProjectID != "" || !n.PushFCMCredentials.IsZero() }

// APNsEnabled reports whether APNs settings are present.
func (n Notify) APNsEnabled() bool {
	return !n.PushAPNsKey.IsZero() || n.PushAPNsKeyID != "" || n.PushAPNsTeamID != "" || n.PushAPNsTopic != ""
}

// svixSecretPrefix starts every svix signing secret.
const svixSecretPrefix = "whsec_"

var reSNSTopicARN = regexp.MustCompile(`^arn:aws[a-z-]*:sns:[a-z0-9-]+:[0-9]{12}:[A-Za-z0-9_-]{1,256}$`)

// ResendWebhookSecret returns the decoded svix secret (nil when unset).
func (n Notify) ResendWebhookSecret() ([]byte, error) {
	raw := strings.TrimSpace(n.EmailWebhookResendSecret.Reveal())
	if raw == "" {
		return nil, nil
	}
	b64, ok := strings.CutPrefix(raw, svixSecretPrefix)
	if !ok {
		return nil, errors.New("EMAIL_WEBHOOK_RESEND_SECRET must start with whsec_")
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(key) < 16 {
		return nil, errors.New("EMAIL_WEBHOOK_RESEND_SECRET must be whsec_ followed by at least 16 base64-encoded bytes")
	}
	return key, nil
}

// SESTopicARNs returns the allowed SNS topic ARNs.
func (n Notify) SESTopicARNs() []string {
	var out []string
	for a := range strings.SplitSeq(n.EmailWebhookSESTopicARNs, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// placeholderTLDs are reserved names that can never be a real sender domain.
var placeholderTLDs = []string{".test", ".example", ".invalid", ".localhost", ".local"}

func (n *Notify) validate(c Common) error {
	var errs []error
	if err := checkProvider(c, "NOTIFY_EMAIL_PROVIDER", &n.EmailProvider, EmailProviders()); err != nil {
		errs = append(errs, err)
	} else if n.EmailProvider == EmailProviderSMTP {
		errs = append(errs, n.validateSMTP(c)...)
	}

	from, err := mail.ParseAddress(n.SMTPFrom)
	switch {
	case err != nil || from.Name != "" || from.Address != n.SMTPFrom || !strings.Contains(from.Address, "@"):
		errs = append(errs, errors.New("SMTP_FROM must be a bare e-mail address (name@domain)"))
	case c.ProdLike() && hasPlaceholderDomain(from.Address):
		errs = append(errs, fmt.Errorf("SMTP_FROM uses a placeholder domain, which is not allowed in %s", c.Env))
	}
	n.SMTPFromName = strings.TrimSpace(n.SMTPFromName)
	if len([]rune(n.SMTPFromName)) > 100 || strings.IndexFunc(n.SMTPFromName, unicode.IsControl) >= 0 {
		errs = append(errs, errors.New("SMTP_FROM_NAME must be at most 100 characters without control characters"))
	}

	if _, err := n.ResendWebhookSecret(); err != nil {
		errs = append(errs, err)
	}
	for _, a := range n.SESTopicARNs() {
		if !reSNSTopicARN.MatchString(a) {
			errs = append(errs, errors.New("EMAIL_WEBHOOK_SES_TOPIC_ARNS must list SNS topic ARNs (arn:aws:sns:<region>:<account>:<topic>)"))
			break
		}
	}

	errs = append(errs, n.validatePush(c)...)
	return errors.Join(errs...)
}

// validateSMTP checks the relay settings of the smtp adapter.
func (n *Notify) validateSMTP(c Common) []error {
	var errs []error
	n.SMTPHost = strings.TrimSpace(n.SMTPHost)
	if n.SMTPHost == "" || strings.ContainsAny(n.SMTPHost, " \t\r\n/@") {
		errs = append(errs, errors.New("SMTP_HOST must be a host name or IP address"))
	}
	if n.SMTPPort < 1 || n.SMTPPort > 65535 {
		errs = append(errs, fmt.Errorf("SMTP_PORT must be between 1 and 65535 (got %d)", n.SMTPPort))
	}
	switch n.SMTPTLS {
	case SMTPTLSNone, SMTPTLSStartTLS, SMTPTLSImplicit:
	default:
		errs = append(errs, fmt.Errorf("SMTP_TLS must be none, starttls or tls (got %q)", n.SMTPTLS))
	}
	if (n.SMTPUsername == "") != n.SMTPPassword.IsZero() {
		errs = append(errs, errors.New("SMTP_USERNAME and SMTP_PASSWORD must be set together"))
	}
	if n.SMTPUsername != "" && n.SMTPTLS == SMTPTLSNone {
		errs = append(errs, errors.New("SMTP_USERNAME needs SMTP_TLS=starttls or tls (the password must not travel in clear text)"))
	}
	// The delivery lease (notify.DefaultLease, 2 minutes) must outlast one
	// send, or a slow attempt could be taken over and sent twice.
	if n.SMTPTimeout <= 0 || n.SMTPTimeout > time.Minute {
		errs = append(errs, errors.New("SMTP_TIMEOUT must be positive and at most 1m"))
	}
	if c.ProdLike() {
		if isLoopbackHost(n.SMTPHost) {
			errs = append(errs, fmt.Errorf("SMTP_HOST must be a real relay in %s (got a loopback address)", c.Env))
		}
		if n.SMTPTLS == SMTPTLSNone {
			errs = append(errs, fmt.Errorf("SMTP_TLS=none is allowed only with APP_ENV=dev (got %s)", c.Env))
		}
	}
	return errs
}

// validatePush checks that each push provider is complete or absent; the
// credentials themselves are parsed when the adapter is built (a broken key
// stops the process there, without repeating it).
func (n *Notify) validatePush(c Common) []error {
	var errs []error
	n.PushFCMProjectID = strings.TrimSpace(n.PushFCMProjectID)
	if n.FCMEnabled() && (n.PushFCMProjectID == "" || n.PushFCMCredentials.IsZero()) {
		errs = append(errs, errors.New("PUSH_FCM_PROJECT_ID and PUSH_FCM_CREDENTIALS must be set together"))
	}
	if n.APNsEnabled() && (n.PushAPNsKey.IsZero() || n.PushAPNsKeyID == "" || n.PushAPNsTeamID == "" || n.PushAPNsTopic == "") {
		errs = append(errs, errors.New("PUSH_APNS_KEY, PUSH_APNS_KEY_ID, PUSH_APNS_TEAM_ID and PUSH_APNS_TOPIC must be set together"))
	}
	errs = append(errs, n.validatePushProvider(c)...)
	if c.Env == EnvProd && n.PushAPNsSandbox {
		errs = append(errs, errors.New("PUSH_APNS_SANDBOX=true is not allowed in prod"))
	}
	if n.PushTimeout <= 0 || n.PushTimeout > time.Minute {
		errs = append(errs, errors.New("PUSH_TIMEOUT must be positive and at most 1m"))
	}
	return errs
}

// validatePushProvider checks NOTIFY_PUSH_PROVIDER: auto or noop alone, or
// a list of configured providers; PUSH_REQUIRED needs at least one.
func (n *Notify) validatePushProvider(c Common) []error {
	n.PushProvider = strings.ToLower(strings.TrimSpace(n.PushProvider))
	items := splitList(n.PushProvider)
	if len(items) == 1 && (items[0] == PushProviderAuto || items[0] == PushProviderNoop) {
		if err := checkProvider(c, "NOTIFY_PUSH_PROVIDER", &n.PushProvider, []string{PushProviderAuto, PushProviderNoop}); err != nil {
			return []error{err}
		}
	} else {
		n.PushProvider = strings.Join(items, ",")
		seen := map[string]bool{}
		for _, p := range items {
			if !slices.Contains(PushProviders(), p) || seen[p] {
				return []error{fmt.Errorf("NOTIFY_PUSH_PROVIDER must be auto, noop or a list of %s without repeats (got %q)",
					orList(PushProviders()), n.PushProvider)}
			}
			seen[p] = true
		}
		if seen[PushProviderFCM] && !n.FCMEnabled() {
			return []error{errors.New("NOTIFY_PUSH_PROVIDER lists fcm: PUSH_FCM_PROJECT_ID and PUSH_FCM_CREDENTIALS are required")}
		}
		if seen[PushProviderAPNs] && !n.APNsEnabled() {
			return []error{errors.New("NOTIFY_PUSH_PROVIDER lists apns: PUSH_APNS_KEY, PUSH_APNS_KEY_ID, PUSH_APNS_TEAM_ID and PUSH_APNS_TOPIC are required")}
		}
	}
	if n.PushRequired && len(n.ActivePushProviders()) == 0 {
		return []error{errors.New("PUSH_REQUIRED=true needs FCM (PUSH_FCM_*) or APNs (PUSH_APNS_*) settings and NOTIFY_PUSH_PROVIDER other than noop")}
	}
	return nil
}

func hasPlaceholderDomain(addr string) bool {
	domain := strings.ToLower(addr[strings.LastIndex(addr, "@")+1:])
	for _, tld := range placeholderTLDs {
		if strings.HasSuffix(domain, tld) {
			return true
		}
	}
	return false
}

func isLoopbackHost(h string) bool {
	h = strings.ToLower(h)
	return h == "localhost" || h == "::1" || h == "0.0.0.0" || strings.HasPrefix(h, "127.")
}
