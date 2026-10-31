package authz

// paymentsPolicy is the policy of the payments module (03 §5.16). Only
// tickets of the payments module edit this file (BE-001 R5). Written by
// BE-041:
//
//	payments.collection_account.list     admin surface: pad, fin
//	payments.collection_account.create   admin surface: fin (R is the service's, 03 §6.2)
//	payments.collection_account.approve  on the account: pad·4, fin·4 (R); never its creator (the
//	                                     core four-eyes table). The service's "403 before reading"
//	                                     gate is collection_account.list on the admin surface (the
//	                                     same pad, fin cells), because the core demands created_by
//	                                     on every resource of this action.
//	payments.collection_account.retire   admin surface: pad, fin (R; retire and make-default)
//	payments.transfer.list / read        admin surface: pad, fin
//	payments.transfer.create             admin surface: fin (manual entry, CSV import)
//	payments.transfer.match              admin surface: fin (k27: the capture moves no money out; the
//	                                     overpayment refund it opens takes four eyes in BE-043). Also
//	                                     the model C direct confirmation (T-18; doc patch).
//	payments.transfer.ignore             admin surface: fin
//	payments.transfer.mark_returned      on the transfer: fin (R, k28; 06 H-18, H-56: request,
//	                                     second eye, receipt). The core four-eyes table refuses the
//	                                     requester when the service passes requested_by (the second
//	                                     eye's decision); the request and the receipt pass it empty.
//
// BE-043 adds the payments and the refunds (03 §5.16, §6.1 "İade"):
//
//	payments.payment.list / read         admin surface: pad, fin
//	payments.refund.list                 admin surface: pad, fin (the refunds and the refunds due)
//	payments.refund.create               admin surface: pad, fin (Idempotency-Key)
//	payments.refund.approve              on the refund: fin·4 (R is the service's); never its requester
//	                                     (the core four-eyes table demands requested_by). The
//	                                     service's "403 before reading" gate is payments.refund.list
//	                                     on the admin surface (ledger's payout approval pattern).
//	payments.refund.reject               admin surface: fin (the requester may withdraw it)
//	payments.refund.complete             admin surface: fin (any finance admin, with the receipt)
//
// BE-129 (no full IBAN in a list or detail response):
//
//	payments.iban.reveal                 admin surface: pad, fin (one full IBAN of a collection
//	                                     account, an incoming transfer or a refund; the access reason
//	                                     and the audit record are the service's)
//	payments.transfer.read               also the download of a transfer's bank receipt
//
// BE-132 (Q-26) adds the refund requests of a buying company: the buyer
// asks for its money back, finance decides, an accepted request opens a
// refund (payments.refund.create's path: its approval keeps the four eyes).
//
//	payments.refund_request.create        org (the buyer): cma S (company admin or owner
//	                                      of a company org; the order, its state, the line,
//	                                      the window and the amount are the service's);
//	                                      refund_request: the buyer's cma S (its rejected
//	                                      evidence uploaded again, BE-154b)
//	payments.refund_request.list          org (the buyer): cmm S, cma S (the org's requests
//	                                      and one order's refund options)
//	payments.refund_request.list_sales    org (the seller): cla S, uni S, career_staff (no
//	                                      k-cs, as orders.line.list_sales), pad, fin
//	payments.refund_request.read          refund_request: the buyer's cmm S, cma S; the
//	                                      seller's cla, uni, career_staff (the service's
//	                                      seller view: no IBAN, no buyer user); sup, pad, fin
//	payments.refund_request.withdraw      refund_request: the buyer's cma S
//	payments.refund_request.read_evidence refund_request: the buyer's cma S; the seller's
//	                                      cla S, uni S·k-cs; pad, fin (the buyer's evidence
//	                                      file: the parties of the dispute only, KVKK)
//	payments.refund_request.list_admin    admin surface: pad, fin (the queue and one
//	                                      request with its money)
//	payments.refund_request.review /      admin surface: pad, fin (403 before reading), then
//	  accept / reject                     on the request: pad, fin, never its requester (a
//	                                      staff member who asked for its own company's money
//	                                      does not decide it: requested_by, four eyes)
//
// Resource refund_request: OrgID = the buyer, Attrs status, seller_org_id
// (the line's seller) and requested_by. Visibility (03 §3.6, as an order):
// the buyer org's members, the seller org's members and every platform
// role; anyone else gets 404. The decisions are visible to the admin
// surface only.
//
// Visibility: the admin surface, a collection account, a transfer and a
// refund are visible to every signed-in user with a platform role path
// (03 §7.2-3: 403 for an actor without the role, never 404). MFA follows
// from the role paths (k-mfa: pad, fin).
func paymentsPolicy() Policy {
	return Policy{
		Types: map[string][]string{
			TypeReceivableCollection: {AttrStatus, AttrRequestedBy},
			TypeRefundRequest:        {AttrStatus, AttrOrderLineSellerOrgID, AttrRequestedBy},
		},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch r.Type {
			case TypeAdminSurface:
				return AdminSurfaceVisible(a)
			case TypeOrg:
				switch act {
				case PaymentsRefundRequestCreate, PaymentsRefundRequestList, PaymentsRefundRequestListSales:
					return OrgSurfaceVisible(a, r.OrgID)
				}
			case TypeRefundRequest:
				switch act {
				case PaymentsRefundRequestRead, PaymentsRefundRequestWithdraw, PaymentsRefundRequestReadEvidence,
					PaymentsRefundRequestCreate:
					return orderVisible(a, r.OrgID, lineSeller(r))
				case PaymentsRefundRequestReview, PaymentsRefundRequestAccept, PaymentsRefundRequestReject:
					return AdminSurfaceVisible(a)
				}
			case TypeCollectionAccount:
				return act == PaymentsCollectionAccountApprove && AdminSurfaceVisible(a)
			case TypeIncomingTransfer:
				return act == PaymentsTransferMarkReturned && AdminSurfaceVisible(a)
			case TypeReceivableCollection:
				return act == PaymentsReceivableApprove && AdminSurfaceVisible(a)
			case TypeRefund:
				return (act == PaymentsRefundApprove || act == PaymentsRefundCancel || act == PaymentsRefundMarkFailed) && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			pad, fin := Grant{Role: RolePlatformAdmin}, Grant{Role: RoleFinanceAdmin}
			switch r.Type {
			case TypeOrg:
				return refundRequestOrgGrants(a, act, r, pad, fin)
			case TypeRefundRequest:
				return refundRequestGrants(a, act, r, pad, fin)
			case TypeAdminSurface:
				switch act {
				case PaymentsCollectionAccountList, PaymentsCollectionAccountRetire, PaymentsTransferList, PaymentsTransferRead,
					PaymentsPaymentList, PaymentsPaymentRead, PaymentsRefundList, PaymentsRefundCreate, PaymentsReceivableList,
					PaymentsIbanReveal, PaymentsRefundRequestListAdmin, PaymentsRefundRequestReview, PaymentsRefundRequestAccept,
					PaymentsRefundRequestReject:
					return []Grant{pad, fin}
				case PaymentsCollectionAccountCreate, PaymentsTransferCreate, PaymentsTransferMatch, PaymentsTransferIgnore,
					PaymentsRefundReject, PaymentsRefundComplete, PaymentsReceivableCreate, PaymentsReceivableReject:
					return []Grant{fin}
				}
			case TypeCollectionAccount:
				if act == PaymentsCollectionAccountApprove {
					return []Grant{pad, fin}
				}
			case TypeIncomingTransfer:
				if act == PaymentsTransferMarkReturned {
					return []Grant{fin}
				}
			case TypeReceivableCollection:
				if act == PaymentsReceivableApprove {
					return []Grant{fin}
				}
			case TypeRefund:
				if act == PaymentsRefundApprove || act == PaymentsRefundCancel || act == PaymentsRefundMarkFailed {
					// The eyes: the core table refuses the refund's requester.
					return []Grant{fin}
				}
			}
			return nil
		},
	}
}

