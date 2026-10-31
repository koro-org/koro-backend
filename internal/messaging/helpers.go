package messaging

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
	"uuid"

	"golang.org/x/text/unicode/norm"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Limits of 02 §14.1 (char_length CHECKs).
const (
	minSubject, maxSubject = 3, 120
	minMessage, maxMessage = 10, 1000
	// maxAnonID bounds the pseudonym a request names (16 characters; a
	// longer one resolves to nobody anyway).
	maxAnonID = 64
)

// cleanSubject normalises a one-line subject (NFC, trimmed; no control or
// bidi characters).
func cleanSubject(s string) (string, error) {
	n, err := textx.NormalizeText(s)
	if err != nil {
		return "", problem.Field("body.subject", "contains control or bidi characters")
	}
	if c := utf8.RuneCountInString(n); c < minSubject || c > maxSubject {
		return "", problem.Field("body.subject", "must be "+strconv.Itoa(minSubject)+" to "+strconv.Itoa(maxSubject)+" characters")
	}
	return n, nil
}

// cleanMessage normalises the message of a contact request.
func cleanMessage(s string) (string, error) {
	return cleanText("body.message", s, minMessage, maxMessage)
}

// cleanText normalises a multi-line free text (a request's message, a
// conversation message): line breaks and tabs stay, other control and bidi
// characters are refused; NFC, trimmed, min to max characters (the
// char_length CHECK of its column).
func cleanText(field, s string, minLen, maxLen int) (string, error) {
	if !utf8.ValidString(s) {
		return "", problem.Field(field, "must be UTF-8")
	}
	for _, r := range s {
		if (unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r') || bidi(r) {
			return "", problem.Field(field, "contains control or bidi characters")
		}
	}
	n := norm.NFC.String(strings.TrimSpace(strings.ReplaceAll(s, "\r", "")))
	if c := utf8.RuneCountInString(n); c < minLen || c > maxLen {
		return "", problem.Field(field, "must be "+strconv.Itoa(minLen)+" to "+strconv.Itoa(maxLen)+" characters")
	}
	return n, nil
}

// bidi reports the Unicode bidirectional control characters (Trojan
// Source; the same set textx refuses in one-line text).
func bidi(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F || r == 0x061C
}

// cleanCreate validates createContactRequest (422 naming the member).
func cleanCreate(in CreateInput) (CreateInput, error) {
	var err error
	in.AnonID = strings.TrimSpace(in.AnonID)
	if in.AnonID == "" || len(in.AnonID) > maxAnonID {
		return in, problem.Field("body.anon_id", "is required (the company's pseudonym of the student)")
	}
	if in.Subject, err = cleanSubject(in.Subject); err != nil {
		return in, err
	}
	if in.Message, err = cleanMessage(in.Message); err != nil {
		return in, err
	}
	if in.OpportunityPostID != nil && *in.OpportunityPostID == uuid.Nil() {
		return in, problem.Field("body.opportunity_post_id", "must be a post ID")
	}
	return in, nil
}

// companySurface is the authz view of the company path before a request is
// read: OrgID = the path's company; the owner is not known yet (H-27: the
// key is present, nil UUID).
func companySurface(orgID uuid.UUID) authz.Resource {
	org, nobody := orgID, uuid.Nil()
	return authz.Resource{Type: authz.TypeContactRequest, OrgID: &org, OwnerUserID: &nobody,
		Attrs: map[string]string{authz.AttrStatus: ""}}
}

// companyResource is the authz view of a request on the company path: the
// request's company and student (03 §3.5; H-27).
func companyResource(r ContactRequest, st Status) authz.Resource {
	org, student := r.OrgID, r.StudentUserID
	return authz.Resource{Type: authz.TypeContactRequest, ID: r.ID, OrgID: &org, OwnerUserID: &student,
		Attrs: map[string]string{authz.AttrStatus: string(st)}}
}

// ownSurface is the authz view of the caller's own requests (/me: no org).
func ownSurface(a authz.Actor) authz.Resource {
	me := a.UserID
	return authz.Resource{Type: authz.TypeContactRequest, OwnerUserID: &me, Attrs: map[string]string{authz.AttrStatus: ""}}
}

// ownResource is the authz view of one of the caller's requests.
func ownResource(r ContactRequest, st Status) authz.Resource {
	student := r.StudentUserID
	return authz.Resource{Type: authz.TypeContactRequest, ID: r.ID, OwnerUserID: &student,
		Attrs: map[string]string{authz.AttrStatus: string(st)}}
}

// ownBlocks is the authz view of the caller's blocks.
func ownBlocks(a authz.Actor) authz.Resource {
	me := a.UserID
	return authz.Resource{Type: authz.TypeContactBlock, OwnerUserID: &me}
}

// company reads the org of a company path: an unknown org or one that is not
// a company is not found. write: the org acts (03 §2.3: a suspended org may
// not write; a company not yet verified may not send).
func (s *service) company(ctx context.Context, orgID uuid.UUID, send, write bool) (orgs.OrgSummary, error) {
	o, err := s.d.Orgs.Get(ctx, orgID)
	switch {
	case errors.Is(err, orgs.ErrNotFound):
		return orgs.OrgSummary{}, ErrNotFound
	case err != nil:
		return orgs.OrgSummary{}, err
	case o.Kind != orgs.KindCompany:
		return orgs.OrgSummary{}, ErrNotFound
	}
	switch o.Status {
	case orgs.StatusActive:
	case orgs.StatusSuspended:
		if write {
			return o, orgs.ErrOrgSuspended
		}
	case orgs.StatusPendingReview:
		if send {
			return o, orgs.ErrOrgNotVerified
		}
	default: // closed
		if write {
			return o, ErrNotFound
		}
	}
	return o, nil
}

// requestEvent is the audit record of a request change. org is the scope
// of audit.event.list_org: the company for its own actions, nil for the
// student's (the company never sees who declined nor who blocked it). The
// student is named only in the metadata (admin view).
func requestEvent(action authz.Action, r ContactRequest, org *uuid.UUID, meta map[string]any) audit.Event {
	id := r.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta[audit.MetaSubjectUserID] = r.StudentUserID.String()
	if org == nil {
		meta["company_org_id"] = r.OrgID.String()
	}
	return audit.Event{Action: string(action), Outcome: audit.Success, OrgID: org, ResourceType: authz.TypeContactRequest,
		ResourceID: &id, Metadata: meta}
}
