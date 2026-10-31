//go:build integration

package migrations_test

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"koro/internal/platform/migratekit"
	"koro/internal/platform/obs"
	"koro/internal/platform/testutil"
)

// p2Grants lists the tables of migrations 000094–000120 with the privileges
// app_rw holds on them. The rule (02 §0.5, BE-034 decision): financial
// records (financial_records, 10 years; meta "silinmez") have no DELETE —
// their end-of-retention purge is a separate SECURITY DEFINER function, as
// for the other immutable tables (06 H-34/H-41); the ledger is append-only
// (S+I); the chart of accounts is read-only (seeded by 000110); offerings
// keep DELETE for drafts (trigger-guarded, like events); payout_items are
// deleted only with a failed or cancelled payout (trigger-guarded, no
// UPDATE); the provider maps and the integration tokens are hard-delete
// caches.
var p2Grants = map[string]string{
	// parties
	"legal_parties":         "SIU",
	"legal_party_documents": "SIU",
	"payees":                "SIU",
	"payee_bank_accounts":   "SIU",
	// catalog
	"services":          "SIU",
	"offerings":         "SIUD",
	"commerce_policies": "SIU",
	// orders
	"orders":      "SIU",
	"order_lines": "SIU",
	// payments
	"collection_accounts":     "SIU",
	"incoming_bank_transfers": "SIU",
	"payments":                "SIU",
	"payment_splits":          "SIU",
	"refunds":                 "SIU",
	"receivable_collections":  "SIU",
	// ledger
	"ledger_accounts":     "S",
	"ledger_transactions": "SI",
	"ledger_entries":      "SI",
	"payouts":             "SIU",
	"payout_items":        "SID",
	// billing
	"invoices":                "SIU",
	"invoice_lines":           "SIU",
	"provider_contact_map":    "SIUD",
	"provider_product_map":    "SIUD",
	"integration_credentials": "SIUD",
}

// p2TableNames returns the keys of p2Grants, sorted.
func p2TableNames() []string { return slices.Sorted(maps.Keys(p2Grants)) }

// p2TableOIDs turns $1 (table names, text[]) into public table oids.
const p2TableOIDs = `(SELECT ('public.' || quote_ident(t))::regclass FROM unnest($1::text[]) t)`

// TestSchema_P2SchemaGrantsAndOwnership: every table of 000094–000120
// belongs to app_owner; app_rw holds exactly the privileges of p2Grants and
// nothing else; no other role and not PUBLIC can touch them; the helper
// functions belong to app_owner.
func TestSchema_P2SchemaGrantsAndOwnership(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	privs := map[byte]string{'S': "SELECT", 'I': "INSERT", 'U': "UPDATE", 'D': "DELETE"}
	for _, table := range p2TableNames() {
		want := p2Grants[table]
		for letter, priv := range privs {
			got := queryString(t, w.d, `SELECT has_table_privilege('app_rw', $1, $2)::text`, table, priv)
			if wantIt := strings.IndexByte(want, letter) >= 0; got != strconv.FormatBool(wantIt) {
				t.Errorf("app_rw %s on %s = %s, want %v", priv, table, got, wantIt)
			}
		}
		for _, priv := range []string{"TRUNCATE", "REFERENCES", "TRIGGER"} {
			if got := queryString(t, w.d, `SELECT has_table_privilege('app_rw', $1, $2)::text`, table, priv); got != "false" {
				t.Errorf("app_rw has %s on %s", priv, table)
			}
		}
		for _, role := range []string{"app_ro", "audit_writer", "audit_chainer", "app_talent"} {
			if got := queryString(t, w.d, `SELECT (has_table_privilege($1, $2, 'SELECT') OR has_table_privilege($1, $2, 'INSERT')
				OR has_table_privilege($1, $2, 'UPDATE') OR has_table_privilege($1, $2, 'DELETE'))::text`, role, table); got != "false" {
				t.Errorf("%s has a privilege on %s", role, table)
			}
		}
		if got := queryString(t, w.d, `SELECT count(*)::text FROM pg_class c, aclexplode(c.relacl) a WHERE c.oid = $1::regclass AND a.grantee = 0`, table); got != "0" {
			t.Errorf("PUBLIC holds privileges on %s", table)
		}
		if got := queryString(t, w.d, `SELECT tableowner FROM pg_tables WHERE schemaname = 'public' AND tablename = $1`, table); got != "app_owner" {
			t.Errorf("%s is owned by %q, want app_owner", table, got)
		}
	}
	for _, fn := range []string{"koro_freeze_columns()", "koro_initial_status()", "offerings_forbid_nondraft_delete()",
		"orders_check_lines_total()", "orders_amounts_guard()", "order_lines_drop_guard()", "ledger_check_balanced()", "payouts_check_bank_account()", "payout_items_check()", "payouts_check_items()"} {
		if got := queryString(t, w.d, `SELECT proowner::regrole::text || ' ' || prosecdef::text FROM pg_proc WHERE oid = ('public.' || $1)::regprocedure`, fn); got != "app_owner false" {
			t.Errorf("%s: owner and SECURITY DEFINER = %q, want app_owner, invoker", fn, got)
		}
	}
}

