package identity

import (
	_ "embed"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Password policy (05 §14.1, ASVS 6.2): length counted in characters after
// NFKC (the form platform/passwd hashes), no truncation, an upper bound
// instead; common and breached passwords and passwords built from the
// account are refused. The list is embedded: no network call on the
// request path.
const (
	minPasswordLen    = 15 // account without MFA
	minPasswordLenMFA = 8  // account with MFA
	maxPasswordLen    = 128
	minDistinctRunes  = 5
	minContextPart    = 4 // shorter e-mail local parts and names are not searched
)

//go:embed common_passwords.txt
var commonPasswordsTxt string

// commonPasswords is the lower-cased embedded list (one password or
// password root per line; '#' comments).
var commonPasswords = func() map[string]bool {
	m := map[string]bool{}
	for ln := range strings.SplitSeq(commonPasswordsTxt, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		m[strings.ToLower(norm.NFKC.String(ln))] = true
	}
	return m
}()

// passwordContext is what a password must not be built from.
type passwordContext struct {
	Email     string
	FirstName string
	LastName  string
}

// checkPassword applies the policy to pw; the error names body field
// location and never contains the password.
func checkPassword(pw string, mfa bool, ctx passwordContext, location string) error {
	weak := func(msg string) error {
		return ErrWeakPassword.WithFields(problem.FieldError{Location: location, Message: msg})
	}
	if !utf8.ValidString(pw) {
		return weak("must be valid UTF-8")
	}
	n := norm.NFKC.String(pw)
	length := utf8.RuneCountInString(n)
	minLen := minPasswordLen
	if mfa {
		minLen = minPasswordLenMFA
	}
	switch {
	case length < minLen:
		return weak("is too short")
	case length > maxPasswordLen:
		return weak("is too long")
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return weak("must not contain control characters")
		}
	}
	lower := strings.ToLower(n)
	if distinct(lower) < minDistinctRunes || repeated(lower) || sequential(lower) {
		return weak("is too simple")
	}
	if common(lower) {
		return weak("is a common or breached password")
	}
	for _, part := range contextParts(ctx) {
		if strings.Contains(lower, part) {
			return weak("must not contain your e-mail address or name")
		}
	}
	return nil
}

func distinct(s string) int {
	seen := map[rune]bool{}
	for _, r := range s {
		seen[r] = true
	}
	return len(seen)
}

// repeated reports whether s is a shorter string repeated ("abcabcabc…").
func repeated(s string) bool {
	rs := []rune(s)
	for k := 1; k <= len(rs)/2; k++ {
		if len(rs)%k != 0 {
			continue
		}
		ok := true
		for i := k; i < len(rs) && ok; i++ {
			ok = rs[i] == rs[i-k]
		}
		if ok {
			return true
		}
	}
	return false
}

// sequential reports whether every character follows the previous one by
// +1 or every one by -1 ("abcdefghijklmnop", "9876543210…").
func sequential(s string) bool {
	rs := []rune(s)
	if len(rs) < 3 {
		return false
	}
	step := rs[1] - rs[0]
	if step != 1 && step != -1 {
		return false
	}
	for i := 2; i < len(rs); i++ {
		if rs[i]-rs[i-1] != step {
			return false
		}
	}
	return true
}

// common reports whether lower, or its root without leading digits and
// trailing digits/symbols ("Galatasaray1905!" → "galatasaray"), is listed.
func common(lower string) bool {
	if commonPasswords[lower] {
		return true
	}
	root := strings.TrimRightFunc(lower, func(r rune) bool { return unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) })
	root = strings.TrimLeftFunc(root, unicode.IsDigit)
	return root != "" && commonPasswords[root]
}

// contextParts returns the lower-cased e-mail local part, the full address
// and the names the password must not contain (each at least
// minContextPart characters).
func contextParts(ctx passwordContext) []string {
	var out []string
	add := func(s string) {
		s = strings.ToLower(norm.NFKC.String(strings.TrimSpace(s)))
		if utf8.RuneCountInString(s) >= minContextPart {
			out = append(out, s)
		}
	}
	email := textx.NormalizeEmail(ctx.Email)
	if local, _, ok := strings.Cut(email, "@"); ok {
		add(local)
	}
	add(email)
	add(ctx.FirstName)
	add(ctx.LastName)
	return out
}
