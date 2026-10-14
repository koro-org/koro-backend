package ledger_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
)

// TestCreatePayout_IdempotencyReplay_SinglePayout: the same
// Idempotency-Key and body replays the first payout (one payout, one audit
// row, one payout.created); the key with another body is 422.
func TestCreatePayout_IdempotencyReplay_SinglePayout(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	l1, l2 := e.payable(t, payee), e.payable(t, payee)
	a := actor(t, "finance_admin")
	ctx := key(ctxOf(t, a), a, "payout-key-1")
	first, err := e.svc.AdminCreatePayout(ctx, a, ledger.CreatePayoutInput{PayeeID: payee})
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.AdminCreatePayout(ctx, a, ledger.CreatePayoutInput{PayeeID: payee})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || first.Amount != 2_160_000 || len(first.Items) != 2 {
		t.Fatalf("replay %s, first %s (amount %d, %d items)", again.ID, first.ID, first.Amount, len(first.Items))
	}
	page, err := e.svc.AdminListPayouts(ctxOf(t, a), a, ledger.PayoutQuery{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("payouts %v, %v", page.Items, err)
	}
	if n := len(e.audit.of(authz.LedgerPayoutCreate)); n != 1 {
		t.Fatalf("%d create audit rows, want 1", n)
	}
	if got := e.outbox.Topics(); !slices.Equal(got, []string{domainevent.TopicPayoutCreated}) {
		t.Fatalf("events %v", got)
	}
	_, err = e.svc.AdminCreatePayout(ctx, a, ledger.CreatePayoutInput{PayeeID: payee, LineIDs: []uuid.UUID{l1}})
	wantProblem(t, err, idempotency.ErrKeyReused)
	// Without a key, the lines are held: nothing is left to pay.
	_, err = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee, LineIDs: []uuid.UUID{l2}})
	wantProblem(t, err, ledger.ErrLineNotPayable)
}

// TestCreatePayout_LineAlreadyInPayout_409: a line in a live payout is not
// payable again (409 ledger.line_not_payable); without order_line_ids the
// payout takes only the free lines; nothing left is 422.
func TestCreatePayout_LineAlreadyInPayout_409(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	l1, l2 := e.payable(t, payee), e.payable(t, payee)
	first := e.create(t, "finance_admin", payee, l1)
	if len(first.Items) != 1 || first.Items[0].OrderLineID != l1 || first.Amount != 1_080_000 {
		t.Fatalf("first payout %+v", first)
	}
	a := actor(t, "finance_admin")
	_, err := e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee, LineIDs: []uuid.UUID{l1}})
	wantProblem(t, err, ledger.ErrLineNotPayable)
	_, err = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee, LineIDs: []uuid.UUID{l2, l1}})
	wantProblem(t, err, ledger.ErrLineNotPayable)
	second := e.create(t, "finance_admin_2", payee)
	if len(second.Items) != 1 || second.Items[0].OrderLineID != l2 {
		t.Fatalf("second payout %+v", second.Items)
	}
	_, err = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee})
	wantProblem(t, err, ledger.ErrNothingPayable)
	// Another payee's line is not this payee's.
	other := e.seller(t, "club_b")
	lo := e.payable(t, other)
	_, err = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee, LineIDs: []uuid.UUID{lo}})
	wantProblem(t, err, ledger.ErrLineNotPayable)
	// No approved account: 409 parties.no_approved_account; an unknown payee is 422.
	e.payees.mu.Lock()
	delete(e.payees.accounts, other)
	e.payees.mu.Unlock()
	_, err = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: other})
	wantProblem(t, err, parties.ErrNoApprovedAccount)
	_, err = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: uuid.NewV7()})
	wantProblem(t, err, problem.ErrValidation)
	// Only a finance admin creates (03 §5.17).
	pad := actor(t, "platform_admin")
	_, err = e.svc.AdminCreatePayout(ctxOf(t, pad), pad, ledger.CreatePayoutInput{PayeeID: payee})
	wantProblem(t, err, authz.ErrForbidden)
}

