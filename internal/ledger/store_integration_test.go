//go:build integration

package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/internal/authz"
	"koro/internal/ledger"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/iban"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

var seq atomic.Int64

// pgWorld writes the parent rows of ledger entries through the app_rw pool
// (02 §17–19: payees with their seller org, order lines of a payee with
// their order, offering and policy), following the BE-034 schema tests.
type pgWorld struct {
	t *testing.T
	d *db.DB

	mu    sync.Mutex
	shops map[uuid.UUID]*shop // by payee
}

// shop is a seller (club + foundation payee with an approved account) and
// what its order lines refer to.
type shop struct {
	sellerOrg, party, payee, account uuid.UUID
	iban                             string
	buyerOrg, buyerParty, buyerUser  uuid.UUID
	service, offering, policy        uuid.UUID
}

func newPgWorld(t *testing.T, d *db.DB) *pgWorld {
	return &pgWorld{t: t, d: d, shops: map[uuid.UUID]*shop{}}
}

func n() string { return strconv.FormatInt(seq.Add(1), 10) }

func (w *pgWorld) exec(sql string, args ...any) {
	w.t.Helper()
	if _, err := w.d.Pool.Exec(context.Background(), sql, args...); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
}

func (w *pgWorld) count(sql string, args ...any) int {
	w.t.Helper()
	var c int
	if err := w.d.Pool.QueryRow(context.Background(), sql, args...).Scan(&c); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
	return c
}

// commit runs the statements in one app_rw transaction.
func (w *pgWorld) commit(stmts ...stmt) error {
	ctx := context.Background()
	tx, err := w.d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (w *pgWorld) mustCommit(stmts ...stmt) {
	w.t.Helper()
	if err := w.commit(stmts...); err != nil {
		w.t.Fatalf("commit: %v", err)
	}
}

type stmt struct {
	sql  string
	args []any
}

func st(sql string, args ...any) stmt { return stmt{sql: sql, args: args} }

func (w *pgWorld) user() uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO users (id, status, email, email_normalized, first_name, last_name, signup_persona)
		VALUES ($1, 'active', $2, $2, 'Ada', 'Yılmaz', 'company')`, id, "u"+n()+"@example.test")
	return id
}

func (w *pgWorld) org(kind string) uuid.UUID {
	w.t.Helper()
	id, k := uuid.NewV7(), n()
	w.exec(`INSERT INTO organizations (id, kind, name, slug) VALUES ($1, $2, $3, $4)`, id, kind, "Org "+k, "org-"+k)
	return id
}

func (w *pgWorld) file(purpose string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO files (id, purpose, bucket, storage_key, declared_mime) VALUES ($1, $2, 'koro-files-dev', $3, 'application/pdf')`,
		id, purpose, purpose+"/2026/10/"+id.String())
	return id
}

