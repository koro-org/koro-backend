//go:build integration

package seed_test

import (
	"fmt"
	"maps"
	"os"
	"testing"
	"time"

	"koro/db/seed"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Run(m)) }

// TestSeedDemo_CommerceIdempotent executes the complete plan against real
// PostgreSQL constraints. A later invocation must preserve every persisted
// commerce row, including edits made by a demo operator after the first run.
func TestSeedDemo_CommerceIdempotent(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ref := seedCommerceReference(t, d)
	p := build(t, ref, now)
	first, err := seed.Apply(t.Context(), d.Pool, p)
	if err != nil {
		t.Fatal(err)
	}
	inserted, existing := first.Total()
	if inserted != plannedTotal(p) || existing != 0 {
		t.Fatalf("first application: %d inserted, %d existing; plan has %d rows", inserted, existing, plannedTotal(p))
	}
	for table, want := range map[string]int{"legal_parties": 3, "collection_accounts": 1, "payees": 1, "services": 2,
		"offerings": 3, "commerce_policies": 3, "orders": 4, "order_lines": 4} {
		if got := first.Inserted[table]; got != want {
			t.Errorf("%s inserted = %d, want %d", table, got, want)
		}
	}
	count := func(query string, want int, args ...any) {
		t.Helper()
		var got int
		if err := d.Pool.QueryRow(t.Context(), query, args...).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: %d, want %d (%v)", query, got, want, err)
		}
	}
	count(`SELECT count(*) FROM users WHERE signup_persona = 'student'`, seed.StudentCount)
	count(`SELECT count(*) FROM users u JOIN platform_role_assignments r ON r.user_id = u.id
		WHERE u.password_hash IS NOT NULL`, 0)
	count(`SELECT count(*) FROM legal_parties WHERE status = 'verified' AND tax_no IS NOT NULL
		AND address->>'line1' <> '' AND address->>'city' <> '' AND verified_by <> created_by`, 3)
	count(`SELECT count(*) FROM payees p JOIN legal_parties lp ON lp.id = p.legal_party_id
		WHERE p.status = 'active' AND p.is_default AND p.verified_by <> p.created_by AND lp.status = 'verified'`, 1)
	count(`SELECT count(*) FROM offerings WHERE status = 'active' AND approved_by <> created_by`, 3)
	count(`SELECT count(*) FROM orders WHERE model_code = 'A' AND collection_method = 'bank_transfer'
		AND paid_amount = 0 AND paid_at IS NULL AND collection_account_id IS NULL`, 3)
	count(`SELECT count(*) FROM orders WHERE status = 'awaiting_payment' AND expires_at > $1`, 3, now)
	count(`SELECT count(*) FROM orders WHERE status = 'expired' AND expires_at < $1`, 1, now)
	count(`SELECT count(*) FROM order_lines l JOIN orders o ON o.id = l.order_id
		WHERE l.org_id = o.org_id AND l.quantity = 1 AND l.net_amount = l.unit_price_amount
		AND l.payable_amount = o.payable_amount AND l.vat_amount = o.vat_amount
		AND l.commission_amount = 0 AND l.seller_net_amount = l.payable_amount`, 3)
	count(`SELECT count(*) FROM orders WHERE subtotal_amount = 10000 AND vat_amount = 2000 AND payable_amount = 12000`, 2)
	count(`SELECT count(*) FROM orders WHERE subtotal_amount = 25000 AND vat_amount = 5000 AND payable_amount = 30000`, 1)
	// The Model C split is frozen at checkout: Koro collects only the
	// buyer fee plus its VAT; the seller's gross never becomes Koro cash.
	count(`SELECT count(*) FROM orders o JOIN order_lines l ON l.order_id = o.id
		JOIN offerings f ON f.id = l.offering_id JOIN commerce_policies p ON p.id = l.commerce_policy_id
		WHERE o.id = $1 AND f.id = $2 AND o.model_code = 'C' AND o.collection_method = 'direct_to_payee'
		AND o.status = 'awaiting_payment' AND o.paid_amount = 0 AND o.paid_at IS NULL
		AND o.direct_confirmed_at IS NULL AND o.collection_account_id IS NULL
		AND o.buyer_party_id = $3 AND l.payee_id = $4 AND l.payee_id = f.default_payee_id
		AND l.seller_org_id = f.org_id AND l.org_id = o.org_id AND l.service_id = f.service_id
		AND p.scope = 'offering' AND p.scope_id = f.id AND p.model_code = 'C'
		AND p.collection_method = 'direct_to_payee' AND p.commission_payer = 'buyer'
		AND l.commission_payer = p.commission_payer AND l.commission_rate_bp = p.commission_rate_bp
		AND l.commission_vat_rate_bp = p.commission_vat_rate_bp AND l.commission_basis = p.commission_basis
		AND l.quantity = 1 AND l.unit_price_amount = 10000 AND l.net_amount = 10000
		AND l.vat_rate_bp = 2000 AND l.vat_amount = 2000 AND l.gross_amount = 12000
		AND l.commission_amount = 500 AND l.commission_vat_amount = 100 AND l.buyer_fee_amount = 600
		AND l.seller_net_amount = 12000 AND l.payable_amount = 12600 AND l.fulfillment_status = 'pending'
		AND l.payout_id IS NULL AND l.paid_out_at IS NULL
		AND o.subtotal_amount = l.net_amount AND o.vat_amount = l.vat_amount
		AND o.koro_collect_amount = l.buyer_fee_amount AND o.buyer_fee_amount = l.buyer_fee_amount
		AND o.direct_to_payee_amount = l.seller_net_amount AND o.payable_amount = l.payable_amount
		AND o.total_amount = o.payable_amount AND o.payable_amount = o.koro_collect_amount + o.direct_to_payee_amount`,
		1, p.Demo.Commerce.DirectOrderID, p.Demo.Commerce.DirectOfferingID, p.Demo.Commerce.BuyerPartyID, p.Demo.Commerce.PayeeID)
	count(`SELECT count(*) FROM entitlements e JOIN order_lines l ON l.id = e.source_id
		WHERE l.order_id = $1`, 0, p.Demo.Commerce.DirectOrderID)
	count(`SELECT count(*) FROM offerings f WHERE f.quantity_sold <>
		(SELECT coalesce(sum(l.quantity), 0) FROM order_lines l JOIN orders o ON o.id = l.order_id
		 WHERE l.offering_id = f.id AND o.status NOT IN ('expired','cancelled'))`, 0)
	count(`SELECT count(*) FROM entitlements e JOIN order_lines l ON l.id = e.source_id
		JOIN orders o ON o.id = l.order_id WHERE e.source_type = 'order_line' AND e.org_id = o.org_id
		AND e.quantity_total = 5 AND e.quantity_used = 0 AND e.activated_at IS NULL
		AND ((o.status = 'awaiting_payment' AND e.status = 'pending' AND l.fulfillment_status = 'not_applicable')
		 OR (o.status = 'expired' AND e.status = 'revoked' AND e.revoked_at = o.expires_at AND l.fulfillment_status = 'cancelled'))`, 2)
	for _, table := range []string{"payee_bank_accounts", "payments", "incoming_bank_transfers",
		"ledger_transactions", "ledger_entries", "invoices", "outbox"} {
		count("SELECT count(*) FROM "+table, 0)
	}

	snapshot := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, table := range []string{"legal_parties", "collection_accounts", "payees", "services", "offerings", "commerce_policies", "orders", "order_lines", "entitlements"} {
			var value string
			if err := d.Pool.QueryRow(t.Context(), "SELECT jsonb_agg(to_jsonb(r) ORDER BY r.id)::text FROM "+table+" r").Scan(&value); err != nil {
				t.Fatal(err)
			}
			out[table] = value
		}
		return out
	}
	reapply := func() {
		t.Helper()
		before := snapshot()
		later := build(t, ref, now.Add(48*time.Hour))
		result, err := seed.Apply(t.Context(), d.Pool, later)
		if err != nil {
			t.Fatal(err)
		}
		added, kept := result.Total()
		if added != 0 || kept != inserted {
			t.Fatalf("reapplication: %d inserted, %d existing; want 0, %d", added, kept, inserted)
		}
		if after := snapshot(); !maps.Equal(before, after) {
			t.Fatalf("reapplication modified commerce rows")
		}
	}
	reapply()
	// Real operator edits must survive: catalogue prices and billing party
	// details can change, but the old order's money/title snapshot cannot.
	if _, err := d.Pool.Exec(t.Context(), `UPDATE offerings SET title = 'ÖRNEK Güncellenmiş Paket', unit_price_amount = 19000
		WHERE id = $1`, p.Demo.Commerce.PackageOfferingID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(t.Context(), `UPDATE legal_parties SET title = 'ÖRNEK Yeni Demo Unvanı' WHERE id = $1`,
		p.Demo.Commerce.BuyerPartyID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(t.Context(), `UPDATE offerings SET title = 'ÖRNEK Güncellenmiş Doğrudan Paket', unit_price_amount = 17000
		WHERE id = $1`, p.Demo.Commerce.DirectOfferingID); err != nil {
		t.Fatal(err)
	}
	reapply()
	count(`SELECT count(*) FROM order_lines WHERE unit_price_amount = 10000 AND title_snapshot = 'ÖRNEK Hedefli Gönderi Paketi'`, 2)
	count(`SELECT count(*) FROM order_lines l JOIN orders o ON o.id = l.order_id
		WHERE o.id = $1 AND l.unit_price_amount = 10000 AND l.title_snapshot = 'ÖRNEK Doğrudan Kulüp Destek Paketi'
		AND o.koro_collect_amount = 600 AND o.direct_to_payee_amount = 12000 AND o.payable_amount = 12600`,
		1, p.Demo.Commerce.DirectOrderID)
}

// A minimal synthetic reference fixture keeps this test independent of
// the command package while exercising all real demo rows and their FKs.
func seedCommerceReference(t *testing.T, d *db.DB) seed.Reference {
	t.Helper()
	ref := reference()
	if err := d.Pool.QueryRow(t.Context(), `SELECT id FROM organizations WHERE kind = 'platform'`).Scan(&ref.PlatformOrgID); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for i, u := range ref.Universities {
		exec(`INSERT INTO organizations (id, kind, name, slug) VALUES ($1, 'university', $2, $3)`, u.OrgID, u.Name, fmt.Sprintf("demo-uni-%d", i))
		exec(`INSERT INTO universities (id, org_id, yok_code, name, short_name, city, type)
			VALUES ($1, $2, $3, $4, $5, $6, 'state')`, u.ID, u.OrgID, u.YokCode, u.Name, u.ShortName, u.City)
		for _, dept := range u.Departments {
			exec(`INSERT INTO departments (id, university_id, name, degree_level, duration_years)
				VALUES ($1, $2, $3, $4, $5)`, dept.ID, u.ID, dept.Name, dept.DegreeLevel, dept.DurationYears)
		}
	}
	return ref
}
