package audit

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register registers the audit operations (04 §2.21). svc may be nil
// (`koro openapi`): it is only used when a request arrives.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	tags := []string{"audit"}
	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "listMySecurityEvents", Method: http.MethodGet, Path: "/me/security-events", Tags: tags,
		Summary: "List the security events of my account",
		Description: "Sign-in, password, session, MFA and consent events of the caller, break-glass accesses to the caller's data " +
			"and refused sign-ins to the caller's account (wrong password, locked account, wrong second factor), whoever made them. " +
			"Only the caller's own requests show network facts; consecutive sign-in attempts with the same reason are one item " +
			"(attempts, first_occurred_at). A break-glass item (break_glass) shows the staff member's justification as reason. " +
			"A page may hold fewer items than limit while next_cursor is set.",
	}, op.Meta{Action: string(authz.AuditEventListOwn), Surface: op.SurfaceSelf}), h.listOwn)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "listOrgAuditEvents", Method: http.MethodGet, Path: "/orgs/{org_id}/audit-events", Tags: tags,
		Summary:     "List the audit events of an organisation",
		Description: "ip and user_agent are returned to dpo and platform_admin only.",
	}, op.Meta{Action: string(authz.AuditEventListOrg), Surface: op.SurfaceOrg}), h.listOrg)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminListAuditEvents", Method: http.MethodGet, Path: "/admin/audit-events", Tags: tags,
		Summary:     "Search all audit events",
		Description: "The search itself is recorded in the audit log.",
	}, op.Meta{Action: string(authz.AuditEventList), Surface: op.SurfaceAdmin, RateLimit: op.RateSearch}), h.adminList)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminVerifyAuditChain", Method: http.MethodPost, Path: "/admin/audit/verify", Tags: tags,
		Summary: "Verify the audit hash chain and its signed daily roots",
	}, op.Meta{Action: string(authz.AuditChainVerify), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}), h.verify)
}

type handler struct{ svc Service }

// Range is the occurred_from/occurred_to window of a list (04 §1.7:
// RFC 3339, or a date meaning the start/end of that Europe/Istanbul day).
type Range struct {
	OccurredFrom string `query:"occurred_from" required:"true" maxLength:"40" doc:"RFC 3339 time or YYYY-MM-DD (start of that day, Europe/Istanbul)"`
	OccurredTo   string `query:"occurred_to" required:"true" maxLength:"40" doc:"RFC 3339 time or YYYY-MM-DD (end of that day, Europe/Istanbul); inclusive"`
}

// window parses the range into [from, to).
func (r Range) window() (from, to time.Time, err error) {
	if from, err = parseBound(r.OccurredFrom, false); err != nil {
		return from, to, problem.Field("query.occurred_from", "must be an RFC 3339 time or a YYYY-MM-DD date")
	}
	if to, err = parseBound(r.OccurredTo, true); err != nil {
		return from, to, problem.Field("query.occurred_to", "must be an RFC 3339 time or a YYYY-MM-DD date")
	}
	return from, to, nil
}

