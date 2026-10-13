package outreach

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// registerDispatch adds the 4 dispatch operations of BE-035 (04 §2.8, §F.1).
func registerDispatch(api huma.API, h handler) {
	reg(api, huma.Operation{
		OperationID: "createDispatch", Method: http.MethodPost, Path: "/posts/{post_id}/dispatches",
		DefaultStatus: http.StatusCreated,
		Summary:       "Notify a targeted post's audience",
		Description: "Schedules the notification of a targeted post's audience (at most one dispatch per post, 409 " +
			"outreach.dispatch_exists); it runs once the post is published. A company's dispatch, and the dispatch " +
			"of a sponsored post, is commercial: it reaches only the students whose commercial consent of the " +
			"channel is in force, goes by push or e-mail (never in-app) and spends one targeted_dispatch unit (403 " +
			"entitlements.not_entitled without one). A club's or university's informational notice needs neither " +
			"consent nor a unit. Students under 18 and sensitive clubs are never reached; a club's notice to its " +
			"own members goes to its member list. Counts are published estimates only (null below 10).",
	}, op.Meta{
		Action: string(authz.OutreachDispatchCreate), Surface: op.SurfaceResource, Idempotency: op.IdempotencyRequired,
	}, h.createDispatch)

	reg(api, huma.Operation{
		OperationID: "listDispatches", Method: http.MethodGet, Path: "/orgs/{org_id}/dispatches",
		Summary: "List an organization's dispatches", Description: "Newest first.",
	}, op.Meta{Action: string(authz.OutreachDispatchList), Surface: op.SurfaceOrg}, h.listDispatches)

	reg(api, huma.Operation{
		OperationID: "getDispatch", Method: http.MethodGet, Path: "/orgs/{org_id}/dispatches/{dispatch_id}",
		Summary: "Get a dispatch",
		Description: "eligible_count, consented_count and sent_count are published estimates (rounded down like " +
			"the audience count) and null below 10 or before the run: an exact delivery count is never shown.",
	}, op.Meta{Action: string(authz.OutreachDispatchRead), Surface: op.SurfaceOrg}, h.getDispatch)

	reg(api, huma.Operation{
		OperationID: "cancelDispatch", Method: http.MethodPost, Path: "/orgs/{org_id}/dispatches/{dispatch_id}/cancel",
		Summary: "Cancel a scheduled dispatch",
		Description: "Only a scheduled dispatch (409 outreach.invalid_state once it runs); a company's " +
			"targeted_dispatch unit goes back. Cancelling a cancelled dispatch changes nothing.",
	}, op.Meta{
		Action: string(authz.OutreachDispatchCancel), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional,
	}, h.cancelDispatch)
}

// DispatchPath is the org_id and dispatch_id path parameters.
type DispatchPath struct {
	OrgID      string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	DispatchID string `path:"dispatch_id" doc:"Dispatch ID (an unknown, malformed or other org's ID is 404)"`
}

// DispatchBody is the request of a dispatch (createDispatch; the feed's
// targeted post with "notify on publish").
type DispatchBody struct {
	Channels []string `json:"channels" minItems:"1" maxItems:"3" uniqueItems:"true" enum:"push,email,in_app" doc:"push, email, in_app (a commercial dispatch: push and email only)"`
	// Commercial is optional: a company's and a sponsored post's dispatch is
	// always commercial; true makes an informational post's dispatch reach
	// consenting students only.
	Commercial *bool `json:"commercial,omitempty" doc:"Default: the post's nature (company or sponsored post: true). false is refused for a commercial post."`
}

// Request returns the service request of b; an unknown channel is 422 at
// body.channels[i] (huma's enum check comes first on the HTTP path).
func (b DispatchBody) Request() DispatchRequest {
	r := DispatchRequest{Commercial: b.Commercial}
	for _, c := range b.Channels {
		r.Channels = append(r.Channels, Channel(c))
	}
	return r
}

