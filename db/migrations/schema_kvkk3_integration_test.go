//go:build integration

package migrations_test

import (
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// Tests of the BE-057 (kvkk-3) part of 000121: the purge functions of the
// rows app_rw cannot delete at the end of their retention: entitlements
// with their append-only usages, the signed audit roots and the financial
// records (06 H-57, T-224).

// purgeAt calls a purge function as app_rw with the cut-off now() and
// returns its result or SQLSTATE.
func (w *p1aWorld) purgeAt(fn string, limit int, dryRun bool) (int64, string) {
	w.t.Helper()
	return w.purge(fn, limit, dryRun)
}

// grant inserts an admin grant of the company whose validity ended
// endedAgo (an interval text) ago and returns its id.
func (w *p1aWorld) grant(company, admin uuid.UUID, source, endedAgo string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO entitlements (id, org_id, feature, kind, quantity_total, valid_from, valid_to, status, source_type, source_id,
		granted_by, activated_at)
		VALUES ($1, $2, 'targeted_post', 'quota', 5, now() - $5::interval - interval '30 days', now() - $5::interval, 'expired', $3::text,
		        CASE WHEN $3::text = 'promo' THEN gen_random_uuid() END, $4, now() - $5::interval - interval '30 days')`,
		id, company, source, admin, endedAgo)
	return id
}

// use records one consumption (and optionally its reversal) of the grant.
func (w *p1aWorld) use(ent, company uuid.UUID, reverse bool) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO entitlement_usages (id, entitlement_id, org_id, quantity, ref_type, ref_id) VALUES ($1, $2, $3, 1, 'post', $4)`,
		id, ent, company, uuid.NewV7())
	if reverse {
		w.exec(`INSERT INTO entitlement_usages (entitlement_id, org_id, quantity, ref_type, ref_id, reversal_of) VALUES ($1, $2, -1, 'post', $3, $4)`,
			ent, company, uuid.NewV7(), id)
	}
	return id
}

// TestSchema_EntitlementsPurge (entitlement_other; T-149/T-217): an admin
// or promo grant goes 2 years after it ended, together with its
// append-only usages; a usage that still proves a post keeps the grant;
// order rights and younger grants stay; the batch is bounded by rows.
func TestSchema_EntitlementsPurge(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company, admin := w.org("company"), w.user()
	old := w.grant(company, admin, "admin_grant", "3 years")
	w.use(old, company, true)
	promo := w.grant(company, admin, "promo", "30 months")
	young := w.grant(company, admin, "admin_grant", "1 year")
	proven := w.grant(company, admin, "admin_grant", "3 years")
	usage := w.use(proven, company, false)
	w.post(company, "company", w.user(), companySegmentPost(usage))
	order := w.entitlement(company, map[string]string{"valid_from": "now() - interval '4 years'", "valid_to": "now() - interval '3 years'",
		"status": "'expired'"})

	const fn = "entitlements_purge"
	if _, state := w.purgeAt(fn, 1000, false); state != "55000" {
		t.Fatalf("purge before the approval: SQLSTATE %q, want 55000", state)
	}
	if n, state := w.purgeAt(fn, 1000, true); n != 2 || state != "" {
		t.Fatalf("dry run = %d, %q; want the 2 ended grants", n, state)
	}
	if n := w.count(`SELECT count(*) FROM entitlements`); n != 5 {
		t.Fatalf("a dry run deleted: %d grants left", n)
	}
	w.approve("entitlement_other")
	// Rows: the old grant (1 + 2 usages = 3) does not fit a limit of 2 but,
	// being the first, goes alone; the promo grant waits for the next batch.
	if n, state := w.purgeAt(fn, 2, false); n != 3 || state != "" {
		t.Fatalf("first batch = %d, %q; want the grant with its 2 usages", n, state)
	}
	if n, _ := w.purgeAt(fn, 2, false); n != 1 {
		t.Fatalf("second batch = %d, want the promo grant", n)
	}
	if n, _ := w.purgeAt(fn, 1000, false); n != 0 {
		t.Fatalf("third batch = %d, want 0 (idempotent)", n)
	}
	for id, want := range map[uuid.UUID]int{old: 0, promo: 0, young: 1, proven: 1, order: 1} {
		if n := w.count(`SELECT count(*) FROM entitlements WHERE id = $1`, id); n != want {
			t.Errorf("grant %s: %d rows, want %d", id, n, want)
		}
	}
	if n := w.count(`SELECT count(*) FROM entitlement_usages WHERE entitlement_id = $1`, old); n != 0 {
		t.Errorf("usages of the purged grant left: %d", n)
	}
	// Outside the purge the usages stay append-only.
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw deletes a usage", `DELETE FROM entitlement_usages WHERE id = $1`, []any{usage}, db.CodeInsufficientPriv, ""},
	})
}

