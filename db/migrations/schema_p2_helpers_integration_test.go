//go:build integration

package migrations_test

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5"

	"koro/internal/platform/db"
	"koro/internal/platform/iban"
)

// Test helpers of the P2 schema tests (migrations 000094–000120: parties,
// catalog, orders, payments, ledger, billing). They extend p1aWorld: every
// row is written through the app_rw pool, so a missing GRANT shows up as a
// failing fixture, and the four-eyes rows go through their real state
// machine (insert in the first state, then UPDATE by another user), because
// koro_initial_status rejects a row born approved.

// ibanFor returns a valid TR IBAN (bank code 99999, which does not exist)
// that differs for every n.
func ibanFor(n int64) string {
	bban := "99999" + "0" + fmt.Sprintf("%016d", n)
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

// iban returns a valid IBAN no other call of this world returned.
func (w *p1aWorld) iban() string { return ibanFor(w.seq.Add(1)) }

// taxNo returns a 10-digit VKN no other call of this world returned.
func (w *p1aWorld) taxNo() string {
	n := w.n()
	return strings.Repeat("0", 10-len(n)) + n
}

// orderNo returns an order number (KR + 8 Crockford characters) that is new
// in this world.
func (w *p1aWorld) orderNo() string {
	n := w.n()
	return "KR" + strings.Repeat("0", 8-len(n)) + n
}

// commit runs fn in a transaction of the app_rw pool and commits it; it
// returns the SQLSTATE ("" on success) and the constraint name of the first
// error, from fn or from COMMIT (deferred constraint triggers fire there).
func (w *p1aWorld) commit(fn func(tx pgx.Tx) error) (state, constraint string) {
	w.t.Helper()
	tx, err := w.d.Pool.Begin(w.t.Context())
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(w.t.Context()) }()
	if err := fn(tx); err != nil {
		return db.SQLState(err), db.ConstraintName(err)
	}
	err = tx.Commit(w.t.Context())
	return db.SQLState(err), db.ConstraintName(err)
}

// stmts returns an fn for commit that runs the statements in order.
func stmts(t *testing.T, list ...stmt) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		for _, s := range list {
			if _, err := tx.Exec(t.Context(), s.sql, s.args...); err != nil {
				return err
			}
		}
		return nil
	}
}

// stmt is one statement of a transaction.
type stmt struct {
	sql  string
	args []any
}

func st(sql string, args ...any) stmt { return stmt{sql: sql, args: args} }

// mustCommit runs the statements in one transaction and fails on error.
func (w *p1aWorld) mustCommit(list ...stmt) {
	w.t.Helper()
	if state, c := w.commit(stmts(w.t, list...)); state != "" {
		w.t.Fatalf("transaction failed: SQLSTATE %s %s", state, c)
	}
}

// expectCommit checks that the statements fail (in the statement or at
// COMMIT) with the SQLSTATE and constraint.
func expectCommit(t *testing.T, w *p1aWorld, name, state, constraint string, list ...stmt) {
	t.Helper()
	gotState, gotConstraint := w.commit(stmts(t, list...))
	if gotState != state || (constraint != "" && gotConstraint != constraint) {
		t.Errorf("%s: SQLSTATE %q constraint %q, want %s %s", name, gotState, gotConstraint, state, constraint)
	}
}

// legalParty inserts a legal party (kind, legal type, org or nil) in
// status draft and returns its id.
func (w *p1aWorld) legalParty(kind, legalType string, org *uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO legal_parties (id, kind, legal_type, org_id, title, tax_no) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, kind, legalType, org, "Taraf "+w.n(), w.taxNo())
	return id
}

// platformParty returns the Koro legal party (one per database).
func (w *p1aWorld) platformParty() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	err := w.d.Pool.QueryRow(w.t.Context(), `SELECT id FROM legal_parties WHERE kind = 'platform'`).Scan(&id)
	if err == nil {
		return id
	}
	if !db.IsNotFound(err) {
		w.t.Fatal(err)
	}
	platform := w.platformOrg()
	return w.legalParty("platform", "company", &platform)
}

// payeeWorld is a seller org with its active payee and the payee's
// approved bank account.
type payeeWorld struct {
	org, party, payee, account uuid.UUID
	iban, holder               string
}

