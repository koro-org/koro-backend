package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Field limits (02 §12.2–12.3).
const (
	maxTitle   = 140
	maxMedia   = 10
	maxAltText = 300
	maxCity    = 60
	maxURL     = 2048
)

// MediaInput is one image of PostWrite (04 §2.7: media[{file_id, alt_text?}]).
type MediaInput struct {
	FileID  uuid.UUID
	AltText string
}

// PostInput is PostWrite of createPost (04 §2.7) with audience_kind.
type PostInput struct {
	Kind           Kind
	Title          string
	Body           []byte // Tiptap JSON as sent; nil when absent
	EventID        *uuid.UUID
	Opportunity    *Opportunity
	Media          []MediaInput
	CommercialFlag bool
	AudienceKind   AudienceKind // public (default) or followers
}

// PostPatch is "PostWrite kısmi" of updatePost: a nil field stays as it is;
// Clear empties optional fields (title, body_json).
type PostPatch struct {
	Kind           *Kind
	Title          *string
	Body           []byte
	EventID        *uuid.UUID
	Opportunity    *Opportunity
	Media          *[]MediaInput
	CommercialFlag *bool
	AudienceKind   *AudienceKind
	Clear          []string
}

func (p PostPatch) empty() bool {
	return p.Kind == nil && p.Title == nil && p.Body == nil && p.EventID == nil && p.Opportunity == nil &&
		p.Media == nil && p.CommercialFlag == nil && p.AudienceKind == nil && len(p.Clear) == 0
}

// patch turns a create input into the patch applied to an empty draft.
func (in PostInput) patch() PostPatch {
	p := PostPatch{
		Kind: &in.Kind, Title: &in.Title, Body: in.Body, EventID: in.EventID, Opportunity: in.Opportunity,
		CommercialFlag: &in.CommercialFlag, AudienceKind: &in.AudienceKind,
	}
	media := in.Media
	p.Media = &media
	return p
}

// applied is a patched post with the media to write (nil: unchanged).
type applied struct {
	next  Post
	media []MediaInput // nil: the media are not touched
}

// apply validates p against cur (the stored post or an empty draft of o)
// and returns the next row. A published post keeps its kind, audience and
// commercial label (409 feed.published_immutable).
func (s *service) apply(ctx context.Context, a authz.Actor, o orgs.OrgSummary, cur Post, p PostPatch, current []Media) (applied, error) {
	next := cur.clone()
	if cur.Status != StatusDraft && cur.Status != StatusPendingReview && cur.ID != uuid.Nil() {
		if p.Kind != nil && *p.Kind != cur.Kind || p.AudienceKind != nil && *p.AudienceKind != cur.AudienceKind ||
			p.CommercialFlag != nil && *p.CommercialFlag != cur.CommercialFlag && cur.AuthorOrgKind != AuthorCompany {
			return applied{}, ErrPublishedImmutable
		}
	}
	for _, c := range p.Clear {
		switch c {
		case "title":
			next.Title = nil
		case "body_json":
			next.BodyJSON, next.BodyText = nil, nil
		default:
			return applied{}, fieldErr("body.clear", "lists a field that cannot be cleared")
		}
	}
	if p.Kind != nil {
		if !p.Kind.Valid() {
			return applied{}, fieldErr("body.kind", "must be text, image, event_share or opportunity_call")
		}
		next.Kind = *p.Kind
	}
	if p.AudienceKind != nil {
		switch *p.AudienceKind {
		case "", AudiencePublic:
			next.AudienceKind = AudiencePublic
		case AudienceFollowers:
			next.AudienceKind = AudienceFollowers
		default: // segment is createTargetedPost's (BE-032)
			return applied{}, fieldErr("body.audience_kind", "must be public or followers")
		}
	}
	if p.Title != nil {
		t, err := cleanTitle("body.title", *p.Title)
		if err != nil {
			return applied{}, err
		}
		next.Title = t
	}
	if p.Body != nil {
		body, text, err := sanitizeBody(p.Body)
		if err != nil {
			return applied{}, fieldErr("body.body_json", err.Error())
		}
		next.BodyJSON, next.BodyText = body, nil
		if text != "" {
			next.BodyText = &text
		}
	}
	if p.CommercialFlag != nil {
		next.CommercialFlag = *p.CommercialFlag
	}
	// 03 §5.7, posts_company_commercial_chk: a company post is commercial.
	if next.AuthorOrgKind == AuthorCompany {
		next.CommercialFlag = true
	}
	switch {
	case p.EventID != nil:
		next.EventID = p.EventID
	case p.Kind != nil && next.Kind != KindEventShare:
		next.EventID = nil
	}
	switch {
	case p.Opportunity != nil:
		op, err := cleanOpportunity(*p.Opportunity, next.AuthorOrgKind)
		if err != nil {
			return applied{}, err
		}
		next.Opportunity = op
	case p.Kind != nil && next.Kind != KindOpportunityCall:
		next.Opportunity = nil
	}
	next.AudienceKeys, next.AudienceDimCount = []string{KeyAll}, 1
	out := applied{next: next}
	media := len(current)
	if p.Media != nil {
		out.media = slices.Clone(*p.Media)
		if out.media == nil {
			out.media = []MediaInput{}
		}
		media = len(out.media)
		if err := s.checkMedia(ctx, a, o, out.media, next.Status == StatusPublished); err != nil {
			return applied{}, err
		}
	}
	if err := shape(next, media); err != nil {
		return applied{}, err
	}
	if next.EventID != nil && (cur.EventID == nil || *cur.EventID != *next.EventID) {
		if err := s.checkEvent(ctx, o, *next.EventID); err != nil {
			return applied{}, err
		}
	}
	return out, nil
}

