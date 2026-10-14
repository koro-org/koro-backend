package authz

import "uuid"

// billingPolicy is the policy of the billing module (03 §5.18). Only tickets of
// the billing module edit this file (BE-001 R5). Written by BE-042:
//
//	billing.invoice.list        invoice collection of the org (/orgs/{org_id}/invoices):
//	                              cla S, uni S, cmm S, cma S; pad, fin
//	billing.invoice.read        invoice: the issuer's or the recipient's cla S, uni S,
//	                              cmm S, cma S; pad, fin
//	billing.invoice.upload      invoice: the issuer's cla S·k29, uni S·k29 (only a
//	                              seller_uploaded invoice: the payee's own service
//	                              invoice); fin
//	billing.invoice.verify      invoice: fin·4 (never its uploader: the core four-eyes
//	                              table demands uploaded_by). The service's "403 before
//	                              reading" gate is billing.invoice.list_admin on the
//	                              admin surface.
//	billing.invoice.cancel      admin surface: fin
//	billing.invoice.list_admin  admin surface: pad, fin
//	billing.integration.manage  admin surface: pad·R, fin·R (R is the service's, 03 §6.2)
//
// Resources: admin_surface (403 without a platform role before anything is
// read; 03 §3.2, §7.2-3) and invoice (OrgID = issuer_org_id, nil for Koro's
// own invoices; Attrs recipient_org_id, issuing_mode, uploaded_by; 03 §3.5,
// 06 H-27). The org collection of listOrgInvoices is an invoice resource
// whose issuer and recipient org are both the org of the path.
//
// Visibility (03 §3.6): an invoice is visible to the active members of its
// issuing and receiving orgs and to every platform role; anyone else gets
// 404. A visible invoice without the grant is 403. k-cs: the uni column is
// university_rep, never career_staff. MFA follows from the role paths
// (k-mfa: uni, cma, pad, fin; not cla, cmm).
func billingPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch r.Type {
			case TypeAdminSurface:
				switch act {
				case BillingInvoiceListAdmin, BillingInvoiceCancel, BillingIntegrationManage:
					return AdminSurfaceVisible(a)
				}
			case TypeInvoice:
				switch act {
				case BillingInvoiceList, BillingInvoiceRead, BillingInvoiceUpload:
					return invoiceVisible(a, r)
				case BillingInvoiceVerify:
					return a.HasAnyPlatformRole()
				}
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			pad, fin := Grant{Role: RolePlatformAdmin}, Grant{Role: RoleFinanceAdmin}
			switch r.Type {
			case TypeAdminSurface:
				switch act {
				case BillingInvoiceListAdmin, BillingIntegrationManage:
					return []Grant{pad, fin}
				case BillingInvoiceCancel:
					return []Grant{fin}
				}
			case TypeInvoice:
				switch act {
				case BillingInvoiceList, BillingInvoiceRead:
					return append(invoiceOrgGrants(a, r), pad, fin)
				case BillingInvoiceUpload:
					gs := []Grant{fin}
					if r.Attr("issuing_mode") == "seller_uploaded" && r.OrgID != nil { // k29
						gs = append(gs, invoiceSellerGrants(a, *r.OrgID)...)
					}
					return gs
				case BillingInvoiceVerify:
					return []Grant{fin}
				}
			}
			return nil
		},
	}
}

// invoiceOrgs returns the issuing and the receiving org of an invoice
// resource (either may be missing).
func invoiceOrgs(r Resource) []uuid.UUID {
	var out []uuid.UUID
	if r.OrgID != nil {
		out = append(out, *r.OrgID)
	}
	if id, err := uuid.Parse(r.Attr("recipient_org_id")); err == nil && (r.OrgID == nil || id != *r.OrgID) {
		out = append(out, id)
	}
	return out
}

// invoiceVisible: an active member of the issuing or the receiving org, or
// any platform role (03 §3.6).
func invoiceVisible(a Actor, r Resource) bool {
	if a.HasAnyPlatformRole() {
		return true
	}
	for _, org := range invoiceOrgs(r) {
		if a.MemberOf(org) {
			return true
		}
	}
	return false
}

// invoiceOrgGrants are the S cells of list and read in either org: club
// admin, university rep (k-cs), recruiter (cmm) and company admin or owner.
func invoiceOrgGrants(a Actor, r Resource) []Grant {
	var gs []Grant
	for _, org := range invoiceOrgs(r) {
		gs = append(gs, invoiceSellerGrants(a, org)...)
		if a.HasOrgRole(org, MemberRecruiter) {
			gs = append(gs, Grant{Role: RoleCompanyMember})
		}
		if a.HasOrgRole(org, MemberCompanyAdmin, MemberCompanyOwner) {
			gs = append(gs, Grant{Role: RoleCompanyAdmin})
		}
	}
	return gs
}

// invoiceSellerGrants are the cla S and uni S cells (k-cs) in org.
func invoiceSellerGrants(a Actor, org uuid.UUID) []Grant {
	var gs []Grant
	if a.HasOrgRole(org, MemberClubAdmin) {
		gs = append(gs, Grant{Role: RoleClubAdmin})
	}
	if a.HasOrgRole(org, MemberUniversityRep) {
		gs = append(gs, Grant{Role: RoleUniversityRep})
	}
	return gs
}