// payee inserts an active payee of the org for the legal party.
func (w *p1aWorld) payee(org, party uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO payees (id, legal_party_id, org_id, status, verified_at, verified_by, created_by)
		VALUES ($1, $2, $3, 'active', now(), $4, $5)`, id, party, org, w.user(), w.user())
	return id
}

// bankAccountSQL inserts an admin-submitted account waiting for approval;
// $1 id, $2 payee, $3 org, $4 iban, $5 evidence file, $6 submitter.
const bankAccountSQL = `INSERT INTO payee_bank_accounts (id, payee_id, org_id, iban, holder_name, evidence_file_id, status, submitted_by, submitted_by_admin)
	VALUES ($1, $2, $3, $4, 'Ankara Üniversitesi Vakfı', $5, 'pending_approval', $6, true)`

// approvedAccount inserts an admin-submitted account of the payee and has a
// second admin approve it; it returns the account id and IBAN.
func (w *p1aWorld) approvedAccount(payee, org uuid.UUID) (uuid.UUID, string) {
	w.t.Helper()
	id, number := uuid.NewV7(), w.iban()
	w.exec(bankAccountSQL, id, payee, org, number, w.file("bank_account_evidence"), w.user())
	w.exec(`UPDATE payee_bank_accounts SET status = 'approved', approved_by = $2, approved_at = now() WHERE id = $1`, id, w.user())
	return id, number
}

// seller builds a club with a payee (foundation) and its approved account.
func (w *p1aWorld) seller() payeeWorld {
	w.t.Helper()
	uni, _ := w.university()
	p := payeeWorld{org: w.club(uni), holder: "Ankara Üniversitesi Vakfı"}
	p.party = w.legalParty("payee", "foundation", nil)
	p.payee = w.payee(p.org, p.party)
	p.account, p.iban = w.approvedAccount(p.payee, p.org)
	return p
}

// service inserts an active event-bound service and returns its id.
func (w *p1aWorld) service() uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO services (id, code, name_tr, category, fulfillment_type) VALUES ($1, $2, 'Stant', 'booth', 'org_bound')`,
		id, "booth_"+w.n())
	return id
}

// offering inserts an approved, active offering of the seller (unit price
// 1,000,000 kuruş, VAT 20 %) and returns its id.
func (w *p1aWorld) offering(seller payeeWorld, service uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO offerings (id, service_id, seller_kind, org_id, default_payee_id, title, unit_price_amount, vat_rate_bp,
		status, submitted_at, approved_by, approved_at, created_by)
		VALUES ($1, $2, 'org', $3, $4, 'Bahar şenliği stant', 1000000, 2000, 'active', now(), $5, now(), $6)`,
		id, service, seller.org, seller.payee, w.user(), w.user())
	return id
}

// policy inserts an open model-A policy (10 % from the seller) for the
// offering and returns its id.
func (w *p1aWorld) policy(offering uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis,
		commission_payer, valid_from, created_by)
		VALUES ($1, 'offering', $2, 'A', 'bank_transfer', 1000, 'net_excl_vat', 'seller', now() - interval '1 day', $3)`,
		id, offering, w.user())
	return id
}

// buyer is a company with its legal party and the user who orders.
type buyer struct{ org, party, user uuid.UUID }

func (w *p1aWorld) buyer() buyer {
	w.t.Helper()
	b := buyer{org: w.org("company"), user: w.user()}
	b.party = w.legalParty("buyer_company", "company", &b.org)
	return b
}

// orderDefaults are the columns of a model-A order of one default line
// (1,000,000 net + 200,000 VAT); $1 id, $2 org, $3 placer, $4 buyer party,
// $5 order no.
var orderDefaults = map[string]string{
	"id":                  "$1",
	"order_no":            "$5",
	"org_id":              "$2",
	"placed_by_user_id":   "$3",
	"buyer_party_id":      "$4",
	"buyer_snapshot":      `'{"title":"Örnek A.Ş."}'::jsonb`,
	"model_code":          "'A'",
	"collection_method":   "'bank_transfer'",
	"subtotal_amount":     "1000000",
	"vat_amount":          "200000",
	"total_amount":        "1200000",
	"payable_amount":      "1200000",
	"koro_collect_amount": "1200000",
	"expires_at":          "now() + interval '7 days'",
}

