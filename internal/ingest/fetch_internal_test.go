package ingest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"koro/internal/platform/clock"
)

func testFetcherConfig() FetcherConfig {
	return FetcherConfig{UserAgent: "KoroBot/1.0 (+https://example.org/bot)", Timeout: 5 * time.Second, MaxBytes: 1 << 10,
		RobotsTTL: time.Hour, Clock: clock.NewFake(time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC))}
}

// loopbackDialer lets the fetcher reach the local test server (the guard
// itself is tested apart): the SSRF policy of every other check stays.
func loopbackDialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: time.Second}
	return d.DialContext
}

// TestFetch_SSRFGuardRefusesNonPublicAddresses: the URL check refuses other
// schemes, credentials, odd ports, IP literals of private, loopback,
// link-local (cloud metadata) and CGNAT ranges and dotless or .local hosts;
// the dialer refuses them after DNS; the production fetcher cannot reach a
// loopback server, directly or through a redirect.
func TestFetch_SSRFGuardRefusesNonPublicAddresses(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"ftp://example.org/x", "file:///etc/passwd", "gopher://example.org", "https://user:pw@example.org/",
		"https://example.org:8443/", "http://example.org:22/", "http://127.0.0.1/", "http://10.0.0.5/",
		"http://192.168.1.1/", "http://172.16.0.1/", "http://169.254.169.254/latest/meta-data/", "http://[::1]/",
		"http://[fd00:ec2::254]/", "http://100.64.0.1/", "http://0.0.0.0/", "http://localhost/", "http://intranet/",
		"http://printer.local/", "http://metadata.google.internal/", "http://[::ffff:127.0.0.1]/",
	} {
		if _, _, err := ParseSourceURL(raw); !errors.Is(err, ErrBlocked) {
			t.Errorf("ParseSourceURL(%q) = %v, want ErrBlocked", raw, err)
		}
	}
	for _, ok := range []string{"https://kim.metu.edu.tr/tr/etkinlikler", "http://cdt.example.edu.tr/", "https://8.8.8.8/x",
		"https://example.org:443/a?b=c#frag"} {
		u, host, err := ParseSourceURL(ok)
		if err != nil || host == "" || strings.Contains(u, "#") {
			t.Errorf("ParseSourceURL(%q) = %q %q %v", ok, u, host, err)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "::1", "fe80::1", "100.100.100.100",
		"::ffff:10.0.0.1", "224.0.0.1", "0.0.0.0", "198.18.0.1", "255.255.255.255"} {
		if publicAddr(netip.MustParseAddr(ip)) {
			t.Errorf("publicAddr(%s) = true", ip)
		}
	}
	if !publicAddr(netip.MustParseAddr("93.184.215.14")) || !publicAddr(netip.MustParseAddr("2606:4700::1111")) {
		t.Error("a public address was refused")
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("secret"))
	}))
	t.Cleanup(srv.Close)
	// A public-looking name that resolves to the loopback test server: the
	// dial check refuses it after resolution (DNS rebinding included).
	f := NewHTTPFetcher(testFetcherConfig())
	u, _ := url.Parse(srv.URL)
	tr := f.client.Transport.(*http.Transport)
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dial(ctx, network, u.Host) // every name "resolves" to 127.0.0.1:port
	}
	_, err := f.Fetch(t.Context(), FetchRequest{URL: "http://rebind.example.org/"})
	if !errors.Is(err, ErrBlocked) || hits.Load() != 0 {
		t.Fatalf("fetch through the guard = %v (hits %d), want ErrBlocked and no request", err, hits.Load())
	}

	// A public server redirecting to a private address: the hop is refused.
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	open := newHTTPFetcher(testFetcherConfig(), loopbackDialer())
	ru, _ := url.Parse(redirect.URL)
	open.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, ru.Host)
	}
	if _, err := open.Fetch(t.Context(), FetchRequest{URL: "http://public.example.org/"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("redirect to metadata = %v, want ErrBlocked", err)
	}
}

// TestFetch_RobotsConditionalGetAndLimits: robots.txt is read once per
// host (a disallowed path is refused, a 404 robots allows, a 5xx refuses),
// the User-Agent is the bot's, a matching ETag gives 304, a body over the
// limit and an unexpected content type are errors.
func TestFetch_RobotsConditionalGetAndLimits(t *testing.T) {
	t.Parallel()
	var robots, agentOK atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		robots.Add(1)
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\nCrawl-delay: 15\n"))
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("User-Agent"), "KoroBot/1.0") {
			agentOK.Add(1)
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<p>ok</p>"))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(strings.Repeat("x", 4<<10)))
	})
	mux.HandleFunc("/pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	f := newHTTPFetcher(testFetcherConfig(), loopbackDialer())
	f.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, u.Host)
	}
	base := "http://site.example.org"
	ctx := t.Context()
	if v, err := f.Robots(ctx, base+"/private/x"); err != nil || v.Allowed {
		t.Fatalf("private = %+v %v", v, err)
	}
	if v, err := f.Robots(ctx, base+"/page"); err != nil || !v.Allowed || v.CrawlDelay != 15*time.Second {
		t.Fatalf("page = %+v %v", v, err)
	}
	if robots.Load() != 1 {
		t.Fatalf("robots.txt read %d times, want once per host", robots.Load())
	}
	p, err := f.Fetch(ctx, FetchRequest{URL: base + "/page", Accept: acceptHTML})
	if err != nil || p.Status != 200 || p.ETag != `"v1"` || string(p.Body) != "<p>ok</p>" || p.ContentType != "text/html" {
		t.Fatalf("page = %+v %v", p, err)
	}
	if p, err := f.Fetch(ctx, FetchRequest{URL: base + "/page", ETag: `"v1"`, Accept: acceptHTML}); err != nil || !p.NotModified {
		t.Fatalf("conditional GET = %+v %v", p, err)
	}
	if agentOK.Load() != 2 {
		t.Fatalf("User-Agent seen %d times", agentOK.Load())
	}
	if _, err := f.Fetch(ctx, FetchRequest{URL: base + "/big", Accept: acceptHTML}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("big = %v", err)
	}
	if _, err := f.Fetch(ctx, FetchRequest{URL: base + "/pdf", Accept: acceptHTML}); !errors.Is(err, ErrContentType) {
		t.Fatalf("pdf = %v", err)
	}
	if p, err := f.Fetch(ctx, FetchRequest{URL: base + "/missing"}); err != nil || p.Status != 404 {
		t.Fatalf("missing = %+v %v", p, err)
	}

	// A 5xx robots.txt refuses for this run; a 404 one allows everything.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(down.Close)
	du, _ := url.Parse(down.URL)
	g := newHTTPFetcher(testFetcherConfig(), loopbackDialer())
	g.client.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, du.Host)
	}
	if v, err := g.Robots(ctx, "http://down.example.org/x"); err != nil || v.Allowed || !v.Unreachable {
		t.Fatalf("5xx robots = %+v %v, want unreachable (not fetched, nothing decided)", v, err)
	}
}