// TestSchema_P2SchemaExplicitConstraintAndIndexNames (02 §23): every
// constraint and index of the P2 tables has an explicit name of the form
// <table>_<purpose>_{chk,uq,fkey,idx}; the exclusion constraint of
// commerce_policies is an index named …_uq.
func TestSchema_P2SchemaExplicitConstraintAndIndexNames(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	tables := p2TableNames()
	bad := queryStrings(t, w, `
		SELECT c.conrelid::regclass::text || ' ' || c.conname || ' (' || c.contype::text || ')'
		  FROM pg_constraint c
		 WHERE c.conrelid IN `+p2TableOIDs+`
		   AND c.contype IN ('p', 'u', 'f', 'c', 'x')
		   AND NOT (c.conname LIKE (c.conrelid::regclass::text || '\_%') ESCAPE '\'
		        AND ((c.contype = 'p' AND c.conname = c.conrelid::regclass::text || '_pkey')
		          OR (c.contype IN ('u', 'x') AND c.conname LIKE '%\_uq' ESCAPE '\')
		          OR (c.contype = 'f' AND c.conname LIKE '%\_fkey' ESCAPE '\')
		          OR (c.contype = 'c' AND c.conname LIKE '%\_chk' ESCAPE '\')))
		 ORDER BY 1`, tables)
	if len(bad) > 0 {
		t.Errorf("constraints without an explicit name of the form <table>_<purpose>_{chk,uq,fkey}:\n%s", strings.Join(bad, "\n"))
	}
	badIdx := queryStrings(t, w, `
		SELECT indexname FROM pg_indexes
		 WHERE schemaname = 'public' AND tablename = ANY ($1::text[])
		   AND NOT (indexname LIKE (tablename || '\_%') ESCAPE '\'
		        AND (indexname LIKE '%\_idx' ESCAPE '\' OR indexname LIKE '%\_uq' ESCAPE '\' OR indexname LIKE '%\_pkey' ESCAPE '\'))
		 ORDER BY 1`, tables)
	if len(badIdx) > 0 {
		t.Errorf("indexes without an explicit name of the form <table>_<purpose>_{idx,uq}:\n%s", strings.Join(badIdx, "\n"))
	}
	if n := queryString(t, w.d, `SELECT count(DISTINCT conrelid)::text FROM pg_constraint WHERE conrelid IN `+p2TableOIDs, tables); n != strconv.Itoa(len(tables)) {
		t.Errorf("tables with constraints = %s, want %d (a typo in the table list?)", n, len(tables))
	}
	if got := queryString(t, w.d, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'commerce_policies_no_overlap_uq'`); !strings.Contains(got, "EXCLUDE USING gist") ||
		!strings.Contains(got, "tstzrange(valid_from, COALESCE(valid_to, 'infinity'::timestamp with time zone), '[)'::text) WITH &&") {
		t.Errorf("commerce_policies_no_overlap_uq = %s", got)
	}
}

// TestSchema_P2SchemaForeignKeys (02 §0.1, 06 H-38): every FK is NO ACTION
// (nothing cascades in money tables) but one: a receivable collection's
// receipt is its transfer's, so replacing the transfer's rejected receipt
// replaces the collection's (ON UPDATE CASCADE; 000163, BE-154); the
// composite FKs that bind a child to its parent's org / order / payee are
// exactly these.
func TestSchema_P2SchemaForeignKeys(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	tables := p2TableNames()
	got := queryStrings(t, w, `
		SELECT conrelid::regclass::text || '.' || conname || ' delete=' || confdeltype::text || ' update=' || confupdtype::text
		  FROM pg_constraint
		 WHERE contype = 'f' AND (confdeltype <> 'a' OR confupdtype <> 'a') AND conrelid IN `+p2TableOIDs+` ORDER BY 1`, tables)
	if want := []string{"receivable_collections.receivable_collections_source_fkey delete=a update=c"}; !slices.Equal(got, want) {
		t.Errorf("foreign keys with an action other than NO ACTION:\n got %v\nwant %v", got, want)
	}
	composite := queryStrings(t, w, `
		SELECT conname || ' ' || pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE contype = 'f' AND cardinality(conkey) = 2 AND conrelid IN `+p2TableOIDs+` ORDER BY 1`, tables)
	want := []string{
		"collection_accounts_legal_party_id_party_kind_fkey FOREIGN KEY (legal_party_id, party_kind) REFERENCES legal_parties(id, kind)",
		"ledger_entries_order_line_id_payee_id_fkey FOREIGN KEY (order_line_id, payee_id) REFERENCES order_lines(id, payee_id)",
		"offerings_default_payee_id_org_id_fkey FOREIGN KEY (default_payee_id, org_id) REFERENCES payees(id, org_id)",
		"offerings_event_id_org_id_fkey FOREIGN KEY (event_id, org_id) REFERENCES events(id, org_id)",
		"order_lines_event_id_seller_org_id_fkey FOREIGN KEY (event_id, seller_org_id) REFERENCES events(id, org_id)",
		"order_lines_offering_id_seller_org_id_fkey FOREIGN KEY (offering_id, seller_org_id) REFERENCES offerings(id, org_id)",
		"order_lines_order_id_org_id_fkey FOREIGN KEY (order_id, org_id) REFERENCES orders(id, org_id)",
		"order_lines_payee_id_seller_org_id_fkey FOREIGN KEY (payee_id, seller_org_id) REFERENCES payees(id, org_id)",
		"order_lines_payout_id_id_fkey FOREIGN KEY (payout_id, id) REFERENCES payout_items(payout_id, order_line_id)",
		"order_lines_payout_id_payee_id_fkey FOREIGN KEY (payout_id, payee_id) REFERENCES payouts(id, payee_id)",
		"orders_buyer_party_id_org_id_fkey FOREIGN KEY (buyer_party_id, org_id) REFERENCES legal_parties(id, org_id)",
		"orders_org_id_org_kind_fkey FOREIGN KEY (org_id, org_kind) REFERENCES organizations(id, kind)",
		"payee_bank_accounts_payee_id_org_id_fkey FOREIGN KEY (payee_id, org_id) REFERENCES payees(id, org_id)",
		"payee_bank_accounts_superseded_by_id_payee_id_fkey FOREIGN KEY (superseded_by_id, payee_id) REFERENCES payee_bank_accounts(id, payee_id)",
		"payees_legal_party_id_party_kind_fkey FOREIGN KEY (legal_party_id, party_kind) REFERENCES legal_parties(id, kind)",
		"payment_splits_order_line_id_order_id_fkey FOREIGN KEY (order_line_id, order_id) REFERENCES order_lines(id, order_id)",
		"payment_splits_payment_id_order_id_fkey FOREIGN KEY (payment_id, order_id) REFERENCES payments(id, order_id)",
		"payments_incoming_transfer_id_order_id_fkey FOREIGN KEY (incoming_transfer_id, order_id) REFERENCES incoming_bank_transfers(id, matched_order_id)",
		"payments_order_id_org_id_fkey FOREIGN KEY (order_id, org_id) REFERENCES orders(id, org_id)",
		"payout_items_order_line_id_payee_id_fkey FOREIGN KEY (order_line_id, payee_id) REFERENCES order_lines(id, payee_id)",
		"payout_items_payout_id_payee_id_fkey FOREIGN KEY (payout_id, payee_id) REFERENCES payouts(id, payee_id)",
		"payouts_bank_account_id_payee_id_fkey FOREIGN KEY (bank_account_id, payee_id) REFERENCES payee_bank_accounts(id, payee_id)",
		"payouts_payee_id_org_id_fkey FOREIGN KEY (payee_id, org_id) REFERENCES payees(id, org_id)",
		"receivable_collections_line_order_fkey FOREIGN KEY (order_line_id, order_id) REFERENCES order_lines(id, order_id)",
		"receivable_collections_line_payee_fkey FOREIGN KEY (order_line_id, payee_id) REFERENCES order_lines(id, payee_id)",
		"receivable_collections_payee_org_fkey FOREIGN KEY (payee_id, org_id) REFERENCES payees(id, org_id)",
		"refunds_order_line_id_order_id_fkey FOREIGN KEY (order_line_id, order_id) REFERENCES order_lines(id, order_id)",
		"refunds_payment_id_order_id_fkey FOREIGN KEY (payment_id, order_id) REFERENCES payments(id, order_id)",
	}
	if !slices.Equal(composite, want) {
		t.Errorf("composite FKs:\n got %v\nwant %v", composite, want)
	}
	// The FK of orders to the collection account (000104).
	if got := queryString(t, w.d, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'orders_collection_account_id_fkey'`); got != "FOREIGN KEY (collection_account_id) REFERENCES collection_accounts(id)" {
		t.Errorf("orders_collection_account_id_fkey = %s", got)
	}
}

