//go:build integration

package payments_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/entitlements"
	"koro/internal/files"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var pgSeq atomic.Int64

func pgN() string { return strconv.FormatInt(pgSeq.Add(1), 10) }

func pgExec(t *testing.T, d *db.DB, sql string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func pgUserID(t *testing.T, d *db.DB, uid uuid.UUID) {
	t.Helper()
	pgExec(t, d, `INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company') ON CONFLICT (id) DO NOTHING`, uid, "p"+pgN()+"@example.test")
}

func pgUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	uid := uuid.NewV7()
	pgUserID(t, d, uid)
	return uid
}

// pgPlatformParty inserts Koro's own legal party (one per database).
func pgPlatformParty(t *testing.T, d *db.DB, pid uuid.UUID) {
	t.Helper()
	pgExec(t, d, `INSERT INTO legal_parties (id, kind, legal_type, title) VALUES ($1, 'platform', 'company', 'Koro Teknoloji A.Ş.')`, pid)
}

// pgParents inserts the rows an order of company_a needs: the users of the
// personas the tests act as, company_a with its verified buyer party,
// club_a with its active payee, Koro's party. It returns the parent
// factories of a booth line and of a platform-native line.
func pgParents(t *testing.T, d *db.DB, platformParty uuid.UUID) (booth, platform func(t *testing.T) lineParents) {
	t.Helper()
	for _, n := range []string{"finance_admin", "finance_admin_2", "platform_admin", "platform_admin_2", "company_admin"} {
		pgUserID(t, d, id(n))
	}
	n := pgN()
	pgExec(t, d, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'company', 'Şirket A', $2)`, id("company_a"), "company-a-"+n)
	pgExec(t, d, `INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'club', 'Kulüp A', $2)`, id("club_a"), "club-a-"+n)
	pgExec(t, d, `INSERT INTO legal_parties (id, kind, legal_type, org_id, title) VALUES ($1, 'buyer_company', 'company', $2, 'Örnek A.Ş.')`,
		id("party_buyer_a"), id("company_a"))
	foundation := uuid.NewV7()
	pgExec(t, d, `INSERT INTO legal_parties (id, kind, legal_type, title) VALUES ($1, 'payee', 'foundation', 'Vakıf A')`, foundation)
	pgExec(t, d, `INSERT INTO payees (id, legal_party_id, org_id, status, verified_at, verified_by, created_by)
		VALUES ($1, $2, $3, 'active', now(), $4, $5)`, id("payee_club_a"), foundation, id("club_a"), id("platform_admin_2"), id("platform_admin_2"))
	pgPlatformParty(t, d, platformParty)
	var plat uuid.UUID // the seeded platform org (000031)
	if err := d.Pool.QueryRow(t.Context(), `SELECT id FROM organizations WHERE kind = 'platform'`).Scan(&plat); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	offering := func(t *testing.T, org uuid.UUID, payee *uuid.UUID, native bool) lineParents {
		t.Helper()
		p := lineParents{offering: uuid.NewV7(), service: uuid.NewV7(), policy: uuid.NewV7(), seller: org, payee: payee}
		k := pgN()
		if native {
			pgExec(t, d, `INSERT INTO services (id, code, name_tr, category, fulfillment_type, entitlement_template, accounting_reviewed_at)
				VALUES ($1, $2, 'Hedefli gönderi', 'targeted_post_quota', 'platform_native',
				'{"feature":"targeted_post","kind":"quota","quantity_per_unit":5,"valid_days":90}', now())`, p.service, "tp_p"+k)
			pgExec(t, d, `INSERT INTO offerings (id, service_id, seller_kind, org_id, title, unit_price_amount, vat_rate_bp,
				status, submitted_at, approved_by, approved_at, created_by)
				VALUES ($1, $2, 'platform', $3, '5 hedefli gönderi', 250000, 2000, 'active', now(), $4, now(), $5)`,
				p.offering, p.service, org, id("platform_admin_2"), id("platform_admin"))
		} else {
			pgExec(t, d, `INSERT INTO services (id, code, name_tr, category, fulfillment_type, accounting_reviewed_at)
				VALUES ($1, $2, 'Stant', 'booth', 'org_bound', now())`, p.service, "booth_p"+k)
			pgExec(t, d, `INSERT INTO offerings (id, service_id, seller_kind, org_id, default_payee_id, title, unit_price_amount, vat_rate_bp,
				status, submitted_at, approved_by, approved_at, created_by)
				VALUES ($1, $2, 'org', $3, $4, 'Stant', 1000000, 2000, 'active', now(), $5, now(), $6)`,
				p.offering, p.service, org, *payee, id("platform_admin_2"), id("company_admin"))
		}
		pgExec(t, d, `INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis,
			commission_payer, valid_from, created_by)
			VALUES ($1, 'offering', $2, 'A', 'bank_transfer', 1000, 'net_excl_vat', 'seller', $3, $4)`, p.policy, p.offering, from, id("platform_admin"))
		return p
	}
	payee := id("payee_club_a")
	return func(t *testing.T) lineParents { return offering(t, id("club_a"), &payee, false) },
		func(t *testing.T) lineParents { return offering(t, plat, nil, true) }
}

// newPGEnv is the env over PostgreSQL: the GORM stores of payments, orders,
// entitlements, ledger and idempotency in one database, one TxManager.
func newPGEnv(t *testing.T, opts ...envOption) (*env, *db.DB) {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(t0)
	platform := uuid.NewV7()
	booth, plat := pgParents(t, d, platform)
	b := backend{
		tx: d.Tx, store: payments.NewGormStore(d.ORM), ordStore: orders.NewGormStore(d.ORM), entStore: entitlements.NewGormStore(d.ORM),
		ledStore: ledger.NewGormStore(d.ORM), idem: idempotency.NewGormStore(d.ORM), booth: booth, platform: plat,
		fileAdded: func(t *testing.T, fid uuid.UUID, purpose files.Purpose) {
			pgExec(t, d, `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime) VALUES ($1, $2, 'koro-files-dev', $3, 'application/pdf')`,
				fid, string(purpose), string(purpose)+"/2026/10/"+fid.String())
		},
	}
	return newEnvOn(t, clk, b, platform, opts...), d
}

// pgWorld is the contract world over PostgreSQL.
func pgContractWorld(t *testing.T, d *db.DB) contractWorld {
	t.Helper()
	party := uuid.NewV7()
	booth, _ := pgParents(t, d, party)
	return contractWorld{
		user:  func(t *testing.T) uuid.UUID { return pgUser(t, d) },
		party: party,
		order: func(t *testing.T) contractOrder {
			e := &env{clk: clock.NewFake(t0), b: backend{tx: d.Tx, booth: booth}, ordStore: orders.NewGormStore(d.ORM)}
			p := e.order(t, orderSpec{})
			return contractOrder{id: p.order.ID, org: p.order.OrgID, line: p.lines[0].ID}
		},
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (payments.Store, contractWorld) {
		d := testutil.NewDB(t)
		return payments.NewGormStore(d.ORM), pgContractWorld(t, d)
	})
}

// pgCount returns a count(*) query's answer.
func pgCount(t *testing.T, d *db.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// switchPoster fails the posts of one ledger kind while fail is set.
type switchPoster struct {
	next payments.Poster
	kind ledger.Kind
	fail *atomic.Bool
}

func (p switchPoster) Post(ctx context.Context, t ledger.Txn) (uuid.UUID, error) {
	if p.fail.Load() && t.Kind == p.kind {
		return uuid.Nil(), errors.New("ledger is down")
	}
	return p.next.Post(ctx, t)
}

// TestMatch_SingleTxChain_AllOrNothing (01 §6.3): a ledger failure at the
// last step of the capture (order_allocated) rolls back the whole chain on
// PostgreSQL — the transfer stays unmatched, no payment, split, ledger row
// or refund exists, the order waits for payment with its right pending, no
// event leaves, and the Idempotency-Key is not consumed: the retry with the
// same key captures once the ledger is back.
func TestMatch_SingleTxChain_AllOrNothing(t *testing.T) {
	t.Parallel()
	fail := &atomic.Bool{}
	fail.Store(true)
	e, d := newPGEnv(t, withPoster(func(p payments.Poster) payments.Poster {
		return switchPoster{next: p, kind: ledger.KindOrderAllocated, fail: fail}
	}))
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{platformLine: true})
	tr := e.transfer(t, acct, 1_600_000, p.order.OrderNo, true)
	fin := actor(t, "finance_admin")
	ctx := idempotency.WithKey(ctxOf(t, fin), idempotency.Key{
		Scope: fin.UserID.String() + ":POST /admin/incoming-transfers/{transfer_id}/match", Value: "pg-match-1", Status: 200})
	if _, err := e.matchCtx(ctx, tr, p, ""); err == nil {
		t.Fatal("the match succeeded with the ledger down")
	}
	got, err := e.store.GetTransfer(t.Context(), tr.ID, false)
	if err != nil || got.Status != payments.TransferUnmatched || got.MatchedOrderID != nil {
		t.Fatalf("transfer after the failure = %+v, %v", got, err)
	}
	for table, sql := range map[string]string{
		"payments":            `SELECT count(*) FROM payments`,
		"payment_splits":      `SELECT count(*) FROM payment_splits`,
		"refunds":             `SELECT count(*) FROM refunds`,
		"ledger_transactions": `SELECT count(*) FROM ledger_transactions`,
		"idempotency_keys":    `SELECT count(*) FROM idempotency_keys`,
	} {
		if n := pgCount(t, d, sql); n != 0 {
			t.Errorf("%s has %d rows after the rollback", table, n)
		}
	}
	o, err := e.ordersM.Orders.ForPayment(t.Context(), p.order.ID)
	if err != nil || o.Status != orders.StatusAwaitingPayment || o.PaidAmount != 0 {
		t.Fatalf("order = %+v, %v", o, err)
	}
	rights, _ := e.entStore.BySource(t.Context(), entitlements.SourceOrderLine, p.lines[1].ID)
	if len(rights) != 1 || rights[0].Status != entitlements.StatusPending {
		t.Fatalf("right = %+v", rights)
	}
	if n := len(e.events(domainevent.TopicPaymentCaptured)); n != 0 {
		t.Fatalf("payment.captured left a rolled back transaction (%d)", n)
	}

	fail.Store(false)
	res, err := e.matchCtx(ctx, tr, p, "")
	if err != nil || !res.Order.Paid || res.OverpaymentRefundID == nil {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM payments`); n != 1 {
		t.Fatalf("payments = %d", n)
	}
}

