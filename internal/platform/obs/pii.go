package obs

import (
	"log/slog"
	"regexp"
	"strings"
)

// Redacted replaces values that must never be logged, not even masked.
const Redacted = "[REDACTED]"

// PII masking (05 §11.2). Three layers:
//
//  1. types: Email, Phone, IBAN, TCKN (and config.Secret) implement
//     slog.LogValuer and mask themselves;
//  2. keys: ReplaceAttr redacts secret-like keys and masks PII-like keys;
//  3. Scrub: every string value and error text passes the regexes below.
//
// Layer 3 is the last resort; code must not rely on it.
var (
	reIBAN    = regexp.MustCompile(`(?i)\bTR\d{2}(?:\s?\d{4}){5}\s?\d{2}\b`)
	reEmail   = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reTCKN    = regexp.MustCompile(`\b[1-9]\d{10}\b`)
	rePhone   = regexp.MustCompile(`(?:(?:\+90|0090|90|0)[\s.\-]?)?\(?5\d{2}\)?[\s.\-]?\d{3}[\s.\-]?\d{2}[\s.\-]?\d{2}\b`)
	reUUID    = regexp.MustCompile(`\b[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\b`)
	reBearer  = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=\-]+`)
	reCookie  = regexp.MustCompile(`(?i)(__Host-koro_session|koro_session)=[^;\s"]+`)
	reKVToken = regexp.MustCompile(`(?i)\b((?:access_|refresh_|session_|reset_)?token|password|passwd|secret|api_key)=[^&\s"]+`)
	reJSONSec = regexp.MustCompile(`(?i)"((?:access_|refresh_|session_|reset_)?token|password|passwd|secret|api_key)"\s*:\s*"(?:[^"\\]|\\.)*"`)
)

// Scrub masks e-mail addresses, phone numbers, TR IBANs, TCKNs and
// credential-looking substrings (bearer tokens, session cookies,
// token=/password= pairs) in s.
func Scrub(s string) string {
	s = reBearer.ReplaceAllString(s, "Bearer "+Redacted)
	s = reCookie.ReplaceAllString(s, "${1}="+Redacted)
	s = reKVToken.ReplaceAllString(s, "${1}="+Redacted)
	s = reJSONSec.ReplaceAllString(s, `"${1}":"`+Redacted+`"`)
	s = reIBAN.ReplaceAllStringFunc(s, MaskIBAN)
	s = reEmail.ReplaceAllStringFunc(s, MaskEmail)
	s = reTCKN.ReplaceAllStringFunc(s, MaskTCKN)
	return scrubPhones(s)
}

