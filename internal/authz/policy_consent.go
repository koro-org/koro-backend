package authz

// consentPolicy is the policy of the consent module (03 §5.5). Only tickets of
// the consent module edit this file (BE-001 R5). Written by BE-012.
//
//	consent.notice.read        P (everyone, anonymous included)
//	consent.record.create      O for every signed-in user (k12, minors, is a
//	consent.record.list_own    service rule: 422 consent.minor_restricted)
//	consent.record.list_admin  dpo ✓, support B (break-glass reason)
//	consent.notice.manage      dpo · R (re-authentication in the service)
//	consent.purpose.manage     dpo · R
//
// MFA follows from the role paths (dpo, support: k-mfa).
//
// Resources (03 §3.5; 06 H-27):
//
//	notice_version  no org, no owner; Attrs status (draft|published|retired):
//	                drafts are visible to an MFA-verified dpo only, so a
//	                missing status hides the notice (safe default)
//	consent_record  OwnerUserID = the data subject (record.create and
//	                list_own: the actor itself; list_admin: the target
//	                user), Attrs access_reason on list_admin
//	admin_surface   consent.purpose.manage
//
// Visibility (03 §3.6): /me/consents is the actor's own ledger (another
// subject → 404); the /admin operations are the admin surface (visible to
// every user, 403 without the role, 03 §7.2-3).
func consentPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case ConsentNoticeRead:
				if r.Type != TypeNoticeVersion {
					return false
				}
				switch r.Attr(AttrStatus) {
				case "published", "retired":
					return true
				case "draft":
					return a.HasPlatformRole(PlatformDPO) && a.MFAVerified
				}
				return false
			case ConsentRecordCreate, ConsentRecordListOwn:
				return r.Type == TypeConsentRecord && a.Owns(r)
			case ConsentRecordListAdmin:
				return r.Type == TypeConsentRecord && r.OwnerUserID != nil && AdminSurfaceVisible(a)
			case ConsentNoticeManage:
				return r.Type == TypeNoticeVersion && AdminSurfaceVisible(a)
			case ConsentPurposeManage:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(_ Actor, act Action, _ Resource) []Grant {
			switch act {
			case ConsentNoticeRead:
				return []Grant{{Role: RolePublic}}
			case ConsentRecordCreate, ConsentRecordListOwn:
				return []Grant{{Role: RoleSelf}}
			case ConsentRecordListAdmin:
				return []Grant{{Role: RoleDPO}, {Role: RoleSupport, BreakGlass: true}}
			case ConsentNoticeManage, ConsentPurposeManage:
				return []Grant{{Role: RoleDPO}}
			}
			return nil
		},
	}
}