// DispatchDTO is 04 §2.8 DispatchDTO.
type DispatchDTO struct {
	DispatchID     string     `json:"dispatch_id" format:"uuid"`
	PostID         string     `json:"post_id" format:"uuid"`
	SegmentID      string     `json:"segment_id,omitempty" format:"uuid" doc:"The saved segment the post's audience came from"`
	Status         string     `json:"status" enum:"scheduled,running,completed,cancelled,failed"`
	Channels       []string   `json:"channels"`
	Commercial     bool       `json:"commercial" doc:"true: consenting students only (6563); false: an informational notice"`
	EligibleCount  *int       `json:"eligible_count" doc:"Reached students (published estimate); null below 10 or before the run"`
	ConsentedCount *int       `json:"consented_count" doc:"Of them, reachable on a channel (commercial: consenting); null below 10"`
	SentCount      *int       `json:"sent_count" doc:"Of them, notified; null below 10"`
	FailureReason  string     `json:"failure_reason,omitempty" doc:"Why a cancelled or failed dispatch did not send (a problem code)"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// DispatchList is the body of listDispatches.
type DispatchList struct {
	Items      []DispatchDTO `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// ToDispatchDTO is the API view of a dispatch.
func ToDispatchDTO(v DispatchView) DispatchDTO {
	out := DispatchDTO{
		DispatchID: v.ID.String(), PostID: v.PostID.String(), Status: string(v.Status),
		Channels: append([]string{}, v.Channels...), Commercial: v.Commercial, EligibleCount: v.Counts.Eligible,
		ConsentedCount: v.Counts.Consented, SentCount: v.Counts.Sent, CreatedAt: v.CreatedAt.UTC(),
		StartedAt: utc(v.StartedAt), CompletedAt: utc(v.CompletedAt),
	}
	if v.SegmentID != nil {
		out.SegmentID = v.SegmentID.String()
	}
	if v.LastError != nil && v.Status != DispatchCompleted {
		out.FailureReason = *v.LastError
	}
	return out
}

type dispatchOutput struct{ Body DispatchDTO }

// ---- createDispatch

type createDispatchInput struct {
	idempotency.RequiredHeader
	PostID string `path:"post_id" doc:"Post ID (an unknown, malformed or other org's post is 404)"`
	Body   DispatchBody
}

func (h handler) createDispatch(ctx context.Context, in *createDispatchInput) (*dispatchOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CreateDispatch(ctx, a, parseID(in.PostID), in.Body.Request())
	if err != nil {
		return nil, err
	}
	return &dispatchOutput{Body: ToDispatchDTO(v)}, nil
}

// ---- listDispatches

type listDispatchesInput struct {
	OrgPath
	page.Params
}

type dispatchListOutput struct{ Body DispatchList }

func (h handler) listDispatches(ctx context.Context, in *listDispatchesInput) (*dispatchListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	orgID := parseID(in.OrgID)
	fp := page.Fingerprint("listDispatches", orgID.String())
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
	p, err := h.svc.ListDispatches(ctx, a, orgID, DispatchQuery{Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &dispatchListOutput{Body: DispatchList{Items: make([]DispatchDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = ToDispatchDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortNewest, fp, page.UUIDVal(*p.Next))
	}
	return out, nil
}

// ---- getDispatch

type getDispatchInput struct {
	DispatchPath
}

func (h handler) getDispatch(ctx context.Context, in *getDispatchInput) (*dispatchOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetDispatch(ctx, a, parseID(in.OrgID), parseID(in.DispatchID))
	if err != nil {
		return nil, err
	}
	return &dispatchOutput{Body: ToDispatchDTO(v)}, nil
}

// ---- cancelDispatch

type cancelDispatchInput struct {
	idempotency.OptionalHeader
	DispatchPath
}

func (h handler) cancelDispatch(ctx context.Context, in *cancelDispatchInput) (*dispatchOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CancelDispatch(ctx, a, parseID(in.OrgID), parseID(in.DispatchID))
	if err != nil {
		return nil, err
	}
	return &dispatchOutput{Body: ToDispatchDTO(v)}, nil
}
