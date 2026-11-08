package authz

// statsPolicy is the policy of the stats module (03 §5.23; BE-163): the
// read-only panel summaries of SR §8. Only tickets of the stats module
// edit this file (BE-001 R5).
//
//	stats.platform.read    platform_admin (admin surface)
//	stats.club.read        S: club_admin of the club; platform_admin (not on a sensitive club)
//	stats.company.read     S: company_member (recruiter), company_admin/owner; platform_admin
//	stats.university.read  S: university_rep, career_staff; platform_admin
//	stats.queues.read      every platform role (admin surface; BE-196)
//
// Resources (03 §3.5): the org panels pass the TypeOrg resource of the
// path's org (org_kind, org_status) and, for the club panel, Attrs
// sensitive. The org surface is visible to the org's active members (a
// member without the role gets 403) and to every platform role (403
// unless platform_admin); everyone else gets 404 (03 §3.6). A sensitive
// club's panel is visible to its members only (Y2: its member figures are
// membership data; platform staff get 404, like the member list, k4). The
// platform panel and the queue sizes are the admin surface (03 §7.2-3):
// 403 without the role. The queue sizes are open to every platform role:
// each counter is gated again by its queue's own list action (k37), so the
// action only says "may ask which of the queues it works are how long".
// MFA follows from the role paths (k-mfa: uni, cma, pad; not cla, cmm,
// career_staff).
func statsPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case StatsPlatformRead, StatsQueuesRead:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			case StatsClubRead:
				if r.Type != TypeOrg {
					return false
				}
				if r.Flag(AttrSensitive) {
					return r.OrgID != nil && a.MemberOf(*r.OrgID) // Y2: no staff
				}
				return OrgSurfaceVisible(a, r.OrgID)
			case StatsCompanyRead, StatsUniversityRead:
				return r.Type == TypeOrg && OrgSurfaceVisible(a, r.OrgID)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			pad := Grant{Role: RolePlatformAdmin}
			switch act {
			case StatsPlatformRead:
				return []Grant{pad}
			case StatsQueuesRead: // k37: every platform role; the counters are gated again
				return []Grant{{Role: RoleModerator}, {Role: RoleSupport}, {Role: RoleDPO}, pad,
					{Role: RoleFinanceAdmin}, {Role: RoleTalentAuditor}}
			default:
			}
			var gs []Grant
			if act != StatsClubRead || !r.Flag(AttrSensitive) { // Y2: no staff on a sensitive club
				gs = append(gs, pad)
			}
			if r.OrgID == nil {
				return gs
			}
			org := *r.OrgID
			switch act {
			case StatsClubRead:
				if a.HasOrgRole(org, MemberClubAdmin) {
					gs = append(gs, Grant{Role: RoleClubAdmin})
				}
			case StatsCompanyRead:
				if a.HasOrgRole(org, MemberRecruiter) {
					gs = append(gs, Grant{Role: RoleCompanyMember})
				}
				if a.HasOrgRole(org, MemberCompanyAdmin, MemberCompanyOwner) {
					gs = append(gs, Grant{Role: RoleCompanyAdmin})
				}
			case StatsUniversityRead:
				if a.HasOrgRole(org, MemberUniversityRep) {
					gs = append(gs, Grant{Role: RoleUniversityRep})
				}
				if a.HasOrgRole(org, MemberCareerStaff) {
					gs = append(gs, Grant{Role: RoleCareerStaff})
				}
			default:
				return nil
			}
			return gs
		},
	}
}
