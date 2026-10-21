package authz

import "uuid"

// Resource types of the messaging module beyond 03 §3.5 (Policy.Types).
const (
	// TypeContactBlock is a student's block of a company (contact_blocks,
	// 02 §14.2): OwnerUserID = the student; the company never learns that
	// it exists (there is no org scope).
	TypeContactBlock = "contact_block"
)

// messagingPolicy is the policy of the messaging module (03 §5.9). Only tickets of
// the messaging module edit this file (BE-001 R5). Written by BE-056:
//
//	messaging.contact_request.create         cmm S·k19, cma S·k19 (the company's surface)
//	messaging.contact_request.list_sent      cmm S, cma S
//	messaging.contact_request.withdraw       cmm S, cma S
//	messaging.contact_request.read           company path (/orgs/{org_id}/...): cmm S, cma S;
//	                                         student path (/me/...): O
//	messaging.contact_request.list_received  O for every signed-in user (/me)
//	messaging.contact_request.respond        O
//	messaging.block.manage                   O
//	messaging.block.list_own                 O
//
// The company path is the org management surface (03 §3.6, §7.6): the
// recruiter (cmm) and the company_admin/owner (cma, k-mfa) of the company
// act; other members of the org and every platform role see the surface
// (403), everyone else gets 404. k19 (an active company, a target that is
// discoverable, adult, verified and has not blocked the company, the
// 90-day rule after a decline, the contact_request unit) is the service's
// (03 §3.3: I/O); a target that does not qualify is 404 there.
//
// The O cells (03 §5.9: usr…stf O, and through the usr column every
// account; 03 §5.0) are RoleSelf on the student's own resource: the /me
// paths pass contact_request without OrgID and OwnerUserID = the student
// (another student's request is not visible: 404, 03 §7.2); blocks pass
// contact_block with OwnerUserID = the student. Acceptance needs an active
// student verification (students.verification_required, the service:
// AccessGrants.Grant, BE-077); declining never does.
//
// Resource (03 §3.5; 06 H-27): contact_request with Attrs status; on the
// company path OrgID = the company of the path and OwnerUserID = the
// student (the O action read always carries it).
func messagingPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeContactBlock: nil},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case MessagingContactRequestCreate, MessagingContactRequestListSent, MessagingContactRequestWithdraw:
				return r.Type == TypeContactRequest && r.OrgID != nil && OrgSurfaceVisible(a, r.OrgID)
			case MessagingContactRequestRead:
				if r.Type != TypeContactRequest {
					return false
				}
				if r.OrgID != nil {
					return OrgSurfaceVisible(a, r.OrgID)
				}
				return a.Owns(r)
			case MessagingContactRequestListReceived, MessagingContactRequestRespond:
				return r.Type == TypeContactRequest && r.OrgID == nil && a.Owns(r)
			case MessagingBlockManage, MessagingBlockListOwn:
				return r.Type == TypeContactBlock && r.OrgID == nil && a.Owns(r)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			switch act {
			case MessagingContactRequestCreate, MessagingContactRequestListSent, MessagingContactRequestWithdraw:
				return messagingCompanyGrants(a, r.OrgID)
			case MessagingContactRequestRead:
				if r.OrgID != nil {
					return messagingCompanyGrants(a, r.OrgID)
				}
				return []Grant{{Role: RoleSelf}}
			case MessagingContactRequestListReceived, MessagingContactRequestRespond, MessagingBlockManage,
				MessagingBlockListOwn:
				return []Grant{{Role: RoleSelf}}
			}
			return nil
		},
	}
}

// messagingCompanyGrants returns the S grants of cmm (recruiter) and cma
// (company_admin, company_owner) the actor holds in orgID (03 §1.2; T-59:
// the recruiter spends the contact_request quota too).
func messagingCompanyGrants(a Actor, orgID *uuid.UUID) []Grant {
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
