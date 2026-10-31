//go:build integration

package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

// pgPayoutWorld is the payout store contract's world over pgWorld: real
// payees, approved accounts, fulfilled lines and receipts; the lines are
// marked paid out with orders' own statement.
func pgPayoutWorld(t *testing.T) (ledger.PayoutStore, payoutWorld) {
	d := testutil.NewDB(t)
	w := newPgWorld(t, d)
	return ledger.NewGormPayoutStore(d.ORM), payoutWorld{
		user: func(*testing.T) uuid.UUID { return w.user() },
		seller: func(*testing.T) payoutSeller {
			p := w.payee()
			s := w.shopOf(p)
			return payoutSeller{payee: p, org: s.sellerOrg, account: s.account, iban: s.iban, holder: "Ankara Üniversitesi Vakfı"}
		},
		line: func(_ *testing.T, payee uuid.UUID) uuid.UUID {
			_, l := w.line(payee)
			return l
		},
		receipt: func(*testing.T) uuid.UUID { return w.file("payout_receipt") },
		rejectFile: func(_ *testing.T, id uuid.UUID) {
			w.exec(`UPDATE files SET scan_status = 'infected' WHERE id = $1`, id) // BE-048: the scan found malware
		},
		markPaidOut: func(ctx context.Context, payout uuid.UUID, lines []uuid.UUID) error {
			return db.Conn(ctx, d.ORM).Exec(`UPDATE order_lines SET payout_id = ?, paid_out_at = now() WHERE id IN ?`,
				payout, lines).Error
		},
		tx: d.Tx,
	}
}

func TestPayoutStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	payoutStoreContract(t, pgPayoutWorld)
}

// pgPayees is the parties payee port over the payees and
// payee_bank_accounts rows (parties' own read, in SQL).
type pgPayees struct{ d *db.DB }

func (p pgPayees) Get(ctx context.Context, id uuid.UUID) (parties.Payee, error) {
	var out parties.Payee
	err := db.Conn(ctx, p.d.ORM).Where("id = ?", id).Take(&out).Error
	if db.IsNotFound(err) {
		return parties.Payee{}, parties.ErrNotFound
	}
	return out, err
}

func (p pgPayees) ApprovedAccount(ctx context.Context, id uuid.UUID) (parties.BankAccount, error) {
	var out parties.BankAccount
	err := db.Conn(ctx, p.d.ORM).Where("payee_id = ? AND status = 'approved'", id).Take(&out).Error
	if db.IsNotFound(err) {
		return parties.BankAccount{}, parties.ErrNoApprovedAccount
	}
	return out, err
}

func (p pgPayees) IsActive(ctx context.Context, id uuid.UUID) (bool, error) {
	py, err := p.Get(ctx, id)
	return py.Status == parties.PayeeActive, err
}

// Unused ports of the real orders module (a call would panic).
type (
	nopOrdersAudit struct{ orders.AuditLogger }
	nopOrgs        struct{ orders.Orgs }
	nopOfferings   struct{ orders.Offerings }
	nopParties     struct{ orders.Parties }
	nopPayees      struct{ orders.Payees }
	nopEnts        struct{ orders.Entitlements }
)

// pgPayoutEnv is the payout flow over PostgreSQL: the ledger's GORM
// stores, the real orders module (PayableLines, MarkLinesPaidOut over
// order_lines), the payees in SQL, the real outbox writer; audit and files
// are the recording fakes (audit can be made to fail inside the
// transaction).
type pgPayoutEnv struct {
	*payoutEnv
	d *db.DB
	w *pgWorld
}

