package ratelimit

import (
	"context"
	"net/http"
	"time"

	"koro/internal/authz"
	"koro/internal/platform/token"
)

// koro-web's cookie-free server calls (BE-119).
//
// koro-web renders its public pages (club, event, company, sitemap) from
// cached data ("use cache"): the fetch runs outside any visitor's request,
// so it cannot forward a client IP. Without a bucket of its own, every
// anonymous page view that misses the web cache counts against koro-web's
// single IP (global_ip, and read_default/search keyed by IP): a scan of
// random slugs fills it and every public page answers 429.
//
// koro-web therefore presents a shared key (WebKeyHeader) on those calls,
// and a request that matches is counted once in WebRule, one bucket shared
// by every web instance, instead of global_ip and the anonymous per-IP
// classes. The per-visitor limit of public pages belongs to the edge proxy
// in front of koro-web, not here.
//
// The key only picks a counter. It is never a credential: it grants no
// identity, permission or access (authz still sees an anonymous caller,
// protected operations still answer 401), and it is honoured only when the
// request
//   - uses a safe method (GET, HEAD),
//   - carries no credential (no Cookie, no Authorization header) and
//   - carries no forwarding header (X-Forwarded-For, X-Real-IP, Forwarded):
//     the edge proxy adds X-Forwarded-For to every request it forwards, so
//     a leaked key is useless from outside the internal network.
//
// Strict classes (sign-in, tokens, reauth, export, break-glass) and the
// classes keyed by actor, org or provider keep their own counters. Any
// other request, and every request while no key is configured, falls back
// to the normal IP buckets. A leaked key thus lets a caller inside the
// internal network at most share, and exhaust, koro-web's bucket.
//
// Keys come from RATE_LIMIT_WEB_KEYS (config.RateLimit; *_FILE in staging
// and prod): the active one and, during a rotation, the previous one. Only
// their SHA-256 digests are kept, the comparison is constant time over
// digests (the length leaks nothing), and GlobalIP removes the header from
// every request, so no handler, log line or later middleware sees it.

// WebKeyHeader carries koro-web's key on its cookie-free server calls
// (koro-web publicFetch; BE-119).
const WebKeyHeader = "X-Koro-Web-Key"

// maxWebKey bounds a presented key before hashing (config accepts at most
// 256 characters).
const maxWebKey = 256

// webBucket is the counter key of WebRule: koro-web is one caller.
const webBucket = "web"

// WebRule is the bucket of koro-web's cookie-free calls. It is not an
// operation class (04 §1.9): only GlobalIP applies it, in place of
// global_ip. 50 requests per second sustained (cache misses and
// revalidations only); tuned by measurement like the classes (05
// §18.2-10). It fails open like global_ip.
func WebRule() Rule {
	return Rule{Name: "web_internal", Rate: 3000, Per: time.Minute, Key: KeyWeb}
}

// WebKeys is the set of accepted koro-web keys, held as digests. The zero
// value accepts none.
type WebKeys struct{ digests [][]byte }

// NewWebKeys returns the set of keys (config.RateLimit.WebKeyList); empty
// strings are skipped.
func NewWebKeys(keys []string) WebKeys {
	var w WebKeys
	for _, k := range keys {
		if k != "" {
			w.digests = append(w.digests, token.Hash(k))
		}
	}
	return w
}

// Len returns the number of accepted keys.
func (w WebKeys) Len() int { return len(w.digests) }

// forwardingHeaders are the headers the edge proxy (or any proxy) adds;
// their presence means the request did not come straight from koro-web.
var forwardingHeaders = [...]string{"X-Forwarded-For", "X-Real-Ip", "Forwarded"}

// webCall reports whether r is one of koro-web's cookie-free calls (see
// the rules above). Every key is compared, so the time does not depend on
// which one matches.
func (w WebKeys) webCall(r *http.Request) bool {
	if len(w.digests) == 0 || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	vals := r.Header.Values(WebKeyHeader)
	if len(vals) != 1 || vals[0] == "" || len(vals[0]) > maxWebKey {
		return false
	}
	if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
		return false
	}
	for _, h := range forwardingHeaders {
		if len(r.Header.Values(h)) > 0 {
			return false
		}
	}
	got, ok := token.Hash(vals[0]), false
	for _, d := range w.digests {
		ok = token.Equal(got, d) || ok
	}
	return ok
}

type webCallKey struct{}

// withWebCall marks ctx as one of koro-web's cookie-free calls (GlobalIP).
func withWebCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, webCallKey{}, true)
}

// isWebCall reports whether GlobalIP counted the request in WebRule. An
// authenticated actor is never one (defence in depth: the call carries no
// credential).
func isWebCall(ctx context.Context) bool {
	v, _ := ctx.Value(webCallKey{}).(bool)
	return v && !authz.ActorOrAnonymous(ctx).IsUser()
}
