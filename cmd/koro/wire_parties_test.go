package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/crypto/rewrap"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// useMemStore installs the in-memory parties store used by service, wiring
// and authz matrix tests (05 §13.2). First owner: BE-036.
// Only tickets of the parties module edit this file (BE-001 R4).
// updated_at uses the real clock: these tests do not depend on it.
func (w *partiesWiring) useMemStore() {
	w.store = parties.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_Parties pins what the parties module provides: the service, its
// KVKK provider, the rewrap declaration of legal_parties.tckn_enc, and
// 06 H-54 (a natural-person payee party is refused; cmd/koro never lifts it).
func TestWire_Parties(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.parties.svc == nil {
		t.Fatal("parties service not built")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "parties" }) {
		t.Fatal("parties KVKK provider not registered")
	}
	if !slices.ContainsFunc(c.keys.columns, func(col rewrap.Column) bool {
		return col.Table == "legal_parties" && col.Column == "tckn_enc" && col.PurposePrefix == parties.TCKNSealPurposePrefix &&
			col.PurposeColumn == "id"
	}) {
		t.Fatal("legal_parties.tckn_enc is not declared for crypto.rewrap")
	}
	pad, _ := authztest.Get("platform_admin")
	_, err := c.parties.svc.AdminCreateParty(authz.WithActor(t.Context(), pad.Actor), pad.Actor, parties.PartyInput{
		Kind: parties.KindPayee, LegalType: parties.LegalIndividual, Title: "Ayşe Yılmaz", TCKN: "10000000146",
	})
	if !errors.Is(err, parties.ErrNaturalPersonPayee) {
		t.Fatalf("individual payee party: %v, want parties.natural_person_payee (06 H-54)", err)
	}
	// BE-037: the payee contract for catalog, orders and ledger, the notice
	// subscriptions of the three payee_bank_account topics and the revocation task.
	if c.parties.payees == nil {
		t.Fatal("parties.Payees not provided")
	}
	for topic, name := range map[string]string{
		domainevent.TopicPayeeBankAccountSubmitted: parties.NotifySubmittedTaskName,
		domainevent.TopicPayeeBankAccountApproved:  parties.NotifyApprovedTaskName,
		domainevent.TopicPayeeBankAccountRejected:  parties.NotifyRejectedTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name }) {
			t.Errorf("%s has no %s subscription", topic, name)
		}
	}
	if !slices.ContainsFunc(c.infra.jobs.Tasks(), func(task queue.Task) bool { return task.Name == parties.NotifyRevokedTaskName }) {
		t.Error("the revocation notice task is not registered")
	}
}

