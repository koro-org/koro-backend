package outreach

import (
	"context"
	"net/http"
	"strconv"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// Register adds the 6 segment and audience count operations of BE-030
// and the 4 dispatch operations of BE-035 (04 §2.8) to api. svc may be nil
// (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	op.Register(api, "outreach", huma.Operation{
		OperationID: "listSegments", Method: http.MethodGet, Path: "/orgs/{org_id}/segments",
		Summary: "List an organization's segments",
		Description: "Newest first. archived=true lists the archived segments, otherwise the active ones. The count " +
			"is the stored estimate (refreshed daily at 06:00): null below 10 students.",
	}, op.Meta{Action: string(authz.OutreachSegmentList), Surface: op.SurfaceOrg}, h.list)

	op.Register(api, "outreach", huma.Operation{
		OperationID: "createSegment", Method: http.MethodPost, Path: "/orgs/{org_id}/segments", DefaultStatus: http.StatusCreated,
		Summary: "Create a segment",
		Description: "OR inside a dimension, AND across dimensions; one to four dimensions (a department already " +
			"implies its university). Scope (03 k16): a club targets only its own members (club = [the club]); a " +
			"university its own university (uni = [it]), narrowed by dept or year; a company every dimension but " +
			"never a sensitive club (422 outreach.sensitive_target) nor an event that is not targetable (422 " +
			"outreach.event_not_targetable; BE-161: the event dimension counts only students whose commercial " +
			"message consent is in force). Out of scope → 422 outreach.target_out_of_scope. The estimate is null " +
			"below 10 students and rounded down otherwise; counting spends the org's audience count budget.",
	}, op.Meta{
		Action: string(authz.OutreachSegmentManage), Surface: op.SurfaceOrg, RateLimit: op.RateAudienceCount,
		Idempotency: op.IdempotencyOptional,
	}, h.create)

	op.Register(api, "outreach", huma.Operation{
		OperationID: "getSegment", Method: http.MethodGet, Path: "/orgs/{org_id}/segments/{segment_id}",
		Summary: "Get a segment", Description: "The ETag is the If-Match value of updateSegment.",
	}, op.Meta{Action: string(authz.OutreachSegmentRead), Surface: op.SurfaceOrg}, h.get)

	op.Register(api, "outreach", huma.Operation{
		OperationID: "updateSegment", Method: http.MethodPatch, Path: "/orgs/{org_id}/segments/{segment_id}",
		Summary: "Update a segment",
		Description: "If-Match with the ETag of getSegment is required: missing → 428, stale → 412. A new definition " +
			"follows the rules of createSegment and is counted again. An archived segment cannot change (409 " +
			"outreach.invalid_state).",
	}, op.Meta{Action: string(authz.OutreachSegmentManage), Surface: op.SurfaceOrg, RateLimit: op.RateAudienceCount}, h.update)

	op.Register(api, "outreach", huma.Operation{
		OperationID: "archiveSegment", Method: http.MethodPost, Path: "/orgs/{org_id}/segments/{segment_id}/archive",
		Summary: "Archive a segment", Description: "Archiving an archived segment changes nothing.",
	}, op.Meta{Action: string(authz.OutreachSegmentManage), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.archive)

	op.Register(api, "outreach", huma.Operation{
		OperationID: "countAudience", Method: http.MethodPost, Path: "/orgs/{org_id}/audience-counts",
		Summary: "Estimate an audience",
		Description: "Level 0 (04 §V.2): a number only, never an identity or a sample. Below 10 students count is " +
			"null and below_threshold true; from 10 on the count is rounded down (to 10 below 100, 50 below 1000, " +
			"100 below 10000, then 1000). Only audience eligible students count (verified, adult, active). The " +
			"scope rules of createSegment apply. Each count spends the org's daily audience count budget (429 " +
			"rate_limited when spent).",
	}, op.Meta{
		Action: string(authz.OutreachAudienceCount), Surface: op.SurfaceOrg, RateLimit: op.RateAudienceCount,
		Idempotency: op.IdempotencyOptional,
	}, h.count)

	registerDispatch(api, h)
}

type handler struct{ svc Service }

// Cursor sort key (04 §1.6).
const sortNewest = "-id"

// OrgPath is the org_id path parameter.
type OrgPath struct {
	OrgID string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
}

// SegmentPath is the org_id and segment_id path parameters.
type SegmentPath struct {
	OrgID     string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	SegmentID string `path:"segment_id" doc:"Segment ID (an unknown, malformed or other org's ID is 404)"`
}

// ---- DTOs

