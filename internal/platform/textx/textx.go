// Package textx holds the text rules of 05 §9.5 and §14.6:
//
//   - Turkish casing (UpperTR, LowerTR, EqualFoldTR) for human text shown to
//     users: "i" ↔ "İ", "ı" ↔ "I". strings.ToUpper/ToLower are wrong for it.
//   - Machine identifiers (e-mail, user name, slug, domain) never use Turkish
//     casing: NormalizeEmail lower-cases with the Unicode default mapping, so
//     "ISPARTA@…" becomes "isparta@…", not "ısparta@…".
//   - NormalizeText for names and titles (NFC, trimmed, single spaces, no
//     control characters), Slug, EscapeLike for LIKE patterns and SafeCell
//     against CSV/Excel formula injection.
//
// The package is a leaf (standard library and golang.org/x/text only).
package textx

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// ErrInvalidText is returned by NormalizeText for input that must be
// rejected (invalid UTF-8, control or bidirectional override characters).
var ErrInvalidText = errors.New("textx: invalid text")

// UpperTR upper-cases s with Turkish rules after NFC normalization.
func UpperTR(s string) string { return strings.ToUpperSpecial(unicode.TurkishCase, norm.NFC.String(s)) }

// LowerTR lower-cases s with Turkish rules after NFC normalization
// (a decomposed "I" + U+0307 becomes "İ" first and then "i").
func LowerTR(s string) string { return strings.ToLowerSpecial(unicode.TurkishCase, norm.NFC.String(s)) }

// EqualFoldTR reports whether a and b are equal under Turkish case folding
// ("ISPARTA" == "ısparta", "İstanbul" == "istanbul", "I" != "i").
func EqualFoldTR(a, b string) bool { return LowerTR(a) == LowerTR(b) }

// NormalizeEmail returns the comparison form of an e-mail address
// (users.email_normalized, 02 §0.4): trimmed, NFC and lower-cased with the
// Unicode default mapping, never the Turkish one (05 §9.5). It does not
// validate the address.
func NormalizeEmail(s string) string {
	return strings.ToLower(norm.NFC.String(strings.TrimSpace(s)))
}

// NormalizeText normalizes a single-line human text (name, title, search
// input) before it is stored or compared (05 §9.5): NFC, any run of Unicode
// whitespace (tabs and newlines included) becomes one space, leading and
// trailing spaces are removed and invisible zero-width characters are
// dropped. Invalid UTF-8, other control characters and bidirectional
// embedding/override/isolate characters (display spoofing) are rejected.
func NormalizeText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrInvalidText
	}
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			pendingSpace = b.Len() > 0
			continue
		case invisible(r):
			continue
		case unicode.IsControl(r) || bidiControl(r):
			return "", ErrInvalidText
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteRune(r)
	}
	// NFC last: dropping a zero-width character can join a base letter and
	// a combining mark ("e" ZWSP U+0301 → "é").
	return norm.NFC.String(b.String()), nil
}

// invisible reports zero-width characters that carry no meaning in names
// (ZWJ/ZWNJ are kept: emoji sequences and some scripts need them).
func invisible(r rune) bool {
	switch r {
	case '\u00ad', '\u200b', '\u2060', '\ufeff':
		return true
	}
	return false
}

func bidiControl(r rune) bool {
	return r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069'
}

// turkishASCII maps the Turkish letters that do not decompose under NFKD.
var turkishASCII = strings.NewReplacer("ı", "i", "İ", "i")

// Slug returns a URL slug matching ^[a-z0-9]+(-[a-z0-9]+)*$ (02 slug
// CHECKs): Turkish-aware lower case, letters transliterated to ASCII
// ("Çağla Işık Şenyürek" → "cagla-isik-senyurek"), every other run of
// characters becomes one hyphen. If maxLen > 0 the slug is cut to maxLen
// bytes without a trailing hyphen. The result can be empty (e.g. "!!!" or
// non-Latin text); callers must handle that.
func Slug(s string, maxLen int) string {
	s = norm.NFKD.String(turkishASCII.Replace(LowerTR(s)))
	var b strings.Builder
	b.Grow(len(s))
	hyphen := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			hyphen = false
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z': // fullwidth/compatibility forms after NFKD
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			hyphen = false
			b.WriteRune(unicode.ToLower(r))
		case unicode.Is(unicode.Mn, r): // combining marks of decomposed letters
		default:
			hyphen = true
		}
	}
	out := b.String()
	if maxLen > 0 && len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-")
	}
	return out
}

// likeEscaper escapes the LIKE/ILIKE wildcards with PostgreSQL's default
// escape character (backslash).
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// EscapeLike escapes %, _ and \ so that user input matches literally inside a
// LIKE pattern: "WHERE name ILIKE ?" with "%" + EscapeLike(q) + "%".
func EscapeLike(s string) string { return likeEscaper.Replace(s) }

// SafeCell neutralizes spreadsheet formula injection in exported CSV/Excel
// cells (05 §14.6, R-002 §5.5): a value starting with =, +, -, @, tab or
// carriage return gets a leading apostrophe.
func SafeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
