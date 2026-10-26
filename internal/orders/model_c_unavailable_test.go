package orders_test

import (
	"net/http"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/problem/problemtest"
)

// Synthetic payee accounts (bank code 99999 is not a bank); the fakes never
// validate them.
const (
	payeeIBAN1 = "TR809999900000000000000001"
	payeeIBAN2 = "TR809999900000000000000002"
)

// directOrder places an unpaid model C order of two lines: one of club_a's
// payee A, one of club_b's payee B, both foundations with an approved
// account. It returns the order and payee B.
func (e *env) directOrder(t *testing.T) (orders.Order, uuid.UUID) {
	t.Helper()
	b := e.payees.add("club_b", parties.PayeeActive)
	e.payees.accounts = map[uuid.UUID]parties.BankAccount{}
	for _, id := range []uuid.UUID{e.payeeA, b} {
		py := e.payees.m[id]
		py.LegalPartyID = e.parties.add(parties.LegalParty{Kind: parties.KindPayee, LegalType: parties.LegalFoundation,
			Title: "Synthetic Foundation", Status: parties.StatusVerified})
		e.payees.m[id] = py
		e.payees.accounts[id] = parties.BankAccount{ID: uuid.NewV7(), PayeeID: id, IBAN: payeeIBAN1,
			HolderName: "Synthetic Foundation", Status: parties.AccountApproved}
	}
	aOff := e.offering(t, nil)
	bOff := e.offering(t, func(o *catalog.Offering) { o.OrgID = org("club_b"); o.DefaultPayeeID = &b })
	for _, id := range []uuid.UUID{aOff, bOff} {
		e.addPolicy(t, catalog.ScopeOffering, &id, modelCPolicy)
	}
	e.acct = &orders.CollectionAccount{IBAN: "TR039999901234567890123456", HolderName: "Synthetic Koro"}
	return e.place(t, line(aOff, 2), line(bOff, 1))[0].Order, b
}

// TestGetOrder_ModelC_PayeeAccountRevoked (BE-109, BE-127): a payee's
// approved account is revoked while a model C order waits for payment.
// getOrder still answers the whole order to the buyer administrator, with
// direct_payments_status unavailable; it never carries a payee account and
// never audits an access; the payment instructions keep their 409. A newly
// approved account shows again in the instructions (and is audited).
func TestGetOrder_ModelC_PayeeAccountRevoked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o, b := e.directOrder(t)
	buyer := actor(t, "company_admin")
	v, err := e.svc.GetOrder(ctxOf(t, buyer), buyer, o.ID)
	if err != nil || v.DirectStatus != orders.DirectAvailable {
		t.Fatalf("approved accounts: status=%q err=%v", v.DirectStatus, err)
	}
	if n := len(e.audit.Of(authz.OrdersOrderPaymentInstructions)); n != 0 {
		t.Fatalf("getOrder audited an account access: %d", n)
	}

	delete(e.payees.accounts, b) // revoked (or superseded and never re-approved)
	v, err = e.svc.GetOrder(ctxOf(t, buyer), buyer, o.ID)
	if err != nil {
		t.Fatalf("GetOrder without B's account: %v", err)
	}
	if v.DirectStatus != orders.DirectUnavailable {
		t.Fatalf("revoked: status=%q", v.DirectStatus)
	}
	if v.Order.ID != o.ID || v.Order.DirectToPayeeAmount != 3_600_000 || len(v.Lines) != 2 || v.SellerView {
		t.Fatalf("the rest of the order: %+v lines=%d", v.Order, len(v.Lines))
	}
	if n := len(e.audit.Of(authz.OrdersOrderPaymentInstructions)); n != 0 {
		t.Fatalf("an unavailable read was audited as an account access: %d", n)
	}
	_, err = e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "parties.no_approved_account")

	// Readers that never see the direct transfers get no status either.
	for _, name := range []string{"company_member", "finance_admin", "platform_admin", "support"} {
		a := actor(t, name)
		v, err := e.svc.GetOrder(ctxOf(t, a), a, o.ID)
		if err != nil || v.DirectStatus != "" {
			t.Fatalf("%s: status=%q err=%v", name, v.DirectStatus, err)
		}
	}
	// A buyer member who also holds an admin role on an MFA-less session
	// reads the order without the direct part (no status either).
	mixed := actor(t, "company_member")
	mixed.MFAVerified = false
	mixed.Memberships = append(append([]authz.Membership{}, mixed.Memberships...),
		authz.Membership{OrgID: o.OrgID, OrgKind: authz.OrgCompany, Role: authz.MemberCompanyAdmin})
	if v, err := e.svc.GetOrder(ctxOf(t, mixed), mixed, o.ID); err != nil || v.DirectStatus != "" {
		t.Fatalf("admin role without MFA: status=%q err=%v", v.DirectStatus, err)
	}

	// Finance approves B's new account: the instructions name it again.
	e.payees.accounts[b] = parties.BankAccount{ID: uuid.NewV7(), PayeeID: b, IBAN: payeeIBAN2,
		HolderName: "Synthetic Foundation", Status: parties.AccountApproved}
	v, err = e.svc.GetOrder(ctxOf(t, buyer), buyer, o.ID)
	if err != nil || v.DirectStatus != orders.DirectAvailable {
		t.Fatalf("re-approved: status=%q err=%v", v.DirectStatus, err)
	}
	ins, err := e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID)
	if err != nil || len(ins.DirectPayments) != 2 {
		t.Fatalf("re-approved instructions: direct=%d err=%v", len(ins.DirectPayments), err)
	}
	for _, p := range ins.DirectPayments {
		if p.PayeeID == b && p.IBAN != payeeIBAN2 {
			t.Fatalf("payee B shows %q, want its current approved account", p.IBAN)
		}
	}
	if n := len(e.audit.Of(authz.OrdersOrderPaymentInstructions)); n != 1 {
		t.Fatalf("access audit=%d, want 1 (the instructions only)", n)
	}
}

