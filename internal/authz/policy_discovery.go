package authz

import "uuid"

// discoveryPolicy is the policy of the discovery module (03 §5.10). Only tickets of
// the discovery module edit this file (BE-001 R5). Written by BE-051:
//
//	discovery.profile.search  cmm S·k20, cma S·k20: the recruiter and the company_admin/owner of the
//	discovery.profile.read    company, on its own org surface (/orgs/{org_id}/discovery/...).
//	                          Active members of the org without these roles and every platform
//	                          role see the surface (403: "platform rolleri dahil başka kimse keşif
//	                          yüzeyini kullanamaz"); everyone else 404 (03 §3.6, §7.6).
//
// k20 (an active company with a running discovery_access right: 403
// orgs.org_suspended / orgs.org_not_verified / entitlements.not_entitled)
// and "another company's anon_id → 404" are the service's (03 §3.3: the
// entitlement and the pseudonym's resolution are I/O).
//
// Resource (03 §3.5): anon_profile with OrgID = the company of the path and
// Attrs company_org_id = the company the pseudonym is derived for. The
// discovery service only ever resolves a pseudonym under the path's
// company, so both are equal; a resource whose company_org_id names
// another org is not visible (defence in depth: a pseudonym is never
// read for a company other than its own). MFA follows from the role paths
// (k-mfa: company_admin; not company_member).
func discoveryPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			return r.Type == TypeAnonProfile && r.OrgID != nil && r.Attr("company_org_id") == r.OrgID.String() &&
				OrgSurfaceVisible(a, r.OrgID)
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			switch act {
			case DiscoveryProfileSearch, DiscoveryProfileRead:
				return discoveryGrants(a, r.OrgID)
			}
			return nil
		},
	}
}

// discoveryGrants returns the S grants of cmm (recruiter) and cma
// (company_admin, company_owner) the actor holds in orgID (03 §1.2).
func discoveryGrants(a Actor, orgID *uuid.UUID) []Grant {
	if orgID == nil {
		return nil
	}
	var gs []Grant
	if a.HasOrgRole(*orgID, MemberRecruiter) {
		gs = append(gs, Grant{Role: RoleCompanyMember})
	}
	if a.HasOrgRole(*orgID, MemberCompanyAdmin, MemberCompanyOwner) {
		gs = append(gs, Grant{Role: RoleCompanyAdmin})
	}
	return gs
}
