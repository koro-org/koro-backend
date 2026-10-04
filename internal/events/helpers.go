package events

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Field limits (02 §11.1).
const (
	minTitle        = 3
	maxTitle        = 160
	maxSlug         = 100
	maxSummary      = 300
	maxLocation     = 200
	maxAddress      = 500
	maxCity         = 60
	maxURL          = 2048
	maxCancelReason = 500
	minCancelReason = 3
	maxCapacity     = 100000
	slugAttempts    = 5
	coverTTL        = 5 * time.Minute
)

// catalogResource is the public event list (event_catalog).
var catalogResource = authz.Resource{Type: authz.TypeEventCatalog}

// orgResource is the authz view of the organiser's management surface
// (/orgs/{org_id}/events; 03 §3.5 org: Attrs org_kind, org_status).
func orgResource(o orgs.OrgSummary) authz.Resource {
	id := o.ID
	return authz.Resource{Type: authz.TypeOrg, ID: id, OrgID: &id, Attrs: map[string]string{
		"org_kind": string(o.Kind), "org_status": string(o.Status),
	}}
}

// eventResource is the authz view of an event (03 §3.5: OrgID = the
// organiser; Attrs status, visibility, visibility_university_id, sensitive,
// event_staff_role of the caller). The sensitive flag is never logged.
func eventResource(e Event, sensitive bool, staff StaffRole) authz.Resource {
	org := e.OrgID
	uni := ""
	if e.VisibilityUniversityID != nil {
		uni = e.VisibilityUniversityID.String()
	}
	role := "none"
	if staff != "" {
		role = string(staff)
	}
	return authz.Resource{Type: authz.TypeEvent, ID: e.ID, OrgID: &org, Attrs: map[string]string{
		authz.AttrStatus: string(e.Status), "visibility": string(e.Visibility), "visibility_university_id": uni,
		authz.AttrSensitive: strconv.FormatBool(sensitive), authz.AttrEventStaffRole: role,
	}}
}

// sensitive is 02 §11.1: is_sensitive_category OR the organiser is a
// sensitive club (directory).
func (s *service) sensitive(ctx context.Context, e Event) (bool, error) {
	if e.IsSensitiveCategory {
		return true, nil
	}
	m, err := s.d.Catalog.SensitiveOrgIDs(ctx, []uuid.UUID{e.OrgID})
	if err != nil {
		return false, err
	}
	return m[e.OrgID], nil
}

// staffRole is the caller's staff role on e ("" for none or anonymous).
func (s *service) staffRole(ctx context.Context, a authz.Actor, e Event) (StaffRole, error) {
	if !a.IsUser() {
		return "", nil
	}
	r, ok, err := s.d.Store.StaffRole(ctx, e.OrgID, e.ID, a.UserID)
	if err != nil || !ok {
		return "", err
	}
	return r, nil
}

// authorize builds the event's resource for a (its form sub-surface for
// the form actions) and runs authz.Can.
func (s *service) authorize(ctx context.Context, a authz.Actor, act authz.Action, e Event) error {
	res, err := s.resource(ctx, a, e)
	if err != nil {
		return err
	}
	if formActions[act] {
		res = formResource(res)
	}
	return authz.Can(ctx, a, act, res)
}

// formActions address the form of an event (event_form, 03 §3.5).
var formActions = map[authz.Action]bool{
	authz.EventsFormRead: true, authz.EventsFormManage: true, authz.EventsFormPublish: true,
}

// formResource is the event_form view of an event's resource: the same
// organiser and Attrs plus event_status (03 §3.5).
func formResource(r authz.Resource) authz.Resource {
	attrs := maps.Clone(r.Attrs)
	attrs["event_status"] = attrs[authz.AttrStatus]
	r.Type, r.Attrs = authz.TypeEventForm, attrs
	return r
}

// org returns the organiser's summary; a missing org is 404.
func (s *service) org(ctx context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	o, err := s.d.Orgs.Get(ctx, id)
	if errors.Is(err, orgs.ErrNotFound) {
		return orgs.OrgSummary{}, ErrNotFound
	}
	return o, err
}

