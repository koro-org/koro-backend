package parties

import (
	"strings"
)

// Tax identifiers (05 §11.2: never logged, not even masked; TCKN never in a
// response). The format checks below run before anything is stored; the
// database repeats the digit-count CHECKs (02 §17.1).

// digitsOnly removes the spaces, dots and hyphens people type into numbers
// and reports whether only ASCII digits remain.
func digitsOnly(s string) (string, bool) {
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
	_, ok := digitsOnly(s)
	return ok
}

// maskTail keeps the last n digits of an identifier for the org roles
// (BE-036: VKN and MERSİS are masked outside finance and platform admins):
// "1234567890" → "******7890".
func maskTail(s string, n int) string {
	if len(s) <= n {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-n) + s[len(s)-n:]
}