// SegmentDefinitionDTO is a targeting (04 §2.8): OR inside a dimension, AND across
// dimensions; one to four dimensions.
type SegmentDefinitionDTO struct {
	Uni           []string `json:"uni,omitempty" maxItems:"100" doc:"University IDs (universities)"`
	Dept          []string `json:"dept,omitempty" maxItems:"100" doc:"Department IDs"`
	Year          []int    `json:"year,omitempty" maxItems:"8" doc:"Class years (1-8)"`
	Club          []string `json:"club,omitempty" maxItems:"100" doc:"Club org IDs (never a sensitive club, except a club's own members)"`
	Event         []string `json:"event,omitempty" maxItems:"100" doc:"Event IDs (BE-161; companies and the platform only): the students who registered for one of the events (a confirmed, checked-in or no-show registration). Only events whose targetable flag is true (listEvents, getEvent: a published or completed public event, neither of a sensitive category nor organised by a sensitive club, that started at most 365 days ago or has yet to start); any other is 422 outreach.event_not_targetable. Only students whose commercial message consent is in force are counted or reached through this dimension."`
	EventAttended bool     `json:"event_attended,omitempty" doc:"With event: only the students checked in at one of the events (an event without check-in has none)"`
}

// SegmentDTO is 04 §2.8 SegmentDTO.
type SegmentDTO struct {
	SegmentID     string                    `json:"segment_id" format:"uuid"`
	Name          string                    `json:"name"`
	Definition    SegmentDefinitionDTO      `json:"definition"`
	Dimensions    int                       `json:"dimensions" doc:"Number of dimensions of the definition (1-4)"`
	Count         students.AudienceCountDTO `json:"count" doc:"Level 0 estimate (04 §V.2): rounded down, null below 10 students"`
	LastCountedAt *time.Time                `json:"last_counted_at,omitempty"`
	ArchivedAt    *time.Time                `json:"archived_at,omitempty"`
	CreatedAt     time.Time                 `json:"created_at"`
	UpdatedAt     time.Time                 `json:"updated_at"`
}