// TestSchema_P2SchemaIDColumnsAndTriggers: every `id uuid` column defaults
// to uuidv7(); every table with updated_at has its touch trigger; the other
// triggers are exactly the guards of this schema, and the constraint
// triggers are DEFERRABLE INITIALLY DEFERRED (they run at commit).
func TestSchema_P2SchemaIDColumnsAndTriggers(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	tables := p2TableNames()
	if n := queryString(t, w.d, `SELECT count(*)::text FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name = 'id' AND table_name = ANY ($1::text[]) AND data_type = 'uuid' AND column_default = 'uuidv7()'`, tables); n != "20" {
		t.Errorf("id columns with DEFAULT uuidv7() = %s, want 20", n)
	}
	if bad := queryStrings(t, w, `SELECT table_name FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'id'
		AND table_name = ANY ($1::text[]) AND (data_type <> 'uuid' OR column_default IS DISTINCT FROM 'uuidv7()') ORDER BY 1`, tables); len(bad) > 0 {
		t.Errorf("id columns without DEFAULT uuidv7(): %v", bad)
	}
	missing := queryStrings(t, w, `
		SELECT c.relname FROM pg_class c
		  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'updated_at' AND NOT a.attisdropped
		 WHERE c.oid IN `+p2TableOIDs+`
		   AND NOT EXISTS (SELECT 1 FROM pg_trigger g
		        WHERE g.tgrelid = c.oid AND NOT g.tgisinternal
		          AND g.tgname = c.relname || '_touch_updated_at'
		          AND g.tgfoid = 'public.koro_touch_updated_at'::regproc
		          AND g.tgtype & 1 = 1 AND g.tgtype & 2 = 2 AND g.tgtype & 16 = 16)
		 ORDER BY 1`, tables)
	if len(missing) > 0 {
		t.Errorf("tables with updated_at but without the koro_touch_updated_at trigger: %v", missing)
	}
	if n := queryString(t, w.d, `SELECT count(*)::text FROM pg_attribute WHERE attname = 'updated_at' AND NOT attisdropped AND attrelid IN `+p2TableOIDs, tables); n != "18" {
		t.Errorf("tables with updated_at = %s, want 18", n)
	}
	others := queryStrings(t, w, `
		SELECT c.relname || '.' || g.tgname || ' ' || p.proname || CASE WHEN g.tgconstraint <> 0 THEN
		         ' constraint deferrable=' || g.tgdeferrable::text || ' deferred=' || g.tginitdeferred::text ELSE '' END
		  FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid JOIN pg_proc p ON p.oid = g.tgfoid
		 WHERE NOT g.tgisinternal AND c.oid IN `+p2TableOIDs+` AND g.tgname <> c.relname || '_touch_updated_at'
		 ORDER BY 1`, tables)
	const deferred = " constraint deferrable=true deferred=true"
	wantOthers := []string{
		"collection_accounts.collection_accounts_frozen koro_freeze_columns",
		"collection_accounts.collection_accounts_initial_status koro_initial_status",
		"commerce_policies.commerce_policies_close_only koro_policy_close_only",
		"commerce_policies.commerce_policies_no_truncate koro_forbid_mutation",
		"incoming_bank_transfers.incoming_bank_transfers_collection_consistent incoming_transfer_check_collection" + deferred,
		"incoming_bank_transfers.incoming_bank_transfers_frozen koro_freeze_columns",
		"incoming_bank_transfers.incoming_bank_transfers_initial_status koro_initial_status",
		"incoming_bank_transfers.incoming_bank_transfers_receipt_guard koro_evidence_file_guard", // 000163 (BE-154)
		"incoming_bank_transfers.incoming_bank_transfers_return_frozen koro_freeze_columns",
		"incoming_bank_transfers.incoming_bank_transfers_return_receipt_guard koro_evidence_file_guard", // 000163 (BE-154)
		"incoming_bank_transfers.incoming_bank_transfers_returned_frozen koro_freeze_columns",
		"invoices.invoices_frozen koro_freeze_columns",
		"invoices.invoices_pdf_guard koro_evidence_file_guard",     // 000164 (BE-154b)
		"invoices.invoices_pdf_reverify koro_invoice_pdf_reverify", // 000164 (BE-154b)
		"ledger_entries.ledger_entries_immutable koro_forbid_mutation",
		"ledger_entries.ledger_entries_no_truncate koro_forbid_mutation",
		"ledger_entries.ledger_txn_balanced ledger_check_balanced" + deferred,
		"ledger_transactions.ledger_transactions_immutable koro_forbid_mutation",
		"ledger_transactions.ledger_transactions_no_truncate koro_forbid_mutation",
		"ledger_transactions.ledger_txn_balanced ledger_check_balanced" + deferred,
		"legal_party_documents.legal_party_documents_file_guard koro_evidence_file_guard", // 000163 (BE-154)
		"offerings.offerings_draft_delete_only offerings_forbid_nondraft_delete",
		"order_lines.order_lines_drop_guard order_lines_drop_guard",        // 000157 (BE-134)
		"order_lines.order_lines_dropped_frozen koro_freeze_columns",       // 000157 (BE-134)
		"order_lines.order_lines_evidence_frozen koro_evidence_file_guard", // 000137 (BE-093), 000163 (BE-154)
		"order_lines.order_lines_frozen koro_freeze_columns",
		"order_lines.order_lines_paid_out_frozen koro_freeze_columns",
		"order_lines.orders_totals_match_lines orders_check_lines_total" + deferred,
		"orders.orders_amounts_guard orders_amounts_guard", // 000157 (BE-134)
		"orders.orders_direct_confirmation_guard koro_orders_direct_confirmation_guard",
		"orders.orders_frozen koro_freeze_columns",
		"orders.orders_totals_match_lines orders_check_lines_total" + deferred,
		"payee_bank_accounts.payee_bank_accounts_evidence_guard koro_evidence_file_guard", // 000163 (BE-154)
		"payee_bank_accounts.payee_bank_accounts_frozen koro_freeze_columns",
		"payee_bank_accounts.payee_bank_accounts_initial_status koro_initial_status",
		"payment_splits.payment_splits_frozen koro_freeze_columns",
		"payments.payments_evidence_guard koro_evidence_file_guard", // 000163 (BE-154)
		"payments.payments_frozen koro_freeze_columns",
		"payout_items.payout_items_guard payout_items_check",
		"payout_items.payout_items_sum payouts_check_items" + deferred,
		"payouts.payout_items_sum payouts_check_items" + deferred,
		"payouts.payouts_approval_frozen koro_freeze_columns",
		"payouts.payouts_bank_account_guard payouts_check_bank_account",
		"payouts.payouts_frozen koro_freeze_columns",
		"payouts.payouts_initial_status koro_initial_status",
		"payouts.payouts_paid_frozen koro_freeze_columns",
		"payouts.payouts_receipt_guard koro_evidence_file_guard", // 000163 (BE-154)
		"receivable_collections.receivable_collections_approved_frozen koro_freeze_columns",
		"receivable_collections.receivable_collections_consistent receivable_collections_check" + deferred,
		"receivable_collections.receivable_collections_frozen koro_freeze_columns",
		"receivable_collections.receivable_collections_initial_status koro_initial_status",
		"receivable_collections.receivable_collections_receipt_guard koro_evidence_file_guard", // 000163 (BE-154)
		"receivable_collections.receivable_collections_rejected_frozen koro_freeze_columns",
		"refunds.refunds_approval_frozen koro_freeze_columns",
		"refunds.refunds_closure_frozen koro_freeze_columns",
		"refunds.refunds_closure_guard refunds_guard_closure",
		"refunds.refunds_completed_frozen koro_freeze_columns",
		"refunds.refunds_frozen koro_freeze_columns",
		"refunds.refunds_initial_status koro_initial_status",
		"refunds.refunds_receipt_guard koro_evidence_file_guard", // 000163 (BE-154)
	}
	if !slices.Equal(others, wantOthers) {
		t.Errorf("triggers besides touch_updated_at:\n got %v\nwant %v", others, wantOthers)
	}
}

