package entitlements

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

// Register adds the entitlements operations (04 §2.11) to api. svc may be
// nil (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	tags := []string{"entitlements"}
	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "listOrgEntitlements", Method: http.MethodGet, Path: "/orgs/{org_id}/entitlements", Tags: tags,
		Summary: "List the entitlements of an organisation",
		Description: "Rights of the org (quota or access), newest first, with a summary of what is usable now " +
			"(available: units left per quota feature, access end for discovery_access).",
	}, op.Meta{Action: string(authz.EntitlementsEntitlementList), Surface: op.SurfaceOrg}), h.list)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "getOrgEntitlement", Method: http.MethodGet, Path: "/orgs/{org_id}/entitlements/{entitlement_id}", Tags: tags,
		Summary: "Get an entitlement of an organisation",
	}, op.Meta{Action: string(authz.EntitlementsEntitlementRead), Surface: op.SurfaceOrg}), h.get)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "listOrgEntitlementUsages", Method: http.MethodGet, Path: "/orgs/{org_id}/entitlement-usages", Tags: tags,
		Summary: "List the entitlement usage history of an organisation",
		Description: "Consumptions (positive quantity) and their reversals (negative quantity), newest first. " +
			"actor_user_id is returned to platform_admin and finance_admin only.",
	}, op.Meta{Action: string(authz.EntitlementsUsageList), Surface: op.SurfaceOrg}), h.listUsages)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminGrantEntitlement", Method: http.MethodPost, Path: "/admin/orgs/{org_id}/entitlements", Tags: tags,
		Summary: "Grant an entitlement to an organisation", DefaultStatus: http.StatusCreated,
		Description: "Grants a right by hand (pilot packages are sold offline; promotions). The grant is " +
			"active at once and usable inside [valid_from, valid_to). Idempotency-Key is required: a retry " +
			"replays the first grant. Rights go to companies only: another kind of org is 422 " +
			"entitlements.org_not_company.",
	}, op.Meta{
		Action: string(authz.EntitlementsEntitlementGrant), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyRequired,
	}), h.adminGrant)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminRevokeEntitlement", Method: http.MethodPost, Path: "/admin/entitlements/{entitlement_id}/revoke", Tags: tags,
		Summary: "Revoke an entitlement",
		Description: "Units already used stay used. Revoking an expired right is 409; a revoked one is returned " +
			"unchanged. A right bought with an order is revoked without a refund: warning " +
			"order_refund_not_started tells the admin to start the refund separately.",
	}, op.Meta{
		Action: string(authz.EntitlementsEntitlementRevoke), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional,
	}), h.adminRevoke)
}

type handler struct{ svc Service }

// Cursor sort keys and fingerprints (04 §1.6).
const (
	sortNewest = "-id"
	// WarnOrderRefund is the revoke warning for a right bought with an order.
	WarnOrderRefund = "order_refund_not_started"
)

func cursorOf(fp string, next *uuid.UUID) string {
	if next == nil {
		return ""
	}
	return page.Next(sortNewest, fp, page.UUIDVal(*next))
}

func afterOf(cursor, fp string) (*uuid.UUID, error) {
	c, err := page.Decode(cursor, sortNewest, fp, 1)
	if err != nil || c == nil {
		return nil, err
	}
	id, err := c.UUID(0)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// pathID parses a path ID; a malformed one is simply not found (IDOR rule).
func pathID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil(), ErrNotFound
	}
	return id, nil
}

// ---- DTOs

// EntitlementDTO is 04 §2.11 EntitlementDTO.
type EntitlementDTO struct {
	EntitlementID string    `json:"entitlement_id" format:"uuid"`
	Feature       string    `json:"feature" enum:"targeted_post,targeted_dispatch,contact_request,discovery_access"`
	Kind          string    `json:"kind" enum:"quota,access"`
	QuantityTotal *int      `json:"quantity_total,omitempty" doc:"Units bought (quota only)"`
	QuantityUsed  int       `json:"quantity_used"`
	ValidFrom     time.Time `json:"valid_from"`
	ValidTo       time.Time `json:"valid_to"`
	Status        string    `json:"status" enum:"pending,active,exhausted,expired,revoked"`
	SourceType    string    `json:"source_type" enum:"order_line,admin_grant,promo"`
}