// SegmentList is the body of listSegments.
type SegmentList struct {
	Items      []SegmentDTO `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// AudienceCountResultDTO is the body of countAudience (04 §2.8): the level
// 0 estimate of students (04 §V.2) and the definition's dimension count.
type AudienceCountResultDTO struct {
	students.AudienceCountDTO
	Dimensions int `json:"dimensions" doc:"Number of dimensions of the definition (1-4)"`
}

func ids(in []uuid.UUID) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, id := range in {
		out[i] = id.String()
	}
	return out
}

func toSegmentDefinitionDTO(d Definition) SegmentDefinitionDTO {
	out := SegmentDefinitionDTO{Uni: ids(d.Uni), Dept: ids(d.Dept), Club: ids(d.Club), Event: ids(d.Event),
		EventAttended: d.EventAttended}
	for _, y := range d.Year {
		out.Year = append(out.Year, int(y))
	}
	return out
}

// toCountDTO is the level 0 DTO of an estimate: the bucket, or null below
// 10 (students owns the level DTOs, 04 §V; the rounding is outreach's).
func toCountDTO(e Estimate) students.AudienceCountDTO {
	return students.AudienceCountDTO{Count: e.Count, BelowThreshold: e.BelowThreshold}
}

func toSegmentDTO(v SegmentView) SegmentDTO {
	return SegmentDTO{
		SegmentID: v.ID.String(), Name: v.Name, Definition: toSegmentDefinitionDTO(v.Definition),
		Dimensions: int(v.Definition.DimCount()), Count: toCountDTO(v.Estimate), LastCountedAt: utc(v.LastCountedAt),
		ArchivedAt: utc(v.ArchivedAt), CreatedAt: v.CreatedAt.UTC(), UpdatedAt: v.UpdatedAt.UTC(),
	}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// Definition parses in at loc (the segment operations here, the inline
// audience of the feed's createTargetedPost): a malformed ID or year is 422
// at its location; the service checks the rest (normalize, scope).
func (in SegmentDefinitionDTO) Definition(loc string) (Definition, error) {
	parse := func(dim string, raw []string) ([]uuid.UUID, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		out := make([]uuid.UUID, len(raw))
		for i, s := range raw {
			id, err := uuid.Parse(s)
			if err != nil {
				return nil, problem.Field(loc+"."+dim+"["+strconv.Itoa(i)+"]", "must be a UUID")
			}
			out[i] = id
		}
		return out, nil
	}
	var (
		d   Definition
		err error
	)
	if d.Uni, err = parse("uni", in.Uni); err != nil {
		return Definition{}, err
	}
	if d.Dept, err = parse("dept", in.Dept); err != nil {
		return Definition{}, err
	}
	if d.Club, err = parse("club", in.Club); err != nil {
		return Definition{}, err
	}
	if d.Event, err = parse("event", in.Event); err != nil {
		return Definition{}, err
	}
	d.EventAttended = in.EventAttended
	for i, y := range in.Year {
		if y < minYear || y > maxYear {
			return Definition{}, problem.Field(loc+".year["+strconv.Itoa(i)+"]", "class years are 1-8")
		}
		d.Year = append(d.Year, int16(y))
	}
	return d, nil
}

type segmentOutput struct {
	ETag string `header:"ETag"`
	Body SegmentDTO
}

func segmentOut(v SegmentView) *segmentOutput {
	return &segmentOutput{ETag: v.ETag, Body: toSegmentDTO(v)}
}

// ---- listSegments

type listSegmentsInput struct {
	OrgPath
	page.Params
	Archived bool `query:"archived" doc:"true: archived segments only; false (default): active segments"`
}

type listOutput struct{ Body SegmentList }

func (h handler) list(ctx context.Context, in *listSegmentsInput) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	orgID := httpx.ParseID(in.OrgID)
	fp := page.Fingerprint("listSegments", orgID.String(), strconv.FormatBool(in.Archived))
	var after *uuid.UUID
	c, err := page.Decode(in.Cursor, sortNewest, fp, 1)
	if err != nil {
		return nil, err
	}
	if c != nil {
		id, err := c.UUID(0)
		if err != nil {
			return nil, err
		}
		after = &id
	}
	p, err := h.svc.ListSegments(ctx, a, orgID, ListQuery{Archived: in.Archived, Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &listOutput{Body: SegmentList{Items: make([]SegmentDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toSegmentDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortNewest, fp, page.UUIDVal(*p.Next))
	}
	return out, nil
}

// ---- createSegment

type createSegmentInput struct {
	idempotency.OptionalHeader
	OrgPath
	Body struct {
		Name       string               `json:"name" minLength:"2" maxLength:"100" doc:"Segment name (2-100 characters)"`
		Definition SegmentDefinitionDTO `json:"definition"`
	}
}

func (h handler) create(ctx context.Context, in *createSegmentInput) (*segmentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	def, err := in.Body.Definition.Definition("body.definition")
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CreateSegment(ctx, a, httpx.ParseID(in.OrgID), SegmentInput{Name: in.Body.Name, Definition: def})
	if err != nil {
		return nil, err
	}
	return segmentOut(v), nil
}

// ---- getSegment

type getSegmentInput struct {
	SegmentPath
}

func (h handler) get(ctx context.Context, in *getSegmentInput) (*segmentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetSegment(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.SegmentID))
	if err != nil {
		return nil, err
	}
	return segmentOut(v), nil
}

// ---- updateSegment

type updateSegmentInput struct {
	SegmentPath
	IfMatch string `header:"If-Match" doc:"ETag of getSegment (required: missing → 428, stale → 412)"`
	Body    struct {
		Name       *string               `json:"name,omitempty" minLength:"2" maxLength:"100" doc:"New name"`
		Definition *SegmentDefinitionDTO `json:"definition,omitempty" doc:"New definition (counted again)"`
	}
}

func (h handler) update(ctx context.Context, in *updateSegmentInput) (*segmentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	p := SegmentPatch{Name: in.Body.Name}
	if in.Body.Definition != nil {
		def, err := in.Body.Definition.Definition("body.definition")
		if err != nil {
			return nil, err
		}
		p.Definition = &def
	}
	v, err := h.svc.UpdateSegment(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.SegmentID), in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return segmentOut(v), nil
}

// ---- archiveSegment

type archiveSegmentInput struct {
	idempotency.OptionalHeader
	SegmentPath
}

func (h handler) archive(ctx context.Context, in *archiveSegmentInput) (*segmentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ArchiveSegment(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.SegmentID))
	if err != nil {
		return nil, err
	}
	return segmentOut(v), nil
}

// ---- countAudience

type countAudienceInput struct {
	idempotency.OptionalHeader
	OrgPath
	Body struct {
		Definition SegmentDefinitionDTO `json:"definition"`
	}
}

type countOutput struct{ Body AudienceCountResultDTO }

func (h handler) count(ctx context.Context, in *countAudienceInput) (*countOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	def, err := in.Body.Definition.Definition("body.definition")
	if err != nil {
		return nil, err
	}
	r, err := h.svc.CountAudience(ctx, a, httpx.ParseID(in.OrgID), def)
	if err != nil {
		return nil, err
	}
	return &countOutput{Body: AudienceCountResultDTO{AudienceCountDTO: toCountDTO(r.Estimate), Dimensions: int(r.Dimensions)}}, nil
}