// TestMatch_ConcurrentSameTransfer_OneCapture: six finance admins' clicks
// (different keys) on one transfer at once on PostgreSQL capture it once
// (the transfer row lock, payments_incoming_transfer_id_uq behind it); the
// others get 409 payments.transfer_already_matched.
func TestMatch_ConcurrentSameTransfer_OneCapture(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	tr := e.transfer(t, acct, 1_200_000, p.order.OrderNo, false)
	fin := actor(t, "finance_admin")
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Go(func() {
			ctx := idempotency.WithKey(ctxOf(t, fin), idempotency.Key{
				Scope: fin.UserID.String() + ":POST /admin/incoming-transfers/{transfer_id}/match", Value: fmt.Sprintf("pg-click-%d", i), Status: 200})
			_, errs[i] = e.matchCtx(ctx, tr, p, "")
		})
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, payments.ErrTransferAlreadyMatched):
		default:
			t.Errorf("match: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d captures, want 1", ok)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM payments WHERE incoming_transfer_id = $1`, tr.ID); n != 1 {
		t.Fatalf("payments = %d", n)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE kind = 'payment_received'`); n != 1 {
		t.Fatalf("payment_received = %d", n)
	}
	o, _ := e.ordersM.Orders.ForPayment(t.Context(), p.order.ID)
	if o.PaidAmount != 1_200_000 || o.Status != orders.StatusPaid {
		t.Fatalf("order = %+v", o)
	}
}

