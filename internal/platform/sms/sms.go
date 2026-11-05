// Package sms is the shared vocabulary of Koro's SMS adapters (BE-172): the
// message an adapter sends, its receipt, the errors every adapter maps its
// provider's codes to and the Turkish mobile number rule.
//
// An adapter is a package of its own, chosen by configuration
// (SMS_PROVIDER, cmd/koro/wire_identity.go):
//
//   - netgsm: Netgsm's OTP SMS API (internal/platform/sms/netgsm), the
//     pilot's provider (the same Netgsm account pattern as the İYS adapter,
//     internal/platform/netgsmapi).
//   - mem: the in-memory adapter of tests and development
//     (internal/platform/sms/smsfake); refused in staging and prod.
//   - mailpit: the development SMS box (internal/platform/sms/mailpit,
//     BE-178): every SMS becomes an e-mail in the local Mailpit; refused
//     in staging and prod.
//
// There is no adapter interface here: the consumer declares the small one
// it needs (identity.SMSSender: Send). Rules for every adapter (pinned by
// internal/platform/sms/smscontract):
//
//   - It sends only a message CheckMessage accepts; anything else is
//     ErrRejected (an unusable number: ErrInvalidNumber) without a call.
//   - It maps its provider's codes to ErrRejected (a retry cannot help:
//     credentials, sender name, text, credit), ErrInvalidNumber (the
//     provider refused the number; wraps ErrRejected too) and ErrThrottled
//     (try later); any other error is transient.
//   - Its errors and log lines never hold the number or the text (the text
//     carries a one-time code), nor a credential or the provider's free
//     text.
//   - Ref is the caller's reference of the message (logs, the provider's
//     idempotency where it has one); never personal data.
package sms

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Bounds of a message.
const (
	// MaxText is the longest text: one SMS of the GSM 7-bit alphabet.
	// Texts are plain ASCII (Netgsm's OTP route takes no Turkish
	// characters).
	MaxText = 160
	// MaxRef is the longest reference.
	MaxRef = 64
)

// Message is one SMS.
type Message struct {
	// To is the recipient in E.164 (+905XXXXXXXXX; NormalizeNumber).
	To string
	// Text is the message: printable ASCII, at most MaxText characters.
	Text string
	// Ref is the caller's reference (printable ASCII, at most MaxRef).
	Ref string
}

// String keeps the number and the text (a one-time code) out of %v.
func (m Message) String() string { return "sms.Message{ref:" + m.Ref + "}" }

// GoString keeps the number and the text out of %#v.
func (m Message) GoString() string { return m.String() }

// Receipt is the provider's answer to an accepted message.
type Receipt struct {
	// ID is the provider's reference of the message (Netgsm's job ID).
	ID string
}

// Errors every adapter maps to. Adapters wrap them (errors.Is); the texts
// they add never repeat the number or the message.
var (
	// ErrRejected: the provider refused the message for good; a retry of
	// the same message cannot help.
	ErrRejected = errors.New("sms: refused permanently")
	// ErrInvalidNumber: the number is not a Turkish mobile number, or the
	// provider refused it. Always wrapped together with ErrRejected.
	ErrInvalidNumber = errors.New("sms: invalid recipient number")
	// ErrThrottled: a rate limit of the provider was reached, or it reported
	// a passing system error; try again later.
	ErrThrottled = errors.New("sms: rate limit reached")
)

// errInvalidNumber is ErrInvalidNumber wrapped with ErrRejected.
var errInvalidNumber = fmt.Errorf("%w: %w", ErrRejected, ErrInvalidNumber)

var (
	reE164 = regexp.MustCompile(`^\+905[0-9]{9}$`)
	reRef  = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)
)

// NormalizeNumber returns a Turkish mobile number in E.164
// (+905XXXXXXXXX). It accepts the usual written forms: "+90 5xx xxx xx xx",
// "0090…", "90…", "05xx…" and "5xx…", with spaces, dots, dashes and
// parentheses. Anything else (a landline, a foreign number, letters) is
// ErrInvalidNumber (wrapped with ErrRejected): the pilot sends to Turkish
// mobile numbers only.
func NormalizeNumber(raw string) (string, error) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", errInvalidNumber
		}
	}
	d := b.String()
	switch {
	case len(d) == 14 && strings.HasPrefix(d, "0090"):
		d = d[4:]
	case len(d) == 12 && strings.HasPrefix(d, "90"):
		d = d[2:]
	case len(d) == 11 && strings.HasPrefix(d, "0"):
		d = d[1:]
	}
	n := "+90" + d
	if !reE164.MatchString(n) {
		return "", errInvalidNumber
	}
	return n, nil
}

// ValidNumber reports whether n is a Turkish mobile number in E.164.
func ValidNumber(n string) bool { return reE164.MatchString(n) }

// National returns the ten digits of an E.164 Turkish mobile number
// (5XXXXXXXXX: the form Netgsm takes); "" when n is not one.
func National(n string) string {
	if !ValidNumber(n) {
		return ""
	}
	return n[3:]
}

// CheckMessage applies the rules every adapter shares before a call: a
// Turkish mobile number in E.164 (ErrInvalidNumber), a text of 1–MaxText
// printable ASCII characters and a reference (ErrRejected).
func CheckMessage(m Message) error {
	switch {
	case !ValidNumber(m.To):
		return errInvalidNumber
	case m.Text == "" || len(m.Text) > MaxText:
		return fmt.Errorf("%w: the text must be 1-%d characters", ErrRejected, MaxText)
	case strings.ContainsFunc(m.Text, func(r rune) bool { return r < 0x20 && r != '\n' || r > 0x7e }):
		return fmt.Errorf("%w: the text must be printable ASCII", ErrRejected)
	case !reRef.MatchString(m.Ref):
		return fmt.Errorf("%w: the message needs a reference of at most %d printable characters", ErrRejected, MaxRef)
	}
	return nil
}
