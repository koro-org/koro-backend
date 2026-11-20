package parties_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/parties"
)

// TestParties_PartyActsAsMember (BE-214, Q-69): on its org's surface a
// finance admin who is club_a's club_admin submits a bank account as the
// club_admin (pending_verification: both eyes still to come, no admin
// submission), and a platform admin who is company_a's company_admin reads
// its buyer party with the tax identifiers masked. Staff of no org keep the
// unmasked identifiers.
func TestParties_PartyActsAsMember(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	join := func(a authz.Actor, o uuid.UUID, kind authz.OrgKind, role authz.MemberRole) authz.Actor {
		a.Memberships = append(slices.Clone(a.Memberships), authz.Membership{OrgID: o, OrgKind: kind, Role: role})
		return a
	}
	py := e.payee(t, vknC)
	club := org("club_a")
	fin := join(actor(t, "finance_admin"), club, authz.OrgClub, authz.MemberClubAdmin)
	b, err := e.svc.SubmitPayeeBankAccount(ctxOf(t, fin), fin, club, py.Payee.ID, e.accountInput(t, ibanA, "finance_admin"), proof())
	if err != nil || b.Account.Status != parties.AccountPendingVerification || b.Account.SubmittedByAdmin {
		t.Fatalf("the party's submission = %+v, %v", b.Account, err)
	}

	buyer := e.buyer(t, vknA)
	company := org("company_a")
	pad := join(actor(t, "platform_admin"), company, authz.OrgCompany, authz.MemberCompanyAdmin)
	v, err := e.svc.GetOrgParty(ctxOf(t, pad), pad, company, buyer.Party.ID)
	if err != nil || v.FullTaxIDs {
		t.Fatalf("the party's read = full %v, %v", v.FullTaxIDs, err)
	}
	outsider := actor(t, "platform_admin")
	if v, err := e.svc.GetOrgParty(ctxOf(t, outsider), outsider, company, buyer.Party.ID); err != nil || !v.FullTaxIDs {
		t.Fatalf("a platform admin of no company = full %v, %v", v.FullTaxIDs, err)
	}
}
