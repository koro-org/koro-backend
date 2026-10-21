package authz

import "slices"

// talentPolicy is the policy of the talent module (03 §5.12). Only tickets of
// the talent module edit this file (BE-001 R5). BE-047: the system actions;
// BE-052: the talent_auditor endpoints (04 §T).
//
//	talent.report.read            talent_auditor (k-flag)
//	talent.distribution.read      talent_auditor (k-flag)
//	talent.neutralization.manage  talent_auditor (k-flag, R)
//	talent.reversal.create        talent_auditor (k-flag, R)
//	talent.config.read            talent_auditor (k-flag)
//	talent.config.manage          talent_auditor (k-flag, R)
//
// Every other role is ✗ (03 §5.12), platform_admin included: the core
// drops any other role path of a talent action anyway (Y1). MFA follows
// from the role (k-mfa). Neither the flag (talent.auditor_api_enabled: the
// service answers 404 before calling Can; authz reads no configuration)
// nor the re-authentication proof (R: I/O, the service after Can) is
// decided here.
//
// Resources: talent_subject (OwnerUserID = the subject, nil before the
// row naming the subject is read) and admin_surface (distribution,
// config). /admin/talent/** is an admin surface: visible to every user, so
// an actor without the role gets 403 before anything is read (03 §7.2-3),
// the subject included (no O cell: a student never reads their talent).
//
// The talent consumers act as SystemActor(<subscription>) on
// talent.ledger.write and the nightly score as
// SystemActor("talent.recompute") on talent.score.compute; both are on the
// system allow-list of 03 §3.7, decided by the core before any module
// policy (TestTalentPolicy_SystemActionsOnly).
func talentPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			if !slices.Contains(talentActions, act) {
				return false
			}
			return (r.Type == TypeTalentSubject || r.Type == TypeAdminSurface) && AdminSurfaceVisible(a)
		},
		Grants: func(_ Actor, act Action, _ Resource) []Grant {
			if slices.Contains(talentActions, act) {
				return []Grant{{Role: RoleTalentAuditor}}
			}
			return nil
		},
	}
}