// TestCreatePayout_LedgerMustOwe: a line whose allocation was never posted
// cannot be paid out (409 ledger.negative_payee_balance at creation).
func TestCreatePayout_LedgerMustOwe(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	py, _ := e.payees.Get(t.Context(), payee)
	e.orders.add(fakeLine{id: uuid.NewV7(), order: uuid.NewV7(), payee: payee, seller: py.OrgID, net: 1_000,
		fulfilled: t0.Add(-time.Hour)})
	a := actor(t, "finance_admin")
	_, err := e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee})
	wantProblem(t, err, ledger.ErrNegativeBalance)
}

// TestApprovePayout_Creator_403FourEyes (03 §6.1, §7.7 "4 göz – payout"):
// the creator may not approve, even with a valid proof; the platform admin
// has no approve cell; another finance admin approves.
func TestApprovePayout_Creator_403FourEyes(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	e.payable(t, payee)
	p := e.create(t, "finance_admin", payee)
	for _, name := range []string{"finance_admin", "platform_admin", "support"} {
		a := actor(t, name)
		_, err := e.svc.AdminApprovePayout(ctxOf(t, a), a, p.ID, identity.ReauthProof{Password: reauthPassword})
		want := authz.ErrForbidden
		if name == "finance_admin" {
			want = authz.ErrFourEyes
		}
		wantProblem(t, err, want)
	}
	got, err := e.svc.AdminGetPayout(ctxOf(t, actor(t, "finance_admin")), actor(t, "finance_admin"), p.ID)
	if err != nil || got.Status != ledger.StatusPendingApproval || got.ApprovedBy != nil {
		t.Fatalf("after refused approvals: %+v, %v", got.Payout, err)
	}
	v := e.approve(t, "finance_admin_2", p.ID)
	if v.Status != ledger.StatusApproved || v.ApprovedBy == nil || *v.ApprovedBy != authztest.ID("finance_admin_2") {
		t.Fatalf("approved %+v", v.Payout)
	}
}

// TestApprovePayout_RequiresReauth (03 §6.2): no proof → 403
// identity.reauth_required, a wrong one → 403 identity.reauth_failed, the
// right one approves (audit metadata reauth, payout.approved in the
// outbox); a second approval is 409.
func TestApprovePayout_RequiresReauth(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	e.payable(t, payee)
	p := e.create(t, "finance_admin_2", payee)
	a := actor(t, "finance_admin")
	_, err := e.svc.AdminApprovePayout(ctxOf(t, a), a, p.ID, identity.ReauthProof{})
	wantProblem(t, err, identity.ErrReauthRequired)
	_, err = e.svc.AdminApprovePayout(ctxOf(t, a), a, p.ID, identity.ReauthProof{Password: "wrong-password"})
	wantProblem(t, err, identity.ErrReauthFailed)
	if n := len(e.audit.of(authz.LedgerPayoutApprove)); n != 0 {
		t.Fatalf("%d approval audit rows before a valid proof", n)
	}
	e.approve(t, "finance_admin", p.ID)
	rows := e.audit.of(authz.LedgerPayoutApprove)
	if len(rows) != 1 || rows[0].Metadata["reauth"] != "password" || rows[0].Metadata["iban_masked"] == p.IBANSnapshot {
		t.Fatalf("approval audit %+v", rows)
	}
	if got := e.outbox.Topics(); !slices.Contains(got, domainevent.TopicPayoutApproved) {
		t.Fatalf("events %v", got)
	}
	_, err = e.svc.AdminApprovePayout(ctxOf(t, a), a, p.ID, identity.ReauthProof{Password: reauthPassword})
	wantProblem(t, err, ledger.ErrInvalidState)
	_, err = e.svc.AdminApprovePayout(ctxOf(t, a), a, uuid.NewV7(), identity.ReauthProof{Password: reauthPassword})
	wantProblem(t, err, ledger.ErrNotFound)
}

