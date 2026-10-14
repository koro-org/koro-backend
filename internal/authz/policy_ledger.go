package authz

// ledgerPolicy is the policy of the ledger module (03 §5.17). Only tickets of
// the ledger module edit this file (BE-001 R5). Written by BE-039 (the
// journal); BE-044 adds the payee balance and the payout flow:
//
//	ledger.journal.list        admin surface: pad, fin
//	ledger.balance.read        org surface (payee): cla S, uni S; pad, fin
//	ledger.payable.list        admin surface: fin
//	ledger.payout.list / read  org surface (payout): cla S, uni S; pad, fin
//	                           (admin surface: pad, fin)
//	ledger.payout.create       admin surface: fin
//	ledger.payout.approve      on the payout: fin·4·R (never its creator: the core four-eyes
//	                           table; R is the service's, 03 §6.2). The core table demands
//	                           created_by on every resource of this action, so the service's
//	                           "403 before reading" gate of adminApprovePayout is
//	                           ledger.payout.read on the admin surface.
//	ledger.payout.mark_paid    admin surface: fin (any finance admin; the receipt and the bank
//	ledger.payout.mark_failed  reference are the service's)
//	ledger.payout.cancel       admin surface: fin
//
// Resources: admin_surface (the /admin operations: 403 without a platform
// role before anything is read; 03 §3.2, §7.2-3), payee (OrgID = the org
// of the path: the balance) and payout (OrgID = the seller org; Attrs
// status, created_by — empty on the org's collection).
//
// Org surface (03 §3.6 "Payee, banka hesabı, payout, hakediş bakiyesi"):
// visible to the org's active members (403 without a role) and to every
// platform role; anyone else gets 404. k-cs: the uni column is
// university_rep, never career_staff. MFA follows from the role paths
// (k-mfa: uni, pad, fin).
func ledgerPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch r.Type {
			case TypeAdminSurface:
				switch act {
				case LedgerJournalList, LedgerPayableList, LedgerPayoutList, LedgerPayoutRead, LedgerPayoutCreate,
					LedgerPayoutMarkPaid, LedgerPayoutMarkFailed, LedgerPayoutCancel:
					return AdminSurfaceVisible(a)
				}
			case TypePayee:
				return act == LedgerBalanceRead && OrgSurfaceVisible(a, r.OrgID)
			case TypePayout:
				switch act {
				case LedgerPayoutList, LedgerPayoutRead:
					return r.OrgID != nil && OrgSurfaceVisible(a, r.OrgID)
				case LedgerPayoutApprove:
					return a.HasAnyPlatformRole()
				}
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			staff := []Grant{{Role: RolePlatformAdmin}, {Role: RoleFinanceAdmin}}
			fin := []Grant{{Role: RoleFinanceAdmin}}
			switch r.Type {
			case TypeAdminSurface:
				switch act {
				case LedgerJournalList, LedgerPayoutList, LedgerPayoutRead:
					return staff
				case LedgerPayableList, LedgerPayoutCreate, LedgerPayoutMarkPaid, LedgerPayoutMarkFailed, LedgerPayoutCancel:
					return fin
				}
			case TypePayee:
				if act == LedgerBalanceRead {
					return orgSellerGrants(a, r, staff)
				}
			case TypePayout:
				switch act {
				case LedgerPayoutApprove:
					// The eyes: the core table refuses the payout's creator.
					return fin
				case LedgerPayoutList, LedgerPayoutRead:
					return orgSellerGrants(a, r, staff)
				}
			}
			return nil
		},
	}
}

// orgSellerGrants are the seller cells of the ledger's org surface (03
// §5.17 S: the club admin of the club, the university rep of the
// university; k-cs) plus the staff.
func orgSellerGrants(a Actor, r Resource, staff []Grant) []Grant {
	if r.OrgID == nil {
		return nil
	}
	gs := staff
	if a.HasOrgRole(*r.OrgID, MemberClubAdmin) {
		gs = append(gs, Grant{Role: RoleClubAdmin})
	}
	if a.HasOrgRole(*r.OrgID, MemberUniversityRep) {
		gs = append(gs, Grant{Role: RoleUniversityRep})
	}
	return gs
}
