package authz

import (
	"strings"
	"uuid"
)

// Attrs of the orders resources (03 §3.5: order seller_org_ids, order_line
// seller_org_id). The orders service derives every value from the order and
// its lines (06 H-38), never from the request.
const (
	// AttrOrderSellerOrgIDs is the comma-separated org IDs of the sellers of
	// an order's lines (03 k26: a seller sees the order through its lines).
	AttrOrderSellerOrgIDs = "seller_org_ids"
	// AttrOrderLineSellerOrgID is the seller org of an order line.
	AttrOrderLineSellerOrgID = "seller_org_id"
)

// ordersPolicy is the policy of the orders module (03 §5.15). Only tickets of
// the orders module edit this file (BE-001 R5). Written by BE-040:
//
//	orders.order.create              org (the buyer): cma S·k25 (a company org; the
//	                                   active org and the verified buyer party of k25
//	                                   are the service's: 422 orders.buyer_party_required)
//	orders.order.quote               org (the buyer): the cell of create (BE-136: the
//	                                   price preview of the same cart, written nowhere;
//	                                   its own action so the preview's cell can change
//	                                   without the purchase's, and its denials are told apart)
//	orders.order.list                org (the buyer): cmm S, cma S
//	orders.order.read                order: the buyer's cmm S, cma S; the sellers'
//	                                   club_admin, university_rep and career_staff (k26:
//	                                   their own lines only, the service's view); sup, pad, fin
//	orders.order.payment_instructions order: the buyer's cmm S, cma S; fin
//	orders.order.cancel              order: the buyer's cma S; pad; fin
//	orders.order.list_admin          admin surface: sup, pad, fin
//	orders.line.list_sales           org (the seller): cla S, uni S (career_staff too:
//	                                   no k-cs), pad, fin
//	orders.line.mark_fulfilled       order_line: the seller's cla S, uni S·k-cs; pad
//	orders.line.read_evidence        order_line: the buyer's cma S; the seller's cla S,
//	                                   uni S·k-cs (who may attach it); pad, fin (BE-093,
//	                                   T-292: the delivery evidence file, itiraz)
//	orders.line.confirm / dispute    order_line: the buyer's cma S
//	orders.line.resolve_dispute      admin surface: pad, fin (the gate before the line
//	                                   is read: 403 for everyone else, 03 §7.2-3)
//
// Resources: org (/orgs/{org_id}/orders, /orgs/{org_id}/sales: the org's
// management surface, 03 §3.6), order (OrgID = the buyer; Attrs
// seller_org_ids), order_line (OrgID = the buyer; Attrs seller_org_id) and
// admin_surface.
//
// Visibility (03 §3.6, §7.6, k26): an order or a line is visible to the
// buyer org's active members, to the active members of a seller org of its
// lines and to every platform role; anyone else gets 404 (another company's
// order is never probed). A visible order without the grant is 403.
//
// MFA follows from the role paths (k-mfa: cma, uni, sup, pad, fin; not cmm,
// cla, career_staff). The status machine, the payment window, k25's party
// and the seller view of k26 are the service's.
func ordersPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch r.Type {
			case TypeOrg:
				switch act {
				case OrdersOrderCreate, OrdersOrderQuote, OrdersOrderList, OrdersLineListSales:
					return OrgSurfaceVisible(a, r.OrgID)
				}
			case TypeAdminSurface:
				switch act {
				case OrdersOrderListAdmin, OrdersLineResolveDispute:
					return AdminSurfaceVisible(a)
				}
			case TypeOrder:
				switch act {
				case OrdersOrderRead, OrdersOrderPaymentInstructions, OrdersOrderCancel:
					return orderVisible(a, r.OrgID, orderSellers(r))
				}
			case TypeOrderLine:
				switch act {
				case OrdersLineMarkFulfilled, OrdersLineReadEvidence, OrdersLineConfirm, OrdersLineDispute, OrdersLineResolveDispute:
					return orderVisible(a, r.OrgID, lineSeller(r))
				}
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			pad, fin, sup := Grant{Role: RolePlatformAdmin}, Grant{Role: RoleFinanceAdmin}, Grant{Role: RoleSupport}
			switch r.Type {
			case TypeOrg:
				return ordersOrgGrants(a, act, r, pad, fin)
			case TypeAdminSurface:
				switch act {
				case OrdersOrderListAdmin:
					return []Grant{sup, pad, fin}
				case OrdersLineResolveDispute:
					return []Grant{pad, fin}
				}
			case TypeOrder:
				return orderGrants(a, act, r, sup, pad, fin)
			case TypeOrderLine:
				return orderLineGrants(a, act, r, pad, fin)
			}
			return nil
		},
	}
}