// approved returns an approved payout of one payable line of a new payee.
func (e *payoutEnv) approved(t *testing.T) (ledger.PayoutView, uuid.UUID, uuid.UUID) {
	t.Helper()
	payee := e.seller(t, "club_a")
	l := e.payable(t, payee)
	p := e.create(t, "finance_admin_2", payee)
	return e.approve(t, "finance_admin", p.ID), payee, l
}

// TestMarkPaid_WithoutReceipt_422: the receipt (a payout_receipt upload)
// and the bank reference are required; nothing moves without them.
func TestMarkPaid_WithoutReceipt_422(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	p, payee, l := e.approved(t)
	a := actor(t, "finance_admin")
	invoice := e.files.put(files.PurposeInvoicePDF)
	missing := uuid.NewV7()
	for _, c := range []struct {
		name string
		in   ledger.MarkPaidInput
		loc  string
	}{
		{"no receipt", ledger.MarkPaidInput{BankReference: "EFT-1"}, "body.receipt_file_id"},
		{"unknown receipt", ledger.MarkPaidInput{BankReference: "EFT-1", ReceiptFileID: &missing}, "body.receipt_file_id"},
		{"invoice as receipt", ledger.MarkPaidInput{BankReference: "EFT-1", ReceiptFileID: &invoice}, "body.receipt_file_id"},
		{"no bank reference", ledger.MarkPaidInput{BankReference: "  ", ReceiptFileID: &invoice}, "body.bank_reference"},
	} {
		_, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, a), a, p.ID, c.in)
		wantProblem(t, err, problem.ErrValidation)
		var pe *problem.Error
		if !errors.As(err, &pe) || len(pe.Fields()) == 0 || pe.Fields()[0].Location != c.loc {
			t.Errorf("%s: %v, want a field error at %s", c.name, err, c.loc)
		}
	}
	got, _ := e.svc.AdminGetPayout(ctxOf(t, a), a, p.ID)
	if got.Status != ledger.StatusApproved || e.orders.paidOut(l) != nil {
		t.Fatalf("a refused mark-paid moved the payout: %s, line paid out %v", got.Status, e.orders.paidOut(l))
	}
	if _, err := e.store.ByRef(t.Context(), ledger.KindPayoutPaid, ledger.RefPayout, p.ID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("payout_paid posted without a receipt: %v", err)
	}
	if b := e.balance(t, payee); b != 1_080_000 {
		t.Fatalf("balance %d", b)
	}
	// Only a finance admin records the transfer.
	pad := actor(t, "platform_admin")
	receipt := e.files.put(files.PurposePayoutReceipt)
	_, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, pad), pad, p.ID, ledger.MarkPaidInput{BankReference: "EFT-1", ReceiptFileID: &receipt})
	wantProblem(t, err, authz.ErrForbidden)
}

// TestMarkPaid_Chain: the chain of 01 §6.3 "Payout ödendi" over the
// in-memory stores: payout_paid in the ledger, the lines paid out, the
// receipt attached, paid by/at with the reference, payout.paid, the
// balance down and the integrity check clean; a second mark-paid is 409.
func TestMarkPaid_Chain(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	p, payee, l := e.approved(t)
	v := e.markPaid(t, "finance_admin_2", p.ID)
	if v.Status != ledger.StatusPaid || v.PaidBy == nil || *v.PaidBy != authztest.ID("finance_admin_2") ||
		v.BankReference == nil || *v.BankReference != "EFT-1" || v.ReceiptFileID == nil || v.ReceiptURL == "" {
		t.Fatalf("paid payout %+v", v)
	}
	posted, err := e.store.ByRef(t.Context(), ledger.KindPayoutPaid, ledger.RefPayout, p.ID)
	if err != nil || len(posted.Entries) != 2 {
		t.Fatalf("payout_paid %+v, %v", posted, err)
	}
	if got := e.orders.paidOut(l); got == nil || *got != p.ID {
		t.Fatalf("line paid out by %v", got)
	}
	if o, ok := e.files.attached[*v.ReceiptFileID]; !ok || o.Module != "ledger" || o.EntityID != p.ID {
		t.Fatalf("receipt owner %+v", o)
	}
	if b := e.balance(t, payee); b != 0 {
		t.Fatalf("balance after the payout %d", b)
	}
	if got := e.outbox.Topics(); got[len(got)-1] != domainevent.TopicPayoutPaid {
		t.Fatalf("events %v", got)
	}
	rep, err := e.mod.CheckIntegrity(t.Context())
	if err != nil || !rep.OK() {
		t.Fatalf("integrity %+v, %v", rep.Findings, err)
	}
	a := actor(t, "finance_admin")
	receipt := e.files.put(files.PurposePayoutReceipt)
	_, err = e.svc.AdminMarkPayoutPaid(ctxOf(t, a), a, p.ID, ledger.MarkPaidInput{BankReference: "EFT-2", ReceiptFileID: &receipt})
	wantProblem(t, err, ledger.ErrInvalidState)
	bal, err := e.svc.GetPayeeBalance(ctxOf(t, a), a, authztest.ID("club_a"), payee)
	if err != nil || bal.Payable != 0 || bal.PaidOut != 1_080_000 || bal.PendingPayout != 0 {
		t.Fatalf("balance %+v, %v", bal, err)
	}
}