// lineDefaults are the columns of the default line (02 §19.2 example):
// $1 id, $2 order, $3 buyer org, $4 seller org, $5 payee, $6 offering,
// $7 service, $8 policy.
var lineDefaults = map[string]string{
	"id":                     "$1",
	"order_id":               "$2",
	"org_id":                 "$3",
	"seller_org_id":          "$4",
	"payee_id":               "$5",
	"offering_id":            "$6",
	"service_id":             "$7",
	"commerce_policy_id":     "$8",
	"title_snapshot":         "'Bahar şenliği stant'",
	"quantity":               "1",
	"unit_price_amount":      "1000000",
	"net_amount":             "1000000",
	"vat_rate_bp":            "2000",
	"vat_amount":             "200000",
	"gross_amount":           "1200000",
	"commission_rate_bp":     "1000",
	"commission_basis":       "'net_excl_vat'",
	"commission_payer":       "'seller'",
	"commission_amount":      "100000",
	"commission_vat_rate_bp": "2000",
	"commission_vat_amount":  "20000",
	"buyer_fee_amount":       "0",
	"seller_net_amount":      "1080000",
	"payable_amount":         "1200000",
	"fulfillment_status":     "'pending'",
}

// shop is everything an order line refers to.
type shop struct {
	seller                    payeeWorld
	buyer                     buyer
	service, offering, policy uuid.UUID
}

func (w *p1aWorld) shop() shop {
	w.t.Helper()
	s := shop{seller: w.seller(), buyer: w.buyer(), service: w.service()}
	s.offering = w.offering(s.seller, s.service)
	s.policy = w.policy(s.offering)
	return s
}

// orderStmt and lineStmt build the inserts with overrides; lineStmt takes
// the order id.
func (w *p1aWorld) orderStmt(s shop, id uuid.UUID, over map[string]string) stmt {
	return st(insertSQL("orders", orderDefaults, over), id, s.buyer.org, s.buyer.user, s.buyer.party, w.orderNo())
}

func lineStmt(s shop, id, order uuid.UUID, over map[string]string) stmt {
	return st(insertSQL("order_lines", lineDefaults, over), id, order, s.buyer.org, s.seller.org, s.seller.payee, s.offering, s.service, s.policy)
}

// order writes an order with one default line in one transaction and
// returns (order id, line id).
func (w *p1aWorld) order(s shop) (order, line uuid.UUID) {
	w.t.Helper()
	order, line = uuid.NewV7(), uuid.NewV7()
	w.mustCommit(w.orderStmt(s, order, nil), lineStmt(s, line, order, nil))
	return order, line
}

// markPaid moves the order to 'paid' (paid_amount = koro_collect_amount).
func (w *p1aWorld) markPaid(order uuid.UUID) {
	w.t.Helper()
	w.exec(`UPDATE orders SET status = 'paid', paid_at = now(), paid_amount = koro_collect_amount WHERE id = $1`, order)
}

// fulfil marks the line fulfilled (buyer confirmed).
func (w *p1aWorld) fulfil(line uuid.UUID) {
	w.t.Helper()
	w.exec(`UPDATE order_lines SET fulfillment_status = 'fulfilled', fulfilled_at = now(), fulfilled_via = 'buyer_confirmed' WHERE id = $1`, line)
}

// payableLine returns a paid order with one fulfilled line.
func (w *p1aWorld) payableLine(s shop) (order, line uuid.UUID) {
	w.t.Helper()
	order, line = w.order(s)
	w.markPaid(order)
	w.fulfil(line)
	return order, line
}

// collectionAccount inserts a Koro account and has a second admin approve it.
func (w *p1aWorld) collectionAccount() uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(`INSERT INTO collection_accounts (id, label, legal_party_id, iban, holder_name, bank_name, created_by)
		VALUES ($1, 'Ana hesap', $2, $3, 'Koro Teknoloji A.Ş.', 'Örnek Bankası', $4)`, id, w.platformParty(), w.iban(), w.user())
	w.exec(`UPDATE collection_accounts SET status = 'active', approved_by = $2, approved_at = now() WHERE id = $1`, id, w.user())
	return id
}