// TestSchema_AuditRootsPurge (audit_roots: 10 years): an old root goes only
// when a later root whose chained rows are all purged exists, so the
// verification always has its starting root.
func TestSchema_AuditRootsPurge(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	root := func(daysAgo, years int, lastSeq int64) {
		w.exec(`INSERT INTO audit_daily_roots (day, last_chain_seq, root_hash, signature, key_id, external_ref, created_at)
			VALUES (current_date - $1::int, $2, $3, '\x00', 'k1', 'roots/' || $1::text, now() - make_interval(years => $4))`,
			daysAgo, lastSeq, bytes32(strconv.Itoa(daysAgo)), years)
	}
	root(4500, 12, 10) // purged rows, a later purged root exists → goes
	root(4400, 12, 20) // purged rows; the next root still has rows → the verification starts here
	root(4300, 11, 30) // still holds chained row 25
	root(1, 0, 40)
	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), `INSERT INTO audit_events (actor_kind, system_job, action, outcome, chain_seq, prev_hash, row_hash, chained_at)
		VALUES ('system', 'test', 'test.event.write', 'success', 25, $1, $2, now())`, bytes32("p"), bytes32("r")); err != nil {
		t.Fatal(err)
	}
	const fn = "audit_daily_roots_purge"
	if _, state := w.purgeAt(fn, 1000, false); state != "55000" {
		t.Fatalf("purge before the approval: SQLSTATE %q, want 55000", state)
	}
	if n, _ := w.purgeAt(fn, 1000, true); n != 1 {
		t.Fatalf("dry run = %d, want 1", n)
	}
	w.approve("audit_roots")
	if n, state := w.purgeAt(fn, 1000, false); n != 1 || state != "" {
		t.Fatalf("purge = %d, %q; want 1", n, state)
	}
	if n := w.count(`SELECT count(*) FROM audit_daily_roots`); n != 3 {
		t.Fatalf("roots left = %d, want 3", n)
	}
	if n := w.count(`SELECT count(*) FROM audit_daily_roots WHERE day = current_date - 4400`); n != 1 {
		t.Fatal("the starting root of the verification was purged")
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw deletes a root", `DELETE FROM audit_daily_roots`, nil, db.CodeInsufficientPriv, ""},
	})
}

// financialTables are the tables 000121's financial_records_purge deletes
// from (the target of retention_policies financial_records, files aside).
var financialTables = []string{"orders", "order_lines", "payments", "payment_splits", "refunds", "receivable_collections", "incoming_bank_transfers",
	"ledger_transactions", "ledger_entries", "payouts", "payout_items", "invoices", "invoice_lines", "legal_parties",
	"legal_party_documents", "payees", "payee_bank_accounts"}

