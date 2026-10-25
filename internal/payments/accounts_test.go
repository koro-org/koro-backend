package payments_test

import (
	"errors"
	"testing"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/payments"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
)

func (e *env) createAccount(t *testing.T, who, number string) payments.CollectionAccount {
	t.Helper()
	a := actor(t, who)
	c, err := e.svc.CreateCollectionAccount(ctxOf(t, a), a, payments.AccountInput{Label: "Ana hesap", LegalPartyID: e.platform,
		IBAN: number, HolderName: "Koro Teknoloji A.Ş.", BankName: "Örnek Bank"}, good)
	if err != nil {
		t.Fatalf("CreateCollectionAccount: %v", err)
	}
	return c
}

// TestCollectionAccount_SameUserApprove_403FourEyes (03 §6.1): the admin who
// created an account never decides it — not even a platform admin's own
// creation; another finance admin approves it, the security notice event
// is written and the database's CHECK is the second layer.
func TestCollectionAccount_SameUserApprove_403FourEyes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.createAccount(t, "finance_admin", ibanKoro)
	if c.Status != payments.AccountPending || c.CreatedBy != id("finance_admin") || c.IsDefault {
		t.Fatalf("created = %+v", c)
	}
	fin := actor(t, "finance_admin")
	for _, d := range []payments.Decision{payments.DecisionApproved, payments.DecisionRejected} {
		_, err := e.svc.DecideCollectionAccount(ctxOf(t, fin), fin, c.ID, d, good)
		wantErrIs(t, err, authz.ErrFourEyes)
	}
	if got, _ := e.store.GetAccount(t.Context(), c.ID, false); got.Status != payments.AccountPending {
		t.Fatalf("after the refused decision: %s", got.Status)
	}
	// The store refuses it too (collection_accounts_four_eyes_chk).
	self := c
	by, at := c.CreatedBy, e.clk.Now()
	self.Status, self.ApprovedBy, self.ApprovedAt = payments.AccountActive, &by, &at
	if _, err := e.store.UpdateAccount(t.Context(), self, payments.AccountPending); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("store four eyes = %v", err)
	}

	fin2 := actor(t, "finance_admin_2")
	got, err := e.svc.DecideCollectionAccount(ctxOf(t, fin2), fin2, c.ID, payments.DecisionApproved, good)
	if err != nil {
		t.Fatalf("second eye: %v", err)
	}
	if got.Status != payments.AccountActive || got.ApprovedBy == nil || *got.ApprovedBy != fin2.UserID || got.IsDefault {
		t.Fatalf("approved = %+v", got)
	}
	if evs := e.events(domainevent.TopicCollectionAccountApproved); len(evs) != 1 {
		t.Fatalf("collection_account.approved events = %d", len(evs))
	}
	if _, err := e.svc.DecideCollectionAccount(ctxOf(t, fin2), fin2, c.ID, payments.DecisionRejected, good); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("second decision = %v, want 409", err)
	}
	ev := e.audit.Of(authz.PaymentsCollectionAccountApprove)
	if len(ev) != 1 || ev[0].Metadata["iban_masked"] != "TR33 **** **** **** **** 8413 26" || ev[0].Metadata[audit.MetaReauth] != "password" {
		t.Fatalf("audit = %+v", ev)
	}

	// A platform admin's own creation is refused too; a rejection by another admin ends it.
	pad := actor(t, "platform_admin")
	c2 := e.createAccount(t, "finance_admin", ibanKoro2)
	if _, err := e.svc.DecideCollectionAccount(ctxOf(t, pad), pad, c2.ID, payments.DecisionRejected, good); err != nil {
		t.Fatalf("platform admin rejects: %v", err)
	}
	if evs := e.events(domainevent.TopicCollectionAccountApproved); len(evs) != 1 {
		t.Fatal("a rejection emitted collection_account.approved")
	}
}

// TestCollectionAccount_ApproveRequiresReauth (03 §6.2): creating and
// deciding an account need the re-authentication proof in the request;
// without it (403 identity.reauth_required) or with a wrong one (403
// identity.reauth_failed) nothing changes.
func TestCollectionAccount_ApproveRequiresReauth(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin := actor(t, "finance_admin")
	in := payments.AccountInput{Label: "Ana hesap", LegalPartyID: e.platform, IBAN: ibanKoro, HolderName: "Koro Teknoloji A.Ş.", BankName: "Örnek Bank"}
	if _, err := e.svc.CreateCollectionAccount(ctxOf(t, fin), fin, in, identity.ReauthProof{}); !errors.Is(err, identity.ErrReauthRequired) {
		t.Fatalf("create without proof = %v", err)
	}
	c := e.createAccount(t, "finance_admin", ibanKoro)
	pad := actor(t, "platform_admin")
	for proof, want := range map[identity.ReauthProof]error{
		{}:                       identity.ErrReauthRequired,
		{Password: "wrong-pass"}: identity.ErrReauthFailed,
	} {
		if _, err := e.svc.DecideCollectionAccount(ctxOf(t, pad), pad, c.ID, payments.DecisionApproved, proof); !errors.Is(err, want) {
			t.Fatalf("decide with %v = %v, want %v", proof, err, want)
		}
	}
	if got, _ := e.store.GetAccount(t.Context(), c.ID, false); got.Status != payments.AccountPending || len(e.events(domainevent.TopicCollectionAccountApproved)) != 0 {
		t.Fatalf("a refused proof changed the account: %+v", got)
	}
	if _, err := e.svc.DecideCollectionAccount(ctxOf(t, pad), pad, c.ID, payments.DecisionApproved, good); err != nil {
		t.Fatalf("decide with the proof: %v", err)
	}
	for _, f := range []func() error{
		func() error {
			_, err := e.svc.MakeDefaultCollectionAccount(ctxOf(t, pad), pad, c.ID, identity.ReauthProof{})
			return err
		},
		func() error {
			_, err := e.svc.RetireCollectionAccount(ctxOf(t, pad), pad, c.ID, identity.ReauthProof{})
			return err
		},
	} {
		if err := f(); !errors.Is(err, identity.ErrReauthRequired) {
			t.Fatalf("retire/make-default without proof = %v", err)
		}
	}
}

