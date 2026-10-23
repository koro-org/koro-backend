package main

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// useMemStore installs the in-memory ledger stores used by service,
// wiring and authz matrix tests (05 §13.2): the MemStore is the ledger
// store and the payout store (BE-044). First owner: BE-039.
// Only tickets of the ledger module edit this file (BE-001 R4).
func (w *ledgerWiring) useMemStore() {
	w.store = ledger.NewMemStore(clock.Real().Now)
	w.tx = db.NopTx{}
}

// TestWire_LedgerPayouts pins what the payout flow registers (BE-044): the
// payout store is the in-memory store, the notices of payout.created,
// .approved and .paid (01 §6.2) and the security notice of a payout.paid
// dated before its approval (BE-092, critical queue), the KVKK provider of
// the payouts and the "open payouts" closeOrg check after orders' "open
// orders" (both kept).
func TestWire_LedgerPayouts(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []struct{ topic, name, queue string }{
		{domainevent.TopicPayoutCreated, ledger.NotifyPayoutCreatedTaskName, queue.Default},
		{domainevent.TopicPayoutApproved, ledger.NotifyPayoutApprovedTaskName, queue.Default},
		{domainevent.TopicPayoutPaid, ledger.NotifyPayoutPaidTaskName, queue.Default},
		{domainevent.TopicPayoutPaid, ledger.NotifyPayoutPaidBeforeApprovalTaskName, queue.Critical},
	} {
		if !slices.ContainsFunc(c.infra.subs.For(sub.topic), func(s outbox.Subscription) bool {
			return s.Name == sub.name && s.Queue == sub.queue
		}) {
			t.Errorf("%s has no %s subscription on the %s queue", sub.topic, sub.name, sub.queue)
		}
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "ledger" }) {
		t.Error("the ledger KVKK provider is not registered")
	}
	if n := len(c.hooks.openCommerce); n < 2 {
		t.Fatalf("hooks.openCommerce has %d checks, want orders' and ledger's", n)
	}
	for i, h := range c.hooks.openCommerce {
		if open, err := h(t.Context(), uuid.NewV7()); err != nil || open {
			t.Errorf("openCommerce[%d](unknown org) = %v, %v", i, open, err)
		}
	}
}

// TestWire_LedgerJobs pins what the ledger module registers (01 §7):
// ledger.integrity_check daily at 05:15 on the low queue and, before it,
// the late recognition of Koro's own sales at 05:05 (BE-067).
func TestWire_LedgerJobs(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "ledger.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"ledger.integrity_check 15 5 * * * low", "ledger.recognize_platform_sales 5 5 * * * low"}; !slices.Equal(jobs, want) {
		t.Fatalf("ledger jobs %q, want %q", jobs, want)
	}
	if c.ledger.svc == nil {
		t.Fatal("ledger service not built")
	}
}

// TestWire_LedgerPaidBeforeApprovalNoticeReachesAdmins runs the security
// notice of a payout.paid dated before its approval (00 T-298, BE-092)
// through the wired services: the finance and platform admins in effect
// come from identity's store (06 H-33; a pending grant is not one), their
// addresses and names from identity, and notify sends them the notice in-app
// and by e-mail (masked IBAN only) although one of them switched the
// orders_payments category off: the template is in the security category.
// The admin who marked the payout paid gets nothing.
func TestWire_LedgerPaidBeforeApprovalNoticeReachesAdmins(t *testing.T) {
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
	grant("dpo", authz.PlatformAdmin, identity.RolePending) // not in effect (06 H-33)
	pad := authztest.ID("platform_admin")
	if err := c.notify.store.UpsertPreferences(ctx, pad, []notify.Preference{
		{Category: notify.PrefOrdersPayments, Channel: notify.ChannelEmail},
		{Category: notify.PrefOrdersPayments, Channel: notify.ChannelInApp},
	}, now); err != nil {
		t.Fatal(err)
	}

	// A payout created by finance_admin, approved by finance_admin_2 and
	// marked paid by finance_admin with a transfer an hour before the approval.
	store, ok := c.ledger.store.(*ledger.MemStore)
	if !ok {
		t.Fatalf("ledger store %T", c.ledger.store)
	}
	const rawIBAN = "TR330006100519786457841326"
	creator, approver := authztest.ID("finance_admin"), authztest.ID("finance_admin_2")
	p := ledger.Payout{ID: uuid.NewV7(), PayeeID: uuid.NewV7(), OrgID: authztest.ID("club_a"), BankAccountID: uuid.NewV7(),
		IBANSnapshot: rawIBAN, HolderSnapshot: "Kanca Vakfı", Amount: 1_080_000, Status: ledger.StatusPendingApproval,
		CreatedBy: creator}
	p, err := store.InsertPayout(ctx, p, []ledger.PayoutItem{{PayoutID: p.ID, OrderLineID: uuid.NewV7(), PayeeID: p.PayeeID,
		Amount: p.Amount}})
	if err != nil {
		t.Fatal(err)
	}
	approvedAt, paidAt, ref, receipt := now, now.Add(-time.Hour), "EFT-KANCA-1", uuid.NewV7()
	p.Status, p.ApprovedBy, p.ApprovedAt = ledger.StatusApproved, &approver, &approvedAt
	if p, err = store.TransitionPayout(ctx, p, ledger.StatusPendingApproval); err != nil {
		t.Fatal(err)
	}
	p.Status, p.PaidBy, p.PaidAt, p.BankReference, p.ReceiptFileID = ledger.StatusPaid, &creator, &paidAt, &ref, &receipt
	if p, err = store.TransitionPayout(ctx, p, ledger.StatusApproved); err != nil {
		t.Fatal(err)
	}

	env, err := domainevent.New(domainevent.PayoutPaid{PayoutID: p.ID, PayeeID: p.PayeeID, Amount: p.Amount}, "payout", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(c.infra.subs.For(domainevent.TopicPayoutPaid), func(s outbox.Subscription) bool {
		return s.Name == ledger.NotifyPayoutPaidBeforeApprovalTaskName
	})
	if i < 0 {
		t.Fatal("no security notice subscription on payout.paid")
	}
	if err := c.infra.subs.For(domainevent.TopicPayoutPaid)[i].Handle(workerContext(t), env); err != nil {
		t.Fatal(err)
	}

	var to []string
	for _, e := range c.notify.transport.(*notify.MemTransport).Sent() {
		to = append(to, e.To)
		if !strings.Contains(e.Text, "TR33 **** **** **** **** 8413 26") || strings.Contains(e.Text, rawIBAN) ||
			!strings.Contains(e.Text, "Ayla finance_admin_2") || !strings.Contains(e.Text, "EFT-KANCA-1") {
			t.Errorf("notice to %s:\n%s", e.To, e.Text)
		}
	}
	slices.Sort(to)
	want := []string{matrixEmail("finance_admin_2"), matrixEmail("platform_admin")}
	if !slices.Equal(to, want) {
		t.Fatalf("security notice mailed to %v, want %v", to, want)
	}
	for name, n := range map[string]int{"finance_admin_2": 1, "platform_admin": 1, "finance_admin": 0, "dpo": 0} {
		got, err := c.notify.store.ListInApp(ctx, authztest.ID(name), false, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != n || (n == 1 && (got[0].Kind != "ledger.payout_paid_before_approval" ||
			got[0].LinkPath != "/admin/payouts/"+p.ID.String())) {
			t.Errorf("in-app notices of %s: %+v, want %d", name, got, n)
		}
	}
}
