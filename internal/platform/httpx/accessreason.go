package httpx

import (
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// HeaderAccessReason carries the justification of a break-glass request
// (03 §6.3, 06 H-16).
const HeaderAccessReason = "X-Koro-Access-Reason"

// accessReasonExt is the RFC 8187 ext-value prefix of an encoded
// justification (charset, then an optional language tag between quotes).
const accessReasonExt = "utf-8'"

// MaxAccessReasonHeader bounds the raw header: the ext-value prefix and
// 500 characters of 4 UTF-8 bytes, each byte percent-encoded (3 octets).
// The 10–500 character rule applies to the decoded text (authz).
const MaxAccessReasonHeader = len(accessReasonPrefix) + 500*4*3

// accessReasonPrefix is the prefix koro-web writes (no language tag).
const accessReasonPrefix = "UTF-8''"

// AccessReason is the X-Koro-Access-Reason header of a break-glass
// operation (BE-128). An HTTP header carries Latin-1 at best and browsers
// refuse anything else, so a justification in Turkish is sent as an
// RFC 8187 ext-value: the charset UTF-8, an optional language tag between
// two apostrophes, then the percent-encoded UTF-8 text. In JavaScript:
//
//	"UTF-8''" + encodeURIComponent(reason)
//
// It is decoded here; a plain ASCII value is taken as it is, as before.
// Either way the text is normalized like any staff text
// (textx.NormalizeText: NFC, one space per run of whitespace, no control
// or bidi characters, which the user it concerns would be shown), so the
// audit record and the support access notice hold exactly what was
// typed. A malformed value is 422 at header.X-Koro-Access-Reason, with
// the request's syntax, before the operation runs.
type AccessReason string

// Resolve decodes the header in place (huma.Resolver: it runs after the
// parameters are parsed).
func (r *AccessReason) Resolve(huma.Context) []error {
	text, err := decodeAccessReason(string(*r))
	if err != nil {
		return []error{err}
	}
	*r = AccessReason(text)
	return nil
}

// String returns the decoded justification.
func (r AccessReason) String() string { return string(r) }

// decodeAccessReason returns the text of a raw header value.
func decodeAccessReason(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	text := raw
	if len(raw) >= len(accessReasonExt) && strings.EqualFold(raw[:len(accessReasonExt)], accessReasonExt) {
		rest := raw[len(accessReasonExt):]
		lang, value, ok := strings.Cut(rest, "'")
		if !ok || !languageTag(lang) {
			return "", accessReasonInvalid("must be plain text or UTF-8''<percent-encoded UTF-8> (RFC 8187)")
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return "", accessReasonInvalid("has a malformed percent-encoding")
		}
		text = decoded
	}
	norm, err := textx.NormalizeText(text)
	if err != nil {
		return "", accessReasonInvalid("must be UTF-8 text without control characters")
	}
	return norm, nil
}

// languageTag reports an empty or plausible RFC 5646 tag (letters, digits
// and hyphens, at most 35 characters).
func languageTag(s string) bool {
	if len(s) > 35 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func accessReasonInvalid(msg string) error {
	return problem.Field("header."+HeaderAccessReason, msg)
}

// AccessReasonHeader is the break-glass justification header of an
// operation input (embed it). Operations that take a support ticket
// reference declare X-Koro-Ticket-Ref next to it.
type AccessReasonHeader struct {
	AccessReason AccessReason `header:"X-Koro-Access-Reason" maxLength:"6007" doc:"Break-glass justification (03 §6.3; 10–500 characters, audited): plain ASCII text, or UTF-8'' followed by the percent-encoded UTF-8 text (RFC 8187 ext-value, e.g. \"UTF-8''\" + encodeURIComponent(reason)) for any other text"`
}