func newPgPayoutEnv(t *testing.T) *pgPayoutEnv {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	keeper := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()})
	om, err := orders.New(orders.Deps{
		Store: orders.NewGormStore(d.ORM), Tx: d.Tx, Audit: nopOrdersAudit{}, Idempotency: keeper, Orgs: nopOrgs{},
		Offerings: nopOfferings{}, Parties: nopParties{}, Payees: nopPayees{}, Entitlements: nopEnts{},
		Outbox: outbox.NewMem(clk), Clock: clk, Log: obs.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := newPayoutEnvWith(t, clk, ledger.NewGormStore(d.ORM), ledger.NewGormPayoutStore(d.ORM), d.Tx, om.Orders)
	ob := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(), Queue: queue.NewMem(),
		Clock: clk, Log: obs.Discard()})
	// Rebuild the module with the real payees and outbox.
	mod, err := ledger.New(ledger.Deps{
		Store: e.store, Tx: d.Tx, Clock: clk, Log: obs.Discard(), Alarm: e.alarms.raise,
		Payouts: ledger.NewGormPayoutStore(d.ORM), Orders: om.Orders, Payees: pgPayees{d: d}, Parties: fakeParties{p: e.payees},
		Orgs: fakeOrgs{}, Files: e.files, Audit: e.audit, Outbox: ob, Idempotency: keeper, Reauth: fakeReauth{},
		Sender: e.sender, Staff: fakeStaff{}, Users: fakeUsers{},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return &pgPayoutEnv{payoutEnv: e, d: d, w: newPgWorld(t, d)}
}

// finance returns a finance admin actor backed by a users row (the
// created_by/approved_by/paid_by FKs).
func (e *pgPayoutEnv) finance(t *testing.T) authz.Actor {
	t.Helper()
	a := actor(t, "finance_admin")
	a.UserID = e.w.user()
	return a
}

// paidLine writes a paid order with one fulfilled line of payee and posts
// its payment and allocation; the clock moves past the fulfilment.
func (e *pgPayoutEnv) paidLine(t *testing.T, payee uuid.UUID) uuid.UUID {
	t.Helper()
	order, l := e.w.line(payee)
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 1_200_000, time.Time{})))
	e.post(t, txn(t)(ledger.OrderAllocated(ledger.ModelA, order, []ledger.AllocationLine{allocation(payee, l)}, time.Time{})))
	e.clk.Set(time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond))
	return l
}

// approvedPgPayout creates (finance A) and approves (finance B) a payout of
// the payee's payable lines.
func (e *pgPayoutEnv) approvedPgPayout(t *testing.T, payee uuid.UUID) ledger.PayoutView {
	t.Helper()
	a, b := e.finance(t), e.finance(t)
	p, err := e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	v, err := e.svc.AdminApprovePayout(ctxOf(t, b), b, p.ID, identity.ReauthProof{Password: reauthPassword})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return v
}

// receipt registers a payout_receipt file row and its metadata.
func (e *pgPayoutEnv) receipt(t *testing.T) uuid.UUID {
	t.Helper()
	id := e.w.file("payout_receipt")
	e.files.mu.Lock()
	e.files.meta[id] = files.FileMeta{ID: id, Purpose: files.PurposePayoutReceipt, Status: files.StatusActive,
		ScanStatus: files.ScanSkipped, AttachedEntityID: uuid.Nil()}
	e.files.mu.Unlock()
	return id
}

// state reads what the mark-paid chain writes: the payout's status, the
// payout_paid transactions, the line's payout and the payout.paid outbox rows.
func (e *pgPayoutEnv) state(t *testing.T, payout, line uuid.UUID) (status string, txns, linePaid, events int) {
	t.Helper()
	if err := e.d.Pool.QueryRow(t.Context(), `SELECT status FROM payouts WHERE id = $1`, payout).Scan(&status); err != nil {
		t.Fatal(err)
	}
	txns = e.w.count(`SELECT count(*) FROM ledger_transactions WHERE kind = 'payout_paid' AND ref_id = $1`, payout)
	linePaid = e.w.count(`SELECT count(*) FROM order_lines WHERE id = $1 AND payout_id = $2 AND paid_out_at IS NOT NULL`, line, payout)
	events = e.w.count(`SELECT count(*) FROM outbox WHERE topic = 'payout.paid' AND aggregate_id = $1`, payout)
	return status, txns, linePaid, events
}

