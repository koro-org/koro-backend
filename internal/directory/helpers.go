package directory

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Field limits.
const (
	maxName      = 200
	maxShortName = 60
	maxCity      = 60
	maxURL       = 2048
	maxNote      = 500
	maxSocial    = 10
	maxYokCode   = 40
	maxSlug      = 80
)

var (
	socialKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,29}$`)
	yokCodeRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)
)

// catalogResource is the public directory lists (directory_catalog).
var catalogResource = authz.Resource{Type: authz.TypeDirectoryCatalog}

// adminSurface is the authz view of /admin/** (03 §3.2: 403 before reading).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// clubResource is the authz view of a club (03 §3.5: OrgID = the club's
// org; Attrs sensitive, university_id).
func clubResource(c Club) authz.Resource {
	id := c.OrgID
	return authz.Resource{Type: authz.TypeClub, ID: id, OrgID: &id, Attrs: map[string]string{
		authz.AttrSensitive: strconv.FormatBool(c.IsSensitive), "university_id": c.UniversityID.String(),
	}}
}

// universityResource is the authz view of a university (03 §3.5; Attr
// is_active: an inactive university is visible to platform admins only).
func universityResource(u University) authz.Resource {
	id := u.OrgID
	return authz.Resource{Type: authz.TypeUniversity, ID: u.ID, OrgID: &id, Attrs: map[string]string{
		"is_active": strconv.FormatBool(u.IsActive),
	}}
}

// cleanText normalises a single-line name (05 §9.5) and checks its length.
func cleanText(loc, s string, minRunes, maxRunes int) (string, error) {
	n, err := textx.NormalizeText(s)
	if err != nil {
		return "", problem.Field(loc, "contains control or bidi characters")
	}
	if c := utf8.RuneCountInString(n); c < minRunes || c > maxRunes {
		return "", problem.Field(loc, "must be "+strconv.Itoa(minRunes)+"-"+strconv.Itoa(maxRunes)+" characters")
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

// cleanQuery normalises a free-text query (04 §1.7: 2–100 characters).
func cleanQuery(loc, q string) (string, error) {
	if q == "" {
		return "", nil
	}
	return cleanText(loc, q, 2, 100)
}

// httpsURL checks an https URL with a host and no credentials.
func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && len(s) <= maxURL &&
		strings.HasPrefix(s, "https://")
}

// cleanWebsite checks an optional university website (02 §8.1: http or
// https); "" clears it.
func cleanWebsite(loc, s string) (*string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || len(s) > maxURL ||
		!websiteRe.MatchString(s) {
		return nil, problem.Field(loc, "must be an http(s):// URL")
	}
	return &s, nil
}

// cleanSocialLinks checks clubs.social_links (02 §8.5): at most 10 networks,
// each name lower-case ([a-z][a-z0-9_]{1,29}) and each URL https only.
func cleanSocialLinks(loc string, in map[string]string) (SocialLinks, error) {
	if len(in) > maxSocial {
		return nil, problem.Field(loc, "must have at most 10 links")
	}
	out := SocialLinks{}
	for k, v := range in {
		if !socialKeyRe.MatchString(k) {
			return nil, problem.Field(loc+"."+k, "network name must match ^[a-z][a-z0-9_]{1,29}$")
		}
		v = strings.TrimSpace(v)
		if !httpsURL(v) {
			return nil, problem.Field(loc+"."+k, "must be an https:// URL")
		}
		out[k] = v
	}
	return out, nil
}

// cleanYokCode checks a YÖK code (letters, digits, . _ -; ≤ 40).
func cleanYokCode(loc, s string) (string, error) {
	s = strings.TrimSpace(s)
	if !yokCodeRe.MatchString(s) {
		return "", problem.Field(loc, "must be 1-40 letters, digits, '.', '_' or '-'")
	}
	return s, nil
}

// normalizeDomain is a machine identifier (05 §9.5: never Turkish lower
// case): trimmed, ASCII lower case, without a trailing dot.
func normalizeDomain(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// domainCandidates returns d and every parent domain of it at a label
// boundary ("ogr.itu.edu.tr" → ogr.itu.edu.tr, itu.edu.tr, edu.tr, tr).
func domainCandidates(d string) []string {
	var out []string
	for d != "" {
		out = append(out, d)
		i := strings.IndexByte(d, '.')
		if i < 0 {
			break
		}
		d = d[i+1:]
	}
	return out
}

// auditEvent is the audit record of a directory write by a.
func auditEvent(a authz.Actor, act authz.Action, resType string, resID uuid.UUID, orgID *uuid.UUID, reason string, meta map[string]any) audit.Event {
	id := resID
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: orgID, ResourceType: resType, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// deref returns *p or the zero value.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// fieldLoc names the offending field of a conflict.
func fieldLoc(location string) problem.FieldError {
	return problem.FieldError{Location: location, Message: "belongs to another university"}
}
