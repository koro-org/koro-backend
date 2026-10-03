package notify

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/inbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// E-mail providers of receiveEmailWebhook (04 §2.0; 00 T-62: both accepted
// until E-31 picks one).
const (
	WebhookSES    = "ses"    // Amazon SES notifications delivered by SNS (signed by SNS)
	WebhookResend = "resend" // Resend events delivered by svix (HMAC-SHA256)
)

// WebhookProviders lists the accepted {provider} path values.
func WebhookProviders() []string { return []string{WebhookSES, WebhookResend} }

// providerHeader carries the provider into the inbox row (the inbox provider
// is "email" for both).
const providerHeader = "x-koro-email-provider"

// Webhook verification defaults.
const (
	// DefaultSvixTolerance is the accepted clock skew of svix-timestamp
	// (svix: 5 minutes; replay protection next to the svix-id dedupe).
	DefaultSvixTolerance = 5 * time.Minute
	// snsMaxAge bounds the SNS Timestamp: SNS retries for at most about an
	// hour; a day leaves room for a backlog and refuses ancient replays.
	snsMaxAge     = 24 * time.Hour
	snsMaxFuture  = 5 * time.Minute
	maxEventIDLen = 150
)

// WebhookConfig holds the provider verification settings. A provider without
// its settings rejects every delivery with 401 (safe default).
type WebhookConfig struct {
	// ResendSecret is the decoded svix signing secret (the base64 part of
	// "whsec_..."). Empty → resend disabled.
	ResendSecret []byte
	// SESTopicARNs are the SNS topics allowed to post (anyone can create an
	// SNS topic, so a valid SNS signature alone proves nothing). Empty → ses
	// disabled.
	SESTopicARNs []string
	// SNSCerts fetches the SNS signing certificates. nil → ses disabled.
	SNSCerts SNSCertFetcher
	// SvixTolerance overrides DefaultSvixTolerance.
	SvixTolerance time.Duration
}

var (
	errSignature   = errors.New("signature does not verify")
	errUnsupported = errors.New("provider not configured")
	reEventID      = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
	reSNSCertHost  = regexp.MustCompile(`^sns\.[a-z0-9-]+\.amazonaws\.com$`)
)

// webhookResource is the authz view of a provider delivery.
var webhookResource = authz.Resource{Type: authz.TypeEmailWebhook}

// ReceiveEmailWebhook implements Service. headers must have lower-case keys.
// The signature is checked before anything is stored; a delivery that does
// not verify is answered 401 and never reaches the inbox. A verified
// delivery is stored under the provider's event ID (a redelivery is a
// no-op) and processed by the worker (webhook_inbox.process).
func (s *service) ReceiveEmailWebhook(ctx context.Context, provider string, headers map[string]string, body []byte) error {
	if err := authz.Can(ctx, authz.ActorOrAnonymous(ctx), authz.NotifyProviderWebhookReceive, webhookResource); err != nil {
		return err
	}
	if s.d.Inbox == nil {
		return problem.ErrUnavailable
	}
	key, err := s.verifyWebhook(ctx, provider, headers, body)
	if err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "email webhook rejected",
			slog.String("provider", provider), slog.String("reason", err.Error()))
		return problem.ErrUnauthenticated
	}
	isNew, err := s.d.Inbox.Receive(ctx, inbox.ProviderEmail, key, true, map[string]string{providerHeader: provider}, body)
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "email webhook received",
		slog.String("provider", provider), slog.String("event_key", key), slog.Bool("new", isNew))
	return nil
}

// verifyWebhook checks the provider signature and returns the inbox event key.
func (s *service) verifyWebhook(ctx context.Context, provider string, h map[string]string, body []byte) (string, error) {
	switch provider {
	case WebhookResend:
		return verifySvix(s.d.Webhooks, s.d.Clock.Now(), h, body)
	case WebhookSES:
		return verifySNS(ctx, s.d.Webhooks, s.d.Clock.Now(), h, body)
	}
	return "", errUnsupported
}

// ---------------------------------------------------------------- svix (Resend)

