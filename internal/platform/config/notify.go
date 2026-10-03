package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
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
// sender identity of e-mails and the provider webhook secrets. Owner: BE-005,
// BE-015. Only tickets of the notify
// module edit this file and the "# --- notify ---" block of .env.example
// (BE-001 R7).
//
// The defaults point at the local Mailpit (deploy/compose.dev.yaml). They are
// rejected in staging and prod (see validate), so a forgotten variable fails
// the process at start instead of failing every send later (05 §12.2).
type Notify struct {
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

	if c.ProdLike() {
		if isLoopbackHost(n.SMTPHost) {
			errs = append(errs, fmt.Errorf("SMTP_HOST must be a real relay in %s (got a loopback address)", c.Env))
		}
		if n.SMTPTLS == SMTPTLSNone {
			errs = append(errs, fmt.Errorf("SMTP_TLS=none is allowed only with APP_ENV=dev (got %s)", c.Env))
		}
	}
	return errors.Join(errs...)
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