// ageAll moves every row of the financial tables (and the collection
// accounts) back by ago, as the owner with the user triggers off (the
// touch and freeze triggers would undo or refuse it).
func ageAll(t *testing.T, owner *pgx.Conn, ago string) {
	t.Helper()
	for _, table := range append([]string{"collection_accounts"}, financialTables...) {
		var sets []string
		for _, col := range []string{"created_at", "updated_at"} {
			var has bool
			if err := owner.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`, table, col).Scan(&has); err != nil {
				t.Fatal(err)
			}
			if has {
				sets = append(sets, col+" = "+col+" - $1::interval")
			}
		}
		if len(sets) == 0 {
			continue
		}
		set := strings.Join(sets, ", ")
		for _, sql := range []string{`ALTER TABLE ` + table + ` DISABLE TRIGGER USER`, `UPDATE ` + table + ` SET ` + set,
			`ALTER TABLE ` + table + ` ENABLE TRIGGER USER`} {
			args := []any{}
			if strings.HasPrefix(sql, "UPDATE") {
				args = append(args, ago)
			}
			if _, err := owner.Exec(t.Context(), sql, args...); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
	}
}

// settledOrder builds a model-A order paid by bank transfer, allocated,
// fulfilled and paid out, with its ledger: the line nets to zero on
// seller_payable. It returns the order, its line and the payout.
func (w *p1aWorld) settledOrder(s shop) (order, line, payout uuid.UUID) {
	w.t.Helper()
	order, line = w.payableLine(s)
	pay := w.payment(order, s.buyer.org)
	w.mustCommit(
		st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES (gen_random_uuid(), 'payment_received', 'payment', $1, now())`, pay),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, amount)
			SELECT id, 'koro_bank', 1200000 FROM ledger_transactions WHERE ref_type = 'payment' AND ref_id = $1
			UNION ALL SELECT id, 'buyer_advances', -1200000 FROM ledger_transactions WHERE ref_type = 'payment' AND ref_id = $1`, pay))
	w.allocate(order, s.seller.payee, line)
	payout, creator := w.payout(s.seller, line)
	w.exec(`UPDATE payouts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`, payout, w.user())
	w.mustCommit(
		st(`UPDATE payouts SET status = 'paid', paid_at = now(), paid_by = $2, bank_reference = $3, receipt_file_id = $4 WHERE id = $1`,
			payout, creator, "EFT-"+w.n(), w.file("payout_receipt")),
		st(`UPDATE order_lines SET payout_id = $2, paid_out_at = now() WHERE id = $1`, line, payout))
	w.payoutTxn(payout, s.seller.payee, line)
	return order, line, payout
}

// allocate posts order_allocated of the order's line: buyer advances +,
// seller_payable − (the line's net), commission and VAT −.
func (w *p1aWorld) allocate(order, payee, line uuid.UUID) {
	w.t.Helper()
	w.mustCommit(
		st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES (gen_random_uuid(), 'order_allocated', 'order', $1, now())`, order),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, payee_id, order_line_id, amount)
			SELECT x.id, v.account, v.payee, v.line, v.amount FROM ledger_transactions x,
			  (VALUES ('buyer_advances', NULL::uuid, NULL::uuid, 1200000::bigint), ('seller_payable', $2, $3, -1080000),
			          ('platform_commission_revenue', NULL, NULL, -100000), ('vat_output', NULL, NULL, -20000)) AS v (account, payee, line, amount)
			 WHERE x.ref_type = 'order' AND x.ref_id = $1`, order, payee, line))
}

// payoutTxn posts payout_paid of the payout: seller_payable + / bank −.
func (w *p1aWorld) payoutTxn(payout, payee, line uuid.UUID) {
	w.t.Helper()
	w.mustCommit(
		st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES (gen_random_uuid(), 'payout_paid', 'payout', $1, now())`, payout),
		st(`INSERT INTO ledger_entries (transaction_id, account_code, payee_id, order_line_id, amount)
			SELECT x.id, v.account, v.payee, v.line, v.amount FROM ledger_transactions x,
			  (VALUES ('seller_payable', $2::uuid, $3::uuid, 1080000::bigint), ('koro_bank', NULL, NULL, -1080000)) AS v (account, payee, line, amount)
			 WHERE x.ref_type = 'payout' AND x.ref_id = $1`, payout, payee, line))
}

// payeeBalances returns Σ seller_payable per payee.
func (w *p1aWorld) payeeBalances() map[uuid.UUID]int64 {
	w.t.Helper()
	rows, err := w.d.Pool.Query(w.t.Context(), `SELECT payee_id, sum(amount)::bigint FROM ledger_entries
		WHERE account_code = 'seller_payable' GROUP BY payee_id`)
	if err != nil {
		w.t.Fatal(err)
	}
	out := map[uuid.UUID]int64{}
	for rows.Next() {
		var id uuid.UUID
		var sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			w.t.Fatal(err)
		}
		out[id] = sum
	}
	return out
}