// scrubPhones masks phone numbers that start at a token boundary.
//
// rePhone has no left boundary of its own (RE2 has no lookbehind, and \b
// would miss "+90…" and "(5xx)…"), so the digits at the tail of an
// identifier used to match: the UUID "…-0b5553195418" logged as "…-0b***18",
// breaking request_id correlation (BE-108). A candidate is masked only when
// it starts the string, starts with a non-alphanumeric byte ('+', '('), or
// follows a byte that is not an ASCII letter or digit, or follows a
// percent-escape ("%3A0555…", "%2B90555…"). Candidates inside a UUID are
// never masked. A rejected candidate is retried one byte further on, so a
// phone that starts later in the same run still has its chance.
func scrubPhones(s string) string {
	loc := rePhone.FindStringIndex(s)
	if loc == nil {
		return s
	}
	uuids := reUUID.FindAllStringIndex(s, -1)
	var b strings.Builder
	last := 0
	for off := 0; loc != nil; loc = rePhone.FindStringIndex(s[off:]) {
		start, end := off+loc[0], off+loc[1]
		if !phoneStartOK(s, start) || overlapsAny(uuids, start, end) {
			off = start + 1 // every rePhone match starts with an ASCII byte
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(MaskPhone(s[start:end]))
		last, off = end, end
	}
	if b.Len() == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func phoneStartOK(s string, i int) bool {
	if i == 0 || !isAlnum(s[i]) {
		return true
	}
	if !isAlnum(s[i-1]) {
		return true
	}
	return i >= 3 && s[i-3] == '%' && isHex(s[i-2]) && isHex(s[i-1])
}

func overlapsAny(spans [][]int, start, end int) bool {
	for _, sp := range spans {
		if start < sp[1] && sp[0] < end {
			return true
		}
	}
	return false
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'F' || c >= 'a' && c <= 'f'
}

// MaskEmail masks an e-mail address: "ayse@ogrenci.edu.tr" → "a***@o***.tr".
func MaskEmail(s string) string {
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || domain == "" {
		return Redacted
	}
	host, tld := domain, ""
	if i := strings.LastIndex(domain, "."); i > 0 {
		host, tld = domain[:i], domain[i:]
	}
	return string([]rune(local)[:1]) + "***@" + string([]rune(host)[:1]) + "***" + tld
}

// MaskPhone keeps the last two digits: "+90 532 123 45 67" → "***67".
func MaskPhone(s string) string {
	if strings.HasPrefix(s, "***") {
		return s
	}
	d := digits(s)
	if len(d) <= 2 {
		return Redacted
	}
	return "***" + d[len(d)-2:]
}

// MaskIBAN keeps the last four digits: "TR33 0006 ... 13 26" → "TR**…1326".
func MaskIBAN(s string) string {
	if strings.HasPrefix(s, "TR**…") {
		return s
	}
	d := digits(s)
	if len(d) <= 4 {
		return Redacted
	}
	return "TR**…" + d[len(d)-4:]
}

// MaskTCKN always masks a Turkish national ID completely.
func MaskTCKN(string) string { return "[TCKN]" }

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Email logs as a masked e-mail address.
type Email string

// LogValue implements slog.LogValuer.
func (e Email) LogValue() slog.Value { return slog.StringValue(MaskEmail(string(e))) }

// Phone logs as a masked phone number.
type Phone string

// LogValue implements slog.LogValuer.
func (p Phone) LogValue() slog.Value { return slog.StringValue(MaskPhone(string(p))) }

// IBAN logs as a masked IBAN.
type IBAN string

// LogValue implements slog.LogValuer.
func (i IBAN) LogValue() slog.Value { return slog.StringValue(MaskIBAN(string(i))) }

// TCKN logs as a fully masked national ID.
type TCKN string

// LogValue implements slog.LogValuer.
func (TCKN) LogValue() slog.Value { return slog.StringValue(MaskTCKN("")) }

var redactKeys = map[string]bool{
	"password": true, "passwd": true, "token": true, "authorization": true, "cookie": true,
	"set-cookie": true, "secret": true, "pepper": true, "api_key": true, "session": true,
	"totp": true, "totp_code": true, "reauth": true,
}

func redactKey(k string) bool {
	if redactKeys[k] {
		return true
	}
	for _, suf := range []string{"_token", "_secret", "_password", "_pepper"} {
		if strings.HasSuffix(k, suf) {
			return true
		}
	}
	return false
}

func keyIs(k, name string) bool { return k == name || strings.HasSuffix(k, "_"+name) }

// RedactAttr applies the key and value rules of the log handler (layers 2
// and 3) to one attribute whose value is already resolved. Other sinks of
// log records (internal/platform/errtrack, OPS-014) use it so that they
// never carry more than the log line does.
func RedactAttr(a slog.Attr) slog.Attr { return redactAttr(nil, a) }

// redactAttr is the slog ReplaceAttr hook (layers 2 and 3). LogValuer values
// are already resolved when it runs.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	k := strings.ToLower(a.Key)
	if redactKey(k) {
		return slog.String(a.Key, Redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		v := a.Value.String()
		switch {
		case keyIs(k, "email"):
			return slog.String(a.Key, MaskEmail(v))
		case keyIs(k, "phone"):
			return slog.String(a.Key, MaskPhone(v))
		case keyIs(k, "iban"):
			return slog.String(a.Key, MaskIBAN(v))
		case keyIs(k, "tckn"):
			return slog.String(a.Key, MaskTCKN(v))
		}
		return slog.String(a.Key, Scrub(v))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, Scrub(err.Error()))
		}
		if st, ok := a.Value.Any().(interface{ String() string }); ok {
			return slog.String(a.Key, Scrub(st.String()))
		}
	}
	return a
}