// TestGetOrder_ModelC_UnavailableStillCancellable (BE-109): with no approved
// account the buyer reads the order and may still cancel it; a cancelled
// order shows no direct status (nothing is due any more).
func TestGetOrder_ModelC_UnavailableStillCancellable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o, _ := e.directOrder(t)
	e.payees.accounts = map[uuid.UUID]parties.BankAccount{}
	buyer := actor(t, "company_admin")
	if v, err := e.svc.GetOrder(ctxOf(t, buyer), buyer, o.ID); err != nil || v.DirectStatus != orders.DirectUnavailable {
		t.Fatalf("GetOrder: status=%q err=%v", v.DirectStatus, err)
	}
	if _, err := e.svc.CancelOrder(ctxOf(t, buyer), buyer, o.ID, "payee account withdrawn"); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	v, err := e.svc.GetOrder(ctxOf(t, buyer), buyer, o.ID)
	if err != nil || v.Order.Status != orders.StatusCancelled || v.DirectStatus != "" {
		t.Fatalf("cancelled: status=%s direct=%q err=%v", v.Order.Status, v.DirectStatus, err)
	}
}

// TestGetOrder_ModelC_NaturalPersonPayeeUnavailable (BE-109, 06 H-54): a
// payee whose legal party is a natural person is never paid directly. The
// order still reads (unavailable, no account); the instructions stay 409
// orders.invalid_state.
func TestGetOrder_ModelC_NaturalPersonPayeeUnavailable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o, b := e.directOrder(t)
	py := e.payees.m[b]
	py.LegalPartyID = e.parties.add(parties.LegalParty{Kind: parties.KindPayee, LegalType: parties.LegalIndividual,
		Title: "Synthetic Person", Status: parties.StatusVerified})
	e.payees.m[b] = py
	buyer := actor(t, "company_admin")
	v, err := e.svc.GetOrder(ctxOf(t, buyer), buyer, o.ID)
	if err != nil || v.DirectStatus != orders.DirectUnavailable {
		t.Fatalf("natural person payee: status=%q err=%v", v.DirectStatus, err)
	}
	if n := len(e.audit.Of(authz.OrdersOrderPaymentInstructions)); n != 0 {
		t.Fatalf("access audit=%d, want 0", n)
	}
	_, err = e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "orders.invalid_state")
}

// TestGetOrder_ModelC_BrokenPayeeStillFails (BE-109): only a payee that
// cannot be paid now degrades the order; a frozen payee that no longer
// exists is a broken invariant and still fails the read.
func TestGetOrder_ModelC_BrokenPayeeStillFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o, b := e.directOrder(t)
	delete(e.payees.m, b)
	buyer := actor(t, "company_admin")
	_, err := e.svc.GetOrder(ctxOf(t, buyer), buyer, o.ID)
	problemtest.WantErr(t, err, parties.ErrNotFound)
}

// TestGetOrder_ModelC_DirectStatusHTTP (BE-109): the OrderDTO carries
// direct_payments_status for the buyer administrator only, and without an
// approved account no direct_payments key at all; getOrderPaymentInstructions
// answers 409 parties.no_approved_account.
func TestGetOrder_ModelC_DirectStatusHTTP(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o, b := e.directOrder(t)
	app := newHTTP(t, e)
	path := "/api/v1/orders/" + o.ID.String()
	rec, body := call(t, app, "company_admin", http.MethodGet, path, nil, nil)
	if rec.Code != http.StatusOK || body["direct_payments_status"] != "available" {
		t.Fatalf("available: %d %s", rec.Code, rec.Body)
	}
	if _, exists := body["direct_payments"]; exists || strings.Contains(rec.Body.String(), `"iban"`) {
		t.Fatalf("getOrder discloses payee accounts (BE-127): %s", rec.Body)
	}
	delete(e.payees.accounts, b)
	rec, body = call(t, app, "company_admin", http.MethodGet, path, nil, nil)
	if rec.Code != http.StatusOK || body["direct_payments_status"] != "unavailable" || body["order_no"] != o.OrderNo {
		t.Fatalf("unavailable: %d %s", rec.Code, rec.Body)
	}
	if _, exists := body["direct_payments"]; exists {
		t.Fatalf("unavailable order discloses payee accounts: %s", rec.Body)
	}
	for _, name := range []string{"company_member", "finance_admin"} {
		rec, body := call(t, app, name, http.MethodGet, path, nil, nil)
		if _, exists := body["direct_payments_status"]; rec.Code != http.StatusOK || exists {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	rec, body = call(t, app, "company_admin", http.MethodGet, path+"/payment-instructions", nil, nil)
	if rec.Code != http.StatusConflict || body["code"] != "parties.no_approved_account" {
		t.Fatalf("instructions: %d %s", rec.Code, rec.Body)
	}
}
