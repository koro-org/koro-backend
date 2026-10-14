package orders

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"regexp"
	"strings"
)

// The order number is the bank-transfer reference (02 §19.1): "KR" and 8
// characters of Crockford's base32 (no I, L, O, U), random, re-drawn on a
// collision. The buyer writes it into the transfer description; finance
// (payments, BE-041) finds the order by it however it was typed: spaces,
// hyphens, dots and underscores are ignored, lower case is upper case, and
// the letters Crockford reads as digits (O → 0, I and L → 1) are mapped.

// crockford is Crockford's base32 alphabet.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	refEncoding = base32.NewEncoding(crockford).WithPadding(base32.NoPadding)
	refRe       = regexp.MustCompile(`^KR[0-9A-HJKMNP-TV-Z]{8}$`)
)

// refPrefix starts every reference code.
const refPrefix = "KR"

// newReference draws a reference code: 40 random bits as 8 Crockford
// characters.
func newReference() (string, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("orders: reference code: %w", err)
	}
	return refPrefix + refEncoding.EncodeToString(b[:]), nil
}

// isSep reports a separator a buyer may type inside a code.
func isSep(c byte) bool { return c == ' ' || c == '-' || c == '.' || c == '_' || c == '\t' }

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// NormalizeReference returns the canonical reference code of s, or false
// when s is not one (04 §C.3, 01 §4.18 FindByReference).
func NormalizeReference(s string) (string, bool) {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case isSep(c):
		case isAlnum(c):
			b.WriteByte(upper(c))
		default:
			return "", false
		}
	}
	return canonical(b.String())
}

// canonical maps the body of an upper-case, separator-free code to
// Crockford digits and validates it.
func canonical(code string) (string, bool) {
	if len(code) != len(refPrefix)+8 || !strings.HasPrefix(code, refPrefix) {
		return "", false
	}
	body := []byte(code[len(refPrefix):])
	for i, c := range body {
		switch c {
		case 'O':
			body[i] = '0'
		case 'I', 'L':
			body[i] = '1'
		}
	}
	out := refPrefix + string(body)
	if !refRe.MatchString(out) {
		return "", false
	}
	return out, true
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}

// ExtractReference finds the first reference code in a free text (a bank
// transfer description: "kr7q 2m9t-xa stant ödemesi"), with the
// separators and the Crockford mapping of NormalizeReference. The code
// must stand on its own (not inside a longer word). payments (BE-041)
// stores it as incoming_bank_transfers.extracted_order_no.
func ExtractReference(text string) (string, bool) {
	for i := 0; i+1 < len(text); i++ {
		if upper(text[i]) != 'K' || upper(text[i+1]) != 'R' || (i > 0 && isAlnum(text[i-1])) {
			continue
		}
		var body []byte
		j := i + 2
		for ; j < len(text) && len(body) < 8; j++ {
			c := text[j]
			if isSep(c) {
				continue
			}
			if !isAlnum(c) {
				break
			}
			body = append(body, upper(c))
		}
		if len(body) != 8 || (j < len(text) && isAlnum(text[j])) {
			continue
		}
		if code, ok := canonical(refPrefix + string(body)); ok {
			return code, true
		}
	}
	return "", false
}