// shape applies the rules of each kind (02 posts CHECKs plus the media
// rule of this ticket) with field errors instead of constraint names.
func shape(p Post, media int) error {
	hasBody := p.BodyText != nil
	switch p.Kind {
	case KindText:
		if !hasBody {
			return fieldErr("body.body_json", "a text post needs a body")
		}
	case KindImage:
		if media == 0 {
			return fieldErr("body.media", "an image post needs 1-10 images")
		}
	case KindEventShare:
		if p.EventID == nil {
			return fieldErr("body.event_id", "an event_share post needs an event")
		}
	case KindOpportunityCall:
		if p.Opportunity == nil {
			return fieldErr("body.opportunity", "an opportunity_call post needs an opportunity")
		}
		if !hasBody {
			return fieldErr("body.body_json", "an opportunity_call post needs a body")
		}
	}
	switch {
	case media > 0 && p.Kind != KindImage:
		return fieldErr("body.media", "images belong to image posts only")
	case media > maxMedia:
		return fieldErr("body.media", "at most 10 images")
	case p.EventID != nil && p.Kind != KindEventShare:
		return fieldErr("body.event_id", "applies to event_share posts only")
	case p.Opportunity != nil && p.Kind != KindOpportunityCall:
		return fieldErr("body.opportunity", "applies to opportunity_call posts only")
	case p.BodyJSON != nil && !hasBody:
		return fieldErr("body.body_json", "has no text")
	}
	return nil
}

// Opportunity enums (R-003 §1.6; 04 §F.1).
var (
	opportunityTypes = []string{"internship", "job", "part_time", "project", "volunteer", "talent_call"}
	applyVias        = []string{"contact_request", "external_url"}
	workModes        = []string{"onsite", "hybrid", "remote"}
)

// cleanOpportunity validates an opportunity and returns its canonical
// JSON. Applying with a contact request is a company's (messaging: the
// company asks, the student accepts).
func cleanOpportunity(o Opportunity, author AuthorKind) (JSON, error) {
	switch {
	case !slices.Contains(opportunityTypes, o.Type):
		return nil, fieldErr("body.opportunity.type", "must be internship, job, part_time, project, volunteer or talent_call")
	case !slices.Contains(applyVias, o.ApplyVia):
		return nil, fieldErr("body.opportunity.apply_via", "must be contact_request or external_url")
	case o.ApplyVia == "contact_request" && author != AuthorCompany:
		return nil, fieldErr("body.opportunity.apply_via", "contact_request is for company posts; use external_url")
	case o.WorkMode != "" && !slices.Contains(workModes, o.WorkMode):
		return nil, fieldErr("body.opportunity.work_mode", "must be onsite, hybrid or remote")
	case len(o.EligibleYears) > 8:
		return nil, fieldErr("body.opportunity.eligible_years", "at most 8 class years")
	}
	out := o
	out.ExternalURL = strings.TrimSpace(o.ExternalURL)
	switch {
	case o.ApplyVia == "external_url" && !httpsURL(out.ExternalURL):
		return nil, fieldErr("body.opportunity.external_url", "must be an https:// URL")
	case o.ApplyVia != "external_url" && out.ExternalURL != "":
		return nil, fieldErr("body.opportunity.external_url", "applies to external_url only")
	}
	if o.Deadline != "" {
		if _, err := time.Parse(time.DateOnly, o.Deadline); err != nil {
			return nil, fieldErr("body.opportunity.deadline", "must be a YYYY-MM-DD date")
		}
	}
	if o.City != "" {
		c, err := textx.NormalizeText(o.City)
		if err != nil || utf8.RuneCountInString(c) > maxCity {
			return nil, fieldErr("body.opportunity.city", "must be at most 60 characters of plain text")
		}
		out.City = c
	}
	years := slices.Clone(o.EligibleYears)
	slices.Sort(years)
	years = slices.Compact(years)
	for _, y := range years {
		if y < 1 || y > 8 {
			return nil, fieldErr("body.opportunity.eligible_years", "class years are 1-8")
		}
	}
	out.EligibleYears = years
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return JSON(b), nil
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && strings.HasPrefix(s, "https://") && u.Host != "" && u.User == nil && len(s) <= maxURL &&
		validText(s) && !strings.ContainsAny(s, " \t\r\n")
}