// transferDefaults are the columns of an unmatched manual transfer;
// $1 id, $2 account, $3 enterer, $4 bank reference.
var transferDefaults = map[string]string{
	"id":                    "$1",
	"collection_account_id": "$2",
	"value_date":            "current_date",
	"amount":                "1200000",
	"description":           "'KR00000001 ödeme'",
	"bank_reference":        "$4",
	"entry_method":          "'manual'",
	"entered_by":            "$3",
}

// transfer inserts an unmatched transfer into the account.
func (w *p1aWorld) transfer(account uuid.UUID, over map[string]string) uuid.UUID {
	w.t.Helper()
	id := uuid.NewV7()
	w.exec(insertSQL("incoming_bank_transfers", transferDefaults, over), id, account, w.user(), "DEK-"+w.n())
	return id
}

// payment matches a fresh transfer of the order's payable amount to the
// order and records the captured bank-transfer payment; it returns the
// payment id.
func (w *p1aWorld) payment(order, buyerOrg uuid.UUID) uuid.UUID {
	w.t.Helper()
	tr := w.transfer(w.collectionAccount(), nil)
	w.exec(`UPDATE incoming_bank_transfers SET status = 'matched', matched_order_id = $2, matched_by = $3, matched_at = now() WHERE id = $1`,
		tr, order, w.user())
	id := uuid.NewV7()
	w.exec(`INSERT INTO payments (id, order_id, org_id, provider, incoming_transfer_id, amount, status, captured_at, captured_by)
		VALUES ($1, $2, $3, 'bank_transfer', $4, 1200000, 'captured', now(), $5)`, id, order, buyerOrg, tr, w.user())
	return id
}

// payoutStmts returns the statements of a payout of the lines (each for
// amount) to the seller's approved account, created by creator.
func payoutStmts(s payeeWorld, id, creator uuid.UUID, amount int64, items map[uuid.UUID]int64) []stmt {
	list := []stmt{st(`INSERT INTO payouts (id, payee_id, org_id, bank_account_id, iban_snapshot, holder_snapshot, amount, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, s.payee, s.org, s.account, s.iban, s.holder, amount, creator)}
	for _, line := range sortedIDs(items) {
		list = append(list, st(`INSERT INTO payout_items (payout_id, order_line_id, payee_id, amount) VALUES ($1, $2, $3, $4)`,
			id, line, s.payee, items[line]))
	}
	return list
}

// sortedIDs returns the keys of m in a stable order.
func sortedIDs(m map[uuid.UUID]int64) []uuid.UUID {
	out := slices.Collect(maps.Keys(m))
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return out
}

// payout creates a pending payout of one line for its seller_net_amount.
func (w *p1aWorld) payout(s payeeWorld, line uuid.UUID) (id, creator uuid.UUID) {
	w.t.Helper()
	id, creator = uuid.NewV7(), w.user()
	w.mustCommit(payoutStmts(s, id, creator, 1080000, map[uuid.UUID]int64{line: 1080000})...)
	return id, creator
}

// ledgerTxn returns the statements of a ledger transaction with the entries
// (account → amount; payee for the per_payee account).
func ledgerTxn(id uuid.UUID, kind string, payee *uuid.UUID, entries ...ledgerEntry) []stmt {
	list := []stmt{st(`INSERT INTO ledger_transactions (id, kind, ref_type, ref_id, occurred_at) VALUES ($1, $2, $3, gen_random_uuid(), now())`,
		id, kind, ledgerRefType[kind])}
	for _, e := range entries {
		var p *uuid.UUID
		if e.account == "seller_payable" {
			p = payee
		}
		list = append(list, st(`INSERT INTO ledger_entries (transaction_id, account_code, payee_id, amount) VALUES ($1, $2, $3, $4)`,
			id, e.account, p, e.amount))
	}
	return list
}

type ledgerEntry struct {
	account string
	amount  int64
}

var ledgerRefType = map[string]string{
	"payment_received": "payment", "order_allocated": "order", "payout_paid": "payout",
	"refund_completed": "refund", "fee": "manual", "adjustment": "manual",
}
