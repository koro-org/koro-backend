package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// RateLimit configures rate limiting (internal/platform/ratelimit; 05 §10.5,
// 04 §1.9). API only. Owner: BE-006. The class limits are code constants
// (ratelimit.Rules) until measurements move them here.
type RateLimit struct {
	// Enabled may be switched off only in dev (load tests, local tooling).
	Enabled bool `env:"RATE_LIMIT_ENABLED" envDefault:"true"`
	// WebKeys (BE-119) are the keys koro-web presents in X-Koro-Web-Key on
	// its cookie-free cached reads (its KORO_API_WEB_KEY): a match is
	// counted in koro-web's own bucket instead of its single IP
	// (ratelimit/web.go). The key only picks a counter; it grants no
	// identity or permission. Comma- or newline-separated, at most
	// RateLimitMaxWebKeys (the active key and, during a rotation, the
	// previous one), each 32–256 characters of [A-Za-z0-9._~+/=-].
	// Optional in every environment: unset, koro-web's calls count against
	// its IP like any other caller (the safe default; staging/prod log a
	// warning). Staging/prod: RATE_LIMIT_WEB_KEYS_FILE (Docker secret). Its
	// own value, never KORO_REVALIDATE_SECRET. Generate: openssl rand -hex 32.
	WebKeys Secret `env:"RATE_LIMIT_WEB_KEYS"`
}

// RateLimitMaxWebKeys bounds RATE_LIMIT_WEB_KEYS: the active key and the
// previous one while koro-web switches over.
const RateLimitMaxWebKeys = 2

var webKeyRe = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{32,256}$`)

// WebKeyList returns the keys of RATE_LIMIT_WEB_KEYS (none when unset).
func (r RateLimit) WebKeyList() []string {
	return strings.FieldsFunc(r.WebKeys.Reveal(), func(c rune) bool { return c == ',' || unicode.IsSpace(c) })
}

// validate never prints a key: errors name the variable and the position.
func (r *RateLimit) validate(c Common) error {
	var errs []error
	if !r.Enabled && c.ProdLike() {
		errs = append(errs, errors.New("RATE_LIMIT_ENABLED=false is allowed only with APP_ENV=dev"))
	}
	keys := r.WebKeyList()
	if len(keys) > RateLimitMaxWebKeys {
		errs = append(errs, fmt.Errorf("RATE_LIMIT_WEB_KEYS: at most %d keys (the active one and the previous one during a rotation)",
			RateLimitMaxWebKeys))
	}
	seen := make(map[string]bool, len(keys))
	for i, k := range keys {
		switch {
		case !webKeyRe.MatchString(k):
			errs = append(errs, fmt.Errorf("RATE_LIMIT_WEB_KEYS: key %d must be 32–256 characters of [A-Za-z0-9._~+/=-]", i+1))
		case seen[k]:
			errs = append(errs, fmt.Errorf("RATE_LIMIT_WEB_KEYS: key %d is listed twice", i+1))
		case c.ProdLike() && weakWebKey(k):
			errs = append(errs, fmt.Errorf("RATE_LIMIT_WEB_KEYS: key %d looks like a placeholder in staging/prod", i+1))
		}
		seen[k] = true
	}
	return errors.Join(errs...)
}

// weakWebKey flags placeholders ("CHANGE-ME…", "abab…"). weakSecret's bound
// of 16 distinct bytes suits decoded key bytes, not text: a random
// `openssl rand -hex 32` has only 16 possible characters and misses one of
// them about one time in four. Any random key of at least 32 characters
// has 10 or more distinct characters (hex: all but about 1e-12 of them).
func weakWebKey(k string) bool {
	distinct := map[rune]bool{}
	for _, r := range k {
		distinct[r] = true
	}
	return len(distinct) < 10 || strings.Contains(strings.ToLower(k), "change")
}