// TestMarkPaid_LedgerOrdersOutboxSameTx (01 §6.3 "Payout ödendi"): the
// ledger's payout_paid, orders.MarkLinesPaidOut, the payout's paid state
// and the payout.paid outbox row commit together: a failure at the last
// step (the audit write) leaves none of them; the retry writes all.
func TestMarkPaid_LedgerOrdersOutboxSameTx(t *testing.T) {
	t.Parallel()
	e := newPgPayoutEnv(t)
	payee := e.w.payee()
	l := e.paidLine(t, payee)
	p := e.approvedPgPayout(t, payee)
	c := e.finance(t)
	receipt := e.receipt(t)
	in := ledger.MarkPaidInput{BankReference: "EFT-20261120-7781", ReceiptFileID: &receipt}

	e.audit.FailOn(authz.LedgerPayoutMarkPaid, errAuditDown)
	if _, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, c), c, p.ID, in); !errors.Is(err, errAuditDown) {
		t.Fatalf("mark-paid with the audit down: %v", err)
	}
	if status, txns, linePaid, events := e.state(t, p.ID, l); status != "approved" || txns != 0 || linePaid != 0 || events != 0 {
		t.Fatalf("after the rollback: status %s, %d payout_paid, line paid %d, %d events", status, txns, linePaid, events)
	}
	if b := e.balance(t, payee); b != 1_080_000 {
		t.Fatalf("balance after the rollback %d", b)
	}

	e.audit.FailOn("", nil)
	v, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, c), c, p.ID, in)
	if err != nil {
		t.Fatalf("mark-paid: %v", err)
	}
	if status, txns, linePaid, events := e.state(t, p.ID, l); status != "paid" || txns != 1 || linePaid != 1 || events != 1 {
		t.Fatalf("after the commit: status %s, %d payout_paid, line paid %d, %d events", status, txns, linePaid, events)
	}
	if v.PaidBy == nil || *v.PaidBy != c.UserID || v.ReceiptFileID == nil || *v.ReceiptFileID != receipt {
		t.Fatalf("paid payout %+v", v.Payout)
	}
	if b := e.balance(t, payee); b != 0 {
		t.Fatalf("balance after the payout %d", b)
	}
	rep, err := e.mod.CheckIntegrity(t.Context())
	if err != nil || !rep.OK() {
		t.Fatalf("integrity %+v, %v", rep.Findings, err)
	}
	// The ledger rows carry the finance admin who recorded the transfer.
	if n := e.w.count(`SELECT count(*) FROM ledger_transactions WHERE kind = 'payout_paid' AND ref_id = $1 AND created_by = $2`,
		p.ID, c.UserID); n != 1 {
		t.Fatalf("payout_paid created_by: %d rows", n)
	}
}

