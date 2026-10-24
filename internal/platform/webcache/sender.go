package webcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/queue"
)

// requestTimeout bounds one POST (the subscription's attempt timeout is
// longer).
const requestTimeout = 10 * time.Second

// SenderConfig configures a Sender.
type SenderConfig struct {
	BaseURL string // koro-web on the internal network, e.g. http://koro-web:3000
	Secret  string // KORO_REVALIDATE_SECRET
	Clock   clock.Clock
	// Client is the HTTP client; nil → one with a 10 s timeout. Redirects
	// are never followed (a redirect is a misconfiguration).
	Client *http.Client
}

// Sender signs and POSTs cache tags to koro-web.
type Sender struct {
	url    string
	secret []byte
	clock  clock.Clock
	client *http.Client
}

// Errors.
var (
	ErrNotConfigured = errors.New("webcache: base URL, secret and clock are required")
	// ErrRejected: koro-web answered with a status retrying cannot fix.
	ErrRejected = errors.New("webcache: revalidation rejected")
)

// NewSender returns a sender, or ErrNotConfigured.
func NewSender(cfg SenderConfig) (*Sender, error) {
	if cfg.BaseURL == "" || cfg.Secret == "" || cfg.Clock == nil {
		return nil, ErrNotConfigured
	}
	c := http.Client{Timeout: requestTimeout}
	if cfg.Client != nil {
		c = *cfg.Client // a copy: the redirect policy is ours
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Sender{
		url: strings.TrimSuffix(cfg.BaseURL, "/") + Path, secret: []byte(cfg.Secret), clock: cfg.Clock, client: &c,
	}, nil
}

// body is the request body of the contract.
type body struct {
	Tags []string `json:"tags"`
}

// Send revalidates tags: duplicates are dropped (order kept) and more than
// MaxTags go in several requests. Every tag must be valid (the caller
// filters). The error is queue.Permanent when retrying cannot help.
func (s *Sender) Send(ctx context.Context, tags []string) error {
	tags = dedupe(tags)
	for _, t := range tags {
		if !ValidTag(t) {
			return queue.Permanent(fmt.Errorf("webcache: invalid tag %q", t))
		}
	}
	for chunk := range slices.Chunk(tags, MaxTags) {
		if err := s.post(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sender) post(ctx context.Context, tags []string) error {
	raw, err := json.Marshal(body{Tags: tags})
	if err != nil {
		return queue.Permanent(fmt.Errorf("webcache: encode body: %w", err))
	}
	if len(raw) > MaxBodyBytes {
		return queue.Permanent(fmt.Errorf("webcache: body of %d bytes exceeds %d", len(raw), MaxBodyBytes))
	}
	ts := strconv.FormatInt(s.clock.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(raw))
	if err != nil {
		return queue.Permanent(fmt.Errorf("webcache: build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(SignatureHeader, Sign(s.secret, ts, raw))
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("webcache: post: %w", err) // network: retry
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) // keep-alive; the body is never logged
	return classify(resp.StatusCode)
}

// classify maps koro-web's answer to nil, a retryable or a permanent error.
func classify(status int) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status >= 500, status == http.StatusUnauthorized, status == http.StatusNotFound,
		status == http.StatusRequestTimeout, status == http.StatusTooEarly, status == http.StatusTooManyRequests:
		return fmt.Errorf("webcache: koro-web answered HTTP %d", status)
	}
	return queue.Permanent(fmt.Errorf("%w: HTTP %d", ErrRejected, status))
}

// dedupe drops repeated and empty tags, keeping the first occurrence.
func dedupe(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, t := range tags {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