// TestMatch_Postgres_Chain: the capture's rows satisfy every CHECK and FK of
// 000101–000112 — a partial payment, then the completing transfer with an
// overpayment on an order with a payee line and Koro's platform-native
// line: the payments, the splits, the overpayment refund, the ledger
// (balanced at COMMIT by ledger_txn_balanced), the right activated.
func TestMatch_Postgres_Chain(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{platformLine: true})
	first, err := e.match(t, e.transfer(t, acct, 500_000, p.order.OrderNo, false), p, "")
	if err != nil || first.Order.Paid {
		t.Fatalf("partial = %+v, %v", first, err)
	}
	second, err := e.match(t, e.transfer(t, acct, 1_100_000, p.order.OrderNo, true), p, "")
	if err != nil || !second.Order.Paid || second.AppliedAmount != 1_000_000 || second.OverpaymentRefundID == nil {
		t.Fatalf("completing = %+v, %v", second, err)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM payment_splits WHERE payment_id = $1`, second.Payment.PaymentID); n != 2 {
		t.Fatalf("splits = %d", n)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM refunds WHERE reason = 'overpayment' AND amount = 100000 AND status = 'requested'
		AND target_iban = $1`, ibanSender); n != 1 {
		t.Fatalf("overpayment refunds = %d", n)
	}
	var sum, bank, advances int64
	if err := d.Pool.QueryRow(t.Context(), `SELECT coalesce(sum(amount), 0),
		coalesce(sum(amount) FILTER (WHERE account_code = 'koro_bank'), 0),
		coalesce(sum(amount) FILTER (WHERE account_code = 'buyer_advances'), 0) FROM ledger_entries`).Scan(&sum, &bank, &advances); err != nil {
		t.Fatal(err)
	}
	if sum != 0 || bank != 1_600_000 || advances != -1_600_000+1_200_000 {
		t.Fatalf("ledger: sum %d, koro_bank %d, buyer_advances %d", sum, bank, advances)
	}
	rights, _ := e.entStore.BySource(t.Context(), entitlements.SourceOrderLine, p.lines[1].ID)
	if len(rights) != 1 || rights[0].Status != entitlements.StatusActive {
		t.Fatalf("right = %+v", rights)
	}
}