func (w *pgWorld) legalParty(kind, legalType string, org *uuid.UUID) uuid.UUID {
	w.t.Helper()
	id, k := uuid.NewV7(), n()
	w.exec(`INSERT INTO legal_parties (id, kind, legal_type, org_id, title, tax_no) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, kind, legalType, org, "Taraf "+k, strings.Repeat("0", 10-len(k))+k)
	return id
}

// ibanFor returns a valid TR IBAN (bank code 99999) that differs for every k.
func ibanFor(k int64) string {
	bban := "99999" + "0" + fmt.Sprintf("%016d", k)
	rem := 0
	for _, part := range []string{bban, "2927", "00"} {
		for i := range len(part) {
			rem = (rem*10 + int(part[i]-'0')) % 97
		}
	}
	s := fmt.Sprintf("TR%02d%s", 98-rem, bban)
	if !iban.Valid(s) {
		panic("ibanFor built an invalid IBAN: " + s)
	}
	return s
}

// payee creates a seller (club at a university, foundation payee, approved
// bank account, offering under a 10 % model A policy) and returns the
// payee ID; line creates order lines for it.
func (w *pgWorld) payee() uuid.UUID {
	w.t.Helper()
	s := &shop{}
	uniOrg := w.org("university")
	w.exec(`INSERT INTO universities (id, org_id, yok_code, name, city, type) VALUES ($1, $2, $3, $4, 'Ankara', 'state')`,
		uuid.NewV7(), uniOrg, "YOK"+n(), "Üniversite "+n())
	var uni uuid.UUID
	if err := w.d.Pool.QueryRow(context.Background(), `SELECT id FROM universities WHERE org_id = $1`, uniOrg).Scan(&uni); err != nil {
		w.t.Fatal(err)
	}
	s.sellerOrg = w.org("club")
	w.exec(`INSERT INTO clubs (org_id, university_id, category) VALUES ($1, $2, 'academic')`, s.sellerOrg, uni)
	s.party = w.legalParty("payee", "foundation", nil)
	s.payee = uuid.NewV7()
	w.exec(`INSERT INTO payees (id, legal_party_id, org_id, status, verified_at, verified_by, created_by)
		VALUES ($1, $2, $3, 'active', now(), $4, $5)`, s.payee, s.party, s.sellerOrg, w.user(), w.user())
	s.account, s.iban = uuid.NewV7(), ibanFor(seq.Add(1))
	w.exec(`INSERT INTO payee_bank_accounts (id, payee_id, org_id, iban, holder_name, evidence_file_id, status, submitted_by, submitted_by_admin)
		VALUES ($1, $2, $3, $4, 'Ankara Üniversitesi Vakfı', $5, 'pending_approval', $6, true)`,
		s.account, s.payee, s.sellerOrg, s.iban, w.file("bank_account_evidence"), w.user())
	w.exec(`UPDATE payee_bank_accounts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`, s.account, w.user())

	s.buyerOrg, s.buyerUser = w.org("company"), w.user()
	s.buyerParty = w.legalParty("buyer_company", "company", &s.buyerOrg)
	s.service = uuid.NewV7()
	w.exec(`INSERT INTO services (id, code, name_tr, category, fulfillment_type) VALUES ($1, $2, 'Stant', 'booth', 'org_bound')`,
		s.service, "booth_"+n())
	s.offering = uuid.NewV7()
	w.exec(`INSERT INTO offerings (id, service_id, seller_kind, org_id, default_payee_id, title, unit_price_amount, vat_rate_bp,
		status, submitted_at, approved_by, approved_at, created_by)
		VALUES ($1, $2, 'org', $3, $4, 'Bahar şenliği stant', 1000000, 2000, 'active', now(), $5, now(), $6)`,
		s.offering, s.service, s.sellerOrg, s.payee, w.user(), w.user())
	s.policy = uuid.NewV7()
	w.exec(`INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis,
		commission_payer, valid_from, created_by)
		VALUES ($1, 'offering', $2, 'A', 'bank_transfer', 1000, 'net_excl_vat', 'seller', now() - interval '1 day', $3)`,
		s.policy, s.offering, w.user())
	w.mu.Lock()
	w.shops[s.payee] = s
	w.mu.Unlock()
	return s.payee
}

func (w *pgWorld) shopOf(payee uuid.UUID) *shop {
	w.t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.shops[payee]
	if !ok {
		w.t.Fatalf("payee %s was not created by this world", payee)
	}
	return s
}

// line writes a paid order of the payee with one fulfilled line (02 §19.2
// example: payable 1,200,000, seller net 1,080,000, commission 100,000 +
// 20,000 VAT) and returns (order, line).
func (w *pgWorld) line(payee uuid.UUID) (order, line uuid.UUID) {
	w.t.Helper()
	s := w.shopOf(payee)
	order, line = uuid.NewV7(), uuid.NewV7()
	k := n()
	w.mustCommit(
		st(`INSERT INTO orders (id, order_no, org_id, placed_by_user_id, buyer_party_id, buyer_snapshot, model_code, collection_method,
			subtotal_amount, vat_amount, total_amount, payable_amount, koro_collect_amount, expires_at)
			VALUES ($1, $2, $3, $4, $5, '{"title":"Örnek A.Ş."}'::jsonb, 'A', 'bank_transfer', 1000000, 200000, 1200000, 1200000, 1200000,
			now() + interval '7 days')`, order, "KR"+strings.Repeat("0", 8-len(k))+k, s.buyerOrg, s.buyerUser, s.buyerParty),
		st(`INSERT INTO order_lines (id, order_id, org_id, seller_org_id, payee_id, offering_id, service_id, commerce_policy_id,
			title_snapshot, quantity, unit_price_amount, net_amount, vat_rate_bp, vat_amount, gross_amount, commission_rate_bp,
			commission_basis, commission_payer, commission_amount, commission_vat_rate_bp, commission_vat_amount, buyer_fee_amount,
			seller_net_amount, payable_amount, fulfillment_status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'Bahar şenliği stant', 1, 1000000, 1000000, 2000, 200000, 1200000, 1000,
			'net_excl_vat', 'seller', 100000, 2000, 20000, 0, 1080000, 1200000, 'pending')`,
			line, order, s.buyerOrg, s.sellerOrg, payee, s.offering, s.service, s.policy),
	)
	w.exec(`UPDATE orders SET status = 'paid', paid_at = now(), paid_amount = koro_collect_amount WHERE id = $1`, order)
	w.exec(`UPDATE order_lines SET fulfillment_status = 'fulfilled', fulfilled_at = now(), fulfilled_via = 'buyer_confirmed' WHERE id = $1`, line)
	return order, line
}

// allocation is the AllocationLine of a line written by pgWorld.line.
func allocation(payee, line uuid.UUID) ledger.AllocationLine {
	return ledger.AllocationLine{OrderLineID: line, PayeeID: &payee, PayableAmount: 1_200_000,
		SellerNetAmount: 1_080_000, CommissionAmount: 100_000, CommissionVATAmount: 20_000}
}

// approvedPayout creates a payout of the line (1,080,000) by one finance
// admin, approved by another, and returns (payout, creator).
func (w *pgWorld) approvedPayout(payee, line uuid.UUID) (uuid.UUID, uuid.UUID) {
	w.t.Helper()
	s := w.shopOf(payee)
	id, creator := uuid.NewV7(), w.user()
	w.mustCommit(
		st(`INSERT INTO payouts (id, payee_id, org_id, bank_account_id, iban_snapshot, holder_snapshot, amount, created_by)
			VALUES ($1, $2, $3, $4, $5, 'Ankara Üniversitesi Vakfı', 1080000, $6)`, id, payee, s.sellerOrg, s.account, s.iban, creator),
		st(`INSERT INTO payout_items (payout_id, order_line_id, payee_id, amount) VALUES ($1, $2, $3, 1080000)`, id, line, payee),
	)
	w.exec(`UPDATE payouts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`, id, w.user())
	return id, creator
}

// markPaid moves an approved payout to paid in ctx's transaction (receipt,
// bank reference, lines paid out), as BE-044 will.
func (w *pgWorld) markPaid(ctx context.Context, payout, creator, line, receipt uuid.UUID) error {
	conn := db.Conn(ctx, w.d.ORM)
	if err := conn.Exec(`UPDATE payouts SET status = 'paid', paid_at = now(), paid_by = ?, bank_reference = ?, receipt_file_id = ? WHERE id = ?`,
		creator, "EFT-"+n(), receipt, payout).Error; err != nil {
		return err
	}
	return conn.Exec(`UPDATE order_lines SET payout_id = ?, paid_out_at = now() WHERE id = ?`, payout, line).Error
}

func pgStoreWorld(t *testing.T) (ledger.Store, world) {
	d := testutil.NewDB(t)
	w := newPgWorld(t, d)
	return ledger.NewGormStore(d.ORM), world{
		user:  func(*testing.T) uuid.UUID { return w.user() },
		payee: func(*testing.T) uuid.UUID { return w.payee() },
		line: func(_ *testing.T, payee uuid.UUID) uuid.UUID {
			_, l := w.line(payee)
			return l
		},
		tx: d.Tx,
	}
}

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, pgStoreWorld)
}

// pgEnv is the ledger module over PostgreSQL (READ COMMITTED transactions
// of the app_rw pool).
type pgEnv struct {
	*env
	d *db.DB
	w *pgWorld
}

func newPgEnv(t *testing.T) *pgEnv {
	t.Helper()
	d := testutil.NewDB(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Microsecond))
	return &pgEnv{env: newEnvWith(t, clk, ledger.NewGormStore(d.ORM), d.Tx), d: d, w: newPgWorld(t, d)}
}

// txns counts the ledger transactions of a business key and their entries.
func (e *pgEnv) txns(t *testing.T, kind ledger.Kind, ref uuid.UUID) (txns, entries int) {
	t.Helper()
	txns = e.w.count(`SELECT count(*) FROM ledger_transactions WHERE kind = $1 AND ref_id = $2`, kind, ref)
	entries = e.w.count(`SELECT count(*) FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.transaction_id
		WHERE t.kind = $1 AND t.ref_id = $2`, kind, ref)
	return txns, entries
}

// TestChart_MatchesSeed: the Go chart of accounts is the seed of 000110
// (codes, kinds, per-payee flags), all active.
func TestChart_MatchesSeed(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	rows, err := d.Pool.Query(t.Context(), `SELECT code, kind, per_payee, active FROM ledger_accounts ORDER BY code`)
	if err != nil {
		t.Fatal(err)
	}
	type acc struct {
		code, kind       string
		perPayee, active bool
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (acc, error) {
		var a acc
		return a, r.Scan(&a.code, &a.kind, &a.perPayee, &a.active)
	})
	if err != nil {
		t.Fatal(err)
	}
	var want []acc
	for _, c := range ledger.Chart() {
		want = append(want, acc{string(c.Code), string(c.Kind), c.PerPayee, true})
	}
	slices.SortFunc(want, func(a, b acc) int { return strings.Compare(a.code, b.code) })
	if !slices.Equal(got, want) {
		t.Fatalf("ledger_accounts %+v\nwant %+v", got, want)
	}
}

// TestPost_Unbalanced_RejectedBeforeDB_Postgres: over the real store, a
// rejected Txn runs no statement at all.
func TestPost_Unbalanced_RejectedBeforeDB_Postgres(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	qc := testutil.NewQueryCounter()
	tx := ledger.Txn{Kind: ledger.KindPaymentReceived, RefType: ledger.RefPayment, RefID: uuid.NewV7(), Entries: []ledger.Entry{
		{Account: ledger.AccountKoroBank, Amount: 1_200_000}, {Account: ledger.AccountBuyerAdvances, Amount: -1_199_999},
	}}
	_, err := e.svc.Post(qc.Context(t.Context()), tx)
	problemtest.WantErr(t, err, ledger.ErrUnbalanced)
	if qc.Count() != 0 {
		t.Fatalf("%d statements ran for a rejected transaction", qc.Count())
	}
	if c, _ := e.txns(t, tx.Kind, tx.RefID); c != 0 {
		t.Fatal("rejected transaction stored")
	}
	// The valid one runs a handful of statements (lookup, insert, entries, check).
	tx.Entries[1].Amount = -1_200_000
	qc.Reset()
	if _, err := e.svc.Post(qc.Context(t.Context()), tx); err != nil {
		t.Fatal(err)
	}
	if qc.Count() == 0 || qc.Count() > 6 {
		t.Fatalf("Post ran %d statements", qc.Count())
	}
}

// TestPost_DBTriggerCatchesBypass (T-44): rows written around Post with raw
// SQL fail at COMMIT through ledger_txn_balanced; the commit error maps to
// the ledger's problem codes and nothing of the transaction stays.
func TestPost_DBTriggerCatchesBypass(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	payee := e.w.payee()
	header := func(ctx context.Context, id uuid.UUID) error {
		return db.Conn(ctx, e.d.ORM).Exec(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES ( ?, 'adjustment', 'manual', ?, now())`,
			id, uuid.NewV7()).Error
	}
	entry := func(ctx context.Context, txn uuid.UUID, account string, payee *uuid.UUID, amount int64) error {
		return db.Conn(ctx, e.d.ORM).Exec(`INSERT INTO ledger_entries (transaction_id, account_code, payee_id, amount) VALUES ( ?, ?, ?, ?)`,
			txn, account, payee, amount).Error
	}
	for _, c := range []struct {
		name string
		fn   func(ctx context.Context, id uuid.UUID) error
		code string
	}{
		{"one entry", func(ctx context.Context, id uuid.UUID) error {
			return errors.Join(header(ctx, id), entry(ctx, id, "koro_bank", nil, 100))
		}, "ledger.unbalanced"},
		{"sum is not zero", func(ctx context.Context, id uuid.UUID) error {
			return errors.Join(header(ctx, id), entry(ctx, id, "koro_bank", nil, 100), entry(ctx, id, "buyer_advances", nil, -99))
		}, "ledger.unbalanced"},
		{"header without entries", func(ctx context.Context, id uuid.UUID) error { return header(ctx, id) }, "ledger.unbalanced"},
		{"seller_payable without payee", func(ctx context.Context, id uuid.UUID) error {
			return errors.Join(header(ctx, id), entry(ctx, id, "seller_payable", nil, 100), entry(ctx, id, "koro_bank", nil, -100))
		}, "ledger.invalid_transaction"},
		{"payee on koro_bank", func(ctx context.Context, id uuid.UUID) error {
			return errors.Join(header(ctx, id), entry(ctx, id, "koro_bank", &payee, 100), entry(ctx, id, "buyer_advances", nil, -100))
		}, "ledger.invalid_transaction"},
		{"an entry appended to a posted transaction", func(ctx context.Context, id uuid.UUID) error {
			posted, err := e.svc.Post(ctx, txn(t)(ledger.PaymentReceived(ledger.ModelA, id, 500, time.Time{})))
			if err != nil {
				return err
			}
			return entry(ctx, posted, "koro_bank", nil, 1)
		}, "ledger.unbalanced"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			id := uuid.NewV7()
			err := e.d.Tx.WithTx(t.Context(), func(ctx context.Context) error { return c.fn(ctx, id) })
			if err == nil {
				t.Fatal("unbalanced rows committed")
			}
			// WithTx maps the commit-time check_violation generically …
			if !errors.Is(err, problem.ErrValidation) {
				t.Fatalf("commit error %v, want validation_failed from db.Translate", err)
			}
			// … and the ledger names it (T-44).
			pe, ok := errors.AsType[*problem.Error](ledger.TranslateError(err))
			if !ok || pe.Code() != c.code || pe.Status() != 422 {
				t.Fatalf("TranslateError(%v) = %v, want %s", err, pe, c.code)
			}
			if got := e.w.count(`SELECT count(*) FROM ledger_transactions WHERE id = $1 OR ref_id = $1`, id); got != 0 {
				t.Fatalf("%d transactions survived the failed commit", got)
			}
		})
	}
	// app_rw can neither change nor delete a posted row (append-only).
	id := e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 100, time.Time{})))
	for _, sql := range []string{
		`UPDATE ledger_entries SET amount = amount * 2 WHERE transaction_id = $1`,
		`DELETE FROM ledger_entries WHERE transaction_id = $1`,
		`UPDATE ledger_transactions SET memo = 'x' WHERE id = $1`,
	} {
		if _, err := e.d.Pool.Exec(t.Context(), sql, id); err == nil {
			t.Errorf("%s succeeded", sql)
		}
	}
}

