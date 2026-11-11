package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"koro/internal/platform/clock"
)

// Fetch errors (run outcomes, fetch.go → pipeline.go).
var (
	// ErrBlocked: the URL or an address it resolves to is not a public
	// http(s) endpoint (SSRF guard).
	ErrBlocked = errors.New("ingest: address not allowed")
	// ErrTooLarge: the body exceeds the size limit.
	ErrTooLarge = errors.New("ingest: response too large")
	// ErrContentType: the answer is not a type the adapter reads.
	ErrContentType = errors.New("ingest: unexpected content type")
)

// FetcherConfig configures HTTPFetcher.
type FetcherConfig struct {
	// UserAgent is "KoroBot/1.0 (+<bot page URL>)" (INGEST_BOT_INFO_URL).
	UserAgent string
	// Timeout bounds one request, redirects and body included.
	Timeout time.Duration
	// MaxBytes bounds a body (robots.txt: 512 KB at most).
	MaxBytes int64
	// RobotsTTL is how long a host's robots.txt is kept.
	RobotsTTL time.Duration
	Clock     clock.Clock
}

// robotsAgent is the product token matched in robots.txt groups.
const robotsAgent = "korobot"

// maxRedirects bounds a redirect chain.
const maxRedirects = 5

// maxRobotsBytes bounds a robots.txt (Google's limit is 500 KiB).
const maxRobotsBytes = 512 << 10

// HTTPFetcher is the production Fetcher: plain GETs with the bot's
// User-Agent, no cookies, no proxy, no credentials; only http(s) URLs on
// ports 80/443 without user info; every connection is checked after DNS
// resolution and refused for a non-public address (loopback, private,
// link-local and cloud metadata, CGNAT, multicast, unspecified,
// IPv4-mapped forms of them), redirects included (each hop is validated
// and dialled through the same guard; DNS rebinding cannot slip through).
// robots.txt is read once per host per RobotsTTL (a 4xx means "allow all",
// a 5xx or a network error "disallow" for this run, RFC 9309 §2.3.1).
type HTTPFetcher struct {
	cfg    FetcherConfig
	client *http.Client

	mu     sync.Mutex
	robots map[string]robotsEntry // by scheme://host
}

type robotsEntry struct {
	rules   robotsRules
	fetched time.Time
	fail    bool
}

var _ Fetcher = (*HTTPFetcher)(nil)

// NewHTTPFetcher returns the guarded fetcher.
func NewHTTPFetcher(cfg FetcherConfig) *HTTPFetcher {
	return newHTTPFetcher(cfg, guardedDialer())
}

// newHTTPFetcher builds the fetcher over a dialer (tests: a dialer that
// lets the loopback test server through).
func newHTTPFetcher(cfg FetcherConfig, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *HTTPFetcher {
	tr := &http.Transport{
		Proxy:                 nil, // never an environment proxy
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
		DisableCompression:    false,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w: too many redirects", ErrBlocked)
			}
			return checkURL(req.URL)
		},
	}
	return &HTTPFetcher{cfg: cfg, client: client, robots: map[string]robotsEntry{}}
}

// guardedDialer dials only public addresses: the check runs on the
// resolved IP of every connection (net.Dialer.Control).
func guardedDialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrBlocked, address)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !publicAddr(ip) {
			return fmt.Errorf("%w: %s", ErrBlocked, host)
		}
		return nil
	}}
	return d.DialContext
}

// blockedPrefixes are the non-public ranges netip has no predicate for.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 (may reach IPv4 internals)
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("fec0::/10"),       // site-local (deprecated)
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local, cloud metadata (169.254.169.254)
	netip.MustParsePrefix("fd00:ec2::/32"),   // AWS IMDS over IPv6
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
}

// publicAddr reports whether ip is a public unicast address.
func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// defaultPorts is the only port each fetched scheme may name.
var defaultPorts = map[string]string{"https": "443", "http": "80"}

// checkURL refuses what the guard never fetches: another scheme, user
// info, a port other than the scheme's, an IP literal of a non-public
// address, a host without a dot ("localhost", intranet names).
func checkURL(u *url.URL) error {
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("%w: scheme %q", ErrBlocked, u.Scheme)
	}
	if u.User != nil || u.Host == "" {
		return fmt.Errorf("%w: user info or empty host", ErrBlocked)
	}
	if p := u.Port(); p != "" && p != defaultPorts[u.Scheme] {
		return fmt.Errorf("%w: port %s", ErrBlocked, p)
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		if !publicAddr(ip) {
			return fmt.Errorf("%w: %s", ErrBlocked, host)
		}
		return nil
	}
	if !strings.Contains(strings.Trim(host, "."), ".") || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("%w: host %s", ErrBlocked, host)
	}
	return nil
}

