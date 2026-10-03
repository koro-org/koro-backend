package authz

// auditPolicy is the policy of the audit module (03 §5.21). Only tickets of
// the audit module edit this file (BE-001 R5).
//
//	audit.event.list_own  O for every signed-in user (their own log)
//	audit.event.list_org  S for club_admin, university_rep (not
//	                      career_staff, k-cs), company_admin/owner of the
//	                      org; dpo, platform_admin (MFA by role, k-mfa)
//	audit.event.list      dpo, platform_admin
//	audit.chain.verify    dpo, platform_admin
//
// Resources: list_own → audit_log with OwnerUserID = the actor; list_org →
// audit_log with OrgID = the queried org (org management surface: non
// members 404, 03 §3.6); list and verify → admin_surface (403 without a
// platform role, 03 §7.2-3).
func auditPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case AuditEventListOwn:
				return a.Owns(r)
			case AuditEventListOrg:
				return r.Type == TypeAuditLog && OrgSurfaceVisible(a, r.OrgID)
			case AuditEventList, AuditChainVerify:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			staff := []Grant{{Role: RoleDPO}, {Role: RolePlatformAdmin}}
			switch act {
			case AuditEventListOwn:
				return []Grant{{Role: RoleSelf}}
			case AuditEventListOrg:
				gs := staff
				if r.OrgID == nil {
					return gs
				}
				org := *r.OrgID
				if a.HasOrgRole(org, MemberClubAdmin) {
					gs = append(gs, Grant{Role: RoleClubAdmin})
				}
				if a.HasOrgRole(org, MemberUniversityRep) {
					gs = append(gs, Grant{Role: RoleUniversityRep})
				}
				if a.HasOrgRole(org, MemberCompanyAdmin, MemberCompanyOwner) {
					gs = append(gs, Grant{Role: RoleCompanyAdmin})
				}
				return gs
			case AuditEventList, AuditChainVerify:
				return staff
			}
			return nil
		},
	}
}