// TestSchema_P2SchemaTurkishCollationOnNameColumns (02 §0.4).
func TestSchema_P2SchemaTurkishCollationOnNameColumns(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	for table, columns := range map[string][]string{
		"legal_parties":       {"title"},
		"payee_bank_accounts": {"holder_name"},
		"services":            {"name_tr"},
		"offerings":           {"title"},
		"collection_accounts": {"holder_name"},
		"ledger_accounts":     {"name_tr"},
	} {
		for _, column := range columns {
			if got := queryString(t, w.d, `SELECT collation_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, column); got != "tr" {
				t.Errorf("%s.%s collation = %q, want tr", table, column, got)
			}
		}
	}
}

// TestSchema_P2SchemaQueryIndexes pins the indexes of 02 §17–§22 (partial
// predicates, key order): the queues of the finance screens, the payable
// lines of a payee, the dispute windows and the unique rules.
func TestSchema_P2SchemaQueryIndexes(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	want := map[string][]string{
		"legal_parties_platform_uq":             {"UNIQUE", "(kind) WHERE (kind = 'platform'"},
		"legal_parties_taxno_uq":                {"UNIQUE", "(kind, tax_no)", "'inactive'"},
		"legal_parties_tckn_uq":                 {"UNIQUE", "(kind, tckn_bidx)", "'inactive'"},
		"legal_parties_org_idx":                 {"(org_id) WHERE (org_id IS NOT NULL)"},
		"legal_party_documents_party_idx":       {"(legal_party_id)"},
		"payees_default_uq":                     {"UNIQUE", "(org_id) WHERE (is_default AND (status = 'active'"},
		"payees_org_party_uq":                   {"UNIQUE", "(org_id, legal_party_id)", "'closed'"},
		"payee_bank_accounts_approved_uq":       {"UNIQUE", "(payee_id) WHERE (status = 'approved'"},
		"payee_bank_accounts_pending_uq":        {"UNIQUE", "(payee_id)", "'pending_verification'", "'pending_approval'"},
		"payee_bank_accounts_queue_idx":         {"(submitted_at)", "'pending_verification'", "'pending_approval'"},
		"offerings_org_idx":                     {"(org_id, status)"},
		"offerings_event_idx":                   {"(event_id) WHERE (event_id IS NOT NULL)"},
		"offerings_active_idx":                  {"(sales_end_at) WHERE (status = 'active'"},
		"offerings_fts_idx":                     {"USING gin (search_vector)"},
		"commerce_policies_lookup_idx":          {"(scope, scope_id, valid_from DESC)"},
		"orders_org_idx":                        {"(org_id, placed_at DESC)"},
		"orders_pending_idx":                    {"(expires_at) WHERE (status = 'awaiting_payment'"},
		"order_lines_order_idx":                 {"(order_id)"},
		"order_lines_seller_idx":                {"(seller_org_id, created_at DESC)"},
		"order_lines_payable_idx":               {"(payee_id, fulfilled_at)", "'fulfilled'", "payout_id IS NULL"},
		"order_lines_window_idx":                {"(dispute_window_ends_at) WHERE (fulfillment_status = 'marked'"},
		"order_lines_offering_idx":              {"(offering_id)"},
		"collection_accounts_default_uq":        {"UNIQUE", "is_default", "'active'"},
		"collection_accounts_iban_uq":           {"UNIQUE", "(iban)", "'pending_approval'", "'active'"},
		"incoming_bank_transfers_unmatched_idx": {"(value_date) WHERE (status = 'unmatched'"},
		"incoming_bank_transfers_ref_idx":       {"(extracted_order_no) WHERE (extracted_order_no IS NOT NULL)"},
		"incoming_bank_transfers_return_idx":    {"(return_requested_at)", "'return_requested'", "'return_approved'"},
		"incoming_bank_transfers_order_idx":     {"(matched_order_id) WHERE (matched_order_id IS NOT NULL)"},
		"payments_provider_uq":                  {"UNIQUE", "(provider, provider_payment_id) WHERE (provider_payment_id IS NOT NULL)"},
		"payments_order_idx":                    {"(order_id)"},
		"payment_splits_line_idx":               {"(order_line_id)"},
		"refunds_open_idx":                      {"(created_at)", "'requested'", "'approved'", "'processing'"},
		"refunds_order_idx":                     {"(order_id)"},
		"ledger_transactions_occurred_idx":      {"(occurred_at DESC, id DESC)"},
		"ledger_transactions_ref_idx":           {"(ref_type, ref_id)"},
		"ledger_entries_txn_idx":                {"(transaction_id)"},
		"ledger_entries_payee_idx":              {"(account_code, payee_id) WHERE (payee_id IS NOT NULL)"},
		"ledger_entries_line_idx":               {"(order_line_id) WHERE (order_line_id IS NOT NULL)"},
		"payouts_payee_idx":                     {"(payee_id, created_at DESC)"},
		"payouts_org_idx":                       {"(org_id, created_at DESC)"},
		"payouts_open_idx":                      {"(created_at)", "'pending_approval'", "'approved'"},
		"payout_items_line_uq":                  {"UNIQUE", "(order_line_id)"},
		"invoices_no_uq":                        {"UNIQUE", "(issuer_party_id, invoice_no) WHERE (invoice_no IS NOT NULL)"},
		"invoices_order_idx":                    {"(order_id) WHERE (order_id IS NOT NULL)"},
		"invoices_payout_idx":                   {"(payout_id) WHERE (payout_id IS NOT NULL)"},
		"invoices_status_idx":                   {"(status, created_at)", "'planned'", "'mismatch'"},
		"invoice_lines_order_line_idx":          {"(order_line_id) WHERE (order_line_id IS NOT NULL)"},
		"incoming_bank_transfers_id_order_uq":   {"UNIQUE", "(id, matched_order_id)"},
		"order_lines_id_payee_uq":               {"UNIQUE", "(id, payee_id)"},
		"payee_bank_accounts_id_payee_uq":       {"UNIQUE", "(id, payee_id)"},
		"commerce_policies_no_overlap_uq":       {"USING gist", "scope", "tstzrange"},
	}
	for name, parts := range want {
		def := queryString(t, w.d, `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name)
		if def == "<NULL>" {
			t.Errorf("index %s does not exist", name)
			continue
		}
		for _, part := range parts {
			if !strings.Contains(def, part) {
				t.Errorf("index %s lacks %q:\n  %s", name, part, def)
			}
		}
	}
}

// TestSchema_P2SeedLedgerAccounts (02 §21.1): the thirteen accounts with
// their kinds (payee_receivable: 06 H-59, BE-043); seller_payable is the only
// per_payee account; re-running the seed changes nothing.
func TestSchema_P2SeedLedgerAccounts(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	got := queryStrings(t, w, `SELECT code || ' ' || kind || ' ' || per_payee::text || ' ' || active::text FROM ledger_accounts ORDER BY code`)
	want := []string{
		"bank_fee_expense expense false true",
		"buyer_advances liability false true",
		"cost_of_services expense false true",
		"koro_bank asset false true",
		"payee_receivable asset false true",
		"platform_commission_revenue revenue false true",
		"platform_sales_revenue revenue false true",
		"psp_clearing asset false true",
		"psp_fee_expense expense false true",
		"refund_payable liability false true",
		"seller_payable liability true true",
		"vat_input asset false true",
		"vat_output liability false true",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ledger accounts:\n got %v\nwant %v", got, want)
	}
	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), `UPDATE ledger_accounts SET name_tr = 'Değiştirildi' WHERE code = 'koro_bank'`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000110_seed_ledger_accounts.up.sql")); err != nil {
		t.Fatalf("re-running the seed: %v", err)
	}
	if got := queryString(t, w.d, `SELECT name_tr FROM ledger_accounts WHERE code = 'koro_bank'`); got != "Değiştirildi" {
		t.Errorf("re-running the seed overwrote an account: %q", got)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw adds an account", `INSERT INTO ledger_accounts (code, name_tr, kind) VALUES ('new_account', 'Yeni', 'asset')`, nil, "42501", ""},
		{"app_rw edits an account", `UPDATE ledger_accounts SET active = false`, nil, "42501", ""},
	})
}