// TestMarkPaid_ItemsSumMatchesPayout (payout_items_sum, 000115): at COMMIT
// the items of a live payout sum to its amount and a paid payout has every
// line marked paid out; the service's mark-paid satisfies both, a row
// written around it does not commit.
func TestMarkPaid_ItemsSumMatchesPayout(t *testing.T) {
	t.Parallel()
	e := newPgPayoutEnv(t)
	payee := e.w.payee()
	l1, l2 := e.paidLine(t, payee), e.paidLine(t, payee)
	p := e.approvedPgPayout(t, payee)
	if p.Amount != 2_160_000 || len(p.Items) != 2 {
		t.Fatalf("payout %d, %d items", p.Amount, len(p.Items))
	}
	ps := ledger.NewGormPayoutStore(e.d.ORM)

	// Paid with one of its two lines marked: refused at COMMIT.
	c, receipt := e.finance(t), e.receipt(t)
	ref, now := "EFT-1", e.clk.Now()
	paid := p.Payout
	paid.Status, paid.PaidBy, paid.PaidAt, paid.BankReference, paid.ReceiptFileID = ledger.StatusPaid, &c.UserID, &now, &ref, &receipt
	err := e.d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		if err := db.Conn(ctx, e.d.ORM).Exec(`UPDATE order_lines SET payout_id = ?, paid_out_at = now() WHERE id = ?`, p.ID, l1).Error; err != nil {
			return err
		}
		_, err := ps.TransitionPayout(ctx, paid, ledger.StatusApproved)
		return err
	})
	if db.ConstraintName(err) != "payouts_lines_paid_out" {
		t.Fatalf("paid with a line unmarked: %v", err)
	}

	// A payout whose items do not sum to its amount: refused at COMMIT.
	s := e.w.shopOf(payee)
	l3 := e.paidLine(t, payee)
	bad := ledger.Payout{ID: uuid.NewV7(), PayeeID: payee, OrgID: s.sellerOrg, BankAccountID: s.account, IBANSnapshot: s.iban,
		HolderSnapshot: "Ankara Üniversitesi Vakfı", Amount: 1_080_001, Currency: ledger.Currency,
		Status: ledger.StatusPendingApproval, CreatedBy: c.UserID}
	err = e.d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		_, err := ps.InsertPayout(ctx, bad, []ledger.PayoutItem{{PayoutID: bad.ID, OrderLineID: l3, PayeeID: payee, Amount: 1_080_000}})
		return err
	})
	if db.ConstraintName(err) != "payout_items_sum" {
		t.Fatalf("items not summing to the amount: %v", err)
	}

	// The service marks every line and the sums hold.
	if _, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, c), c, p.ID, ledger.MarkPaidInput{BankReference: ref, ReceiptFileID: &receipt}); err != nil {
		t.Fatalf("mark-paid: %v", err)
	}
	if n := e.w.count(`SELECT count(*) FROM payouts p WHERE p.id = $1 AND p.status = 'paid'
		AND p.amount = (SELECT sum(amount) FROM payout_items WHERE payout_id = p.id)
		AND (SELECT count(*) FROM order_lines WHERE payout_id = p.id) = 2`, p.ID); n != 1 {
		t.Fatal("the paid payout's items do not sum to its amount or its lines are not marked")
	}
	if n := e.w.count(`SELECT count(*) FROM order_lines WHERE id IN ($1, $2) AND payout_id = $3`, l1, l2, p.ID); n != 2 {
		t.Fatalf("%d lines marked", n)
	}
	// The items of a paid payout are never deleted.
	if _, err := ps.DeleteItems(t.Context(), p.ID); err == nil {
		t.Fatal("deleted the items of a paid payout")
	}
}

// TestCreatePayout_ConcurrentCreates_OnePayoutPerLine: two finance admins
// create payouts of the same payee at once; the payee lock serialises them:
// one takes the line, the other finds nothing payable.
func TestCreatePayout_ConcurrentCreates_OnePayoutPerLine(t *testing.T) {
	t.Parallel()
	e := newPgPayoutEnv(t)
	payee := e.w.payee()
	e.paidLine(t, payee)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		a := e.finance(t)
		wg.Go(func() {
			_, errs[i] = e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee})
		})
	}
	wg.Wait()
	ok, nothing := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ledger.ErrNothingPayable):
			nothing++
		default:
			t.Fatalf("concurrent create: %v", err)
		}
	}
	if ok != 1 || nothing != 1 || e.w.count(`SELECT count(*) FROM payouts WHERE payee_id = $1`, payee) != 1 {
		t.Fatalf("%d created, %d nothing payable", ok, nothing)
	}
}