// ParseSourceURL validates a source URL as the guard would fetch it and
// returns it normalised with its host (lower case, no fragment).
func ParseSourceURL(raw string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || len(raw) > 2048 {
		return "", "", fmt.Errorf("%w: malformed URL", ErrBlocked)
	}
	if err := checkURL(u); err != nil {
		return "", "", err
	}
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	return u.String(), u.Hostname(), nil
}

// Robots implements Fetcher.
func (f *HTTPFetcher) Robots(ctx context.Context, rawURL string) (RobotsVerdict, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return RobotsVerdict{}, fmt.Errorf("%w: malformed URL", ErrBlocked)
	}
	if err := checkURL(u); err != nil {
		return RobotsVerdict{}, err
	}
	key := u.Scheme + "://" + strings.ToLower(u.Host)
	now := f.cfg.Clock.Now()
	f.mu.Lock()
	e, ok := f.robots[key]
	f.mu.Unlock()
	if !ok || now.Sub(e.fetched) > f.cfg.RobotsTTL || e.fail {
		e = f.loadRobots(ctx, key)
		e.fetched = now
		f.mu.Lock()
		f.robots[key] = e
		f.mu.Unlock()
	}
	if e.fail {
		return RobotsVerdict{Unreachable: true}, nil
	}
	path := u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return RobotsVerdict{Allowed: e.rules.allowed(path), CrawlDelay: e.rules.crawlDelay}, nil
}

// loadRobots fetches key/robots.txt: 2xx parsed, 4xx allow all, anything
// else "unreachable" (disallow).
func (f *HTTPFetcher) loadRobots(ctx context.Context, key string) robotsEntry {
	p, err := f.get(ctx, FetchRequest{URL: key + "/robots.txt"}, maxRobotsBytes)
	switch {
	case err != nil:
		return robotsEntry{fail: true}
	case p.Status >= 200 && p.Status < 300:
		return robotsEntry{rules: parseRobots(p.Body, robotsAgent)}
	case p.Status >= 400 && p.Status < 500:
		return robotsEntry{}
	}
	return robotsEntry{fail: true}
}

// Fetch implements Fetcher.
func (f *HTTPFetcher) Fetch(ctx context.Context, req FetchRequest) (Page, error) {
	p, err := f.get(ctx, req, f.cfg.MaxBytes)
	if err != nil || p.Status < 200 || p.Status > 299 || len(req.Accept) == 0 {
		return p, err
	}
	if !slices.Contains(req.Accept, p.ContentType) {
		return p, fmt.Errorf("%w: %s", ErrContentType, p.ContentType)
	}
	return p, nil
}

// get performs one guarded GET.
func (f *HTTPFetcher) get(ctx context.Context, req FetchRequest, maxBytes int64) (Page, error) {
	u, err := url.Parse(req.URL)
	if err != nil {
		return Page{}, fmt.Errorf("%w: malformed URL", ErrBlocked)
	}
	if err := checkURL(u); err != nil {
		return Page{}, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Page{}, err
	}
	r.Header.Set("User-Agent", f.cfg.UserAgent)
	r.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,application/rss+xml,application/atom+xml,application/xml,text/xml,text/plain;q=0.8,*/*;q=0.1")
	r.Header.Set("Accept-Language", "tr,en;q=0.8")
	if req.ETag != "" {
		r.Header.Set("If-None-Match", req.ETag)
	}
	if req.LastModified != "" {
		r.Header.Set("If-Modified-Since", req.LastModified)
	}
	resp, err := f.client.Do(r)
	if err != nil {
		if errors.Is(err, ErrBlocked) {
			return Page{}, ErrBlocked
		}
		return Page{}, err
	}
	defer resp.Body.Close()
	p := Page{URL: resp.Request.URL.String(), Status: resp.StatusCode, ETag: resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified")}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil {
		p.ContentType = strings.ToLower(mt)
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		p.NotModified = true
		return p, nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return p, nil
	case resp.ContentLength > maxBytes:
		return p, ErrTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return p, err
	}
	if int64(len(body)) > maxBytes {
		return p, ErrTooLarge
	}
	p.Body = body
	return p, nil
}
