package orders_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/problem/problemtest"
)

func modelCPolicy(p *catalog.CommercePolicy) {
	p.ModelCode, p.CollectionMethod, p.CommissionPayer = catalog.ModelC, catalog.CollectDirectToPayee, catalog.PayerBuyer
}

func TestModelC_DirectInstructionsMultiplePayees(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.payees.add("club_b", parties.PayeeActive)
	e.payees.accounts = map[uuid.UUID]parties.BankAccount{}
	for _, id := range []uuid.UUID{e.payeeA, b} {
		py := e.payees.m[id]
		py.LegalPartyID = e.parties.add(parties.LegalParty{Kind: parties.KindPayee, LegalType: parties.LegalFoundation, Title: "Synthetic Foundation", Status: parties.StatusVerified})
		e.payees.m[id] = py
		e.payees.accounts[id] = parties.BankAccount{ID: uuid.NewV7(), PayeeID: id, IBAN: "TR809999900000000000000001", HolderName: "Synthetic Foundation", Status: parties.AccountApproved}
	}
	aOff := e.offering(t, nil)
	bOff := e.offering(t, func(o *catalog.Offering) { o.OrgID = org("club_b"); o.DefaultPayeeID = &b })
	for _, id := range []uuid.UUID{aOff, bOff} {
		e.addPolicy(t, catalog.ScopeOffering, &id, modelCPolicy)
	}
	o := e.place(t, line(aOff, 2), line(bOff, 1))[0].Order
	e.acct = &orders.CollectionAccount{IBAN: "TR039999901234567890123456", HolderName: "Synthetic Koro"}
	buyer := actor(t, "company_admin")
	got, err := e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID)
	if err != nil || len(got.DirectPayments) != 2 || got.AmountDue != 360_000 {
		t.Fatalf("instructions=%+v,%v", got, err)
	}
	amounts := map[uuid.UUID]int64{}
	for _, p := range got.DirectPayments {
		amounts[p.PayeeID] = p.Amount
	}
	if amounts[e.payeeA] != 2_400_000 || amounts[b] != 1_200_000 {
		t.Fatalf("payee amounts=%v", amounts)
	}
	if n := len(e.audit.Of(authz.OrdersOrderPaymentInstructions)); n != 1 {
		t.Fatalf("access audit=%d", n)
	}
	for _, name := range []string{"company_member", "finance_admin"} {
		a := actor(t, name)
		v, err := e.svc.PaymentInstructions(ctxOf(t, a), a, o.ID)
		if err != nil || len(v.DirectPayments) != 0 {
			t.Fatalf("%s instructions=%+v,%v", name, v, err)
		}
	}
	// A lower-privilege grant must not bypass MFA on a second admin role.
	mixed := actor(t, "company_member")
	mixed.MFAVerified = false
	mixed.Memberships = append(append([]authz.Membership{}, mixed.Memberships...), authz.Membership{OrgID: o.OrgID, OrgKind: authz.OrgCompany, Role: authz.MemberCompanyAdmin})
	if v, err := e.svc.GetOrder(ctxOf(t, mixed), mixed, o.ID); err != nil || len(v.DirectPayments) != 0 {
		t.Fatalf("mixed MFA bypass=%+v,%v", v, err)
	}
	delete(e.payees.accounts, b)
	if _, err := e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID); !errors.Is(err, parties.ErrNoApprovedAccount) {
		t.Fatalf("missing approved account=%v", err)
	}
	// Once direct payment is confirmed, fee-only instructions cannot tell the buyer to pay the payees again.
	if eff, err := e.mod.Orders.ConfirmDirectPayment(t.Context(), o.ID, uuid.NewV7()); err != nil || eff.Paid {
		t.Fatalf("early confirmation=%+v,%v", eff, err)
	}
	if v, err := e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID); err != nil || len(v.DirectPayments) != 0 || v.AmountDue != 360_000 {
		t.Fatalf("confirmed instructions=%+v,%v", v, err)
	}
	if eff, err := e.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 360_000); err != nil || !eff.Paid {
		t.Fatalf("late fee=%+v,%v", eff, err)
	}
}

func TestModelC_PlatformProductRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	offer := e.platformOffering(t)
	e.addPolicy(t, catalog.ScopeOffering, &offer, modelCPolicy)
	buyer := actor(t, "company_admin")
	_, err := e.svc.CreateOrder(ctxOf(t, buyer), buyer, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: []orders.LineInput{line(offer, 1)}})
	problemtest.WantProblem(t, err, 422, "validation_failed")
}

func TestModelC_ZeroFeeNeedsNoCollectionAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	py := e.payees.m[e.payeeA]
	py.LegalPartyID = e.parties.add(parties.LegalParty{Kind: parties.KindPayee, LegalType: parties.LegalFoundation, Title: "Synthetic Foundation", Status: parties.StatusVerified})
	e.payees.m[e.payeeA] = py
	e.payees.accounts = map[uuid.UUID]parties.BankAccount{e.payeeA: {ID: uuid.NewV7(), PayeeID: e.payeeA, IBAN: "TR809999900000000000000001", HolderName: "Synthetic Foundation", Status: parties.AccountApproved}}
	offer := e.offering(t, nil)
	e.addPolicy(t, catalog.ScopeOffering, &offer, func(p *catalog.CommercePolicy) {
		modelCPolicy(p)
		p.CommissionRateBP = 0
	})
	o := e.place(t, line(offer, 1))[0].Order
	buyer := actor(t, "company_admin")
	got, err := e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID)
	if err != nil || got.AmountDue != 0 || got.IBAN != "" || len(got.DirectPayments) != 1 || got.DirectPayments[0].Amount != 1_200_000 {
		t.Fatalf("zero-fee instructions=%+v,%v", got, err)
	}
	if eff, err := e.mod.Orders.ConfirmDirectPayment(t.Context(), o.ID, uuid.NewV7()); err != nil || !eff.Paid {
		t.Fatalf("zero-fee confirmation=%+v,%v", eff, err)
	}
}
