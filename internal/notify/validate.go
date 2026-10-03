package notify

import (
	"fmt"
	"mime"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/problem"
)

// Limits of one message. They keep a single send small enough for every
// provider and stop a caller bug from mailing megabytes.
const (
	maxDedupeKey       = 200 // notification_deliveries.dedupe_key CHECK
	maxInline          = 5
	maxInlineBytes     = 512 << 10
	maxAttachments     = 5
	maxAttachmentBytes = 10 << 20 // all attachments together
	maxFilename        = 100
	maxURL             = 2000
)

var (
	inlineTypes     = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true}
	attachmentTypes = map[string]bool{"text/calendar": true, "application/pdf": true}

	reCID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	reFilename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]*$`)
	reMethod   = regexp.MustCompile(`^[A-Z]{1,16}$`)
)

// validateMessage checks everything that does not need the rendered body.
// Field errors name the member, never its value (problem.FieldError has no
// value member by design).
func validateMessage(m Message) []problem.FieldError {
	var fe []problem.FieldError
	bad := func(loc, msg string) { fe = append(fe, problem.FieldError{Location: "message." + loc, Message: msg}) }

	if !m.Channel.valid() {
		bad("channel", "must be email, push or in_app")
	}
	if !m.Category.valid() {
		bad("category", "must be transactional or commercial")
	}
	if !reTemplateID.MatchString(m.Template) {
		bad("template", "must look like <module>.<name> (lower-case letters and underscores)")
	}
	if m.UserID != nil && *m.UserID == uuid.Nil() {
		bad("user_id", "must not be the nil UUID")
	}
	if n := utf8.RuneCountInString(m.DedupeKey); n == 0 || n > maxDedupeKey {
		bad("dedupe_key", fmt.Sprintf("must be 1 to %d characters", maxDedupeKey))
	} else if strings.IndexFunc(m.DedupeKey, unicode.IsControl) >= 0 || strings.Contains(m.DedupeKey, "@") {
		// The key is stored in clear: no addresses, no control characters.
		bad("dedupe_key", "must not contain control characters or '@' (it is stored in clear; use IDs)")
	}

	switch m.Channel {
	case ChannelEmail:
		if !validEmail(m.ToEmail) {
			bad("to_email", "must be a bare e-mail address")
		}
		fe = append(fe, validateAttachments(m)...)
	case ChannelPush, ChannelInApp:
		if m.UserID == nil {
			bad("user_id", "is required for push and in_app messages")
		}
		if len(m.Inline) > 0 || len(m.Attachments) > 0 {
			bad("attachments", "are only supported on the email channel")
		}
	}

	if m.Category == CategoryCommercial {
		if m.UserID == nil {
			bad("user_id", "is required for commercial messages (consent is per user)")
		}
		if m.Channel == ChannelInApp {
			bad("channel", "commercial messages are not supported on in_app")
		}
		if m.Channel == ChannelEmail && !validUnsubscribeURL(m.UnsubscribeURL) {
			bad("unsubscribe_url", "must be an https URL for commercial e-mail (RFC 8058 one-click unsubscribe)")
		}
	}
	return fe
}

func validateAttachments(m Message) []problem.FieldError {
	var fe []problem.FieldError
	bad := func(loc, msg string) { fe = append(fe, problem.FieldError{Location: "message." + loc, Message: msg}) }

	if len(m.Inline) > maxInline {
		bad("inline", fmt.Sprintf("at most %d inline attachments", maxInline))
	}
	seen := map[string]bool{}
	for i, in := range m.Inline {
		loc := fmt.Sprintf("inline[%d]", i)
		if !reCID.MatchString(in.CID) {
			bad(loc+".cid", "must be 1 to 63 characters of letters, digits, '.', '_' or '-'")
		} else if seen[in.CID] {
			bad(loc+".cid", "must be unique")
		}
		seen[in.CID] = true
		if _, ok := normalizeContentType(in.ContentType, inlineTypes); !ok {
			bad(loc+".content_type", "must be image/png, image/jpeg or image/gif")
		}
		if len(in.Data) == 0 || len(in.Data) > maxInlineBytes {
			bad(loc+".data", fmt.Sprintf("must be 1 to %d bytes", maxInlineBytes))
		}
	}

	if len(m.Attachments) > maxAttachments {
		bad("attachments", fmt.Sprintf("at most %d attachments", maxAttachments))
	}
	total := 0
	for i, a := range m.Attachments {
		loc := fmt.Sprintf("attachments[%d]", i)
		if len(a.Filename) > maxFilename || !reFilename.MatchString(a.Filename) || strings.Contains(a.Filename, "..") {
			bad(loc+".filename", "must be a plain ASCII file name (letters, digits, '.', '_', '-', space; no path)")
		}
		if _, ok := normalizeContentType(a.ContentType, attachmentTypes); !ok {
			bad(loc+".content_type", "must be text/calendar or application/pdf")
		}
		if len(a.Data) == 0 {
			bad(loc+".data", "must not be empty")
		}
		total += len(a.Data)
	}
	if total > maxAttachmentBytes {
		bad("attachments", fmt.Sprintf("together at most %d bytes", maxAttachmentBytes))
	}
	return fe
}

// checkInlineRefs requires every inline image to be referenced by the HTML
// as cid:<CID>; an unreferenced image would show up as a stray attachment.
func checkInlineRefs(m Message, html string) []problem.FieldError {
	var fe []problem.FieldError
	for i, in := range m.Inline {
		re := regexp.MustCompile(`cid:` + regexp.QuoteMeta(in.CID) + `["'\s)>]`)
		if !re.MatchString(html) {
			fe = append(fe, problem.FieldError{
				Location: fmt.Sprintf("message.inline[%d].cid", i),
				Message:  "is not referenced by the rendered HTML as cid:<CID>",
			})
		}
	}
	return fe
}

// normalizeContentType parses ct and returns its canonical form if the media
// type is in allowed. Parameters are rejected except method and charset on
// text/calendar (RFC 5545 §3.2: "text/calendar; method=PUBLISH").
func normalizeContentType(ct string, allowed map[string]bool) (string, bool) {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || !allowed[mt] {
		return "", false
	}
	for k, v := range params {
		switch {
		case mt == "text/calendar" && k == "method" && reMethod.MatchString(v):
		case mt == "text/calendar" && k == "charset" && strings.EqualFold(v, "utf-8"):
		default:
			return "", false
		}
	}
	out := mime.FormatMediaType(mt, params)
	return out, out != ""
}

func validEmail(s string) bool {
	if s == "" || len(s) > 254 || strings.TrimSpace(s) != s {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == s
}

func validUnsubscribeURL(s string) bool {
	if s == "" || len(s) > maxURL || strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '<' || r == '>' }) >= 0 {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}
