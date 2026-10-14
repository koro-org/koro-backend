package orders

import (
	"context"
	"errors"
	"slices"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
)

// Audit resource types and outbox aggregates.
const (
	auditOrder = authz.TypeOrder
	auditLine  = authz.TypeOrderLine
)

// adminSurface is the authz view of /admin/** (03 §3.2: 403 before reading).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// orgResource is the org of /orgs/{org_id}/... (the management surface).
func orgResource(o orgs.OrgSummary) authz.Resource {
	id := o.ID
	return authz.Resource{Type: authz.TypeOrg, ID: id, OrgID: &id, Attrs: map[string]string{
		"org_kind": string(o.Kind), "org_status": string(o.Status),
	}}
}

// sellersOf returns the distinct seller orgs of lines, sorted.
func sellersOf(lines []Line) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		if !slices.Contains(out, l.SellerOrgID) {
			out = append(out, l.SellerOrgID)
		}
	}
	slices.SortFunc(out, compareUUID)
	return out
}

// orderResource is the authz view of an order (03 §3.5): the buyer org and
// the sellers of its lines (k26).
func orderResource(o Order, lines []Line) authz.Resource {
	org := o.OrgID
	ids := make([]string, 0, len(lines))
	for _, s := range sellersOf(lines) {
		ids = append(ids, s.String())
	}
	return authz.Resource{Type: authz.TypeOrder, ID: o.ID, OrgID: &org, Attrs: map[string]string{
		authz.AttrStatus: string(o.Status), authz.AttrOrderSellerOrgIDs: strings.Join(ids, ","),
	}}
}

// lineResource is the authz view of an order line (03 §3.5).
func lineResource(l Line) authz.Resource {
	org := l.OrgID
	return authz.Resource{Type: authz.TypeOrderLine, ID: l.ID, OrgID: &org, Attrs: map[string]string{
		authz.AttrStatus: string(l.FulfillmentStatus), authz.AttrOrderLineSellerOrgID: l.SellerOrgID.String(),
	}}
}

// staff reports whether a acts as platform staff of orders (support,
// platform or finance admin): the buyer's view of every order.
func staff(a authz.Actor) bool {
	return a.HasPlatformRole(authz.PlatformAdmin) || a.HasPlatformRole(authz.PlatformFinanceAdmin) ||
		a.HasPlatformRole(authz.PlatformSupport)
}

// sellerRoles are the org roles that see a seller's lines (03 k26).
var sellerRoles = []authz.MemberRole{authz.MemberClubAdmin, authz.MemberUniversityRep, authz.MemberCareerStaff}

// org returns the org of an org-surface request (404 when it is unknown).
func (s *service) org(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error) {
	o, err := s.d.Orgs.Get(ctx, orgID)
	if errors.Is(err, orgs.ErrNotFound) {
		return orgs.OrgSummary{}, ErrNotFound
	}
	return o, err
}

// orderEvent is the audit record of an order write by a. Metadata carries
// states, codes and IDs, never free text (05 §11.2); reason is the actor's
// free-text justification (cancel reason, delivery note).
func orderEvent(a authz.Actor, act authz.Action, o Order, reason string, meta map[string]any) audit.Event {
	id, org := o.ID, o.OrgID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["status"] = string(o.Status)
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: auditOrder, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// lineEvent is the audit record of a line write by a (the buyer's org is
// the scope; the seller is in the metadata).
func lineEvent(a authz.Actor, act authz.Action, l Line, reason string, meta map[string]any) audit.Event {
	id, org := l.ID, l.OrgID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["fulfillment_status"] = string(l.FulfillmentStatus)
	meta["order_id"] = l.OrderID.String()
	meta["seller_org_id"] = l.SellerOrgID.String()
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: auditLine, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// emitOutbox writes an O event in the transaction carried by ctx.
func (s *service) emitOutbox(ctx context.Context, aggregate string, id uuid.UUID, p domainevent.Payload) error {
	env, err := domainevent.New(p, aggregate, id)
	if err != nil {
		return err
	}
	return s.d.Outbox.Add(ctx, env)
}

// emitAfterCommit publishes an A event once the transaction commits.
func (s *service) emitAfterCommit(ctx context.Context, aggregate string, id uuid.UUID, p domainevent.Payload) error {
	env, err := domainevent.New(p, aggregate, id)
	if err != nil {
		return err
	}
	return s.d.Outbox.AfterCommit(ctx, env)
}

func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}
