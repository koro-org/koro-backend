package authz

// kvkkPolicy is the policy of the kvkk module (03 §5.22). Only tickets of
// the kvkk module edit this file (BE-001 R5). Written by BE-021 (data
// subject requests) and BE-049 (retention, destruction log, breaches,
// sub-processors).
//
//	kvkk.dsr.create, list_own,    O for every signed-in user (k31: erasure
//	read_own, cancel_own          needs R, checked by the service)
//	kvkk.dsr.export_download      O, dpo ✓
//	kvkk.dsr.list, read           dpo ✓, platform_admin k32 (only requests
//	                              awaiting the second approval)
//	kvkk.dsr.create_admin,        dpo ✓ (complete: R in the service)
//	update, complete, reject
//	kvkk.dsr.approve              dpo ✓·4, platform_admin ✓·4·k32 (second
//	                              stage only); four eyes: first_approver ≠
//	                              actor (core table, 03 §6.1)
//
//	kvkk.retention_policy.list    dpo ✓, platform_admin ✓
//	kvkk.retention_policy.update  dpo ✓ (R in the service)
//	kvkk.destruction_log.list     dpo ✓, platform_admin ✓
//	kvkk.breach.list, manage      dpo ✓, platform_admin ✓
//	kvkk.subprocessor.list        dpo ✓, platform_admin ✓
//	kvkk.subprocessor.manage      dpo ✓
//	kvkk.metrics.read             platform_admin ✓ (BE-164: product KPIs of
//	                              SR §6, aggregates only, k ≥ 10)
//
// The kvkk-2 records have no owner and no org: the resource is always the
// admin surface (visible to every user, 403 without the role).
//
// MFA follows from the role paths (dpo, platform_admin: k-mfa).
//
// Resources (03 §3.5; 06 H-27):
//
//	dsr            OwnerUserID = the data subject; Attrs type, status,
//	               first_approver ("" before the first approval) and, on
//	               approve, stage ("first" | "second")
//	admin_surface  the pre-check of every /admin operation (403 before
//	               anything is read, 03 §7.2-3); approve carries
//	               first_approver "" there
//
// Visibility (03 §3.6): /me requests are the actor's own (the service reads
// them by owner, so a foreign ID is 404 before authz); the admin surface is
// visible to every user (403 without the role). k32: a platform_admin who
// is not dpo does not see requests in any other status (404).
func kvkkPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case KvkkDsrCreate, KvkkDsrListOwn, KvkkDsrReadOwn, KvkkDsrCancelOwn:
				return r.Type == TypeDSR && a.Owns(r)
			case KvkkDsrExportDownload:
				if r.Type == TypeDSR && a.Owns(r) {
					return true
				}
				return kvkkAdminVisible(a, r)
			case KvkkDsrList, KvkkDsrRead, KvkkDsrCreateAdmin, KvkkDsrUpdate, KvkkDsrApprove,
				KvkkDsrComplete, KvkkDsrReject:
				return kvkkAdminVisible(a, r)
			case KvkkRetentionPolicyList, KvkkRetentionPolicyUpdate, KvkkDestructionLogList, KvkkBreachList,
				KvkkBreachManage, KvkkSubprocessorList, KvkkSubprocessorManage, KvkkMetricsRead:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			switch act {
			case KvkkDsrCreate, KvkkDsrListOwn, KvkkDsrReadOwn, KvkkDsrCancelOwn:
				return []Grant{{Role: RoleSelf}}
			case KvkkDsrExportDownload:
				return []Grant{{Role: RoleSelf}, {Role: RoleDPO}}
			case KvkkDsrList, KvkkDsrRead:
				return []Grant{{Role: RoleDPO}, {Role: RolePlatformAdmin}}
			case KvkkDsrCreateAdmin, KvkkDsrUpdate, KvkkDsrComplete, KvkkDsrReject,
				KvkkRetentionPolicyUpdate, KvkkSubprocessorManage:
				return []Grant{{Role: RoleDPO}}
			case KvkkRetentionPolicyList, KvkkDestructionLogList, KvkkBreachList, KvkkBreachManage, KvkkSubprocessorList:
				return []Grant{{Role: RoleDPO}, {Role: RolePlatformAdmin}}
			case KvkkMetricsRead:
				return []Grant{{Role: RolePlatformAdmin}}
			case KvkkDsrApprove:
				gs := []Grant{{Role: RoleDPO}}
				// k32: platform_admin gives the second approval only. The
				// admin pre-check (no stage yet) lets it through to the row.
				if r.Type == TypeAdminSurface || r.Attr("stage") == "second" {
					gs = append(gs, Grant{Role: RolePlatformAdmin})
				}
				return gs
			}
			return nil
		},
	}
}

// kvkkAdminVisible is the DSR admin surface: visible to every user (403
// without a role); a request row is hidden from a platform_admin who is
// not dpo unless it awaits the second approval (03 k32).
func kvkkAdminVisible(a Actor, r Resource) bool {
	if !AdminSurfaceVisible(a) {
		return false
	}
	switch r.Type {
	case TypeAdminSurface:
		return true
	case TypeDSR:
		if a.HasPlatformRole(PlatformAdmin) && !a.HasPlatformRole(PlatformDPO) {
			return r.Attr(AttrStatus) == "awaiting_second_approval"
		}
		return true
	}
	return false
}