// TestWire_PartiesApprovalNoticeReachesFinanceAdmins runs the
// payee_bank_account.approved subscription through the wired services: the
// finance admins in effect come from identity's store (06 H-33; a pending
// grant is not one), their addresses from identity, and notify mails them.
func TestWire_PartiesApprovalNoticeReachesFinanceAdmins(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	now := c.infra.clock.Now().UTC().Truncate(time.Microsecond)
	for _, name := range []string{"finance_admin", "finance_admin_2", "platform_admin", "dpo"} {
		p, _ := authztest.Get(name)
		email := matrixEmail(name)
		if err := c.identity.store.InsertUser(ctx, identity.User{
			ID: p.User(), Status: identity.StatusActive, Email: email, EmailNormalized: email, EmailVerifiedAt: &now,
			PasswordHash: testPasswordHash(), FirstName: "Ayla", LastName: name, Persona: identity.PersonaPlatform,
			Locale: identity.LocaleTR, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	grant := func(name string, role authz.PlatformRole, st identity.RoleStatus) {
		ra := identity.RoleAssignment{ID: uuid.NewV7(), UserID: authztest.ID(name), Role: role, Reason: "bootstrap",
			Status: st, RequestedAt: now}
		if st == identity.RoleActive {
			ra.GrantedAt = &now
		}
		if err := c.identity.store.InsertRoleAssignment(ctx, ra); err != nil {
			t.Fatal(err)
		}
	}
	grant("finance_admin", authz.PlatformFinanceAdmin, identity.RoleActive)
	grant("finance_admin_2", authz.PlatformFinanceAdmin, identity.RoleActive)
	grant("platform_admin", authz.PlatformAdmin, identity.RoleActive)
	grant("dpo", authz.PlatformFinanceAdmin, identity.RolePending) // not in effect (06 H-33)

	store := c.parties.store.(*parties.MemStore)
	party := parties.LegalParty{ID: uuid.NewV7(), Kind: parties.KindPayee, LegalType: parties.LegalFoundation,
		Title: "Kanca Vakfı", VATStatus: parties.VATUnknown, Status: parties.StatusPendingVerification}
	if _, err := store.InsertParty(ctx, party); err != nil {
		t.Fatal(err)
	}
	payee, club := uuid.NewV7(), uuid.NewV7()
	store.LinkPayee(payee, club, party.ID, "active")
	b, err := store.InsertBankAccount(ctx, parties.BankAccount{ID: uuid.NewV7(), PayeeID: payee, OrgID: club,
		IBAN: "TR330006100519786457841326", HolderName: "Kanca Vakfı", EvidenceFileID: uuid.NewV7(),
		Status: parties.AccountPendingApproval, SubmittedBy: authztest.ID("finance_admin_2"), SubmittedByAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	by := authztest.ID("platform_admin")
	b.Status, b.ApprovedBy, b.ApprovedAt = parties.AccountApproved, &by, &now
	if _, err := store.TransitionBankAccount(ctx, b, parties.AccountPendingApproval); err != nil {
		t.Fatal(err)
	}
	env, err := domainevent.New(domainevent.PayeeBankAccountApproved{BankAccountID: b.ID, PayeeID: payee}, "payee_bank_account", b.ID)
	if err != nil {
		t.Fatal(err)
	}
	subs := c.infra.subs.For(domainevent.TopicPayeeBankAccountApproved)
	if len(subs) != 1 {
		t.Fatalf("approved subscriptions = %d", len(subs))
	}
	if err := subs[0].Handle(workerContext(t), env); err != nil {
		t.Fatal(err)
	}
	var to []string
	for _, e := range c.notify.transport.(*notify.MemTransport).Sent() {
		to = append(to, e.To)
		if !strings.Contains(e.Text, "TR33 **** **** **** **** 8413 26") || strings.Contains(e.Text, "TR330006100519786457841326") {
			t.Errorf("notice to %s does not carry the masked IBAN only:\n%s", e.To, e.Text)
		}
	}
	slices.Sort(to)
	want := []string{matrixEmail("finance_admin"), matrixEmail("finance_admin_2")}
	slices.Sort(want)
	if !slices.Equal(to, want) {
		t.Fatalf("approval notice mailed to %v, want every finance admin in effect %v", to, want)
	}
}

// TestSeed_BootstrapCommerce_DraftIdempotent pins `koro seed
// bootstrap-commerce` (T-31): the first run writes Koro's party as a draft,
// the second leaves it alone; extra arguments are a usage error.
func TestSeed_BootstrapCommerce_DraftIdempotent(t *testing.T) {
	t.Parallel()
	store := parties.NewMemStore(clock.Real().Now)
	auditStore := audit.NewMemStore()
	au := audit.New(audit.Deps{Store: auditStore, Tx: db.NopTx{}, Clock: clock.Real(), Log: obs.Discard()}).Service
	d := parties.SeederDeps{Store: store, Tx: db.NopTx{}, Audit: au, Clock: clock.Real(), Log: obs.Discard()}
	var out bytes.Buffer
	if err := seedBootstrapCommerce(t.Context(), d, &out); err != nil {
		t.Fatal(err)
	}
	if err := seedBootstrapCommerce(t.Context(), d, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "status draft): created as draft") ||
		!strings.Contains(lines[1], "already present, left unchanged") {
		t.Fatalf("seed output:\n%s", out.String())
	}
	p, err := store.GetPlatform(t.Context())
	if err != nil || p.Status != parties.StatusDraft || p.Kind != parties.KindPlatform {
		t.Fatalf("platform party = %+v %v", p, err)
	}
	if err := runSeed(t.Context(), []string{"bootstrap-commerce", "extra"}, nil, &out); !errors.Is(err, errUsage) {
		t.Fatalf("seed bootstrap-commerce extra: %v, want a usage error", err)
	}
}