// TestMigrations_P2SchemaUpDownUp is TestMigrations_UpDownUp pinned to the
// range of this ticket: up to 000120, down to empty, up again; the second
// up is identical to the first (seed included) and down leaves nothing.
func TestMigrations_P2SchemaUpDownUp(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ownerURL := testutil.NewEmptyDB(t)
	m, err := migratekit.New(migrationsUpTo(t, 120), ownerURL, obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	empty := fingerprint(t, conn)
	if err := m.Up(ctx); err != nil {
		t.Fatalf("first up: %v", err)
	}
	if v, dirty, err := m.Version(); err != nil || dirty || v != 120 {
		t.Fatalf("version after up = %d, dirty %v, %v; want 120", v, dirty, err)
	}
	first := fingerprint(t, conn)
	for _, want := range []string{
		"rel public.legal_parties r", "rel public.payee_bank_accounts r", "rel public.commerce_policies r", "rel public.orders r",
		"rel public.incoming_bank_transfers r", "rel public.refunds r", "rel public.ledger_entries r", "rel public.payouts r",
		"rel public.payout_items r", "rel public.invoices r", "rel public.integration_credentials r",
		"fn koro_freeze_columns()", "fn koro_initial_status()", "fn ledger_check_balanced()", "fn payouts_check_items()",
		"trg CREATE CONSTRAINT TRIGGER ledger_txn_balanced AFTER INSERT ON public.ledger_entries DEFERRABLE INITIALLY DEFERRED",
		"trg CREATE CONSTRAINT TRIGGER payout_items_sum AFTER INSERT OR DELETE OR UPDATE ON public.payout_items DEFERRABLE INITIALLY DEFERRED",
		"con orders orders_collection_account_id_fkey", "con order_lines order_lines_payout_id_payee_id_fkey",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("schema after up lacks %q", want)
		}
	}
	accounts := func() int {
		t.Helper()
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := accounts(); n != 13 {
		t.Errorf("ledger accounts after the first up = %d, want 13", n)
	}
	if err := m.Down(ctx, 120); err != nil {
		t.Fatalf("down 120: %v", err)
	}
	if got := fingerprint(t, conn); got != empty {
		t.Fatalf("down left objects behind:\n%s", diff(empty, got))
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if got := fingerprint(t, conn); got != first {
		t.Fatalf("second up differs from the first:\n%s", diff(first, got))
	}
	if n := accounts(); n != 13 {
		t.Errorf("ledger accounts after the second up = %d, want 13", n)
	}
}
