// Package iban validates and formats Turkish IBANs (01 §1.1, 02 parties /
// payments). The rule is the same as the database function
// koro_iban_tr_valid (02 §0.3, migration 000002): the normalized value
// matches ^TR[0-9]{24}$ and passes the ISO 13616 / ISO 7064 mod-97 check.
// Both sides are tested with the shared vectors in package ibantest (T-42).
//
// Deliberately not enforced (T-42, T-101): the reserved 10th character
// ("0") of TR IBANs and the ISO 13616 check-digit range 02–98. Adding
// either must change Go, SQL and the shared vectors together.
//
// The package is a leaf: it imports only the standard library. Callers
// translate ErrInvalid into their own problem code (e.g. parties.iban_invalid).
package iban

import (
	"errors"
	"strings"
	"unicode"
)

// ErrInvalid is returned for anything that is not a valid TR IBAN.
var ErrInvalid = errors.New("iban: invalid TR IBAN")

// Length is the length of a normalized TR IBAN.
const Length = 26

// Normalize removes all whitespace and upper-cases ASCII letters
// ("tr33 0006 1005…" → "TR330006 1005…" without spaces). Other characters
// are kept so that Valid rejects them. Normalization never uses Turkish
// casing rules: IBANs are machine identifiers (05 §9.5).
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Valid reports whether s, taken as is (not normalized), is a valid TR IBAN.
// It is the exact Go counterpart of koro_iban_tr_valid.
func Valid(s string) bool {
	if len(s) != Length || s[0] != 'T' || s[1] != 'R' {
		return false
	}
	for i := 2; i < Length; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	// Rearranged number: BBAN ‖ "2927" (T=29, R=27) ‖ check digits, mod 97.
	// Digit by digit keeps the remainder small (no big integers).
	rem := 0
	for _, part := range []string{s[4:], "2927", s[2:4]} {
		for i := range len(part) {
			rem = (rem*10 + int(part[i]-'0')) % 97
		}
	}
	return rem == 1
}

// Parse normalizes s and validates the result. It returns the normalized
// IBAN (the form stored in the database) or ErrInvalid.
func Parse(s string) (string, error) {
	n := Normalize(s)
	if !Valid(n) {
		return "", ErrInvalid
	}
	return n, nil
}

// Format groups a normalized IBAN in blocks of four for display:
// "TR33 0006 1005 1978 6457 8413 26".
func Format(s string) string {
	n := Normalize(s)
	var b strings.Builder
	for i, r := range []rune(n) {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Mask returns the masked form shown to organisation roles (04, payee bank
// accounts): country code, check digits and the last six digits stay
// visible, "TR33 **** **** **** **** 8413 26". Anything that is not a
// 26-character TR IBAN is masked completely ("****"), so a malformed value
// never leaks. Logs use obs.MaskIBAN instead (05 §11.2).
func Mask(s string) string {
	n := Normalize(s)
	if len(n) != Length || !strings.HasPrefix(n, "TR") {
		return "****"
	}
	return Format(n[:4] + strings.Repeat("*", Length-10) + n[Length-6:])
}
