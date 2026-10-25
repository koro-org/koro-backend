package feed

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// postAggType is the aggregate type of the posts' domain events and audit
// records.
const postAggType = "post"

// feedResource is the reader's feed (getFeed): no org, no owner.
var feedResource = authz.Resource{Type: authz.TypeFeed}

// adminSurface is the platform management surface (/admin/**, 03 §3.6):
// visible to every user, so an actor without a platform role gets 403
// before anything is read (BE-032's moderation endpoints).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// reportAggType is the aggregate type of content_report.created and of
// the report audit records.
const reportAggType = "content_report"

// moderatedPost is the authz view of a post a moderator decides on: the
// post resource with its author user as owner, so the policy refuses a
// moderator who wrote the post or writes for its org (BE-032, 03 Y6's
// rule of not deciding on one's own work).
func moderatedPost(p Post) authz.Resource {
	r := postResource(p, true, true)
	author := p.AuthorUserID
	r.OwnerUserID = &author
	return r
}

// moderatedOrg is the authz view of an org a moderator decides on (03 k17:
// only a platform admin suspends an org or changes a suspended one; nobody
// moderates an org they belong to).
func moderatedOrg(o orgs.OrgSummary, act ModAction, trust TrustLevel) authz.Resource {
	r := authz.OrgResource(o.ID, string(o.Kind), string(o.Status))
	r.Attrs["moderation_action"] = string(act)
	r.Attrs["trust_level"] = string(trust)
	return r
}

// reportResource is the authz view of a report a moderator resolves: the
// reporter as owner (nobody resolves their own report).
func reportResource(r Report) authz.Resource {
	reporter := r.ReporterUserID
	return authz.Resource{Type: authz.TypeContentReport, ID: r.ID, OwnerUserID: &reporter}
}

// postResource is the authz view of a post (03 §3.5: OrgID = the author
// org; Attrs status, audience_kind, audience_match, commercial and
// follows_author of the caller).
func postResource(p Post, match, follows bool) authz.Resource {
	org := p.OrgID
	return authz.Resource{Type: authz.TypePost, ID: p.ID, OrgID: &org, Attrs: map[string]string{
		authz.AttrStatus: string(p.Status), "audience_kind": string(p.AudienceKind),
		"audience_match": strconv.FormatBool(match), "commercial": strconv.FormatBool(p.CommercialFlag),
		"follows_author": strconv.FormatBool(follows),
	}}
}

// audienceMatch is the matching rule of 02 §9.1: a post is the reader's
// when every dimension of its keys ('all', uni, dept, year, club) has a key
// the reader holds (OR within a dimension, AND across dimensions).
func audienceMatch(post []string, dims int16, reader []string) bool {
	matched := map[string]bool{}
	for _, k := range post {
		if slices.Contains(reader, k) {
			dim, _, _ := strings.Cut(k, ":")
			matched[dim] = true
		}
	}
	return len(matched) > 0 && len(matched) == int(dims)
}

// readerOf is the Reader of a: anonymous, or the signed-in reader with the
// keys of students.ViewerKeys ('all' when students is not wired) and the
// active memberships.
func (s *service) readerOf(ctx context.Context, a authz.Actor) (Reader, error) {
	if !a.IsUser() {
		return Reader{Keys: []string{KeyAll}}, nil
	}
	uid := a.UserID
	r := Reader{UserID: &uid, Keys: []string{KeyAll}, Minor: a.IsMinor}
	if s.d.Viewers != nil {
		vk, err := s.d.Viewers.ViewerKeys(ctx, a.UserID)
		if err != nil {
			return Reader{}, err
		}
		if len(vk.Keys) > 0 {
			r.Keys = vk.Keys
		}
		if !slices.Contains(r.Keys, KeyAll) {
			r.Keys = append(r.Keys, KeyAll)
		}
	}
	for _, m := range a.Memberships {
		if !slices.Contains(r.MemberOrgs, m.OrgID) {
			r.MemberOrgs = append(r.MemberOrgs, m.OrgID)
		}
	}
	return r, nil
}

// writerRoles are the org roles that write an org's posts (03 §5.7 S cells;
// career_staff: see the package documentation).
var writerRoles = []authz.MemberRole{
	authz.MemberClubAdmin, authz.MemberUniversityRep, authz.MemberCareerStaff, authz.MemberRecruiter,
	authz.MemberCompanyAdmin, authz.MemberCompanyOwner,
}

// staffReader reports whether a reads every post of orgID: a writer of the
// org, the platform org's platform admin, a moderator or a platform admin.
func staffReader(a authz.Actor, orgID uuid.UUID) bool {
	return a.HasOrgRole(orgID, writerRoles...) || a.HasPlatformRole(authz.PlatformModerator) ||
		a.HasPlatformRole(authz.PlatformAdmin)
}

// org returns the org's summary; a missing org is 404.
func (s *service) org(ctx context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	o, err := s.d.Orgs.Get(ctx, id)
	if errors.Is(err, orgs.ErrNotFound) {
		return orgs.OrgSummary{}, ErrNotFound
	}
	return o, err
}

// writable is the org status rule of writes on behalf of the author org
// (03 §2.3): a suspended org only through a platform admin, an unverified
// company not until it is verified, a closed org not at all. The codes are
// orgs' (06 H-52).
func writable(a authz.Actor, o orgs.OrgSummary) error {
	switch o.Status {
	case orgs.StatusActive:
		return nil
	case orgs.StatusSuspended:
		if a.HasPlatformRole(authz.PlatformAdmin) {
			return nil
		}
		return orgs.ErrOrgSuspended
	case orgs.StatusPendingReview:
		return orgs.ErrOrgNotVerified
	}
	return orgs.ErrInvalidState
}

// authorKinds maps the org kinds that post (posts_author_org_kind_chk; ngo
// is phase 2 and gets no grant).
var authorKinds = map[orgs.Kind]AuthorKind{
	orgs.KindPlatform: AuthorPlatform, orgs.KindUniversity: AuthorUniversity, orgs.KindClub: AuthorClub,
	orgs.KindCompany: AuthorCompany,
}

// cleanTitle normalises an optional single-line title (05 §9.5; ≤ 140
// characters, 02 §0.4); "" clears it.
func cleanTitle(loc, s string) (*string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	n, err := textx.NormalizeText(s)
	if err != nil {
		return nil, problem.Field(loc, "contains control or bidi characters")
	}
	if c := utf8.RuneCountInString(n); c < 1 || c > maxTitle {
		return nil, problem.Field(loc, "must be at most 140 characters")
	}
	return &n, nil
}

// auditEvent is the audit record of a feed write by a.
func auditEvent(a authz.Actor, act authz.Action, p Post, meta map[string]any) audit.Event {
	id, org := p.ID, p.OrgID
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: postAggType, ResourceID: &id,
		Metadata: meta,
	}.By(a)
}