// TestApprovePayout_RevokedAccount_Postgres: the account guard of 000113
// and the service agree: an account superseded after the payout's creation
// blocks the approval (409 ledger.bank_account_changed).
func TestApprovePayout_RevokedAccount_Postgres(t *testing.T) {
	t.Parallel()
	e := newPgPayoutEnv(t)
	payee := e.w.payee()
	e.paidLine(t, payee)
	a, b := e.finance(t), e.finance(t)
	p, err := e.svc.AdminCreatePayout(ctxOf(t, a), a, ledger.CreatePayoutInput{PayeeID: payee})
	if err != nil {
		t.Fatal(err)
	}
	e.w.exec(`UPDATE payee_bank_accounts SET status = 'revoked', revoked_by = $2, revoked_at = now(), revoke_reason = 'test'
		WHERE id = $1`, e.w.shopOf(payee).account, e.w.user())
	_, err = e.svc.AdminApprovePayout(ctxOf(t, b), b, p.ID, identity.ReauthProof{Password: reauthPassword})
	if !errors.Is(err, ledger.ErrBankAccountChanged) {
		t.Fatalf("approval after the revocation: %v", err)
	}
	// The database refuses it too, whatever the service saw.
	now := e.clk.Now()
	next := p.Payout
	next.Status, next.ApprovedBy, next.ApprovedAt = ledger.StatusApproved, &b.UserID, &now
	err = e.d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		_, err := ledger.NewGormPayoutStore(e.d.ORM).TransitionPayout(ctx, next, ledger.StatusPendingApproval)
		return err
	})
	if !errors.Is(err, ledger.ErrBankAccountChanged) {
		t.Fatalf("store approval after the revocation: %v", err)
	}
}

// TestReceivableLines_Postgres (BE-129, 06 H-59): on PostgreSQL the open
// receivable lines of a payee are its paid-out lines whose payee_receivable
// sum is positive, with the payout that paid them, and they add up to the
// balance's receivable; another payee's claim is not listed.
func TestReceivableLines_Postgres(t *testing.T) {
	t.Parallel()
	e := newPgPayoutEnv(t)
	payee, other := e.w.payee(), e.w.payee()
	l1, l2 := e.paidLine(t, payee), e.paidLine(t, payee)
	p := e.approvedPgPayout(t, payee)
	c := e.finance(t)
	receipt := e.receipt(t)
	if _, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, c), c, p.ID, ledger.MarkPaidInput{BankReference: "EFT-RCV-1", ReceiptFileID: &receipt}); err != nil {
		t.Fatal(err)
	}
	refund := split(t)(ledger.SplitRefund(allocation(payee, l1), 600_000))
	refund.PaidOut = true
	e.post(t, txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{refund}, time.Time{})))
	ol := e.paidLine(t, other)
	op := e.approvedPgPayout(t, other)
	r2 := e.receipt(t)
	if _, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, c), c, op.ID, ledger.MarkPaidInput{BankReference: "EFT-RCV-2", ReceiptFileID: &r2}); err != nil {
		t.Fatal(err)
	}
	theirs := split(t)(ledger.SplitRefund(allocation(other, ol), 300_000))
	theirs.PaidOut = true
	e.post(t, txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{theirs}, time.Time{})))

	ls, err := e.store.ReceivableLines(t.Context(), payee)
	if err != nil || len(ls) != 1 || ls[0].OrderLineID != l1 || ls[0].PayoutID != p.ID || ls[0].Amount != refund.SellerAmount {
		t.Fatalf("ReceivableLines = %+v, %v (l2 %s)", ls, err, l2)
	}
	if sum, err := e.store.PayeeReceivable(t.Context(), payee); err != nil || sum != ls[0].Amount {
		t.Fatalf("PayeeReceivable = %d, %v", sum, err)
	}
	// Once collected in full the line is closed.
	e.post(t, ledger.Txn{Kind: ledger.KindAdjustment, RefType: ledger.RefManual, RefID: uuid.NewV7(),
		Entries: []ledger.Entry{{Account: ledger.AccountPayeeReceivable, OrderLineID: &l1, Amount: -refund.SellerAmount},
			{Account: ledger.AccountKoroBank, Amount: refund.SellerAmount}}})
	if ls, err := e.store.ReceivableLines(t.Context(), payee); err != nil || len(ls) != 0 {
		t.Fatalf("ReceivableLines after the collection = %+v, %v", ls, err)
	}
}