// decodeOpportunity reads a stored opportunity (nil when absent).
func decodeOpportunity(j JSON) *Opportunity {
	if j == nil {
		return nil
	}
	var o Opportunity
	if err := json.NewDecoder(bytes.NewReader(j)).Decode(&o); err != nil {
		return nil
	}
	return &o
}

// checkMedia checks the images of a post: post_media uploads of the
// author org (or of the caller), active, not rejected; distinct; alt text
// ≤ 300 characters. Images added to a published post must be clean
// already (04 §1.14).
func (s *service) checkMedia(ctx context.Context, a authz.Actor, o orgs.OrgSummary, media []MediaInput, needClean bool) error {
	if len(media) == 0 {
		return nil
	}
	if len(media) > maxMedia {
		return fieldErr("body.media", "at most 10 images")
	}
	if s.d.Files == nil {
		return problem.ErrUnavailable
	}
	seen := map[uuid.UUID]bool{}
	for i, m := range media {
		loc := "body.media[" + strconv.Itoa(i) + "]"
		if seen[m.FileID] {
			return fieldErr(loc+".file_id", "is listed twice")
		}
		seen[m.FileID] = true
		if m.AltText != "" {
			alt, err := textx.NormalizeText(m.AltText)
			if err != nil || utf8.RuneCountInString(alt) > maxAltText {
				return fieldErr(loc+".alt_text", "must be at most 300 characters of plain text")
			}
		}
		f, err := s.d.Files.Get(ctx, m.FileID)
		switch {
		case errors.Is(err, files.ErrNotFound):
			return fieldErr(loc+".file_id", "must be a post_media upload of this organization")
		case err != nil:
			return err
		}
		ownOrg := f.OrgID != nil && *f.OrgID == o.ID
		ownUpload := f.UploadedBy != nil && a.IsUser() && *f.UploadedBy == a.UserID
		if f.Purpose != files.PurposePostMedia || !ownOrg && !ownUpload {
			return fieldErr(loc+".file_id", "must be a post_media upload of this organization")
		}
		if f.Status == files.StatusRejected {
			return files.ErrRejected
		}
		if needClean && !clean(f) {
			return files.ErrNotClean
		}
	}
	return nil
}

// clean reports whether a file may be shown (files: active and scanned
// clean, or skipped without a real scanner; identity's rule).
func clean(f files.FileMeta) bool {
	return f.Status == files.StatusActive && (f.ScanStatus == files.ScanClean || f.ScanStatus == files.ScanSkipped)
}

// checkEvent is the event of an event_share post (events.Get): a
// published public event; a sensitive event (02 §11.1) only by its own
// org, so a sensitive club's event is never spread by others.
func (s *service) checkEvent(ctx context.Context, o orgs.OrgSummary, id uuid.UUID) error {
	if s.d.Events == nil {
		return problem.ErrUnavailable
	}
	e, err := s.d.Events.Get(ctx, id)
	switch {
	case errors.Is(err, events.ErrNotFound):
		return fieldErr("body.event_id", "must be a published public event")
	case err != nil:
		return err
	}
	if e.Status != events.StatusPublished || e.Visibility != events.VisibilityPublic {
		return fieldErr("body.event_id", "must be a published public event")
	}
	if e.Sensitive && e.OrgID != o.ID {
		return fieldErr("body.event_id", "must be a published public event")
	}
	return nil
}

// mediaRows turns the checked inputs into rows (positions in order).
func mediaRows(in []MediaInput) []Media {
	out := make([]Media, len(in))
	for i, m := range in {
		out[i] = Media{ID: uuid.NewV7(), FileID: m.FileID, Position: int16(i)}
		if alt, err := textx.NormalizeText(m.AltText); err == nil && alt != "" {
			out[i].AltText = &alt
		}
	}
	return out
}