// verifySvix is the svix scheme: HMAC-SHA256 over "<svix-id>.<svix-timestamp>.<body>"
// with the decoded secret, base64; svix-signature lists "v1,<sig>" entries
// separated by spaces (key rotation).
func verifySvix(cfg WebhookConfig, now time.Time, h map[string]string, body []byte) (string, error) {
	if len(cfg.ResendSecret) == 0 {
		return "", errUnsupported
	}
	id, ts, sigs := h["svix-id"], h["svix-timestamp"], h["svix-signature"]
	if id == "" || len(id) > maxEventIDLen || !reEventID.MatchString(id) || ts == "" || sigs == "" {
		return "", errors.New("missing or malformed svix headers")
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", errors.New("malformed svix-timestamp")
	}
	tol := cfg.SvixTolerance
	if tol <= 0 {
		tol = DefaultSvixTolerance
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tol || d < -tol {
		return "", errors.New("svix-timestamp outside the tolerance")
	}
	mac := hmac.New(sha256.New, cfg.ResendSecret)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, entry := range strings.Fields(sigs) {
		version, sig, ok := strings.Cut(entry, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			return WebhookResend + ":" + id, nil
		}
	}
	return "", errSignature
}

// SvixSignature signs a delivery like svix does (tests, fixtures).
func SvixSignature(secret []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------- SNS (SES)

// snsEnvelope is an SNS HTTP(S) delivery.
type snsEnvelope struct {
	Type             string `json:"Type"`
	MessageID        string `json:"MessageId"`
	Token            string `json:"Token"`
	TopicArn         string `json:"TopicArn"`
	Subject          string `json:"Subject"`
	Message          string `json:"Message"`
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
	SubscribeURL     string `json:"SubscribeURL"`
}

// snsStringToSign returns the canonical string SNS signs (AWS "Verifying the
// signatures of Amazon SNS messages").
func snsStringToSign(e snsEnvelope) string {
	var b strings.Builder
	add := func(k, v string) { b.WriteString(k + "\n" + v + "\n") }
	add("Message", e.Message)
	add("MessageId", e.MessageID)
	if e.Type == "Notification" {
		if e.Subject != "" {
			add("Subject", e.Subject)
		}
		add("Timestamp", e.Timestamp)
		add("TopicArn", e.TopicArn)
		add("Type", e.Type)
		return b.String()
	}
	add("SubscribeURL", e.SubscribeURL)
	add("Timestamp", e.Timestamp)
	add("Token", e.Token)
	add("TopicArn", e.TopicArn)
	add("Type", e.Type)
	return b.String()
}

// verifySNS checks an SNS delivery: allowed topic, SignatureVersion 2
// (SHA256withRSA; version 1 is SHA1 and refused), a signing certificate
// from an sns.<region>.amazonaws.com HTTPS URL and a fresh timestamp.
func verifySNS(ctx context.Context, cfg WebhookConfig, now time.Time, h map[string]string, body []byte) (string, error) {
	if len(cfg.SESTopicARNs) == 0 || cfg.SNSCerts == nil {
		return "", errUnsupported
	}
	var e snsEnvelope
	if err := json.Unmarshal(body, &e); err != nil {
		return "", errors.New("body is not an SNS message")
	}
	switch {
	case e.Type != "Notification" && e.Type != "SubscriptionConfirmation" && e.Type != "UnsubscribeConfirmation":
		return "", errors.New("unknown SNS message type")
	case h["x-amz-sns-message-type"] != "" && h["x-amz-sns-message-type"] != e.Type:
		return "", errors.New("SNS message type header does not match the body")
	case e.MessageID == "" || len(e.MessageID) > maxEventIDLen || !reEventID.MatchString(e.MessageID):
		return "", errors.New("malformed SNS MessageId")
	case !slices.Contains(cfg.SESTopicARNs, e.TopicArn):
		return "", errors.New("SNS topic is not allowed")
	case e.SignatureVersion != "2":
		return "", errors.New("SNS SignatureVersion must be 2 (SHA256)")
	}
	at, err := time.Parse(time.RFC3339, e.Timestamp)
	if err != nil {
		return "", errors.New("malformed SNS Timestamp")
	}
	if now.Sub(at) > snsMaxAge || at.Sub(now) > snsMaxFuture {
		return "", errors.New("SNS Timestamp outside the accepted window")
	}
	if err := ValidSNSCertURL(e.SigningCertURL); err != nil {
		return "", err
	}
	sig, err := base64.StdEncoding.DecodeString(e.Signature)
	if err != nil {
		return "", errSignature
	}
	cert, err := cfg.SNSCerts.Fetch(ctx, e.SigningCertURL)
	if err != nil {
		return "", fmt.Errorf("SNS signing certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("SNS signing certificate has no RSA key")
	}
	sum := sha256.Sum256([]byte(snsStringToSign(e)))
	if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
		return "", errSignature
	}
	return WebhookSES + ":" + e.MessageID, nil
}

// ValidSNSCertURL accepts only https://sns.<region>.amazonaws.com/<...>.pem
// (no credentials, port, query or fragment): the URL comes from the
// unauthenticated body, so anything else would be SSRF (05 §14.4).
func ValidSNSCertURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" ||
		!reSNSCertHost.MatchString(u.Hostname()) || !strings.HasSuffix(u.Path, ".pem") || strings.Contains(u.Path, "..") {
		return errors.New("SNS SigningCertURL is not an sns.<region>.amazonaws.com certificate")
	}
	return nil
}

// SNSCertFetcher returns the certificate behind an SNS SigningCertURL.
type SNSCertFetcher interface {
	Fetch(ctx context.Context, certURL string) (*x509.Certificate, error)
}

// certCacheTTL keeps fetched certificates (SNS rotates rarely).
const (
	certCacheTTL  = 24 * time.Hour
	maxCertBytes  = 64 << 10
	certFetchTime = 5 * time.Second
)

// NewSNSCertFetcher returns the HTTPS fetcher of SNS signing certificates:
// only ValidSNSCertURL addresses, no redirects, 5 s timeout, at most 64 KB,
// cached for a day. The TLS connection to the amazonaws.com host is what
// makes the certificate trustworthy.
func NewSNSCertFetcher(clk clock.Clock) SNSCertFetcher {
	return &httpCertFetcher{
		clk: clk,
		client: &http.Client{
			Timeout:       certFetchTime,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		cache: map[string]cachedCert{},
	}
}

type cachedCert struct {
	cert *x509.Certificate
	at   time.Time
}

type httpCertFetcher struct {
	clk    clock.Clock
	client *http.Client
	mu     sync.Mutex
	cache  map[string]cachedCert
}

func (f *httpCertFetcher) Fetch(ctx context.Context, certURL string) (*x509.Certificate, error) {
	if err := ValidSNSCertURL(certURL); err != nil {
		return nil, err
	}
	now := f.clk.Now()
	f.mu.Lock()
	c, ok := f.cache[certURL]
	f.mu.Unlock()
	if ok && now.Sub(c.at) < certCacheTTL {
		return c.cert, nil
	}
	// The URL is restricted to sns.<region>.amazonaws.com by ValidSNSCertURL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, certURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("certificate fetch: status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCertBytes))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("certificate fetch: no PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.cache[certURL] = cachedCert{cert: cert, at: now}
	f.mu.Unlock()
	return cert, nil
}

// ---------------------------------------------------------------- processing

// webhookOutcome is what one provider event means for notify.
type webhookOutcome struct {
	status     Status            // bounced | complained | delivered ("" = nothing to record)
	suppress   SuppressionReason // "" = no suppression
	recipients []string
	messageIDs []string
}

// webhookProcessor is the platform/inbox processor of provider "email": it
// runs in the transaction that marks the inbox row processed, so a delivery
// stored twice (or a task run twice) applies its outcome once.
func (s *service) webhookProcessor() inbox.Processor {
	return inbox.Processor{Provider: inbox.ProviderEmail, Handle: s.processWebhook}
}

func (s *service) processWebhook(ctx context.Context, e inbox.Event) error {
	provider := e.Headers[providerHeader]
	var (
		out webhookOutcome
		err error
	)
	switch provider {
	case WebhookSES:
		out, err = s.parseSNS(ctx, e.Payload)
	case WebhookResend:
		out, err = parseResend(e.Payload)
	default:
		err = fmt.Errorf("unknown e-mail provider %q", provider)
	}
	if err != nil {
		return queue.Permanent(fmt.Errorf("notify: webhook %s: %w", e.ID, err))
	}
	now := s.d.Clock.Now()
	suppressed, updated := 0, 0
	if out.suppress != "" {
		for _, r := range out.recipients {
			added, err := s.d.Store.Suppress(ctx, Suppression{
				EmailIndex: s.indexOf(r), Reason: out.suppress, Source: SourceProviderWebhook, CreatedAt: now,
			})
			if err != nil {
				return err
			}
			if added {
				suppressed++
			}
		}
	}
	if out.status != "" {
		for _, id := range out.messageIDs {
			_, changed, err := s.d.Store.MarkProviderOutcome(ctx, id, out.status, now)
			if err != nil {
				return err
			}
			if changed {
				updated++
			}
		}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "email webhook processed",
		slog.String("provider", provider), slog.String("inbox_id", e.ID.String()), slog.String("status", string(out.status)),
		slog.Int("recipients", len(out.recipients)), slog.Int("suppressed", suppressed), slog.Int("deliveries_updated", updated))
	return nil
}

// sesNotification is the SES notification (or event publishing) JSON inside
// an SNS Notification.
type sesNotification struct {
	NotificationType string `json:"notificationType"`
	EventType        string `json:"eventType"`
	Mail             struct {
		MessageID     string `json:"messageId"`
		CommonHeaders struct {
			MessageID string `json:"messageId"`
		} `json:"commonHeaders"`
	} `json:"mail"`
	Bounce struct {
		BounceType        string `json:"bounceType"`
		BouncedRecipients []struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"bouncedRecipients"`
	} `json:"bounce"`
	Complaint struct {
		ComplainedRecipients []struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"complainedRecipients"`
	} `json:"complaint"`
	Delivery struct {
		Recipients []string `json:"recipients"`
	} `json:"delivery"`
}

func (s *service) parseSNS(ctx context.Context, payload []byte) (webhookOutcome, error) {
	var e snsEnvelope
	if err := json.Unmarshal(payload, &e); err != nil {
		return webhookOutcome{}, err
	}
	if e.Type != "Notification" {
		// The subscription is confirmed by OPS in the AWS console: the
		// processor writes only to the database and never follows a URL
		// from a webhook body.
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "SNS subscription message: confirm the subscription in the AWS console",
			slog.String("type", e.Type), slog.String("topic_arn", e.TopicArn))
		return webhookOutcome{}, nil
	}
	var n sesNotification
	if err := json.Unmarshal([]byte(e.Message), &n); err != nil {
		return webhookOutcome{}, err
	}
	out := webhookOutcome{messageIDs: messageIDs(n.Mail.CommonHeaders.MessageID, n.Mail.MessageID)}
	kind := n.NotificationType
	if kind == "" {
		kind = n.EventType
	}
	switch kind {
	case "Bounce":
		if n.Bounce.BounceType != "Permanent" {
			return webhookOutcome{}, nil // transient or undetermined: the provider retries
		}
		out.status, out.suppress = StatusBounced, SuppressHardBounce
		for _, r := range n.Bounce.BouncedRecipients {
			out.recipients = append(out.recipients, r.EmailAddress)
		}
	case "Complaint":
		out.status, out.suppress = StatusComplained, SuppressComplaint
		for _, r := range n.Complaint.ComplainedRecipients {
			out.recipients = append(out.recipients, r.EmailAddress)
		}
	case "Delivery":
		out.status = StatusDelivered
	default:
		return webhookOutcome{}, nil
	}
	out.recipients = addresses(out.recipients)
	return out, nil
}

// resendEvent is a Resend webhook event.
type resendEvent struct {
	Type string `json:"type"`
	Data struct {
		EmailID string   `json:"email_id"`
		To      []string `json:"to"`
		Bounce  struct {
			Type string `json:"type"`
		} `json:"bounce"`
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	} `json:"data"`
}

func parseResend(payload []byte) (webhookOutcome, error) {
	var e resendEvent
	if err := json.Unmarshal(payload, &e); err != nil {
		return webhookOutcome{}, err
	}
	ids := []string{e.Data.EmailID}
	for _, h := range e.Data.Headers {
		if strings.EqualFold(h.Name, "Message-ID") {
			ids = append(ids, h.Value)
		}
	}
	out := webhookOutcome{messageIDs: messageIDs(ids...)}
	switch e.Type {
	case "email.bounced":
		if strings.EqualFold(e.Data.Bounce.Type, "Transient") {
			return webhookOutcome{}, nil
		}
		out.status, out.suppress, out.recipients = StatusBounced, SuppressHardBounce, addresses(e.Data.To)
	case "email.complained":
		out.status, out.suppress, out.recipients = StatusComplained, SuppressComplaint, addresses(e.Data.To)
	case "email.delivered":
		out.status = StatusDelivered
	default:
		return webhookOutcome{}, nil
	}
	return out, nil
}

// messageIDs strips angle brackets and drops empty and repeated IDs. Our
// SMTP Message-ID is "<delivery id>@<domain>" and is stored without
// brackets as the provider message ID.
func messageIDs(ids ...string) []string {
	var out []string
	for _, id := range ids {
		id = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(id), "<"), ">")
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// addresses extracts bare addresses ("Ayşe <a@b>" → "a@b"); unparsable
// entries are dropped.
func addresses(in []string) []string {
	var out []string
	for _, a := range in {
		if p, err := mail.ParseAddress(strings.TrimSpace(a)); err == nil && p.Address != "" {
			out = append(out, p.Address)
		}
	}
	return out
}