// writable is the org status rule of writes on behalf of the organiser
// (03 §2.3): a suspended org only through a platform admin, a closed (or
// unverified) org not at all. The codes are orgs' (06 H-52).
func writable(a authz.Actor, o orgs.OrgSummary) error {
	switch o.Status {
	case orgs.StatusActive:
		return nil
	case orgs.StatusSuspended:
		if a.HasPlatformRole(authz.PlatformAdmin) {
			return nil
		}
		return orgs.ErrOrgSuspended
	}
	return orgs.ErrInvalidState
}

// organizerKinds may organise events (03 §5.6 k13, T-17: companies are
// closed in v1; ngo is phase 2).
var organizerKinds = map[orgs.Kind]bool{orgs.KindClub: true, orgs.KindUniversity: true, orgs.KindPlatform: true}

// etagOf is the strong ETag of a version (04 §1.13): the base32 of
// updated_at's UnixNano, quoted.
func etagOf(updatedAt time.Time) string {
	return `"` + strconv.FormatInt(updatedAt.UnixNano(), 32) + `"`
}

// checkIfMatch applies If-Match (04 §1.13): missing or "*" → 428, no listed
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

// cleanText normalises a single-line text (05 §9.5) and checks its length.
func cleanText(loc, s string, minRunes, maxRunes int) (string, error) {
	n, err := textx.NormalizeText(s)
	if err != nil {
		return "", fieldErr(loc, "contains control or bidi characters")
	}
	if c := utf8.RuneCountInString(n); c < minRunes || c > maxRunes {
		return "", fieldErr(loc, "must be "+strconv.Itoa(minRunes)+"-"+strconv.Itoa(maxRunes)+" characters")
	}
	return n, nil
}

// cleanOptional normalises an optional single-line field; "" clears it.
func cleanOptional(loc, s string, maxRunes int) (*string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	n, err := cleanText(loc, s, 1, maxRunes)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// cleanSlug checks a requested slug (events_slug_chk).
func cleanSlug(loc, s string) (string, error) {
	if !slugRe.MatchString(s) || len(s) > maxSlug {
		return "", fieldErr(loc, "must be lower-case letters and digits separated by single hyphens, at most 100")
	}
	return s, nil
}

// cleanOnlineURL checks an online link (02 §0.4: https only, no
// credentials); "" clears it.
func cleanOnlineURL(loc, s string) (*string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(s) > maxURL ||
		!strings.HasPrefix(s, "https://") {
		return nil, fieldErr(loc, "must be an https:// URL")
	}
	return &s, nil
}

// cleanQuery normalises a free-text query (04 §1.7: 2–100 characters).
func cleanQuery(loc, q string) (string, error) {
	if q == "" {
		return "", nil
	}
	return cleanText(loc, q, 2, 100)
}

// auditEvent is the audit record of an events write by a. The sensitive
// flag of an event is never written (03 §3.5: authz only).
func auditEvent(a authz.Actor, act authz.Action, resType string, resID, orgID uuid.UUID, reason string, meta map[string]any) audit.Event {
	id, org := resID, orgID
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: resType, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// lastInitial is the initial of a last name followed by a dot ("Yılmaz" →
// "Y."): staff lists show first name and initial only (04 §E.4, 02 §11.2).
func lastInitial(s string) string {
	s = strings.TrimSpace(s)
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || r == utf8.RuneError {
		return ""
	}
	return textx.UpperTR(string(r)) + "."
}

func ptr[T any](v T) *T { return &v }

// textSlug is the slug derived from a title ("event" when nothing is left).
func textSlug(title string) string {
	if b := textx.Slug(title, maxSlug); b != "" {
		return b
	}
	return "event"
}

// trimSlug cuts a slug to n bytes without a trailing hyphen.
func trimSlug(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimRight(s[:n], "-")
}
