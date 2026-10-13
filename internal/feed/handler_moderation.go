package feed

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/outreach"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
)

// registerModeration adds the 9 operations of BE-032 (04 §2.7, §F.1,
// §F.4): targeted posts, reports and moderation.
func registerModeration(api huma.API, h handler) {
	reg(api, huma.Operation{
		OperationID: "createTargetedPost", Method: http.MethodPost, Path: "/orgs/{org_id}/targeted-posts",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a targeted post (draft)",
		Description: "A segment draft: the audience is a saved segment of the organization or an inline definition " +
			"(OR inside a dimension, AND across). A club addresses its own members (club:<id>, a sensitive club too) " +
			"without the k ≥ 10 rule; a university narrows its own university; a company targets any non-sensitive " +
			"audience of at least 10 students and spends one targeted_post unit (403 entitlements.not_entitled " +
			"without one). The audience is fixed here: it never changes, deleting the draft gives the unit back. " +
			"Each call spends one unit of the organization's daily count budget (429). With dispatch, the audience " +
			"is notified once the post is published (createDispatch's rules, in the same transaction).",
	}, op.Meta{Action: string(authz.FeedPostCreateTargeted), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyRequired},
		h.createTargetedPost)

	reg(api, huma.Operation{
		OperationID: "createContentReport", Method: http.MethodPost, Path: "/reports", DefaultStatus: http.StatusCreated,
		Summary: "Report a post or an organization",
		Description: "The target must be a published post or an organization the caller may see (404 otherwise). " +
			"A user has one open report per target: reporting it again returns that report (200). The reported " +
			"organization never learns who reported.",
	}, op.Meta{Action: string(authz.FeedReportCreate), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.createReport)

	reg(api, huma.Operation{
		OperationID: "adminListReports", Method: http.MethodGet, Path: "/admin/reports",
		Summary:     "List content reports",
		Description: "The moderation queue: oldest first (sort=-created_at: newest first). The reporter's identity is shown here.",
	}, op.Meta{Action: string(authz.FeedReportList), Surface: op.SurfaceAdmin}, h.listReports)

	reg(api, huma.Operation{
		OperationID: "adminGetReport", Method: http.MethodGet, Path: "/admin/reports/{report_id}",
		Summary:     "Get a content report",
		Description: "The report with a summary of its target (the post or organization, its trust level, its open reports).",
	}, op.Meta{Action: string(authz.FeedReportRead), Surface: op.SurfaceAdmin}, h.getReport)

	reg(api, huma.Operation{
		OperationID: "adminResolveReport", Method: http.MethodPost, Path: "/admin/reports/{report_id}/resolve",
		Summary: "Take, action or dismiss a report",
		Description: "in_review takes the report (assign_to_me assigns it to the caller); actioned needs a moderation " +
			"action taken with the report (409 feed.moderation_action_required); dismissed closes it. A resolved " +
			"report takes no other change (409 feed.invalid_state). Nobody resolves their own report. The reporter " +
			"is told the report was handled.",
	}, op.Meta{Action: string(authz.FeedReportResolve), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.resolveReport)

	reg(api, huma.Operation{
		OperationID: "adminListReviewQueue", Method: http.MethodGet, Path: "/admin/review-queue",
		Summary:     "List the posts waiting for review",
		Description: "Posts in pending_review (new or restricted organizations), oldest first.",
	}, op.Meta{Action: string(authz.FeedReviewQueueList), Surface: op.SurfaceAdmin}, h.listReviewQueue)

	reg(api, huma.Operation{
		OperationID: "adminModeratePost", Method: http.MethodPost, Path: "/admin/posts/{post_id}/moderation",
		Summary: "Moderate a post",
		Description: "approve: pending_review → published (counts towards the organization's trust); hide: " +
			"published → hidden; remove: published or hidden → removed, pending_review → removed (rejected; a " +
			"targeted_post unit goes back); restore: hidden → published, or a post removed by a moderator → " +
			"published (an appeal). Every decision is kept (append-only) and the author organization is notified " +
			"with the reason code. Nobody moderates a post of an organization they write for.",
	}, op.Meta{Action: string(authz.FeedPostModerate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.moderatePost)

	reg(api, huma.Operation{
		OperationID: "adminModerateOrg", Method: http.MethodPost, Path: "/admin/orgs/{org_id}/moderation",
		Summary: "Moderate an organization",
		Description: "warn_org records a warning; restrict_org sends every post of the organization to review until " +
			"restricted_until (required, at most a year ahead); suspend_org stops its posting (platform admins " +
			"only; only they change a suspended organization). The organization's managers are notified. Nobody " +
			"moderates an organization they belong to.",
	}, op.Meta{Action: string(authz.FeedOrgModerate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.moderateOrg)

	reg(api, huma.Operation{
		OperationID: "adminListModerationActions", Method: http.MethodGet, Path: "/admin/moderation-actions",
		Summary:     "List moderation decisions",
		Description: "Newest first; narrowed by target_type and target_id.",
	}, op.Meta{Action: string(authz.FeedModerationActionList), Surface: op.SurfaceAdmin}, h.listModerationActions)
}

// ---- createTargetedPost

// TargetedAudienceBody is the audience of a targeted post: exactly one of
// a saved segment of the organization and an inline definition.
type TargetedAudienceBody struct {
	SegmentID  string                         `json:"segment_id,omitempty" doc:"An active segment of the organization"`
	Definition *outreach.SegmentDefinitionDTO `json:"definition,omitempty"`
}

// TargetedPostBody is PostWrite of 04 §2.7 with the audience
// (createTargetedPost).
type TargetedPostBody struct {
	Kind           string               `json:"kind" enum:"text,image,event_share,opportunity_call"`
	Title          string               `json:"title,omitempty" maxLength:"140"`
	Body           map[string]any       `json:"body_json,omitempty" doc:"Tiptap JSON: paragraphs, headings 2-3, lists, quotes, hard breaks; bold, italic, https/mailto links"`
	EventID        string               `json:"event_id,omitempty" doc:"event_share: a published public event"`
	Opportunity    *OpportunityBody     `json:"opportunity,omitempty"`
	Media          []MediaBody          `json:"media,omitempty" maxItems:"10" doc:"image posts: 1-10 images"`
	CommercialFlag bool                 `json:"commercial_flag,omitempty" doc:"Sponsored content; always true for companies"`
	Audience       TargetedAudienceBody `json:"audience"`
	// Dispatch notifies the audience once the post is published (BE-035;
	// the rules of createDispatch).
	Dispatch *outreach.DispatchBody `json:"dispatch,omitempty" doc:"Notify the audience once the post is published (the rules of createDispatch; a company spends one targeted_dispatch unit)"`
}

type createTargetedInput struct {
	OrgPath
	Body TargetedPostBody
}

type targetedOutput struct {
	ETag string `header:"ETag"`
	Body TargetedPostDTO
}

func (h handler) createTargetedPost(ctx context.Context, in *createTargetedInput) (*targetedOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	pi, err := PostWriteBody{
		Kind: b.Kind, Title: b.Title, Body: b.Body, EventID: b.EventID, Opportunity: b.Opportunity, Media: b.Media,
		CommercialFlag: b.CommercialFlag,
	}.input()
	if err != nil {
		return nil, err
	}
	ti := TargetedInput{Post: pi}
	if b.Dispatch != nil {
		r := b.Dispatch.Request()
		ti.Dispatch = &r
	}
	if ti.SegmentID, err = optID("body.audience.segment_id", b.Audience.SegmentID); err != nil {
		return nil, err
	}
	if d := b.Audience.Definition; d != nil {
		def, err := definitionOf("body.audience.definition", *d)
		if err != nil {
			return nil, err
		}
		ti.Definition = &def
	}
	v, err := h.svc.CreateTargetedPost(ctx, a, in.id(), ti)
	if err != nil {
		return nil, err
	}
	return &targetedOutput{ETag: v.ETag, Body: toTargetedDTO(v)}, nil
}

// definitionOf parses the inline definition of a targeted post; outreach
// validates its content (Compile).
func definitionOf(loc string, in outreach.SegmentDefinitionDTO) (outreach.Definition, error) {
	parse := func(dim string, raw []string) ([]uuid.UUID, error) {
		var out []uuid.UUID
		for i, s := range raw {
			id, err := uuid.Parse(s)
			if err != nil {
				return nil, fieldErr(loc+"."+dim+"["+strconv.Itoa(i)+"]", "must be a UUID")
			}
			out = append(out, id)
		}
		return out, nil
	}
	var (
		d   outreach.Definition
		err error
	)
	if d.Uni, err = parse("uni", in.Uni); err != nil {
		return outreach.Definition{}, err
	}
	if d.Dept, err = parse("dept", in.Dept); err != nil {
		return outreach.Definition{}, err
	}
	if d.Club, err = parse("club", in.Club); err != nil {
		return outreach.Definition{}, err
	}
	for i, y := range in.Year {
		if y < 1 || y > 8 {
			return outreach.Definition{}, fieldErr(loc+".year["+strconv.Itoa(i)+"]", "class years are 1-8")
		}
		d.Year = append(d.Year, int16(y))
	}
	return d, nil
}

// ---- createContentReport

// ReportBody is createContentReport's body (04 §2.7).
type ReportBody struct {
	TargetType string `json:"target_type" enum:"post,org"`
	TargetID   string `json:"target_id" doc:"A post or organization ID"`
	Reason     string `json:"reason" enum:"spam,harassment,hate,sexual,scam_or_misleading_job,copyright,privacy,other"`
	Details    string `json:"details,omitempty" maxLength:"1000"`
}

type createReportInput struct {
	Body ReportBody
}

type reportCreatedOutput struct {
	Status int
	Body   ReportCreatedDTO
}

func (h handler) createReport(ctx context.Context, in *createReportInput) (*reportCreatedOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	target, err := uuid.Parse(in.Body.TargetID)
	if err != nil {
		return nil, fieldErr("body.target_id", "must be a UUID")
	}
	res, err := h.svc.CreateReport(ctx, a, ReportInput{
		TargetType: TargetType(in.Body.TargetType), TargetID: target, Reason: ReportReason(in.Body.Reason),
		Details: in.Body.Details,
	})
	if err != nil {
		return nil, err
	}
	status := http.StatusCreated
	if !res.Created {
		status = http.StatusOK
	}
	return &reportCreatedOutput{Status: status, Body: ReportCreatedDTO{ReportID: res.ID.String()}}, nil
}

// ---- adminListReports / adminGetReport / adminResolveReport

// Report list sorts (04 §2.7 "sort: created_at").
const (
	sortReportsOldest = "created_at"
	sortReportsNewest = "-created_at"
)

type listReportsInput struct {
	page.Params
	Status     []string `query:"status,explode" maxItems:"4" enum:"open,in_review,actioned,dismissed" doc:"Statuses (repeat the parameter)"`
	Reason     string   `query:"reason" enum:"spam,harassment,hate,sexual,scam_or_misleading_job,copyright,privacy,other"`
	TargetType string   `query:"target_type" enum:"post,org"`
	Sort       string   `query:"sort" enum:"created_at,-created_at" doc:"created_at (default, oldest first) or -created_at"`
}

type reportListOutput struct {
	Body AdminReportList
}

func (h handler) listReports(ctx context.Context, in *listReportsInput) (*reportListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	sort := in.Sort
	if sort == "" {
		sort = sortReportsOldest
	}
	f := ReportFilter{Reason: ReportReason(in.Reason), TargetType: TargetType(in.TargetType), Newest: sort == sortReportsNewest}
	for _, st := range in.Status {
		f.Statuses = append(f.Statuses, ReportStatus(st))
	}
	fp := page.Fingerprint("adminListReports", strings.Join(in.Status, ","), in.Reason, in.TargetType)
	q := ReportQuery{Filter: f, Limit: in.Limit}
	c, err := page.Decode(in.Cursor, sort, fp, 2)
	if err != nil {
		return nil, err
	}
	if c != nil {
		at, err := c.Time(0)
		if err != nil {
			return nil, err
		}
		id, err := c.UUID(1)
		if err != nil {
			return nil, err
		}
		q.After = &After{At: at, ID: id}
	}
	p, err := h.svc.ListReports(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &reportListOutput{Body: AdminReportList{Items: make([]AdminReportDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toReportDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sort, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

// ReportPath is the report_id path parameter.
type ReportPath struct {
	ReportID string `path:"report_id" doc:"Report ID"`
}

type reportOutput struct {
	Body AdminReportDTO
}

func (h handler) getReport(ctx context.Context, in *ReportPath) (*reportOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetReport(ctx, a, parseID(in.ReportID))
	if err != nil {
		return nil, err
	}
	return &reportOutput{Body: toReportDTO(v)}, nil
}

// ResolveBody is adminResolveReport's body (04 §2.7).
type ResolveBody struct {
	Status         string `json:"status" enum:"in_review,actioned,dismissed"`
	ResolutionNote string `json:"resolution_note,omitempty" maxLength:"1000"`
	AssignToMe     bool   `json:"assign_to_me,omitempty"`
}

type resolveInput struct {
	ReportPath
	Body ResolveBody
}

func (h handler) resolveReport(ctx context.Context, in *resolveInput) (*reportOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ResolveReport(ctx, a, parseID(in.ReportID), ResolveInput{
		Status: ReportStatus(in.Body.Status), Note: in.Body.ResolutionNote, AssignToMe: in.Body.AssignToMe,
	})
	if err != nil {
		return nil, err
	}
	return &reportOutput{Body: toReportDTO(v)}, nil
}

// ---- adminListReviewQueue

const sortReview = "created_at"

type manageListOut struct {
	Body PostManageList
}

func (h handler) listReviewQueue(ctx context.Context, in *page.Params) (*manageListOut, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListReviewQueue")
	q := PageQuery{Limit: in.Limit}
	c, err := page.Decode(in.Cursor, sortReview, fp, 2)
	if err != nil {
		return nil, err
	}
	if c != nil {
		at, err := c.Time(0)
		if err != nil {
			return nil, err
		}
		id, err := c.UUID(1)
		if err != nil {
			return nil, err
		}
		q.After = &After{At: at, ID: id}
	}
	p, err := h.svc.ListReviewQueue(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &manageListOut{Body: PostManageList{Items: make([]PostManageDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toManageDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortReview, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

// ---- adminModeratePost / adminModerateOrg

// ModeratePostBody is adminModeratePost's body (04 §2.7).
type ModeratePostBody struct {
	Action     string `json:"action" enum:"approve,hide,remove,restore"`
	ReasonCode string `json:"reason_code" pattern:"^[a-z_]{2,40}$" doc:"Shown to the author organization (e.g. misleading_job, spam)"`
	Note       string `json:"note,omitempty" maxLength:"1000" doc:"Internal note of the moderators"`
	ReportID   string `json:"report_id,omitempty" doc:"An open report of this post the decision answers"`
}

type moderatePostInput struct {
	PostPath
	Body ModeratePostBody
}

type postModerationOutput struct {
	Body PostModerationDTO
}

func (h handler) moderatePost(ctx context.Context, in *moderatePostInput) (*postModerationOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	mi := ModerationInput{Action: ModAction(in.Body.Action), ReasonCode: in.Body.ReasonCode, Note: in.Body.Note}
	if mi.ReportID, err = optID("body.report_id", in.Body.ReportID); err != nil {
		return nil, err
	}
	r, err := h.svc.ModeratePost(ctx, a, in.id(), mi)
	if err != nil {
		return nil, err
	}
	return &postModerationOutput{Body: PostModerationDTO{PostID: r.Post.ID.String(), Status: string(r.Post.Status),
		ModerationAction: toActionDTO(r.Action)}}, nil
}

// ModerateOrgBody is adminModerateOrg's body (04 §2.7).
type ModerateOrgBody struct {
	Action          string     `json:"action" enum:"warn_org,restrict_org,suspend_org"`
	ReasonCode      string     `json:"reason_code" pattern:"^[a-z_]{2,40}$" doc:"Shown to the organization"`
	Note            string     `json:"note,omitempty" maxLength:"1000" doc:"Internal note of the moderators"`
	RestrictedUntil *time.Time `json:"restricted_until,omitempty" doc:"restrict_org only: the end of the restriction (at most a year ahead)"`
	ReportID        string     `json:"report_id,omitempty" doc:"An open report of this organization the decision answers"`
}

type moderateOrgInput struct {
	OrgPath
	Body ModerateOrgBody
}

type orgModerationOutput struct {
	Body OrgModerationDTO
}

func (h handler) moderateOrg(ctx context.Context, in *moderateOrgInput) (*orgModerationOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	mi := ModerationInput{Action: ModAction(in.Body.Action), ReasonCode: in.Body.ReasonCode, Note: in.Body.Note,
		RestrictedUntil: in.Body.RestrictedUntil}
	if mi.ReportID, err = optID("body.report_id", in.Body.ReportID); err != nil {
		return nil, err
	}
	r, err := h.svc.ModerateOrg(ctx, a, in.id(), mi)
	if err != nil {
		return nil, err
	}
	return &orgModerationOutput{Body: OrgModerationDTO{OrgID: r.Settings.OrgID.String(),
		TrustLevel: string(r.Settings.TrustLevel), RestrictedUntil: r.Settings.RestrictedUntil,
		ModerationAction: toActionDTO(r.Action)}}, nil
}

// ---- adminListModerationActions

type listActionsInput struct {
	page.Params
	TargetType string `query:"target_type" enum:"post,org"`
	TargetID   string `query:"target_id" doc:"A post or organization ID"`
}

type actionListOutput struct {
	Body ModerationActionList
}

func (h handler) listModerationActions(ctx context.Context, in *listActionsInput) (*actionListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	f := ActionFilter{TargetType: TargetType(in.TargetType)}
	if f.TargetID, err = optID("query.target_id", in.TargetID); err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListModerationActions", in.TargetType, in.TargetID)
	q := ActionQuery{Filter: f, Limit: in.Limit}
	c, err := page.Decode(in.Cursor, sortID, fp, 1)
	if err != nil {
		return nil, err
	}
	if c != nil {
		id, err := c.UUID(0)
		if err != nil {
			return nil, err
		}
		q.After = &id
	}
	p, err := h.svc.ListModerationActions(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &actionListOutput{Body: ModerationActionList{Items: make([]ModerationActionDTO, len(p.Items))}}
	for i, m := range p.Items {
		out.Body.Items[i] = toActionDTO(m)
	}
	if p.NextID != nil {
		out.Body.NextCursor = page.Next(sortID, fp, page.UUIDVal(*p.NextID))
	}
	return out, nil
}