// TestPost_InCallerTx_RollbackRemovesEntries (01 §6.3): Post joins the
// caller's transaction; the caller's rollback removes the entries, its
// commit keeps them, and inside the transaction the balance includes its
// own uncommitted entries.
func TestPost_InCallerTx_RollbackRemovesEntries(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	payee := e.w.payee()
	order, line := e.w.line(payee)
	payment := uuid.NewV7()
	capture := func(ctx context.Context) (uuid.UUID, error) {
		if _, err := e.svc.Post(ctx, txn(t)(ledger.PaymentReceived(ledger.ModelA, payment, 1_200_000, time.Time{}))); err != nil {
			return uuid.Nil(), err
		}
		id, err := e.svc.Post(ctx, txn(t)(ledger.OrderAllocated(ledger.ModelA, order, []ledger.AllocationLine{allocation(payee, line)}, time.Time{})))
		if err != nil {
			return uuid.Nil(), err
		}
		b, err := e.svc.PayeeBalance(ctx, payee)
		switch {
		case err != nil:
			return uuid.Nil(), err
		case b != 1_080_000:
			return uuid.Nil(), fmt.Errorf("balance inside the transaction = %d, want 1080000", b)
		}
		// A replay in the same transaction finds the uncommitted row.
		again, err := e.svc.Post(ctx, txn(t)(ledger.OrderAllocated(ledger.ModelA, order, []ledger.AllocationLine{allocation(payee, line)}, time.Time{})))
		switch {
		case err != nil:
			return uuid.Nil(), err
		case again != id:
			return uuid.Nil(), fmt.Errorf("replay in the caller's transaction = %s, want %s", again, id)
		}
		return id, nil
	}

	boom := errors.New("orders.ApplyPayment failed")
	err := e.d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		if _, err := capture(ctx); err != nil {
			return err
		}
		return boom // a later step of the caller fails
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithTx = %v", err)
	}
	for kind, ref := range map[ledger.Kind]uuid.UUID{ledger.KindPaymentReceived: payment, ledger.KindOrderAllocated: order} {
		if tx, en := e.txns(t, kind, ref); tx != 0 || en != 0 {
			t.Fatalf("%s survived the rollback: %d transactions, %d entries", kind, tx, en)
		}
	}
	if b := e.balance(t, payee); b != 0 {
		t.Fatalf("balance after the rollback = %d", b)
	}

	var id uuid.UUID
	if err := e.d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		var err error
		id, err = capture(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if tx, en := e.txns(t, ledger.KindOrderAllocated, order); tx != 1 || en != 4 {
		t.Fatalf("after commit: %d transactions, %d entries", tx, en)
	}
	if b := e.balance(t, payee); b != 1_080_000 {
		t.Fatalf("balance after commit = %d", b)
	}
	// Header and entries carry the same created_at (one transaction, T-226).
	if late := e.w.count(`SELECT count(*) FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.transaction_id
		WHERE t.id = $1 AND e.created_at <> t.created_at`, id); late != 0 {
		t.Fatalf("%d entries with another created_at", late)
	}
}

