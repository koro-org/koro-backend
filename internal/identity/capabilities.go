package identity

import (
	"slices"

	"koro/internal/authz"
)

// Capabilities returns the menu hints of getMe (04 §2.1): actions the
// client may offer the actor, derived from its role paths (03 §1.1). They
// are not authorization decisions (every request goes through authz.Can),
// so the table only needs to be useful, not exhaustive; talent actions are
// never listed (03 Y1, 04 §V.0).
func Capabilities(a authz.Actor) []authz.Action {
	if !a.IsUser() {
		return nil
	}
	set := map[authz.Action]bool{}
	add := func(acts ...authz.Action) {
		for _, x := range acts {
			set[x] = true
		}
	}
	// Every signed-in user: own account, consents, notifications, requests.
	add(authz.IdentityAccountUpdateOwn, authz.IdentityPasswordChange, authz.IdentityEmailChangeRequest,
		authz.IdentitySessionListOwn, authz.ConsentRecordListOwn, authz.NotifyPreferenceReadOwn,
		authz.OrgsMembershipListOwn, authz.KvkkDsrCreate, authz.IdentityEduVerificationReadOwn)
	if a.IsVerifiedStudent() {
		add(authz.StudentsProfileReadOwn, authz.EventsRegistrationListOwn, authz.OrgsMemberJoin)
		if !a.IsMinor {
			add(authz.DirectoryClubClaimCreate)
		}
	} else {
		add(authz.IdentityEduVerificationStart)
	}
	for _, m := range a.Memberships {
		switch m.Role {
		case authz.MemberClubAdmin:
			if !a.IsMinor {
				add(authz.EventsEventCreate, authz.OrgsMemberList, authz.FeedPostCreate, authz.CatalogOfferingCreate)
			}
		case authz.MemberUniversityRep:
			add(authz.EventsEventCreate, authz.OrgsMemberList, authz.FeedPostCreate, authz.CatalogOfferingUniversityDecide)
		case authz.MemberCareerStaff:
			add(authz.EventsEventCreate, authz.FeedPostCreate)
		case authz.MemberCompanyAdmin, authz.MemberCompanyOwner:
			add(authz.OrdersOrderCreate, authz.DiscoveryProfileSearch, authz.OutreachSegmentManage,
				authz.OrgsMemberList, authz.FeedPostCreate, authz.EntitlementsEntitlementList)
		case authz.MemberRecruiter:
			add(authz.DiscoveryProfileSearch, authz.MessagingContactRequestCreate, authz.FeedPostCreate)
		default:
		}
	}
	for _, r := range a.PlatformRoles {
		switch r {
		case authz.PlatformAdmin:
			add(authz.IdentityUserSearch, authz.IdentityPlatformRoleList, authz.IdentityEduVerificationList,
				authz.OrgsOrgListAdmin, authz.CatalogOfferingReviewQueue, authz.AuditEventList, authz.DirectoryClubClaimList)
		case authz.PlatformSupport:
			add(authz.IdentityUserSearch, authz.IdentityEduVerificationList)
		case authz.PlatformDPO:
			add(authz.IdentityUserSearch, authz.KvkkDsrList, authz.ConsentNoticeManage, authz.AuditEventList)
		case authz.PlatformModerator:
			add(authz.FeedReviewQueueList, authz.FeedReportList)
		case authz.PlatformFinanceAdmin:
			add(authz.PaymentsTransferList, authz.LedgerPayoutList, authz.PaymentsRefundList, authz.BillingInvoiceListAdmin)
		default: // talent_auditor: talent is never a menu hint
		}
	}
	out := make([]authz.Action, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	slices.Sort(out)
	return out
}