// ordersOrgGrants are the cells on /orgs/{org_id}/orders (the buyer) and
// /orgs/{org_id}/sales (the seller).
func ordersOrgGrants(a Actor, act Action, r Resource, pad, fin Grant) []Grant {
	if r.OrgID == nil {
		return nil
	}
	org := *r.OrgID
	var gs []Grant
	add := func(ok bool, g Grant) {
		if ok {
			gs = append(gs, g)
		}
	}
	company := r.Attr("org_kind") == string(OrgCompany)
	switch act {
	case OrdersOrderCreate, OrdersOrderQuote:
		add(company && a.HasOrgRole(org, MemberCompanyAdmin, MemberCompanyOwner), Grant{Role: RoleCompanyAdmin}) // k25
	case OrdersOrderList:
		add(company && a.HasOrgRole(org, MemberRecruiter), Grant{Role: RoleCompanyMember})
		add(company && a.HasOrgRole(org, MemberCompanyAdmin, MemberCompanyOwner), Grant{Role: RoleCompanyAdmin})
	case OrdersLineListSales:
		add(a.HasOrgRole(org, MemberClubAdmin), Grant{Role: RoleClubAdmin})
		add(a.HasOrgRole(org, MemberUniversityRep), Grant{Role: RoleUniversityRep})
		add(a.HasOrgRole(org, MemberCareerStaff), Grant{Role: RoleCareerStaff}) // uni S without k-cs
		gs = append(gs, pad, fin)
	}
	return gs
}

// orderGrants are the cells on one order (/orders/{order_id}/...).
func orderGrants(a Actor, act Action, r Resource, sup, pad, fin Grant) []Grant {
	if r.OrgID == nil {
		return nil
	}
	buyer := *r.OrgID
	var gs []Grant
	add := func(ok bool, g Grant) {
		if ok {
			gs = append(gs, g)
		}
	}
	recruiter := a.HasOrgRole(buyer, MemberRecruiter)
	admin := a.HasOrgRole(buyer, MemberCompanyAdmin, MemberCompanyOwner)
	switch act {
	case OrdersOrderRead:
		add(recruiter, Grant{Role: RoleCompanyMember})
		add(admin, Grant{Role: RoleCompanyAdmin})
		for _, s := range orderSellers(r) { // k26: the service shows a seller its own lines only
			add(a.HasOrgRole(s, MemberClubAdmin), Grant{Role: RoleClubAdmin})
			add(a.HasOrgRole(s, MemberUniversityRep), Grant{Role: RoleUniversityRep})
			add(a.HasOrgRole(s, MemberCareerStaff), Grant{Role: RoleCareerStaff})
		}
		gs = append(gs, sup, pad, fin)
	case OrdersOrderPaymentInstructions:
		add(recruiter, Grant{Role: RoleCompanyMember})
		add(admin, Grant{Role: RoleCompanyAdmin})
		gs = append(gs, fin)
	case OrdersOrderCancel:
		add(admin, Grant{Role: RoleCompanyAdmin})
		gs = append(gs, pad, fin)
	}
	return gs
}

// orderLineGrants are the cells on one order line (/order-lines/{line_id}/...).
func orderLineGrants(a Actor, act Action, r Resource, pad, fin Grant) []Grant {
	if r.OrgID == nil {
		return nil
	}
	buyer := *r.OrgID
	var gs []Grant
	add := func(ok bool, g Grant) {
		if ok {
			gs = append(gs, g)
		}
	}
	switch act {
	case OrdersLineMarkFulfilled:
		for _, s := range lineSeller(r) {
			add(a.HasOrgRole(s, MemberClubAdmin), Grant{Role: RoleClubAdmin})
			add(a.HasOrgRole(s, MemberUniversityRep), Grant{Role: RoleUniversityRep}) // k-cs: never career_staff
		}
		gs = append(gs, pad)
	case OrdersLineReadEvidence:
		// The buyer's admins (they confirm or dispute the delivery), the seller's
		// markers (they attached it) and the staff of a dispute (BE-093).
		add(a.HasOrgRole(buyer, MemberCompanyAdmin, MemberCompanyOwner), Grant{Role: RoleCompanyAdmin})
		for _, s := range lineSeller(r) {
			add(a.HasOrgRole(s, MemberClubAdmin), Grant{Role: RoleClubAdmin})
			add(a.HasOrgRole(s, MemberUniversityRep), Grant{Role: RoleUniversityRep}) // k-cs: never career_staff
		}
		gs = append(gs, pad, fin)
	case OrdersLineConfirm, OrdersLineDispute:
		add(a.HasOrgRole(buyer, MemberCompanyAdmin, MemberCompanyOwner), Grant{Role: RoleCompanyAdmin})
	case OrdersLineResolveDispute:
		gs = append(gs, pad, fin)
	}
	return gs
}

// orderVisible is 03 §3.6 for an order or a line: the buyer org's members,
// a seller org's members and every platform role.
func orderVisible(a Actor, buyer *uuid.UUID, sellers []uuid.UUID) bool {
	if OrgSurfaceVisible(a, buyer) {
		return true
	}
	for _, s := range sellers {
		if a.MemberOf(s) {
			return true
		}
	}
	return false
}

// orderSellers parses the seller org IDs of an order (malformed IDs are
// dropped: they never match a membership).
func orderSellers(r Resource) []uuid.UUID {
	var out []uuid.UUID
	for s := range strings.SplitSeq(r.Attr(AttrOrderSellerOrgIDs), ",") {
		if id, err := uuid.Parse(strings.TrimSpace(s)); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// lineSeller is the seller org of an order line (none when malformed).
func lineSeller(r Resource) []uuid.UUID {
	id, err := uuid.Parse(r.Attr(AttrOrderLineSellerOrgID))
	if err != nil {
		return nil
	}
	return []uuid.UUID{id}
}
