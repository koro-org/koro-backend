package orgs

import (
	"context"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Field limits of 02 §7.1–7.2.
const (
	maxSlug        = 80
	maxDescription = 5000
	maxCity        = 60
	maxWebsite     = 2048
	minReason      = 3
	maxReason      = 500
)

// resourceOrg is the audit resource type of an org.
const resourceOrg = authz.TypeOrg

// orgResource is the authz view of an org (03 §3.5: OrgID = the org
// itself; Attrs org_kind and org_status).
func orgResource(o Org) authz.Resource {
	id := o.ID
	return authz.Resource{Type: authz.TypeOrg, ID: id, OrgID: &id, Attrs: map[string]string{
		"org_kind": string(o.Kind), "org_status": string(o.Status),
	}}
}

// newCompanyResource is the authz view of a company that does not exist
// yet (orgs.company.create).
var newCompanyResource = authz.Resource{Type: authz.TypeOrg, Attrs: map[string]string{
	"org_kind": string(KindCompany), "org_status": string(StatusPendingReview),
}}

// catalogResource is the public org directory (orgs.org.search).
var catalogResource = authz.Resource{Type: authz.TypeOrgCatalog}

// adminSurface is the authz view of /admin/** (03 §3.2: 403 before reading).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// etagOf is the strong ETag of an org version (04 §1.13): the base32 of
// updated_at's UnixNano, quoted.
func etagOf(updatedAt time.Time) string {
	return `"` + strconv.FormatInt(updatedAt.UnixNano(), 32) + `"`
}

// checkIfMatch applies If-Match (04 §1.13, RFC 9110 §13.1.1 strong
// comparison): missing (or "*", which names no version) → 428, no listed
// tag equal to the current one → 412.
func checkIfMatch(header, current string) error {
	h := strings.TrimSpace(header)
	if h == "" || h == "*" {
		return ErrPreconditionRequired
	}
	for tag := range strings.SplitSeq(h, ",") {
		if strings.TrimSpace(tag) == current {
			return nil
		}
	}
	return ErrPreconditionFailed
}

func fieldErr(location, msg string) error {
	return problem.ErrValidation.WithFields(problem.FieldError{Location: location, Message: msg})
}

// cleanName normalises an org name (05 §9.5) and checks 2–200 characters.
func cleanName(loc, s string) (string, error) {
	n, err := textx.NormalizeText(s)
	if err != nil {
		return "", fieldErr(loc, "contains control or bidi characters")
	}
	if c := utf8.RuneCountInString(n); c < 2 || c > 200 {
		return "", fieldErr(loc, "must be 2-200 characters")
	}
	return n, nil
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

// cleanDescription keeps line breaks but refuses other control and bidi
// characters; "" clears it.
func cleanDescription(loc, s string) (*string, error) {
	if !utf8.ValidString(s) {
		return nil, fieldErr(loc, "must be UTF-8")
	}
	for _, r := range s {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || bidiControl(r) {
			return nil, fieldErr(loc, "contains control or bidi characters")
		}
	}
	d := strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
	if utf8.RuneCountInString(d) > maxDescription {
		return nil, fieldErr(loc, "must be at most 5000 characters")
	}
	if d == "" {
		return nil, nil
	}
	return &d, nil
}

// bidiControl reports the bidirectional embedding, override and isolate
// characters (U+202A–U+202E, U+2066–U+2069; display spoofing).
func bidiControl(r rune) bool {
	return r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069
}

// cleanWebsite checks an https URL with a host; "" clears it.
func cleanWebsite(loc, s string) (*string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(s) > maxWebsite || !strings.HasPrefix(s, "https://") {
		return nil, fieldErr(loc, "must be an https:// URL")
	}
	return &s, nil
}

// cleanSlug checks a requested slug (02: ^[a-z0-9]+(-[a-z0-9]+)*$, ≤ 80).
func cleanSlug(loc, s string) (string, error) {
	if !slugRe.MatchString(s) || len(s) > maxSlug {
		return "", fieldErr(loc, "must be lower-case letters and digits separated by single hyphens, at most 80")
	}
	return s, nil
}

// cleanReason checks the free-text reason of an admin or owner action.
func cleanReason(loc, s string) (string, error) {
	r := strings.TrimSpace(s)
	switch n := utf8.RuneCountInString(r); {
	case n < minReason:
		return "", fieldErr(loc, "must be at least 3 characters")
	case n > maxReason:
		return "", fieldErr(loc, "must be at most 500 characters")
	}
	return r, nil
}

func cleanSector(loc, s string) (*string, error) {
	if s == "" {
		return nil, nil
	}
	if !sectorRe.MatchString(s) {
		return nil, fieldErr(loc, "must match ^[a-z_]{2,40}$")
	}
	return &s, nil
}

func cleanSizeBand(loc, s string) (*string, error) {
	if s == "" {
		return nil, nil
	}
	if !slices.Contains(sizeBands, s) {
		return nil, fieldErr(loc, "must be one of 1-10, 11-50, 51-200, 201-1000, 1000+")
	}
	return &s, nil
}

func cleanEmailDomain(loc, s string) (*string, error) {
	d := strings.ToLower(strings.TrimSpace(s))
	if d == "" {
		return nil, nil
	}
	if !emailDomainRe.MatchString(d) || len(d) > 253 {
		return nil, fieldErr(loc, "must be a domain name such as example.com")
	}
	return &d, nil
}

// auditEvent is the audit record of an org write by a.
func auditEvent(a authz.Actor, act authz.Action, orgID uuid.UUID, reason string, meta map[string]any) audit.Event {
	id := orgID
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &id, ResourceType: resourceOrg, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// emitStarted / emitEnded publish the membership events (01 §6.2,
// mechanism A: after the commit). A failure is logged, never returned:
// the business write stands (05 §8.1 A semantics).
func (s *service) emitStarted(ctx context.Context, m Membership) {
	s.emit(ctx, domainevent.MembershipStarted{
		MembershipID: m.ID, OrgID: m.OrgID, OrgKind: string(m.OrgKind), UserID: m.UserID, Role: string(m.Role),
	}, m.ID)
}

func (s *service) emitEnded(ctx context.Context, m Membership, reason EndReason) {
	s.emit(ctx, domainevent.MembershipEnded{
		MembershipID: m.ID, OrgID: m.OrgID, OrgKind: string(m.OrgKind), UserID: m.UserID, Role: string(m.Role),
		Reason: string(reason),
	}, m.ID)
}

func (s *service) emit(ctx context.Context, p domainevent.Payload, membershipID uuid.UUID) {
	env, err := domainevent.New(p, "org_member", membershipID)
	if err == nil {
		err = s.d.Outbox.AfterCommit(ctx, env)
	}
	if err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "membership event not emitted",
			slog.String("topic", p.Topic()), slog.String("membership_id", membershipID.String()), slog.String("err", err.Error()))
	}
}

// revokeAfterCommit closes userID's sessions with role_change once the
// transaction carried by ctx commits (see SessionRevoker).
func (s *service) revokeAfterCommit(ctx context.Context, userID uuid.UUID) {
	db.AfterCommit(ctx, func(ctx context.Context) {
		if s.d.RevokeSessions == nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "sessions not revoked after a role change: no revoker wired",
				slog.String("user_id", userID.String()))
			return
		}
		if _, err := s.d.RevokeSessions(ctx, userID); err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelError, "sessions not revoked after a role change",
				slog.String("user_id", userID.String()), slog.String("err", err.Error()))
		}
	})
}

// reauth verifies the proof of an R operation (03 §6.2) and returns its
// kind for the audit row. A refused proof is audited as denied (denied:
// reauthDenied); an error without a verdict (overload, a dependency down)
// writes nothing (audit.ReauthFailed, BE-074).
func (s *service) reauth(ctx context.Context, a authz.Actor, proof reauthProof, denied audit.Event) (string, error) {
	if s.d.Reauth == nil {
		return "", problem.ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		return "", audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied.By(a), err)
	}
	return kind, nil
}

// reauthDenied is the denied record of a refused proof of act on the org
// id: the org scope only when the org row was read (06 H-38; suspend and
// reactivate check the proof before reading). meta names the transition of
// a shared action, never business data: nothing happened.
func reauthDenied(act authz.Action, id uuid.UUID, read bool, meta map[string]any) audit.Event {
	e := audit.Event{Action: string(act), ResourceType: resourceOrg, ResourceID: &id, Metadata: meta}
	if read {
		e.OrgID = &id
	}
	return e
}