func toDTO(e Entitlement) EntitlementDTO {
	return EntitlementDTO{
		EntitlementID: e.ID.String(), Feature: string(e.Feature), Kind: string(e.Kind), QuantityTotal: e.QuantityTotal,
		QuantityUsed: e.QuantityUsed, ValidFrom: e.ValidFrom.UTC(), ValidTo: e.ValidTo.UTC(), Status: string(e.Status),
		SourceType: string(e.SourceType),
	}
}

// AvailabilityDTO is one row of the "available" summary.
type AvailabilityDTO struct {
	Feature     string     `json:"feature" enum:"targeted_post,targeted_dispatch,contact_request,discovery_access"`
	Remaining   *int       `json:"remaining,omitempty" doc:"Units usable now (quota features)"`
	AccessUntil *time.Time `json:"access_until,omitempty" doc:"End of the running access right (discovery_access; absent when none)"`
}

// EntitlementList is the body of listOrgEntitlements.
type EntitlementList struct {
	Items      []EntitlementDTO  `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
	Available  []AvailabilityDTO `json:"available"`
}

// UsageDTO is one row of listOrgEntitlementUsages.
type UsageDTO struct {
	UsageID     string    `json:"usage_id" format:"uuid"`
	Feature     string    `json:"feature" enum:"targeted_post,targeted_dispatch,contact_request,discovery_access"`
	Quantity    int       `json:"quantity" doc:"Positive: consumption; negative: reversal (release)"`
	RefType     string    `json:"ref_type" enum:"post,dispatch,contact_request"`
	RefID       string    `json:"ref_id" format:"uuid"`
	CreatedAt   time.Time `json:"created_at"`
	ActorUserID string    `json:"actor_user_id,omitempty" format:"uuid" doc:"Who consumed; platform_admin and finance_admin only"`
}

// UsageList is the body of listOrgEntitlementUsages.
type UsageList struct {
	Items      []UsageDTO `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// RevokedEntitlementDTO is the body of adminRevokeEntitlement.
type RevokedEntitlementDTO struct {
	EntitlementDTO
	Warning string `json:"warning,omitempty" enum:"order_refund_not_started" doc:"Set when the right was bought with an order: no refund was started"`
}

// ---- listOrgEntitlements

type listInput struct {
	OrgID string `path:"org_id" doc:"Organisation ID (an unknown or malformed ID is 404)"`
	page.Params
	Feature string `query:"feature" enum:"targeted_post,targeted_dispatch,contact_request,discovery_access"`
	Status  string `query:"status" enum:"pending,active,exhausted,expired,revoked"`
}

type listOutput struct{ Body EntitlementList }

func (h handler) list(ctx context.Context, in *listInput) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	orgID, err := pathID(in.OrgID)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listOrgEntitlements", orgID.String(), in.Feature, in.Status)
	after, err := afterOf(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.List(ctx, a, orgID, ListQuery{
		Filter: ListFilter{Feature: Feature(in.Feature), Status: Status(in.Status)}, Limit: in.Limit, After: after,
	})
	if err != nil {
		return nil, err
	}
	out := &listOutput{Body: EntitlementList{
		Items: make([]EntitlementDTO, len(p.Items)), NextCursor: cursorOf(fp, p.Next),
		Available: make([]AvailabilityDTO, len(p.Available)),
	}}
	for i, e := range p.Items {
		out.Body.Items[i] = toDTO(e)
	}
	for i, av := range p.Available {
		d := AvailabilityDTO{Feature: string(av.Feature), AccessUntil: av.AccessUntil}
		if av.Feature.Kind() == KindQuota {
			n := av.Remaining
			d.Remaining = &n
		}
		out.Body.Available[i] = d
	}
	return out, nil
}

// ---- getOrgEntitlement

type getInput struct {
	OrgID         string `path:"org_id" doc:"Organisation ID (an unknown or malformed ID is 404)"`
	EntitlementID string `path:"entitlement_id" doc:"Entitlement ID (an unknown, malformed or other org's ID is 404)"`
}

type entitlementOutput struct{ Body EntitlementDTO }

func (h handler) get(ctx context.Context, in *getInput) (*entitlementOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	orgID, err := pathID(in.OrgID)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.EntitlementID)
	if err != nil {
		id = uuid.Nil() // authorised first (org surface), then 404
	}
	e, err := h.svc.Get(ctx, a, orgID, id)
	if err != nil {
		return nil, err
	}
	return &entitlementOutput{Body: toDTO(e)}, nil
}