// TypeRefundRequest is a buying company's refund request (BE-132).
const TypeRefundRequest = "refund_request"

// refundRequestOrgGrants are the cells on /orgs/{org_id}/orders/…/refund-requests,
// /orgs/{org_id}/refund-requests (the buyer) and
// /orgs/{org_id}/sales/refund-requests (the seller).
func refundRequestOrgGrants(a Actor, act Action, r Resource, pad, fin Grant) []Grant {
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
	case PaymentsRefundRequestCreate:
		add(company && a.HasOrgRole(org, MemberCompanyAdmin, MemberCompanyOwner), Grant{Role: RoleCompanyAdmin})
	case PaymentsRefundRequestList:
		add(company && a.HasOrgRole(org, MemberRecruiter), Grant{Role: RoleCompanyMember})
		add(company && a.HasOrgRole(org, MemberCompanyAdmin, MemberCompanyOwner), Grant{Role: RoleCompanyAdmin})
	case PaymentsRefundRequestListSales:
		add(a.HasOrgRole(org, MemberClubAdmin), Grant{Role: RoleClubAdmin})
		add(a.HasOrgRole(org, MemberUniversityRep), Grant{Role: RoleUniversityRep})
		add(a.HasOrgRole(org, MemberCareerStaff), Grant{Role: RoleCareerStaff}) // as orders.line.list_sales: no k-cs
		gs = append(gs, pad, fin)
	}
	return gs
}

// refundRequestGrants are the cells on one refund request.
func refundRequestGrants(a Actor, act Action, r Resource, pad, fin Grant) []Grant {
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
	admin := a.HasOrgRole(buyer, MemberCompanyAdmin, MemberCompanyOwner)
	switch act {
	case PaymentsRefundRequestRead:
		add(a.HasOrgRole(buyer, MemberRecruiter), Grant{Role: RoleCompanyMember})
		add(admin, Grant{Role: RoleCompanyAdmin})
		for _, s := range lineSeller(r) {
			add(a.HasOrgRole(s, MemberClubAdmin), Grant{Role: RoleClubAdmin})
			add(a.HasOrgRole(s, MemberUniversityRep), Grant{Role: RoleUniversityRep})
			add(a.HasOrgRole(s, MemberCareerStaff), Grant{Role: RoleCareerStaff})
		}
		gs = append(gs, Grant{Role: RoleSupport}, pad, fin)
	case PaymentsRefundRequestWithdraw, PaymentsRefundRequestCreate:
		add(admin, Grant{Role: RoleCompanyAdmin})
	case PaymentsRefundRequestReadEvidence:
		add(admin, Grant{Role: RoleCompanyAdmin})
		for _, s := range lineSeller(r) {
			add(a.HasOrgRole(s, MemberClubAdmin), Grant{Role: RoleClubAdmin})
			add(a.HasOrgRole(s, MemberUniversityRep), Grant{Role: RoleUniversityRep}) // k-cs: never career_staff
		}
		gs = append(gs, pad, fin)
	case PaymentsRefundRequestReview, PaymentsRefundRequestAccept, PaymentsRefundRequestReject:
		// The requester is never the decider (a staff member who is also an
		// admin of the buying company).
		eyes := []string{AttrRequestedBy}
		gs = append(gs, Grant{Role: RolePlatformAdmin, FourEyes: eyes}, Grant{Role: RoleFinanceAdmin, FourEyes: eyes})
	}
	return gs
}
