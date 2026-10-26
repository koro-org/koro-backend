package audit

import (
	"context"
	"net/http"
	"strconv"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
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
		OperationID: "queryOrgAuditEvents", Method: http.MethodPost, Path: "/orgs/{org_id}/audit-events/search", Tags: tags,
		Summary: "Search the audit events of an organisation",
		Description: "Read-only: the criteria are in the body so that a person's user ID never reaches a URL or an " +
			"access log (BE-128). occurred_from and occurred_to are required; next_cursor goes back in the body. ip " +
			"and user_agent are returned to dpo and platform_admin only.",
	}, op.Meta{Action: string(authz.AuditEventListOrg), Surface: op.SurfaceOrg, RateLimit: op.RateReadDefault,
		Idempotency: op.IdempotencyOptional}), h.queryOrg)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "listOrgAuditEvents", Method: http.MethodGet, Path: "/orgs/{org_id}/audit-events", Tags: tags,
		Deprecated: true, Summary: "List the audit events of an organisation (deprecated: queryOrgAuditEvents)",
		Description: "Deprecated (BE-128): queryOrgAuditEvents (POST …/audit-events/search) takes the criteria in " +
			"the body. An actor_user_id in the query string is 422 (personal data stays out of URLs). ip and " +
			"user_agent are returned to dpo and platform_admin only.",
	}, op.Meta{Action: string(authz.AuditEventListOrg), Surface: op.SurfaceOrg}), h.listOrg)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminQueryAuditEvents", Method: http.MethodPost, Path: "/admin/audit-events/search", Tags: tags,
		Summary: "Search all audit events",
		Description: "Read-only: the criteria are in the body so that a person's user ID never reaches a URL or an " +
			"access log (BE-128). occurred_from and occurred_to are required; next_cursor goes back in the body. The " +
			"search itself is recorded in the audit log.",
	}, op.Meta{Action: string(authz.AuditEventList), Surface: op.SurfaceAdmin, RateLimit: op.RateSearch,
		Idempotency: op.IdempotencyOptional}), h.adminQuery)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminListAuditEvents", Method: http.MethodGet, Path: "/admin/audit-events", Tags: tags,
		Deprecated: true, Summary: "Search all audit events (deprecated: adminQueryAuditEvents)",
		Description: "Deprecated (BE-128): adminQueryAuditEvents (POST /admin/audit-events/search) takes the " +
			"criteria in the body. An actor_user_id in the query string is 422 (personal data stays out of URLs). " +
			"The search itself is recorded in the audit log.",
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

// BodyRange is Range in the body of a search sent as POST (BE-128).
type BodyRange struct {
	OccurredFrom string `json:"occurred_from" required:"true" maxLength:"40" doc:"RFC 3339 time or YYYY-MM-DD (start of that day, Europe/Istanbul)"`
	OccurredTo   string `json:"occurred_to" required:"true" maxLength:"40" doc:"RFC 3339 time or YYYY-MM-DD (end of that day, Europe/Istanbul); inclusive"`
}

// window parses the range into [from, to).
func (r Range) window() (from, to time.Time, err error) {
	return window(r.OccurredFrom, r.OccurredTo, "query.")
}

// window parses the body range into [from, to).
func (r BodyRange) window() (from, to time.Time, err error) {
	return window(r.OccurredFrom, r.OccurredTo, "body.")
}

