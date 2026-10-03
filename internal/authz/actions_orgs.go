// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the orgs module (03 §5.2), in table order.
const (
	OrgsOrgRead           Action = "orgs.org.read"
	OrgsOrgSearch         Action = "orgs.org.search"
	OrgsCompanyCreate     Action = "orgs.company.create"
	OrgsOrgUpdate         Action = "orgs.org.update"
	OrgsOrgListAdmin      Action = "orgs.org.list_admin"
	OrgsOrgVerify         Action = "orgs.org.verify"
	OrgsOrgSuspend        Action = "orgs.org.suspend"
	OrgsOrgClose          Action = "orgs.org.close"
	OrgsOwnershipTransfer Action = "orgs.ownership.transfer"
	OrgsMemberList        Action = "orgs.member.list"
	OrgsMemberRead        Action = "orgs.member.read"
	OrgsMemberJoin        Action = "orgs.member.join"
	OrgsMemberDecide      Action = "orgs.member.decide"
	OrgsMemberRemove      Action = "orgs.member.remove"
	OrgsMemberChangeRole  Action = "orgs.member.change_role"
	OrgsMembershipListOwn Action = "orgs.membership.list_own"
	OrgsMembershipLeave   Action = "orgs.membership.leave"
	OrgsInvitationCreate  Action = "orgs.invitation.create"
	OrgsInvitationList    Action = "orgs.invitation.list"
	OrgsInvitationRevoke  Action = "orgs.invitation.revoke"
	OrgsInvitationAccept  Action = "orgs.invitation.accept"
)

// System-only actions of the orgs module (03 §3.7): only SystemActor may perform them.
const (
	OrgsMemberEndRole Action = "orgs.member.end_role"
)

// orgsActions lists the HTTP actions of orgs in 03 §5.2 order.
var orgsActions = []Action{
	OrgsOrgRead,
	OrgsOrgSearch,
	OrgsCompanyCreate,
	OrgsOrgUpdate,
	OrgsOrgListAdmin,
	OrgsOrgVerify,
	OrgsOrgSuspend,
	OrgsOrgClose,
	OrgsOwnershipTransfer,
	OrgsMemberList,
	OrgsMemberRead,
	OrgsMemberJoin,
	OrgsMemberDecide,
	OrgsMemberRemove,
	OrgsMemberChangeRole,
	OrgsMembershipListOwn,
	OrgsMembershipLeave,
	OrgsInvitationCreate,
	OrgsInvitationList,
	OrgsInvitationRevoke,
	OrgsInvitationAccept,
}