// TestTransferReturn_Postgres: the three return steps on PostgreSQL
// (000105's per-state CHECKs, the receipt FK, the frozen target after the
// approval) and the four eyes of the database.
func TestTransferReturn_Postgres(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	tr := e.transfer(t, acct, 77_000, "tanımsız", true)
	fin, fin2 := actor(t, "finance_admin"), actor(t, "finance_admin_2")
	if _, err := e.svc.RequestReturn(ctxOf(t, fin), fin, tr.ID, payments.ReturnRequest{Reason: "Sipariş yok"}, good); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.GetTransfer(t.Context(), tr.ID, false)
	self := got
	at := t0
	self.Status, self.ReturnApprovedBy, self.ReturnApprovedAt = payments.TransferReturnApproved, &fin.UserID, &at
	if err := d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		_, err := e.store.UpdateTransfer(ctx, self, payments.TransferReturnRequested)
		return err
	}); err == nil {
		t.Fatal("the database let the requester approve")
	}
	if _, err := e.svc.DecideReturn(ctxOf(t, fin2), fin2, tr.ID, payments.DecisionApproved, good); err != nil {
		t.Fatal(err)
	}
	approved, _ := e.store.GetTransfer(t.Context(), tr.ID, false)
	moved := approved
	moved.ReturnTargetIBAN = ptr(ibanOther)
	if _, err := e.store.UpdateTransfer(t.Context(), moved, payments.TransferReturnApproved); err == nil {
		t.Fatal("the approved target IBAN changed")
	}
	receipt := e.files.add(files.PurposeRefundReceipt)
	res, err := e.svc.MarkReturned(ctxOf(t, fin), fin, tr.ID, payments.ReturnReceipt{BankReference: "EFT-1", ReceiptFileID: receipt}, good)
	if err != nil || res.Status != payments.TransferReturned {
		t.Fatalf("returned = %+v, %v", res, err)
	}
	final := res
	final.Status = payments.TransferUnmatched
	if _, err := e.store.UpdateTransfer(t.Context(), final, payments.TransferReturned); err == nil {
		t.Fatal("a returned transfer changed")
	}
}

