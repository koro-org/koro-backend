package authz

// ledgerPolicy is the policy of the ledger module (03 §5.17). Only tickets of
// the ledger module edit this file (BE-001 R5). Written by BE-039.
//
//	ledger.journal.list   platform_admin, finance_admin (admin surface)
//
// The admin surface is visible to every signed-in actor: without a platform
// role the answer is 403 before anything is read (03 §3.2, §7.2-3). MFA
// follows from the role paths (k-mfa: pad, fin). The balance and payout
// actions (ledger.balance.read, ledger.payable.list, ledger.payout.*) stay
// default deny until BE-044 binds their endpoints.
func ledgerPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case LedgerJournalList:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(_ Actor, act Action, _ Resource) []Grant {
			switch act {
			case LedgerJournalList:
				return []Grant{{Role: RolePlatformAdmin}, {Role: RoleFinanceAdmin}}
			}
			return nil
		},
	}
}