// parseBound parses one bound; end bounds become exclusive.
func parseBound(s string, end bool) (time.Time, error) {
	if len(s) == len("2006-01-02") {
		d, err := civil.Parse(s)
		if err != nil {
			return time.Time{}, err
		}
		if end {
			d = d.AddDays(1)
		}
		return d.In(clock.Istanbul()).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	if end {
		t = t.Add(time.Microsecond) // inclusive at the database's precision
	}
	return t.UTC(), nil
}

func optionalID(id uuid.UUID) *uuid.UUID {
	if id == (uuid.UUID{}) {
		return nil
	}
	return &id
}

type listOwnInput struct {
	page.Params
}

type listOwnOutput struct{ Body SecurityEventList }

func (h handler) listOwn(ctx context.Context, in *listOwnInput) (*listOwnOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMySecurityEvents")
	after, err := afterOf(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListOwn(ctx, a, OwnQuery{PageQuery{Limit: in.Limit, After: after}})
	if err != nil {
		return nil, err
	}
	out := &listOwnOutput{Body: SecurityEventList{Items: make([]SecurityEventDTO, len(p.Items)), NextCursor: cursorOf(fp, p.Next)}}
	for i, e := range p.Items {
		out.Body.Items[i] = toSecurityEventDTO(e)
	}
	return out, nil
}

type listOrgInput struct {
	OrgID uuid.UUID `path:"org_id" format:"uuid"`
	page.Params
	Range
	Action      string    `query:"action" maxLength:"100" pattern:"^[a-z_]+(\\.[a-z_]+)+$" doc:"Exact action name"`
	ActorUserID uuid.UUID `query:"actor_user_id" format:"uuid"`
}

type eventListOutput struct{ Body AuditEventList }

func (h handler) listOrg(ctx context.Context, in *listOrgInput) (*eventListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	from, to, err := in.window()
	if err != nil {
		return nil, err
	}
	actor := ""
	if id := optionalID(in.ActorUserID); id != nil {
		actor = id.String()
	}
	fp := page.Fingerprint("listOrgAuditEvents", in.OrgID.String(), in.OccurredFrom, in.OccurredTo, in.Action, actor)
	after, err := afterOf(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListOrg(ctx, a, in.OrgID, OrgQuery{
		PageQuery: PageQuery{Limit: in.Limit, After: after}, From: from, To: to,
		Action: in.Action, ActorUserID: optionalID(in.ActorUserID),
	})
	if err != nil {
		return nil, err
	}
	return &eventListOutput{Body: toAuditEventList(p, fp)}, nil
}

type adminListInput struct {
	page.Params
	Range
	ActorUserID  uuid.UUID `query:"actor_user_id" format:"uuid"`
	OrgID        uuid.UUID `query:"org_id" format:"uuid"`
	ResourceType string    `query:"resource_type" maxLength:"64" pattern:"^[a-z_]+$"`
	ResourceID   uuid.UUID `query:"resource_id" format:"uuid"`
	Action       string    `query:"action" maxLength:"100" pattern:"^[a-z_]+(\\.[a-z_]+)+$" doc:"Exact action name"`
	Outcome      string    `query:"outcome" enum:"success,denied,failure"`
	BreakGlass   string    `query:"break_glass" enum:"true,false" doc:"Only break-glass accesses (true) or none of them (false)"`
}

func (h handler) adminList(ctx context.Context, in *adminListInput) (*eventListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	from, to, err := in.window()
	if err != nil {
		return nil, err
	}
	q := AdminQuery{
		From: from, To: to, ActorUserID: optionalID(in.ActorUserID), OrgID: optionalID(in.OrgID),
		ResourceType: in.ResourceType, ResourceID: optionalID(in.ResourceID), Action: in.Action, Outcome: Outcome(in.Outcome),
	}
	if in.BreakGlass != "" {
		bg := in.BreakGlass == "true"
		q.BreakGlass = &bg
	}
	fp := page.Fingerprint("adminListAuditEvents", in.OccurredFrom, in.OccurredTo, idText(q.ActorUserID), idText(q.OrgID),
		in.ResourceType, idText(q.ResourceID), in.Action, in.Outcome, in.BreakGlass)
	if q.After, err = afterOf(in.Cursor, fp); err != nil {
		return nil, err
	}
	q.Limit = in.Limit
	p, err := h.svc.AdminList(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return &eventListOutput{Body: toAuditEventList(p, fp)}, nil
}

func idText(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

type verifyInput struct {
	idempotency.OptionalHeader
	Body VerifyRequest
}

type verifyOutput struct{ Body VerifyDTO }

func (h handler) verify(ctx context.Context, in *verifyInput) (*verifyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	res, err := h.svc.Verify(ctx, a, in.Body.FromDay, in.Body.ToDay)
	if err != nil {
		return nil, err
	}
	return &verifyOutput{Body: VerifyDTO{OK: res.OK, CheckedRows: res.CheckedRows, FirstBadChainSeq: res.FirstBadChainSeq}}, nil
}
