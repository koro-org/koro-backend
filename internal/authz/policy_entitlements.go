package authz

// entitlementsPolicy is the policy of the entitlements module (03 §5.11).
// Only tickets of the entitlements module edit this file (BE-001 R5).
// Written by BE-013.
//
//	entitlements.entitlement.list    S: company_member (recruiter), company_admin/owner of the org;
//	entitlements.entitlement.read      platform_admin, finance_admin
//	entitlements.usage.list
//	entitlements.entitlement.grant   platform_admin, finance_admin
//	entitlements.entitlement.revoke
//
// Resources (03 §3.5): the org endpoints pass Type entitlement with OrgID =
// the org of the path (the holder); the org management surface hides it
// from non-members (404) and shows it to members without the role and to
// every platform role (403, 03 §3.6). The admin actions pass the admin
// surface: an actor without a platform role gets 403 before anything is
// read (03 §3.2, §7.2-3). MFA follows from the role paths (k-mfa: cma, pad,
// fin; not cmm). entitlements.entitlement.expire is a system action
// (03 §3.7), granted by the core's allow-list only.
func entitlementsPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case EntitlementsEntitlementList, EntitlementsEntitlementRead, EntitlementsUsageList:
				return r.Type == TypeEntitlement && OrgSurfaceVisible(a, r.OrgID)
			case EntitlementsEntitlementGrant, EntitlementsEntitlementRevoke:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			staff := []Grant{{Role: RolePlatformAdmin}, {Role: RoleFinanceAdmin}}
			switch act {
			case EntitlementsEntitlementList, EntitlementsEntitlementRead, EntitlementsUsageList:
				gs := staff
				if r.OrgID == nil {
					return gs
				}
				if a.HasOrgRole(*r.OrgID, MemberRecruiter) {
					gs = append(gs, Grant{Role: RoleCompanyMember})
				}
				if a.HasOrgRole(*r.OrgID, MemberCompanyAdmin, MemberCompanyOwner) {
					gs = append(gs, Grant{Role: RoleCompanyAdmin})
				}
				return gs
			case EntitlementsEntitlementGrant, EntitlementsEntitlementRevoke:
				return staff
			}
			return nil
		},
	}
}