// window parses raw bounds into [from, to); loc prefixes the field names
// of the errors ("query." or "body.").
func window(rawFrom, rawTo, loc string) (from, to time.Time, err error) {
	if from, err = parseBound(rawFrom, false); err != nil {
		return from, to, problem.Field(loc+"occurred_from", "must be an RFC 3339 time or a YYYY-MM-DD date")
	}
	if to, err = parseBound(rawTo, true); err != nil {
		return from, to, problem.Field(loc+"occurred_to", "must be an RFC 3339 time or a YYYY-MM-DD date")
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

// nonZeroID maps the zero UUID of an omitted query parameter to nil.
func nonZeroID(id uuid.UUID) *uuid.UUID {
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
	Action      string `query:"action" maxLength:"100" pattern:"^[a-z_]+(\\.[a-z_]+)+$" doc:"Exact action name"`
	ActorUserID string `query:"actor_user_id" hidden:"true" doc:"Moved to queryOrgAuditEvents (BE-128): 422 when set"`
}

type queryOrgInput struct {
	OrgID uuid.UUID `path:"org_id" format:"uuid"`
	Body  struct {
		BodyRange
		Action      string `json:"action,omitempty" maxLength:"100" pattern:"^[a-z_]+(\\.[a-z_]+)+$" doc:"Exact action name"`
		ActorUserID string `json:"actor_user_id,omitempty" format:"uuid"`
		page.BodyParams
	}
}

type eventListOutput struct{ Body AuditEventList }

// listOrg is the deprecated GET twin of queryOrg (no actor).
func (h handler) listOrg(ctx context.Context, in *listOrgInput) (*eventListOutput, error) {
	if in.ActorUserID != "" {
		return nil, httpx.QueryMoved("actor_user_id", "queryOrgAuditEvents")
	}
	from, to, err := in.window()
	if err != nil {
		return nil, err
	}
	return h.orgPage(ctx, "listOrgAuditEvents", in.OrgID, OrgQuery{PageQuery: PageQuery{Limit: in.Limit}, From: from, To: to,
		Action: in.Action}, in.OccurredFrom, in.OccurredTo, in.Cursor)
}

func (h handler) queryOrg(ctx context.Context, in *queryOrgInput) (*eventListOutput, error) {
	b := in.Body
	from, to, err := b.window()
	if err != nil {
		return nil, err
	}
	actor, err := httpx.OptID("body.actor_user_id", b.ActorUserID)
	if err != nil {
		return nil, err
	}
	return h.orgPage(ctx, "queryOrgAuditEvents", in.OrgID, OrgQuery{PageQuery: PageQuery{Limit: b.Limit}, From: from, To: to,
		Action: b.Action, ActorUserID: actor}, b.OccurredFrom, b.OccurredTo, b.Cursor)
}

// orgPage runs one page of an org's audit log; the cursor is bound to the
// operation and the criteria (the raw bounds, as the client sent them).
func (h handler) orgPage(ctx context.Context, opID string, orgID uuid.UUID, q OrgQuery, rawFrom, rawTo, cursor string) (*eventListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint(opID, orgID.String(), rawFrom, rawTo, q.Action, idText(q.ActorUserID))
	if q.After, err = afterOf(cursor, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListOrg(ctx, a, orgID, q)
	if err != nil {
		return nil, err
	}
	return &eventListOutput{Body: toAuditEventList(p, fp)}, nil
}

type adminListInput struct {
	page.Params
	Range
	ActorUserID  string    `query:"actor_user_id" hidden:"true" doc:"Moved to adminQueryAuditEvents (BE-128): 422 when set"`
	OrgID        uuid.UUID `query:"org_id" format:"uuid"`
	ResourceType string    `query:"resource_type" maxLength:"64" pattern:"^[a-z_]+$"`
	ResourceID   uuid.UUID `query:"resource_id" format:"uuid"`
	Action       string    `query:"action" maxLength:"100" pattern:"^[a-z_]+(\\.[a-z_]+)+$" doc:"Exact action name"`
	Outcome      string    `query:"outcome" enum:"success,denied,failure"`
	BreakGlass   string    `query:"break_glass" enum:"true,false" doc:"Only break-glass accesses (true) or none of them (false)"`
}

type adminQueryInput struct {
	Body struct {
		BodyRange
		ActorUserID  string `json:"actor_user_id,omitempty" format:"uuid"`
		OrgID        string `json:"org_id,omitempty" format:"uuid"`
		ResourceType string `json:"resource_type,omitempty" maxLength:"64" pattern:"^[a-z_]+$"`
		ResourceID   string `json:"resource_id,omitempty" format:"uuid"`
		Action       string `json:"action,omitempty" maxLength:"100" pattern:"^[a-z_]+(\\.[a-z_]+)+$" doc:"Exact action name"`
		Outcome      string `json:"outcome,omitempty" enum:"success,denied,failure"`
		BreakGlass   *bool  `json:"break_glass,omitempty" doc:"Only break-glass accesses (true) or none of them (false)"`
		page.BodyParams
	}
}

// adminList is the deprecated GET twin of adminQuery (no actor).
func (h handler) adminList(ctx context.Context, in *adminListInput) (*eventListOutput, error) {
	if in.ActorUserID != "" {
		return nil, httpx.QueryMoved("actor_user_id", "adminQueryAuditEvents")
	}
	from, to, err := in.window()
	if err != nil {
		return nil, err
	}
	q := AdminQuery{
		PageQuery: PageQuery{Limit: in.Limit}, From: from, To: to, OrgID: nonZeroID(in.OrgID),
		ResourceType: in.ResourceType, ResourceID: nonZeroID(in.ResourceID), Action: in.Action, Outcome: Outcome(in.Outcome),
	}
	if in.BreakGlass != "" {
		bg := in.BreakGlass == "true"
		q.BreakGlass = &bg
	}
	return h.adminPage(ctx, "adminListAuditEvents", q, in.OccurredFrom, in.OccurredTo, in.Cursor)
}

func (h handler) adminQuery(ctx context.Context, in *adminQueryInput) (*eventListOutput, error) {
	b := in.Body
	from, to, err := b.window()
	if err != nil {
		return nil, err
	}
	q := AdminQuery{PageQuery: PageQuery{Limit: b.Limit}, From: from, To: to, ResourceType: b.ResourceType, Action: b.Action,
		Outcome: Outcome(b.Outcome), BreakGlass: b.BreakGlass}
	for _, id := range []struct {
		dst      **uuid.UUID
		loc, raw string
	}{{&q.ActorUserID, "body.actor_user_id", b.ActorUserID}, {&q.OrgID, "body.org_id", b.OrgID}, {&q.ResourceID, "body.resource_id", b.ResourceID}} {
		if *id.dst, err = httpx.OptID(id.loc, id.raw); err != nil {
			return nil, err
		}
	}
	return h.adminPage(ctx, "adminQueryAuditEvents", q, b.OccurredFrom, b.OccurredTo, b.Cursor)
}

// adminPage runs one page of the platform audit search; the cursor is
// bound to the operation and the criteria.
func (h handler) adminPage(ctx context.Context, opID string, q AdminQuery, rawFrom, rawTo, cursor string) (*eventListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	bg := ""
	if q.BreakGlass != nil {
		bg = strconv.FormatBool(*q.BreakGlass)
	}
	fp := page.Fingerprint(opID, rawFrom, rawTo, idText(q.ActorUserID), idText(q.OrgID),
		q.ResourceType, idText(q.ResourceID), q.Action, string(q.Outcome), bg)
	if q.After, err = afterOf(cursor, fp); err != nil {
		return nil, err
	}
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
