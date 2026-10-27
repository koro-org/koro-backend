package orders

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
)

// Audit resource types and outbox aggregates.
const (
	auditOrder = authz.TypeOrder
	auditLine  = authz.TypeOrderLine
)

// adminSurface is the authz view of /admin/** (03 §3.2: 403 before reading).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

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
	return outbox.Emit(ctx, s.d.Outbox, p, aggregate, id)
}

// emitAfterCommit publishes an A event once the transaction commits.
func (s *service) emitAfterCommit(ctx context.Context, aggregate string, id uuid.UUID, p domainevent.Payload) error {
	return outbox.EmitAfterCommit(ctx, s.d.Outbox, p, aggregate, id)
}

// lineLoc is the location of the offering of createOrder's line i.
func lineLoc(i int) string { return fmt.Sprintf("body.lines[%d].offering_id", i) }

// atLine points the refusal of one line of a cart at that line (BE-131,
// Q-25): sold out, not on sale, the per-buyer limit, no policy in effect,
// an inactive payee, a commission larger than the seller's share. The
// status and the code stay the ones catalog or orders defines (06 H-52);
// the problem's errors[] names body.lines[i].offering_id so a client of a
// cart of many lines knows which one to change. A refusal that names its
// own location, and any server error, are returned unchanged.
func atLine(i int, err error) error {
	pe, ok := errors.AsType[*problem.Error](err)
	if !ok || !problem.IsClientError(err) || len(pe.Fields()) > 0 {
		return err
	}
	return pe.WithFields(pe.At(lineLoc(i)))
}

// refusedCart is the one problem of a cart with refused lines (BE-139):
// the status, code and title of the first refusal met (the one createOrder
// returned alone before BE-139: lines that cannot be priced in request
// order, then stock in offering ID order), so a client reading code or
// errors[0] sees what it saw; errors[] carries every refused line's
// entries in that same order (body.lines[i].offering_id, the message is
// the refusal's title, the code the refusal's own, BE-140 — so a client
// translates each line, and errors[0].code is the problem's code). A cart
// of one refused line is exactly atLine's problem.
func refusedCart(refused []*problem.Error) error {
	var fields []problem.FieldError
	for _, pe := range refused {
		fields = append(fields, pe.CodedFields()...)
	}
	return refused[0].WithFields(fields...)
}

// lockOrder returns 0…n-1 sorted by the offering of each (offering(i)):
// every path that locks several offering rows in one transaction — the
// reservations of a placement, the release of an unpaid order's units —
// takes them in offering ID order, the order catalog locks an event's
// offerings in, so two carts sharing offerings, or a cart and an expiry,
// never wait on each other in a cycle (no 40P01 deadlock, BE-131).
func lockOrder(n int, offering func(i int) uuid.UUID) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return compareUUID(offering(a), offering(b)) })
	return idx
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
