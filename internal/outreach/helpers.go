package outreach

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/textx"
)

// Limits of a segment name (segments_name_chk).
const (
	minName = 2
	maxName = 100
)

// Audit actions of the segment writes (03 §4.1 format) and the audited
// resource type.
const (
	auditSegmentCreate  = "outreach.segment.create"
	auditSegmentUpdate  = "outreach.segment.update"
	auditSegmentArchive = "outreach.segment.archive"
	auditResource       = "segment"
)

// surface is the authz view of an org's segments and audience counts
// (/orgs/{org_id}/segments, /orgs/{org_id}/audience-counts; 03 §3.5
// segment: OrgID = the org, no Attrs). The org management surface: its
// active members and every platform role see it, everyone else gets 404.
func surface(orgID uuid.UUID) authz.Resource {
	id := orgID
	return authz.Resource{Type: authz.TypeSegment, ID: id, OrgID: &id}
}

// usableOrg loads orgID and applies the org status rule of 03 §2.3 to a
// write or a count on its behalf: a suspended org only through a platform
// admin, a company awaiting verification likewise, a closed org not at
// all. A missing org is 404. The codes are orgs' (06 H-52).
func (s *service) usableOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID) error {
	o, err := s.d.Orgs.Get(ctx, orgID)
	switch {
	case errors.Is(err, orgs.ErrNotFound):
		return ErrNotFound
	case err != nil:
		return err
	}
	pad := a.HasPlatformRole(authz.PlatformAdmin)
	switch o.Status {
	case orgs.StatusActive:
		return nil
	case orgs.StatusSuspended:
		if pad {
			return nil
		}
		return orgs.ErrOrgSuspended
	case orgs.StatusPendingReview:
		if pad {
			return nil
		}
		return orgs.ErrOrgNotVerified
	}
	return orgs.ErrInvalidState
}

// cleanName normalises a segment name (05 §9.5) and checks its length.
func cleanName(loc, s string) (string, error) {
	n, err := textx.NormalizeText(s)
	if err != nil {
		return "", fieldErr(loc, "contains control or bidi characters")
	}
	if c := utf8.RuneCountInString(n); c < minName || c > maxName {
		return "", fieldErr(loc, "must be "+strconv.Itoa(minName)+"-"+strconv.Itoa(maxName)+" characters")
	}
	return n, nil
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

// auditSegment is the audit record of a segment write by a. The metadata
// holds the shape of the targeting, never a count.
func auditSegment(a authz.Actor, action string, seg Segment, meta map[string]any) audit.Event {
	id, org := seg.ID, seg.OrgID
	return audit.Event{
		Action: action, Outcome: audit.Success, ActorOrgID: &org, OrgID: &org, ResourceType: auditResource,
		ResourceID: &id, Metadata: meta,
	}.By(a)
}

// view is the API view of a segment.
func view(seg Segment) SegmentView {
	return SegmentView{Segment: seg, Estimate: estimateOf(seg), ETag: etagOf(seg.UpdatedAt)}
}