// TestMarkPaid_IdempotencyReplay: a retry with the same key and body (no
// paid_at) replays the first mark-paid, a minute later too: one ledger
// transaction, one payout.paid.
func TestMarkPaid_IdempotencyReplay(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	p, _, _ := e.approved(t)
	a := actor(t, "finance_admin")
	ctx := idempotency.WithKey(ctxOf(t, a), idempotency.Key{Scope: a.UserID.String() + ":POST /admin/payouts/{payout_id}/mark-paid",
		Value: "mark-paid-1", Status: 200})
	receipt := e.files.put(files.PurposePayoutReceipt)
	in := ledger.MarkPaidInput{BankReference: "EFT-7", ReceiptFileID: &receipt}
	first, err := e.svc.AdminMarkPayoutPaid(ctx, a, p.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Minute)
	again, err := e.svc.AdminMarkPayoutPaid(ctx, a, p.ID, in)
	if err != nil || again.Status != ledger.StatusPaid || !again.PaidAt.Equal(*first.PaidAt) {
		t.Fatalf("replay %+v, %v", again.Payout, err)
	}
	n := 0
	for _, topic := range e.outbox.Topics() {
		if topic == domainevent.TopicPayoutPaid {
			n++
		}
	}
	if n != 1 || len(e.audit.of(authz.LedgerPayoutMarkPaid)) != 1 {
		t.Fatalf("%d payout.paid events, %d audit rows", n, len(e.audit.of(authz.LedgerPayoutMarkPaid)))
	}
}

// TestMarkFailed_ReleasesLines: a failed transfer releases the lines (the
// items are deleted, the lines are payable again); only an approved payout
// fails.
func TestMarkFailed_ReleasesLines(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	p, payee, l := e.approved(t)
	a := actor(t, "finance_admin")
	v, err := e.svc.AdminMarkPayoutFailed(ctxOf(t, a), a, p.ID, "Banka IBAN'ı reddetti")
	if err != nil || v.Status != ledger.StatusFailed || v.FailureReason == nil || len(v.Items) != 0 {
		t.Fatalf("failed payout %+v, %v", v, err)
	}
	gs, err := e.svc.AdminListPayables(ctxOf(t, a), a, ledger.PayablesQuery{PayeeID: &payee})
	if err != nil || len(gs) != 1 || len(gs[0].Lines) != 1 || gs[0].Lines[0].OrderLineID != l {
		t.Fatalf("payables after the failure %+v, %v", gs, err)
	}
	_, err = e.svc.AdminMarkPayoutFailed(ctxOf(t, a), a, p.ID, "again")
	wantProblem(t, err, ledger.ErrInvalidState)
	again := e.create(t, "finance_admin", payee)
	if len(again.Items) != 1 || again.Items[0].OrderLineID != l {
		t.Fatalf("the released line was not taken again: %+v", again.Items)
	}
	_, err = e.svc.AdminMarkPayoutFailed(ctxOf(t, a), a, again.ID, "not approved yet")
	wantProblem(t, err, ledger.ErrInvalidState)
	_, err = e.svc.AdminMarkPayoutFailed(ctxOf(t, a), a, again.ID, "")
	wantProblem(t, err, problem.ErrValidation)
	if b := e.balance(t, payee); b != 1_080_000 {
		t.Fatalf("a failed payout moved money: balance %d", b)
	}
}