// TestPost_ConcurrentDuplicate_PostsOnce: the same business event posted
// at the same time — each in its own transaction, or each inside a caller
// transaction that stays open a little longer — creates exactly one ledger
// transaction, and every caller gets its ID.
func TestPost_ConcurrentDuplicate_PostsOnce(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	payee := e.w.payee()
	order, line := e.w.line(payee)
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 1_200_000, time.Time{})))
	alloc := txn(t)(ledger.OrderAllocated(ledger.ModelA, order, []ledger.AllocationLine{allocation(payee, line)}, time.Time{}))
	payout := txn(t)(ledger.PayoutPaid(ledger.ModelA, uuid.NewV7(), payee, 1_000_000, time.Time{}))

	const workers = 8
	race := func(t *testing.T, tx ledger.Txn, inCaller bool) uuid.UUID {
		t.Helper()
		ids := make([]uuid.UUID, workers)
		errs := make([]error, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				<-start
				if !inCaller {
					ids[i], errs[i] = e.svc.Post(context.Background(), tx)
					return
				}
				errs[i] = e.d.Tx.WithTx(context.Background(), func(ctx context.Context) error {
					var err error
					if ids[i], err = e.svc.Post(ctx, tx); err != nil {
						return err
					}
					// The caller's other work keeps its transaction open.
					return db.Conn(ctx, e.d.ORM).Exec(`SELECT pg_sleep(0.05)`).Error
				})
			})
		}
		close(start)
		wg.Wait()
		for i := range workers {
			if errs[i] != nil || ids[i] != ids[0] {
				t.Fatalf("worker %d: %s, %v; worker 0: %s", i, ids[i], errs[i], ids[0])
			}
		}
		if c, en := e.txns(t, tx.Kind, tx.RefID); c != 1 || en != len(tx.Entries) {
			t.Fatalf("%s: %d transactions, %d entries; want 1, %d", tx.Kind, c, en, len(tx.Entries))
		}
		return ids[0]
	}
	race(t, alloc, false)
	if b := e.balance(t, payee); b != 1_080_000 {
		t.Fatalf("balance after a concurrent allocation = %d", b)
	}
	// A debit: the payee lock serialises the copies; the later ones see the
	// first as a replay instead of failing the balance check.
	race(t, payout, true)
	if b := e.balance(t, payee); b != 80_000 {
		t.Fatalf("balance after a concurrent payout = %d (debited more than once?)", b)
	}
	race(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 7, time.Time{})), true)
}

