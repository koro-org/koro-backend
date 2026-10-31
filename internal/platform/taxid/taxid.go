// Package taxid checks Turkish tax identifiers: the vergi kimlik numarası
// (VKN), the T.C. kimlik numarası (TCKN) and the MERSİS number. parties
// (the legal party of money, 02 §17.1) and orgs (the company application,
// BE-165) share it (BE-111: one owner per helper).
//
// Tax identifiers are never logged, not even masked (05 §11.2), and a TCKN
// is in no response (06 H-60); VKN and MERSİS are shown masked to all but
// the staff that needs them (Mask). Callers translate a false result into
// their own field error.
//
// The package is a leaf: it imports only the standard library.
package taxid

import (
	"strings"
)

const (
	// MaskedDigits is the number of trailing digits Mask keeps (06 H-60).
	MaskedDigits = 4
	// MaxTaxOffice is the longest vergi dairesi name, in runes (02 §17.1
	// legal_parties.tax_office; companies.tax_office follows it).
	MaxTaxOffice = 100
)

// Digits removes the spaces, dots and hyphens people type into numbers and
// reports whether only ASCII digits remain (an empty result is false).
func Digits(s string) (string, bool) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '.', '-', '\t':
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" {
		return "", false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return s, false
		}
	}
	return s, true
}

// VKN normalizes s (Digits) and reports whether it is a valid VKN.
func VKN(s string) (string, bool) {
	d, ok := Digits(s)
	return d, ok && ValidVKN(d)
}

// TCKN normalizes s (Digits) and reports whether it is a valid TCKN.
func TCKN(s string) (string, bool) {
	d, ok := Digits(s)
	return d, ok && ValidTCKN(d)
}

// MERSIS normalizes s (Digits) and reports whether it is a MERSİS number.
func MERSIS(s string) (string, bool) {
	d, ok := Digits(s)
	return d, ok && ValidMERSIS(d)
}

// ValidVKN reports a vergi kimlik numarası: 10 digits whose last digit is
// the check digit of the Gelir İdaresi algorithm. For the first nine digits
// d1…d9 (i = 1…9): t = (d_i + 10 − i) mod 10; t = 9 adds 9, otherwise
// t·2^(10−i) mod 9 is added; the check digit is (10 − sum mod 10) mod 10.
func ValidVKN(s string) bool {
	if len(s) != 10 {
		return false
	}
	sum := 0
	for i := range 9 {
		d := s[i]
		if d < '0' || d > '9' {
			return false
		}
		t := (int(d-'0') + 10 - (i + 1)) % 10
		if t == 9 {
			sum += 9
			continue
		}
		sum += (t << (9 - i)) % 9 // t · 2^(10−(i+1))
	}
	last := s[9]
	return last >= '0' && last <= '9' && int(last-'0') == (10-sum%10)%10
}

// ValidTCKN reports a T.C. kimlik numarası: 11 digits, no leading zero,
// d10 = ((d1+d3+d5+d7+d9)·7 − (d2+d4+d6+d8)) mod 10 and d11 = (d1+…+d10)
// mod 10.
func ValidTCKN(s string) bool {
	if len(s) != 11 || s[0] == '0' {
		return false
	}
	var d [11]int
	for i := range 11 {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		d[i] = int(s[i] - '0')
	}
	odd := d[0] + d[2] + d[4] + d[6] + d[8]
	even := d[1] + d[3] + d[5] + d[7]
	if ((odd*7-even)%10+10)%10 != d[9] {
		return false
	}
	total := 0
	for _, v := range d[:10] {
		total += v
	}
	return total%10 == d[10]
}

// ValidMERSIS reports a MERSİS number in its published form: 16 digits.
// The registry has no public check-digit rule, so only the form is checked
// (T-235).
func ValidMERSIS(s string) bool {
	if len(s) != 16 {
		return false
	}
	_, ok := Digits(s)
	return ok
}

// Mask keeps the last n characters of an identifier and stars the rest
// (BE-036, 06 H-60): Mask("1234567890", 4) = "******7890". A value of at
// most n characters is starred whole.
func Mask(s string, n int) string {
	if len(s) <= n {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-n) + s[len(s)-n:]
}