// ---- listOrgEntitlementUsages

type listUsagesInput struct {
	OrgID string `path:"org_id" doc:"Organisation ID (an unknown or malformed ID is 404)"`
	page.Params
	Feature string `query:"feature" enum:"targeted_post,targeted_dispatch,contact_request,discovery_access"`
}

type listUsagesOutput struct{ Body UsageList }

func (h handler) listUsages(ctx context.Context, in *listUsagesInput) (*listUsagesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	orgID, err := pathID(in.OrgID)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listOrgEntitlementUsages", orgID.String(), in.Feature)
	after, err := afterOf(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListUsages(ctx, a, orgID, UsageQuery{Filter: UsageFilter{Feature: Feature(in.Feature)}, Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &listUsagesOutput{Body: UsageList{Items: make([]UsageDTO, len(p.Items)), NextCursor: cursorOf(fp, p.Next)}}
	for i, u := range p.Items {
		d := UsageDTO{
			UsageID: u.ID.String(), Feature: string(u.Feature), Quantity: u.Quantity, RefType: string(u.RefType),
			RefID: u.RefID.String(), CreatedAt: u.CreatedAt.UTC(),
		}
		if u.ActorUserID != nil {
			d.ActorUserID = u.ActorUserID.String()
		}
		out.Body.Items[i] = d
	}
	return out, nil
}

// ---- adminGrantEntitlement

type adminGrantInput struct {
	idempotency.RequiredHeader
	OrgID string `path:"org_id" doc:"Organisation ID (an unknown or malformed ID is 404)"`
	Body  struct {
		Feature       string    `json:"feature" enum:"targeted_post,targeted_dispatch,contact_request,discovery_access"`
		Kind          string    `json:"kind" enum:"quota,access" doc:"access for discovery_access, quota otherwise"`
		QuantityTotal *int      `json:"quantity_total,omitempty" minimum:"1" maximum:"100000" doc:"Units (quota only)"`
		ValidFrom     time.Time `json:"valid_from" doc:"Start of the validity window"`
		ValidTo       time.Time `json:"valid_to" doc:"End of the validity window (exclusive; in the future, at most three years after valid_from)"`
		SourceType    string    `json:"source_type" enum:"admin_grant,promo" doc:"admin_grant: a package sold offline; promo: a promotion"`
		Reason        string    `json:"reason" minLength:"3" maxLength:"300" doc:"Why the right is granted (audited; no personal data)"`
	}
}

func (h handler) adminGrant(ctx context.Context, in *adminGrantInput) (*entitlementOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	orgID, err := uuid.Parse(in.OrgID)
	if err != nil {
		orgID = uuid.Nil() // authorised first (admin surface), then 404
	}
	b := in.Body
	e, err := h.svc.AdminGrant(ctx, a, orgID, AdminGrantInput{
		Feature: Feature(b.Feature), Kind: Kind(b.Kind), Quantity: b.QuantityTotal, ValidFrom: b.ValidFrom,
		ValidTo: b.ValidTo, SourceType: SourceType(b.SourceType), Reason: b.Reason,
	})
	if err != nil {
		return nil, err
	}
	return &entitlementOutput{Body: toDTO(e)}, nil
}

// ---- adminRevokeEntitlement

type adminRevokeInput struct {
	idempotency.OptionalHeader
	EntitlementID string `path:"entitlement_id" doc:"Entitlement ID (an unknown or malformed ID is 404)"`
	Body          struct {
		Reason string `json:"reason" minLength:"3" maxLength:"300" doc:"Why the right is taken back (stored and audited; no personal data)"`
	}
}

type revokeOutput struct{ Body RevokedEntitlementDTO }

func (h handler) adminRevoke(ctx context.Context, in *adminRevokeInput) (*revokeOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.EntitlementID)
	if err != nil {
		id = uuid.Nil() // authorised first (admin surface), then 404
	}
	e, err := h.svc.AdminRevoke(ctx, a, id, in.Body.Reason)
	if err != nil {
		return nil, err
	}
	out := &revokeOutput{Body: RevokedEntitlementDTO{EntitlementDTO: toDTO(e)}}
	if e.SourceType == SourceOrderLine {
		out.Body.Warning = WarnOrderRefund
	}
	return out, nil
}