// TestPost_DuplicateWaitsForFirstTransaction: the step-by-step version of
// the race. A second post of the same event while the first caller's
// transaction is still open waits (unique index for credits, payee lock for
// debits) instead of failing the second caller; when the first commits it
// returns the first ID, when the first rolls back it posts its own.
func TestPost_DuplicateWaitsForFirstTransaction(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		debit  bool
		commit bool
	}{
		{"credit, first commits", false, true},
		{"credit, first rolls back", false, false},
		{"debit, first commits", true, true},
		{"debit, first rolls back", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newPgEnv(t) // own database: the lock-wait probe sees only this case
			payee := e.w.payee()
			order, line := e.w.line(payee)
			e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 1_200_000, time.Time{})))
			e.post(t, txn(t)(ledger.OrderAllocated(ledger.ModelA, order, []ledger.AllocationLine{allocation(payee, line)}, time.Time{})))
			// waiting reports whether another session of this database waits for a lock.
			waiting := func() bool {
				return e.w.count(`SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`) > 0
			}
			tx := txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 10, time.Time{}))
			if c.debit {
				tx = txn(t)(ledger.PayoutPaid(ledger.ModelA, uuid.NewV7(), payee, 1_000, time.Time{}))
			}
			posted, release := make(chan uuid.UUID, 1), make(chan error)
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- e.d.Tx.WithTx(context.Background(), func(ctx context.Context) error {
					id, err := e.svc.Post(ctx, tx)
					if err != nil {
						posted <- uuid.Nil()
						return err
					}
					posted <- id
					return <-release
				})
			}()
			first := <-posted
			if first == uuid.Nil() {
				t.Fatal("first post failed")
			}
			type result struct {
				id  uuid.UUID
				err error
			}
			second := make(chan result, 1)
			go func() {
				id, err := e.svc.Post(context.Background(), tx)
				second <- result{id, err}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for !waiting() {
				if time.Now().After(deadline) {
					t.Fatal("the second post never waited for the first transaction")
				}
				select {
				case r := <-second:
					t.Fatalf("the second post returned while the first was open: %+v", r)
				case <-time.After(10 * time.Millisecond):
				}
			}
			if c.commit {
				release <- nil
			} else {
				release <- errors.New("caller failed")
			}
			if err := <-firstDone; (err == nil) != c.commit {
				t.Fatalf("first transaction: %v", err)
			}
			r := <-second
			switch {
			case r.err != nil:
				t.Fatalf("second post: %v", r.err)
			case c.commit && r.id != first:
				t.Fatalf("second post = %s, want the first %s", r.id, first)
			case !c.commit && r.id == first:
				t.Fatal("second post returned the rolled-back ID")
			}
			if n, en := e.txns(t, tx.Kind, tx.RefID); n != 1 || en != 2 {
				t.Fatalf("%d transactions, %d entries; want 1, 2", n, en)
			}
			if want := map[bool]int64{false: 1_080_000, true: 1_079_000}[c.debit]; e.balance(t, payee) != want {
				t.Fatalf("balance %d, want %d (one debit at most)", e.balance(t, payee), want)
			}
		})
	}
}