// TestConfirmDirectPayment_Postgres: the direct_confirmed payment of a model
// C order satisfies 000106 (no transfer, captured by the admin, evidence
// FK) and pays the order (orders_paid_full_chk: paid = the fee).
func TestConfirmDirectPayment_Postgres(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	p := e.order(t, orderSpec{modelC: true, paidFee: true})
	evidence := e.files.add(files.PurposeTransferReceipt)
	fin := actor(t, "finance_admin")
	res, err := e.svc.ConfirmDirectPayment(ctxOf(t, fin), fin, payments.DirectInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo,
		EvidenceFileID: evidence})
	if err != nil || !res.Order.Paid || res.Payment.Amount != 1_200_000 {
		t.Fatalf("confirm = %+v, %v", res, err)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM payments WHERE provider = 'direct_confirmed' AND evidence_file_id = $1`, evidence); n != 1 {
		t.Fatalf("direct payments = %d", n)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM orders WHERE id = $1 AND status = 'paid'`, p.order.ID); n != 1 {
		t.Fatal("the order is not paid")
	}
}

// TestImportTransfers_Postgres: the import's ON CONFLICT skips a reference
// recorded earlier and a repeated one inside the file, in one transaction.
func TestImportTransfers_Postgres(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	known := e.transfer(t, acct, 10_000, "elle", false)
	fin := actor(t, "finance_admin")
	csv := "value_date,amount,description,bank_reference\n" +
		"2026-10-04,\"1.200,00\",KR7Q2M9TXA,B-1\n2026-10-04,5,tekrar,B-1\n2026-10-04,7,elle," + known.BankReference + "\n2026-10-04,9,diğer,B-2\n"
	res, err := e.svc.ImportTransfers(ctxOf(t, fin), fin, payments.ImportInput{CollectionAccountID: acct.ID, CSV: csv})
	if err != nil || res.Imported != 2 || res.Duplicates != 2 || res.Rejected != 0 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM incoming_bank_transfers WHERE entry_method = 'csv_import'`); n != 2 {
		t.Fatalf("imported rows = %d", n)
	}
	sum, err := e.mod.RunDigest(t.Context())
	if err != nil || sum.Count != 0 {
		t.Fatalf("digest of fresh transfers = %+v, %v", sum, err)
	}
	e.clk.Advance(25 * time.Hour)
	if sum, err = e.mod.RunDigest(t.Context()); err != nil || sum.Count != 3 {
		t.Fatalf("digest after 25 h = %+v, %v", sum, err)
	}
}

// TestMatch_ConcurrentTransfersSameOrder_Postgres (T-14): two transfers of
// one order captured at once on PostgreSQL — the second capture finds the
// order locked and partly paid, applies what is left and opens the
// overpayment refund for the rest: the order is paid exactly once in full.
func TestMatch_ConcurrentTransfersSameOrder_Postgres(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	p := e.order(t, orderSpec{})
	trs := []payments.IncomingTransfer{
		e.transfer(t, acct, 800_000, p.order.OrderNo, true),
		e.transfer(t, acct, 800_000, p.order.OrderNo, true),
	}
	var wg sync.WaitGroup
	res := make([]payments.MatchResult, len(trs))
	errs := make([]error, len(trs))
	for i := range trs {
		wg.Go(func() { res[i], errs[i] = e.match(t, trs[i], p, "") })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("match %d: %v", i, err)
		}
	}
	if res[0].AppliedAmount+res[1].AppliedAmount != 1_200_000 {
		t.Fatalf("applied %d + %d", res[0].AppliedAmount, res[1].AppliedAmount)
	}
	o, _ := e.ordersM.Orders.ForPayment(t.Context(), p.order.ID)
	if o.Status != orders.StatusPaid || o.PaidAmount != 1_200_000 {
		t.Fatalf("order = %+v", o)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM refunds WHERE reason = 'overpayment' AND amount = 400000`); n != 1 {
		t.Fatalf("overpayment refunds = %d", n)
	}
}
