package students

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"

	"golang.org/x/text/unicode/norm"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Application limits (02 §9.3: at most 30 skills; the language and
// experience limits are this module's own, see the BE-023 notes).
const (
	maxSkills      = 30
	maxLanguages   = 20
	maxExperiences = 50

	maxHeadline     = 160
	maxAbout        = 2000
	maxCity         = 60
	maxTitle        = 120
	maxOrgName      = 160
	maxDescription  = 2000
	avatarTTL       = 5 * time.Minute // 04 §1.11: signed URLs ≤ 5 min
	studentsModule  = "students"      // files.Owner.Module of the avatar
	resourceProfile = authz.TypeStudentProfile
)

// monthRe is a "YYYY-MM" month (expected_graduation, experience months).
var monthRe = regexp.MustCompile(`^([0-9]{4})-(0[1-9]|1[0-2])$`)

// ownResource is the authz view of the caller's own profile (03 §3.5:
// student_profile with access_grant, discoverable, minor; 06 H-27:
// OwnerUserID always set). The own-profile actions read neither
// access_grant (no company is involved: "none") nor discoverable (the
// settings are not read before authz; the key is present and empty, which
// 03 §3.5 allows for a fact the decision does not need).
func ownResource(a authz.Actor) authz.Resource {
	me := a.UserID
	return authz.Resource{Type: resourceProfile, ID: me, OwnerUserID: &me, Attrs: map[string]string{
		authz.AttrAccessGrant: "none", "discoverable": "", "minor": strconv.FormatBool(a.IsMinor),
	}}
}

// can checks an own-profile action. An account without an active student
// verification gets 403 students.verification_required (03 §2.3, k-uv).
func (s *service) can(ctx context.Context, a authz.Actor, act authz.Action) error {
	err := authz.Can(ctx, a, act, ownResource(a))
	if err != nil && errors.Is(err, authz.ErrForbidden) && a.IsUser() && !a.IsVerifiedStudent() {
		return ErrVerificationRequired
	}
	return err
}

// writeEvent is the audit record of a write on the caller's own data.
func writeEvent(a authz.Actor, act authz.Action, meta map[string]any) audit.Event {
	me := a.UserID
	return audit.Event{Action: string(act), Outcome: audit.Success, ResourceType: resourceProfile, ResourceID: &me,
		Metadata: meta}.By(a)
}

func fieldErr(location, msg string) error {
	return problem.ErrValidation.WithFields(problem.FieldError{Location: location, Message: msg})
}

// cleanLine normalises an optional single-line field; "" clears it (nil).
func cleanLine(loc, s string, maxRunes int) (*string, error) {
	n, err := textx.NormalizeText(s)
	if err != nil {
		return nil, fieldErr(loc, "contains control or bidi characters")
	}
	if utf8.RuneCountInString(n) > maxRunes {
		return nil, fieldErr(loc, "must be at most "+strconv.Itoa(maxRunes)+" characters")
	}
	if n == "" {
		return nil, nil
	}
	return &n, nil
}

// cleanMultiline keeps line breaks but refuses other control and bidi
// characters; "" clears it.
func cleanMultiline(loc, s string, maxRunes int) (*string, error) {
	if !utf8.ValidString(s) {
		return nil, fieldErr(loc, "must be UTF-8")
	}
	for _, r := range s {
		if (unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r') || bidiControl(r) {
			return nil, fieldErr(loc, "contains control or bidi characters")
		}
	}
	d := norm.NFC.String(strings.TrimSpace(strings.ReplaceAll(s, "\r", "")))
	if utf8.RuneCountInString(d) > maxRunes {
		return nil, fieldErr(loc, "must be at most "+strconv.Itoa(maxRunes)+" characters")
	}
	if d == "" {
		return nil, nil
	}
	return &d, nil
}

// bidiControl reports the bidirectional embedding, override and isolate
// characters (display spoofing).
func bidiControl(r rune) bool {
	return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069
}

// parseMonth parses "YYYY-MM" into the first day of the month.
func parseMonth(loc, s string) (civil.Date, error) {
	m := monthRe.FindStringSubmatch(s)
	if m == nil {
		return civil.Date{}, fieldErr(loc, "must be a month YYYY-MM")
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, err := civil.New(y, time.Month(mo), 1)
	if err != nil || y < 1950 || y > 2100 {
		return civil.Date{}, fieldErr(loc, "must be a month YYYY-MM between 1950 and 2100")
	}
	return d, nil
}

// monthString formats a first-of-month date as "YYYY-MM".
func monthString(d civil.Date) string { return fmt.Sprintf("%04d-%02d", d.Year, int(d.Month)) }

// emitAfterCommit writes an A event (01 §6.1): a failure is logged, never
// returned (the write itself succeeded).
func (s *service) emitAfterCommit(ctx context.Context, p domainevent.Payload, aggregate string, id uuid.UUID) {
	env, err := domainevent.New(p, aggregate, id)
	if err == nil {
		err = s.d.Outbox.AfterCommit(ctx, env)
	}
	if err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "students event not emitted",
			slog.String("topic", p.Topic()), slog.String("user_id", id.String()), slog.String("err", err.Error()))
	}
}

// emitOutbox writes an O event in the transaction carried by ctx.
func (s *service) emitOutbox(ctx context.Context, p domainevent.Payload, aggregate string, id uuid.UUID) error {
	env, err := domainevent.New(p, aggregate, id)
	if err != nil {
		return err
	}
	return s.d.Outbox.Add(ctx, env)
}

// fieldError names the offending request member of a 422.
func fieldError(location, msg string) problem.FieldError {
	return problem.FieldError{Location: location, Message: msg}
}