// TestPost_ConcurrentPayouts_NeverOverdraw: different debits of one payee
// at the same time cannot spend the balance twice.
func TestPost_ConcurrentPayouts_NeverOverdraw(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	payee := e.w.payee()
	order, line := e.w.line(payee)
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 1_200_000, time.Time{})))
	e.post(t, txn(t)(ledger.OrderAllocated(ledger.ModelA, order, []ledger.AllocationLine{allocation(payee, line)}, time.Time{})))

	const workers = 6
	var ok, overdrawn atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			err := e.d.Tx.WithTx(context.Background(), func(ctx context.Context) error {
				_, err := e.svc.Post(ctx, txn(t)(ledger.PayoutPaid(ledger.ModelA, uuid.NewV7(), payee, 400_000, time.Time{})))
				return err
			})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ledger.ErrNegativeBalance):
				overdrawn.Add(1)
			default:
				t.Errorf("Post: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if ok.Load() != 2 || overdrawn.Load() != workers-2 {
		t.Fatalf("%d payouts posted, %d rejected; want 2 and %d", ok.Load(), overdrawn.Load(), workers-2)
	}
	if b := e.balance(t, payee); b != 280_000 {
		t.Fatalf("balance = %d, want 280000", b)
	}
	if neg, err := e.store.NegativePayeeBalances(t.Context(), 10); err != nil || len(neg) != 0 {
		t.Fatalf("negative balances %+v, %v", neg, err)
	}
}