// TestCancelPayout_OnlyBeforePaid: pending and approved payouts are
// cancelled (lines released); a paid one is final (409).
func TestCancelPayout_OnlyBeforePaid(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	a := actor(t, "finance_admin")
	payee := e.seller(t, "club_a")
	l := e.payable(t, payee)
	pending := e.create(t, "finance_admin", payee)
	v, err := e.svc.AdminCancelPayout(ctxOf(t, a), a, pending.ID, "Yanlış kalem")
	if err != nil || v.Status != ledger.StatusCancelled || v.CancelledAt == nil || len(v.Items) != 0 {
		t.Fatalf("cancelled pending %+v, %v", v, err)
	}
	approved := e.create(t, "finance_admin_2", payee, l)
	e.approve(t, "finance_admin", approved.ID)
	if v, err = e.svc.AdminCancelPayout(ctxOf(t, a), a, approved.ID, "EFT yapılmadı"); err != nil || v.Status != ledger.StatusCancelled {
		t.Fatalf("cancelled approved %+v, %v", v, err)
	}
	paid, _, _ := e.approved(t)
	e.markPaid(t, "finance_admin", paid.ID)
	_, err = e.svc.AdminCancelPayout(ctxOf(t, a), a, paid.ID, "geç")
	wantProblem(t, err, ledger.ErrInvalidState)
	_, err = e.svc.AdminCancelPayout(ctxOf(t, a), a, pending.ID, "tekrar")
	wantProblem(t, err, ledger.ErrInvalidState)
	receipt := e.files.put(files.PurposePayoutReceipt)
	_, err = e.svc.AdminMarkPayoutPaid(ctxOf(t, a), a, approved.ID, ledger.MarkPaidInput{BankReference: "EFT-9", ReceiptFileID: &receipt})
	wantProblem(t, err, ledger.ErrInvalidState)
	if n := len(e.audit.of(authz.LedgerPayoutCancel)); n != 2 {
		t.Fatalf("%d cancel audit rows", n)
	}
}

// TestPayout_FrozenIBANSnapshot: the payout keeps the IBAN and holder of the
// account approved at its creation; once the payee's account changes, the
// payout cannot be approved (409 ledger.bank_account_changed) and a new
// payout freezes the new account.
func TestPayout_FrozenIBANSnapshot(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	e.payable(t, payee)
	before, _ := e.payees.ApprovedAccount(t.Context(), payee)
	p := e.create(t, "finance_admin_2", payee)
	if p.IBANSnapshot != before.IBAN || p.HolderSnapshot != before.HolderName || p.BankAccountID != before.ID {
		t.Fatalf("snapshot %s/%s/%s, account %s/%s/%s", p.IBANSnapshot, p.HolderSnapshot, p.BankAccountID,
			before.IBAN, before.HolderName, before.ID)
	}
	after := e.payees.approve(payee, testIBANs[3], "Yeni Vakıf")
	a := actor(t, "finance_admin")
	got, err := e.svc.AdminGetPayout(ctxOf(t, a), a, p.ID)
	if err != nil || got.IBANSnapshot != before.IBAN || got.HolderSnapshot != before.HolderName {
		t.Fatalf("snapshot after the change %+v, %v", got.Payout, err)
	}
	_, err = e.svc.AdminApprovePayout(ctxOf(t, a), a, p.ID, identity.ReauthProof{Password: reauthPassword})
	wantProblem(t, err, ledger.ErrBankAccountChanged)
	if _, err := e.svc.AdminCancelPayout(ctxOf(t, a), a, p.ID, "Hesap değişti"); err != nil {
		t.Fatal(err)
	}
	q := e.create(t, "finance_admin_2", payee)
	if q.IBANSnapshot != after.IBAN || q.HolderSnapshot != "Yeni Vakıf" || q.BankAccountID != after.ID {
		t.Fatalf("new payout snapshot %s/%s", q.IBANSnapshot, q.HolderSnapshot)
	}
}