// TestSchema_FinancialRecordsPurge (06 H-57, T-224): a settled order goes
// with its payments, transfer, ledger, payout and buyer party once all of
// it is older than 10 years counted from the end of its calendar year; an
// open payable, a young ledger transaction of the payout and a record of
// the cut-off's own year keep theirs; no payee balance moves.
func TestSchema_FinancialRecordsPurge(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)

	settled := w.shop()
	orderA, lineA, payoutA := w.settledOrder(settled)
	// Open payable: allocated, never paid out (the line does not net to zero).
	open := w.shop()
	orderB, lineB := w.payableLine(open)
	w.allocate(orderB, open.seller.payee, lineB)
	// Allocated and paid out, but the payout's ledger transaction is posted
	// after the aging (young).
	late := w.shop()
	orderC, lineC := w.payableLine(late)
	w.allocate(orderC, late.seller.payee, lineC)
	payoutC, creatorC := w.payout(late.seller, lineC)
	w.exec(`UPDATE payouts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`, payoutC, w.user())
	w.mustCommit(
		st(`UPDATE payouts SET status = 'paid', paid_at = now(), paid_by = $2, bank_reference = $3, receipt_file_id = $4 WHERE id = $1`,
			payoutC, creatorC, "EFT-"+w.n(), w.file("payout_receipt")),
		st(`UPDATE order_lines SET payout_id = $2, paid_out_at = now() WHERE id = $1`, lineC, payoutC))
	// Loose transfers: one 12 years old, one of the cut-off's own year.
	account := w.collectionAccount()
	oldTransfer := w.transfer(account, nil)
	ageAll(t, owner, "12 years")
	edgeTransfer := w.transfer(account, nil)
	if _, err := owner.Exec(t.Context(), `ALTER TABLE incoming_bank_transfers DISABLE TRIGGER USER;
		UPDATE incoming_bank_transfers SET created_at = now() - interval '10 years 1 day', updated_at = now() - interval '10 years 1 day' WHERE id = '`+
		edgeTransfer.String()+`';
		ALTER TABLE incoming_bank_transfers ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	w.payoutTxn(payoutC, late.seller.payee, lineC) // young
	before := w.payeeBalances()

	const fn = "financial_records_purge"
	if _, state := w.purgeAt(fn, 100, false); state != "55000" {
		t.Fatalf("purge before the approval: SQLSTATE %q, want 55000", state)
	}
	dry, state := w.purgeAt(fn, 100, true)
	if state != "" || dry == 0 {
		t.Fatalf("dry run = %d, %q", dry, state)
	}
	if n := w.count(`SELECT count(*) FROM orders`); n != 3 {
		t.Fatalf("a dry run deleted orders: %d left", n)
	}
	w.approve("financial_records")
	var total int64
	for range 5 {
		n, state := w.purgeAt(fn, 100, false)
		if state != "" {
			t.Fatalf("purge: SQLSTATE %s", state)
		}
		total += n
		if n == 0 {
			break
		}
	}
	// The real run also takes the master rows its own deletions freed (the
	// buyer's legal party), which the dry run still saw referenced.
	if total < dry {
		t.Errorf("purged %d rows, the dry run counted %d", total, dry)
	}
	if n, _ := w.purgeAt(fn, 100, true); n != 0 {
		t.Errorf("dry run after the purge = %d, want 0", n)
	}
	gone := func(table, col string, id uuid.UUID) bool {
		return w.count(`SELECT count(*) FROM `+table+` WHERE `+col+` = $1`, id) == 0
	}
	for _, c := range []struct {
		table, col string
		id         uuid.UUID
		gone       bool
	}{
		{"orders", "id", orderA, true}, {"order_lines", "id", lineA, true}, {"payments", "order_id", orderA, true},
		{"incoming_bank_transfers", "matched_order_id", orderA, true}, {"payouts", "id", payoutA, true},
		{"payout_items", "payout_id", payoutA, true}, {"ledger_transactions", "ref_id", orderA, true},
		{"ledger_transactions", "ref_id", payoutA, true}, {"ledger_entries", "order_line_id", lineA, true},
		{"legal_parties", "id", settled.buyer.party, true}, {"incoming_bank_transfers", "id", oldTransfer, true},
		{"orders", "id", orderB, false}, {"ledger_entries", "order_line_id", lineB, false},
		{"orders", "id", orderC, false}, {"payouts", "id", payoutC, false}, {"ledger_entries", "order_line_id", lineC, false},
		{"incoming_bank_transfers", "id", edgeTransfer, false},
		{"payees", "id", settled.seller.payee, false}, {"legal_parties", "id", settled.seller.party, false},
	} {
		if got := gone(c.table, c.col, c.id); got != c.gone {
			t.Errorf("%s.%s = %s: gone %v, want %v", c.table, c.col, c.id, got, c.gone)
		}
	}
	after := w.payeeBalances()
	for payee, bal := range before {
		if after[payee] != bal {
			t.Errorf("payee %s balance %d → %d", payee, bal, after[payee])
		}
	}
	if n, _ := w.purgeAt(fn, 100, false); n != 0 {
		t.Errorf("a rerun purged %d rows (idempotent: 0)", n)
	}
	// Outside the purge the ledger and the paid payout items stay put.
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw deletes an order", `DELETE FROM orders WHERE id = $1`, []any{orderB}, db.CodeInsufficientPriv, ""},
		{"app_rw deletes a ledger entry", `DELETE FROM ledger_entries`, nil, db.CodeInsufficientPriv, ""},
	})
	if _, err := owner.Exec(t.Context(), `DELETE FROM payout_items WHERE payout_id = $1`, payoutC); db.SQLState(err) != db.CodeRestrictViolation {
		t.Errorf("owner deleting a paid payout's item: %v; want the guard (23001)", err)
	}
}

// TestSchema_FinancialRecordsCutoff_RoundedToYearEnd (TTK md. 82): the
// cut-off is the start of the calendar year of now − 10 years in
// Europe/Istanbul, and a later caller cut-off never widens it.
func TestSchema_FinancialRecordsCutoff_RoundedToYearEnd(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	var got, want string
	if err := owner.QueryRow(t.Context(), `SELECT financial_records_cutoff(now(), 1, true)::text,
		(date_trunc('year', (now() - interval '10 years') AT TIME ZONE 'Europe/Istanbul') AT TIME ZONE 'Europe/Istanbul')::text`).
		Scan(&got, &want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cut-off = %s, want %s", got, want)
	}
	if err := owner.QueryRow(t.Context(), `SELECT (financial_records_cutoff(now() + interval '5 years', 1, true) = financial_records_cutoff(now(), 1, true))::text`).
		Scan(&got); err != nil || got != "true" {
		t.Fatalf("a future caller cut-off widened the window: %s, %v", got, err)
	}
	if err := owner.QueryRow(t.Context(), `SELECT to_char(financial_records_cutoff(now(), 1, true) AT TIME ZONE 'Europe/Istanbul', 'MM-DD HH24:MI')`).
		Scan(&got); err != nil || got != "01-01 00:00" {
		t.Fatalf("cut-off is not a year start in Istanbul: %s, %v", got, err)
	}
}

// TestSchema_KVKK3PurgeFunctionsPrivileges: SECURITY DEFINER, owned by
// app_owner, executable by app_rw only (helpers by nobody).
func TestSchema_KVKK3PurgeFunctionsPrivileges(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	one := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := owner.QueryRow(t.Context(), sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	for fn, rw := range map[string]bool{
		"public.entitlements_purge(timestamptz, integer, boolean)":       true,
		"public.audit_daily_roots_purge(timestamptz, integer, boolean)":  true,
		"public.financial_records_purge(timestamptz, integer, boolean)":  true,
		"public.financial_records_cutoff(timestamptz, integer, boolean)": false,
		"public.financial_order_ok(uuid, timestamptz)":                   false,
		"public.financial_payout_ok(uuid, timestamptz)":                  false,
	} {
		if got := one(`SELECT count(*)::text FROM pg_proc p, aclexplode(p.proacl) a WHERE p.oid = $1::regprocedure AND a.grantee = 0`, fn); got != "0" {
			t.Errorf("PUBLIC may execute %s", fn)
		}
		if got := one(`SELECT proowner::regrole::text FROM pg_proc WHERE oid = $1::regprocedure`, fn); got != "app_owner" {
			t.Errorf("%s owner = %s", fn, got)
		}
		if rw {
			if got := one(`SELECT prosecdef::text FROM pg_proc WHERE oid = $1::regprocedure`, fn); got != "true" {
				t.Errorf("%s is not SECURITY DEFINER", fn)
			}
		}
		for role, want := range map[string]bool{"app_rw": rw, "app_ro": false, "app_talent": false} {
			if got := one(`SELECT has_function_privilege($1, $2, 'EXECUTE')::text`, role, fn); got != strconv.FormatBool(want) {
				t.Errorf("EXECUTE on %s for %s = %s, want %v", fn, role, got, want)
			}
		}
	}
}