// TestPayeeBalance_AfterAllocation_Postgres: the balance over real order
// lines (the (order_line_id, payee_id) FK of every seller_payable entry
// holds) and per payee.
func TestPayeeBalance_AfterAllocation_Postgres(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	p1, p2 := e.w.payee(), e.w.payee()
	o1, l1 := e.w.line(p1)
	o2, l2 := e.w.line(p1)
	o3, l3 := e.w.line(p2)
	for _, a := range []struct {
		order uuid.UUID
		line  ledger.AllocationLine
	}{{o1, allocation(p1, l1)}, {o2, allocation(p1, l2)}, {o3, allocation(p2, l3)}} {
		e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 1_200_000, time.Time{})))
		e.post(t, txn(t)(ledger.OrderAllocated(ledger.ModelA, a.order, []ledger.AllocationLine{a.line}, time.Time{})))
	}
	if b1, b2 := e.balance(t, p1), e.balance(t, p2); b1 != 2_160_000 || b2 != 1_080_000 {
		t.Fatalf("balances %d / %d", b1, b2)
	}
	// A line of p1 allocated to p2 breaks the (order_line_id, payee_id) FK.
	o4, l4 := e.w.line(p1)
	_, err := e.svc.Post(t.Context(), txn(t)(ledger.OrderAllocated(ledger.ModelA, o4, []ledger.AllocationLine{allocation(p2, l4)}, time.Time{})))
	problemtest.WantErr(t, err, ledger.ErrInvalidTxn)
	// A half refund of l1 takes back half of each share.
	half := split(t)(ledger.SplitRefund(allocation(p1, l1), 600_000))
	e.post(t, txn(t)(ledger.RefundCompleted(ledger.ModelA, uuid.NewV7(), 0, []ledger.RefundLine{half}, time.Time{})))
	if b := e.balance(t, p1); b != 2_160_000-540_000 {
		t.Fatalf("p1 after the refund = %d", b)
	}
	// The ledger as a whole: koro_bank holds 3 × 1,200,000 − 600,000; the
	// advances of allocated orders are used up.
	var bank, advances int64
	if err := e.d.Pool.QueryRow(t.Context(), `SELECT
			coalesce(sum(amount) FILTER (WHERE account_code = 'koro_bank'), 0),
			coalesce(sum(amount) FILTER (WHERE account_code = 'buyer_advances'), 0) FROM ledger_entries`).Scan(&bank, &advances); err != nil {
		t.Fatal(err)
	}
	if bank != 3_000_000 || advances != 0 {
		t.Fatalf("koro_bank %d, buyer_advances %d", bank, advances)
	}
}