// TestGetPayeeBalance_PayeeOfOtherOrg_404 (06 H-38): the payee must belong
// to the org of the path; a non-member of the org is 404 too.
func TestGetPayeeBalance_PayeeOfOtherOrg_404(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	own, other := e.seller(t, "club_a"), e.seller(t, "club_b")
	e.payable(t, own)
	e.payable(t, other)
	club := actor(t, "club_admin")
	bal, err := e.svc.GetPayeeBalance(ctxOf(t, club), club, authztest.ID("club_a"), own)
	if err != nil || bal.Payable != 1_080_000 || bal.PendingPayout != 0 || bal.PaidOut != 0 {
		t.Fatalf("own balance %+v, %v", bal, err)
	}
	_, err = e.svc.GetPayeeBalance(ctxOf(t, club), club, authztest.ID("club_a"), other)
	wantProblem(t, err, ledger.ErrNotFound)
	fin := actor(t, "finance_admin")
	_, err = e.svc.GetPayeeBalance(ctxOf(t, fin), fin, authztest.ID("club_a"), other)
	wantProblem(t, err, ledger.ErrNotFound)
	_, err = e.svc.GetPayeeBalance(ctxOf(t, club), club, authztest.ID("club_b"), other)
	wantProblem(t, err, authz.ErrNotFound)
	_, err = e.svc.GetPayeeBalance(ctxOf(t, club), club, authztest.ID("club_a"), uuid.NewV7())
	wantProblem(t, err, ledger.ErrNotFound)
	member := actor(t, "club_member")
	_, err = e.svc.GetPayeeBalance(ctxOf(t, member), member, authztest.ID("club_a"), own)
	wantProblem(t, err, authz.ErrForbidden)
	_, err = e.svc.GetPayeeBalance(ctxOf(t, fin), fin, uuid.NewV7(), own)
	wantProblem(t, err, problem.ErrNotFound)
	// A payout in progress is part of the balance and shown as pending.
	e.create(t, "finance_admin", own)
	bal, _ = e.svc.GetPayeeBalance(ctxOf(t, club), club, authztest.ID("club_a"), own)
	if bal.Payable != 1_080_000 || bal.PendingPayout != 1_080_000 {
		t.Fatalf("balance with a pending payout %+v", bal)
	}
}