// TestCollectionAccount_DefaultAndRetire: approval does not make an account
// the default; make-default does (the hook of orders' payment instructions
// returns it), a second make-default moves it, retiring the default leaves
// none; every change is announced (A task) and audited.
func TestCollectionAccount_DefaultAndRetire(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a1, a2 := e.activeAccount(t, ibanKoro, false), e.activeAccount(t, ibanKoro2, false)
	if _, found, _ := e.mod.DefaultAccount(ctx); found {
		t.Fatal("a default without make-default")
	}
	pad := actor(t, "platform_admin")
	if _, err := e.svc.MakeDefaultCollectionAccount(ctxOf(t, pad), pad, a1.ID, good); err != nil {
		t.Fatal(err)
	}
	if got, found, _ := e.mod.DefaultAccount(ctx); !found || got.ID != a1.ID {
		t.Fatalf("default = %+v, %v", got, found)
	}
	fin := actor(t, "finance_admin")
	if _, err := e.svc.MakeDefaultCollectionAccount(ctxOf(t, fin), fin, a2.ID, good); err != nil {
		t.Fatal(err)
	}
	one, _ := e.store.GetAccount(ctx, a1.ID, false)
	if got, _, _ := e.mod.DefaultAccount(ctx); got.ID != a2.ID || one.IsDefault {
		t.Fatalf("default moved to %s; a1 default=%v", got.ID, one.IsDefault)
	}
	// Already the default: no change, no notice.
	if _, err := e.svc.MakeDefaultCollectionAccount(ctxOf(t, fin), fin, a2.ID, good); err != nil {
		t.Fatal(err)
	}
	if n := len(e.queue.Tasks()); n != 2 {
		t.Fatalf("change notices = %d, want 2", n)
	}
	got, err := e.svc.RetireCollectionAccount(ctxOf(t, fin), fin, a2.ID, good)
	if err != nil || got.Status != payments.AccountRetired || got.IsDefault || got.RetiredAt == nil {
		t.Fatalf("retired = %+v, %v", got, err)
	}
	if _, found, _ := e.mod.DefaultAccount(ctx); found {
		t.Fatal("a retired account is still the default")
	}
	if _, err := e.svc.RetireCollectionAccount(ctxOf(t, fin), fin, a2.ID, good); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("retire twice = %v", err)
	}
	if _, err := e.svc.MakeDefaultCollectionAccount(ctxOf(t, fin), fin, a2.ID, good); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("make a retired account the default = %v", err)
	}
	// A pending account can neither be the default nor be retired.
	c := e.createAccount(t, "finance_admin", ibanKoro3)
	if _, err := e.svc.MakeDefaultCollectionAccount(ctxOf(t, fin), fin, c.ID, good); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("pending default = %v", err)
	}
	if n := len(e.audit.Of(authz.PaymentsCollectionAccountRetire)); n != 3 {
		t.Fatalf("retire audits = %d, want 3", n)
	}
	// The notice task reaches every platform and finance admin (in-app and e-mail).
	for _, task := range e.queue.Tasks() {
		h := e.mod.Tasks()[0]
		if task.Type != h.Name {
			t.Fatalf("task %s", task.Type)
		}
		if err := h.Handle(ctx, task.Payload); err != nil {
			t.Fatal(err)
		}
	}
	got2 := e.sender.to("payments.collection_account_changed", notify.ChannelEmail)
	if !containsAll(got2, id("platform_admin"), id("platform_admin_2"), id("finance_admin"), id("finance_admin_2")) {
		t.Fatalf("change notice e-mails to %v", got2)
	}
}

// TestCollectionAccount_CreateValidation: Koro's own party only, a TR IBAN
// (parties.iban_invalid), one open account per IBAN (409); a missing role
// is 403 before anything else.
func TestCollectionAccount_CreateValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin := actor(t, "finance_admin")
	base := payments.AccountInput{Label: "Ana hesap", LegalPartyID: e.platform, IBAN: ibanKoro, HolderName: "Koro Teknoloji A.Ş.", BankName: "Örnek Bank"}
	bad := base
	bad.LegalPartyID = id("party_buyer_a")
	if _, err := e.svc.CreateCollectionAccount(ctxOf(t, fin), fin, bad, good); !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("foreign party = %v", err)
	}
	bad = base
	bad.IBAN = "TR330006100519786457841327"
	if _, err := e.svc.CreateCollectionAccount(ctxOf(t, fin), fin, bad, good); !errors.Is(err, payments.ErrIBANInvalid) {
		t.Fatalf("bad IBAN = %v", err)
	}
	e.createAccount(t, "finance_admin", ibanKoro)
	if _, err := e.svc.CreateCollectionAccount(ctxOf(t, fin), fin, base, good); !errors.Is(err, payments.ErrIBANInUse) {
		t.Fatalf("IBAN twice = %v", err)
	}
	pad := actor(t, "platform_admin") // 03 §5.16: create is fin only
	if _, err := e.svc.CreateCollectionAccount(ctxOf(t, pad), pad, base, good); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform admin creates = %v", err)
	}
	if list, err := e.svc.ListCollectionAccounts(ctxOf(t, pad), pad); err != nil || len(list) != 1 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
}