// TestIntegrityCheck_DetectsNegativeBalance_Postgres: the nightly checks
// over PostgreSQL find what was written around Post: a negative payee
// balance, an unbalanced transaction (trigger disabled by the owner),
// entries appended in a later transaction (T-226) and payout_paid without
// its payout.
func TestIntegrityCheck_DetectsNegativeBalance_Postgres(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	healthy, broken := e.w.payee(), e.w.payee()
	order, line := e.w.line(healthy)
	e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 1_200_000, time.Time{})))
	allocID := e.post(t, txn(t)(ledger.OrderAllocated(ledger.ModelA, order, []ledger.AllocationLine{allocation(healthy, line)}, time.Time{})))
	if rep, err := e.mod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("healthy ledger: %+v, %v", rep, err)
	}

	// 1. A balanced adjustment that debits a payee never credited.
	adj := uuid.NewV7()
	e.w.mustCommit(
		st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES ($1, 'adjustment', 'manual', $2, now())`, adj, uuid.NewV7()),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, payee_id, amount) VALUES ($1, 'seller_payable', $2, 75000), ($1, 'koro_bank', NULL, -75000)`, adj, broken),
	)
	// 2. Two entries appended to the allocation in a later transaction.
	e.w.mustCommit(st(`INSERT INTO ledger_entries (transaction_id, account_code, amount) VALUES ($1, 'koro_bank', 1), ($1, 'buyer_advances', -1)`, allocID))
	// 3. An unbalanced transaction, possible only with the trigger off.
	owner := testutil.OwnerConn(t, e.d)
	unbalanced := uuid.NewV7()
	for _, sql := range []string{
		`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_txn_balanced`,
		`ALTER TABLE ledger_transactions DISABLE TRIGGER ledger_txn_balanced`,
	} {
		if _, err := owner.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	e.w.mustCommit(
		st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES ($1, 'adjustment', 'manual', $2, now())`, unbalanced, uuid.NewV7()),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, amount) VALUES ($1, 'koro_bank', 500)`, unbalanced),
	)
	for _, sql := range []string{
		`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_txn_balanced`,
		`ALTER TABLE ledger_transactions ENABLE TRIGGER ledger_txn_balanced`,
	} {
		if _, err := owner.Exec(t.Context(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// 4. A payout_paid transaction without a paid payout.
	ghost := uuid.NewV7()
	e.post(t, txn(t)(ledger.PayoutPaid(ledger.ModelA, ghost, healthy, 1_000, time.Time{})))

	rep, err := e.mod.CheckIntegrity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[ledger.FindingKind]int{}
	for _, f := range rep.Findings {
		got[f.Kind]++
		switch f.Kind {
		case ledger.FindingNegativeBalance:
			if *f.PayeeID != broken || *f.Amount != -75_000 {
				t.Errorf("negative balance %+v", f)
			}
		case ledger.FindingUnbalanced:
			if *f.TransactionID != unbalanced || *f.Amount != 500 || f.Entries != 1 {
				t.Errorf("unbalanced %+v", f)
			}
		case ledger.FindingLateEntry:
			if *f.TransactionID != allocID {
				t.Errorf("late entry %+v", f)
			}
		case ledger.FindingPayout:
			if *f.PayoutID != ghost || f.Detail != ledger.MismatchNotPaid {
				t.Errorf("payout %+v", f)
			}
		}
	}
	want := map[ledger.FindingKind]int{ledger.FindingNegativeBalance: 1, ledger.FindingUnbalanced: 1, ledger.FindingLateEntry: 2, ledger.FindingPayout: 1}
	if !maps.Equal(got, want) {
		t.Fatalf("findings %v, want %v (%+v)", got, want, rep.Findings)
	}
	if err := e.mod.Jobs()[0].Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.alarms.all()); n != len(rep.Findings) {
		t.Fatalf("%d alarms for %d findings", n, len(rep.Findings))
	}
}

// TestIntegrityCheck_PaidPayout_Postgres: the payout flow of 01 §6.3 in one
// transaction (payouts paid + lines paid out + ledger.Post payout_paid)
// leaves a clean ledger; a paid payout without its posting or with another
// amount is found.
func TestIntegrityCheck_PaidPayout_Postgres(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	payee := e.w.payee()
	var lines []uuid.UUID
	for range 3 {
		order, line := e.w.line(payee)
		e.post(t, txn(t)(ledger.PaymentReceived(ledger.ModelA, uuid.NewV7(), 1_200_000, time.Time{})))
		e.post(t, txn(t)(ledger.OrderAllocated(ledger.ModelA, order, []ledger.AllocationLine{allocation(payee, line)}, time.Time{})))
		lines = append(lines, line)
	}
	pay := func(line uuid.UUID, post int64) uuid.UUID {
		t.Helper()
		payout, creator := e.w.approvedPayout(payee, line)
		receipt := e.w.file("payout_receipt")
		fin := authz.Actor{Kind: authz.KindUser, UserID: creator}
		if err := e.d.Tx.WithTx(authz.WithActor(t.Context(), fin), func(ctx context.Context) error {
			if err := e.w.markPaid(ctx, payout, creator, line, receipt); err != nil {
				return err
			}
			if post == 0 {
				return nil
			}
			_, err := e.svc.Post(ctx, txn(t)(ledger.PayoutPaid(ledger.ModelA, payout, payee, post, time.Time{})))
			return err
		}); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		return payout
	}
	good := pay(lines[0], 1_080_000)
	if rep, err := e.mod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("after a correct payout: %+v, %v", rep, err)
	}
	if e.w.count(`SELECT count(*) FROM ledger_transactions WHERE ref_id = $1 AND created_by IS NOT NULL`, good) != 1 {
		t.Fatal("payout_paid without created_by")
	}
	missing := pay(lines[1], 0)
	wrong := pay(lines[2], 1_000_000)
	rep, err := e.mod.CheckIntegrity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]string{}
	for _, f := range rep.Findings {
		if f.Kind != ledger.FindingPayout {
			t.Fatalf("unexpected finding %+v", f)
		}
		got[*f.PayoutID] = f.Detail
	}
	if len(got) != 2 || got[missing] != ledger.MismatchMissingTxn || got[wrong] != ledger.MismatchEntries {
		t.Fatalf("findings %v", got)
	}
	if b := e.balance(t, payee); b != 3*1_080_000-1_080_000-1_000_000 {
		t.Fatalf("balance %d", b)
	}
}

// TestGormStore_InsertTxn_NeedsTransaction: outside a transaction the
// header alone would commit and fail the deferred check; the store refuses.
func TestGormStore_InsertTxn_NeedsTransaction(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	h, rows := row(ledger.KindAdjustment, ledger.RefManual, t0,
		ledger.Entry{Account: ledger.AccountKoroBank, Amount: 1}, ledger.Entry{Account: ledger.AccountBuyerAdvances, Amount: -1})
	if ok, err := ledger.NewGormStore(d.ORM).InsertTxn(t.Context(), h, rows); err == nil || ok {
		t.Fatalf("InsertTxn without a transaction: %v, %v", ok, err)
	}
}

// TestPurgeFinancialRecords_Gorm (06 H-57; BE-057): the store reaches
// 000121's financial_records_purge as app_rw: a dry run counts, a real
// purge of the unapproved policy is refused by the database (55000), so
// nothing is destroyed before the accountant's approval.
func TestPurgeFinancialRecords_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	s := ledger.NewGormStore(d.ORM)
	cutoff := ledger.FinancialCutoff(time.Now(), 10*366*24*time.Hour)
	if n, err := s.PurgeFinancialRecords(t.Context(), cutoff, ledger.FinancialBatch, true); err != nil || n != 0 {
		t.Fatalf("dry run = %d, %v", n, err)
	}
	err := d.Tx.WithTx(t.Context(), func(ctx context.Context) error {
		_, err := s.PurgeFinancialRecords(ctx, cutoff, ledger.FinancialBatch, false)
		return err
	})
	if db.SQLState(err) != "55000" {
		t.Fatalf("unapproved purge: %v; want SQLSTATE 55000", err)
	}
}