// TestOrgPayouts_IBANMasked (04 §2.17): the organization sees its payouts
// with the IBAN masked and without the finance admins' identities, its own
// payouts only; finance gets the full snapshot on /admin.
func TestOrgPayouts_IBANMasked(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	p, _, _ := e.approved(t)
	paid := e.markPaid(t, "finance_admin", p.ID)
	other := e.seller(t, "club_b")
	e.payable(t, other)
	otherPayout := e.create(t, "finance_admin", other)
	app := newHTTP(t, e.env)

	call := func(persona, path string) (int, string) {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1"+path, nil)
		if pp, _ := authztest.Get(persona); pp.Token != "" {
			req.Header.Set("Authorization", "Bearer "+pp.Token)
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	club := authztest.ID("club_a").String()
	for _, path := range []string{"/orgs/" + club + "/payouts", "/orgs/" + club + "/payouts/" + p.ID.String()} {
		code, body := call("club_admin", path)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		if strings.Contains(body, paid.IBANSnapshot) || strings.Contains(body, "iban_snapshot") ||
			strings.Contains(body, "created_by") || strings.Contains(body, authztest.ID("finance_admin").String()) {
			t.Fatalf("%s leaks the IBAN or the staff: %s", path, body)
		}
		if !strings.Contains(body, paid.MaskedIBAN()) {
			t.Fatalf("%s: no masked IBAN in %s", path, body)
		}
	}
	code, body := call("club_admin", "/orgs/"+club+"/payouts/"+p.ID.String())
	var dto map[string]any
	_ = json.Unmarshal([]byte(body), &dto)
	if code != http.StatusOK || dto["receipt_url"] == nil || dto["status"] != "paid" {
		t.Fatalf("org payout %d %v", code, dto)
	}
	if code, _ := call("club_admin", "/orgs/"+club+"/payouts/"+otherPayout.ID.String()); code != http.StatusNotFound {
		t.Fatalf("another org's payout: %d", code)
	}
	if code, _ := call("other_club_admin", "/orgs/"+club+"/payouts"); code != http.StatusNotFound {
		t.Fatalf("non-member: %d", code)
	}
	code, body = call("finance_admin", "/admin/payouts/"+p.ID.String())
	if code != http.StatusOK || !strings.Contains(body, `"iban_snapshot":"`+paid.IBANSnapshot+`"`) {
		t.Fatalf("admin payout %d %s", code, body)
	}
}

// TestPayout_Notices: payout.created asks the other finance admin,
// payout.approved tells the finance admins but the approver, payout.paid
// e-mails the payee and tells the seller org's admins (01 §6.2).
func TestPayout_Notices(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	reg := outbox.NewRegistry()
	e.mod.Subscribe(reg)
	seen := 0
	dispatch := func() { // the worker runs each task right after its step
		t.Helper()
		evs := e.outbox.Events()
		for _, env := range evs[seen:] {
			for _, s := range reg.For(env.Topic) {
				if err := s.Handle(t.Context(), env); err != nil {
					t.Fatalf("%s: %v", s.Name, err)
				}
			}
		}
		seen = len(evs)
	}
	payee := e.seller(t, "club_a")
	e.payable(t, payee)
	p := e.create(t, "finance_admin_2", payee)
	dispatch()
	e.approve(t, "finance_admin", p.ID)
	dispatch()
	e.markPaid(t, "finance_admin", p.ID)
	dispatch()
	byTmpl := map[string][]notify.Message{}
	for _, m := range e.sender.all() {
		byTmpl[m.Template] = append(byTmpl[m.Template], m)
	}
	fin, fin2 := authztest.ID("finance_admin"), authztest.ID("finance_admin_2")
	if c := byTmpl["ledger.payout_created"]; len(c) != 1 || *c[0].UserID != fin {
		t.Fatalf("created notices %+v", c)
	}
	if c := byTmpl["ledger.payout_approved"]; len(c) != 1 || *c[0].UserID != fin2 {
		t.Fatalf("approved notices %+v", c)
	}
	// A stale task (the payout moved on) sends nothing.
	before := len(e.sender.all())
	for _, env := range e.outbox.Events()[:1] {
		for _, s := range reg.For(env.Topic) {
			if err := s.Handle(t.Context(), env); err != nil {
				t.Fatal(err)
			}
		}
	}
	if after := len(e.sender.all()); after != before {
		t.Fatalf("a stale payout.created sent %d notices", after-before)
	}
	var mail, inApp int
	for _, m := range byTmpl["ledger.payout_paid"] {
		switch m.Channel {
		case notify.ChannelEmail:
			mail++
			if m.ToEmail != "vakif@example.test" || m.Data["iban_masked"] == p.IBANSnapshot || m.Data["bank_reference"] != "EFT-1" {
				t.Fatalf("payee e-mail %+v", m)
			}
		case notify.ChannelInApp:
			inApp++
			if *m.UserID != authztest.ID("club_admin") || !strings.HasSuffix(m.LinkPath, "/payouts/"+p.ID.String()) {
				t.Fatalf("org notice %+v", m)
			}
		}
	}
	if mail != 1 || inApp != 1 {
		t.Fatalf("paid notices: %d e-mail, %d in-app", mail, inApp)
	}
}

// TestPayout_OpenCommerceAndPrivacy: an open payout blocks closing its
// seller org; the KVKK provider exports the staff's payout actions and
// retains them.
func TestPayout_OpenCommerceAndPrivacy(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	payee := e.seller(t, "club_a")
	e.payable(t, payee)
	club := authztest.ID("club_a")
	if open, err := e.mod.OpenCommerce(t.Context(), club); err != nil || open {
		t.Fatalf("no payout yet: %v, %v", open, err)
	}
	p := e.create(t, "finance_admin", payee)
	if open, _ := e.mod.OpenCommerce(t.Context(), club); !open {
		t.Fatal("a pending payout does not block closing the org")
	}
	if open, _ := e.mod.OpenCommerce(t.Context(), authztest.ID("club_b")); open {
		t.Fatal("another org is blocked")
	}
	e.approve(t, "finance_admin_2", p.ID)
	e.markPaid(t, "finance_admin", p.ID)
	if open, _ := e.mod.OpenCommerce(t.Context(), club); open {
		t.Fatal("a paid payout still blocks closing the org")
	}
	sec, err := e.mod.Privacy.Export(t.Context(), authztest.ID("finance_admin"))
	if err != nil || len(sec.Items) != 2 || sec.Items[0]["record"] != "payout_created" || sec.Items[1]["record"] != "payout_marked_paid" {
		t.Fatalf("export %+v, %v", sec.Items, err)
	}
	res, err := e.mod.Privacy.Erase(t.Context(), authztest.ID("finance_admin_2"))
	if err != nil || res.Retained != 1 || res.RetainedReason == "" || res.Deleted != 0 {
		t.Fatalf("erase %+v, %v", res, err)
	}
}

// TestAdminListPayables_AsOf: the payout hold is measured at as_of; lines
// in a payout are left out; every payee the ledger owes is listed.
func TestAdminListPayables_AsOf(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	a := actor(t, "finance_admin")
	p1, p2 := e.seller(t, "club_a"), e.seller(t, "club_b")
	l1 := e.payable(t, p1)
	e.payable(t, p2)
	before := t0.Add(-48 * time.Hour)
	gs, err := e.svc.AdminListPayables(ctxOf(t, a), a, ledger.PayablesQuery{AsOf: &before})
	if err != nil || len(gs) != 0 {
		t.Fatalf("payables two days ago %+v, %v", gs, err)
	}
	gs, err = e.svc.AdminListPayables(ctxOf(t, a), a, ledger.PayablesQuery{})
	if err != nil || len(gs) != 2 || gs[0].Total != 1_080_000 {
		t.Fatalf("payables now %+v, %v", gs, err)
	}
	e.create(t, "finance_admin", p1, l1)
	gs, _ = e.svc.AdminListPayables(ctxOf(t, a), a, ledger.PayablesQuery{})
	if len(gs) != 1 || gs[0].PayeeID != p2 {
		t.Fatalf("payables after a payout %+v", gs)
	}
	pad := actor(t, "platform_admin")
	_, err = e.svc.AdminListPayables(ctxOf(t, pad), pad, ledger.PayablesQuery{})
	wantProblem(t, err, authz.ErrForbidden)
}

// TestPayouts_WithoutPayoutFlow: a poster-only ledger (no Deps.Payouts)
// answers the payout operations with 503 and blocks nothing.
func TestPayouts_WithoutPayoutFlow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := actor(t, "finance_admin")
	_, err := e.svc.AdminListPayouts(ctxOf(t, a), a, ledger.PayoutQuery{})
	wantProblem(t, err, problem.ErrUnavailable)
	if open, err := e.mod.OpenCommerce(t.Context(), uuid.NewV7()); err != nil || open {
		t.Fatalf("open commerce %v, %v", open, err)
	}
	// A half-wired payout flow stops the process at start.
	_, err = ledger.New(ledger.Deps{Store: e.store, Tx: nil, Clock: e.clk, Log: nil, Payouts: ledger.NewMemStore(e.clk.Now)})
	if err == nil || !strings.Contains(err.Error(), "Orders") {
		t.Fatalf("half-wired New: %v", err)
	}
}
