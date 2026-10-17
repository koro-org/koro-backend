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
	if inserted != len(p.Rows) || existing != 0 {
		t.Fatalf("first application: %d inserted, %d existing; plan has %d rows", inserted, existing, len(p.Rows))
	}
	for table, want := range map[string]int{"legal_parties": 2, "payees": 1, "services": 2,
		"offerings": 2, "commerce_policies": 2, "orders": 3, "order_lines": 3} {
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
		AND address->>'line1' <> '' AND address->>'city' <> '' AND verified_by <> created_by`, 2)
	count(`SELECT count(*) FROM payees p JOIN legal_parties lp ON lp.id = p.legal_party_id
		WHERE p.status = 'active' AND p.is_default AND p.verified_by <> p.created_by AND lp.status = 'verified'`, 1)
	count(`SELECT count(*) FROM offerings WHERE status = 'active' AND approved_by <> created_by`, 2)
	count(`SELECT count(*) FROM orders WHERE model_code = 'A' AND collection_method = 'bank_transfer'
		AND paid_amount = 0 AND paid_at IS NULL AND collection_account_id IS NULL`, 3)
	count(`SELECT count(*) FROM orders WHERE status = 'awaiting_payment' AND expires_at > $1`, 2, now)
	count(`SELECT count(*) FROM orders WHERE status = 'expired' AND expires_at < $1`, 1, now)
	count(`SELECT count(*) FROM order_lines l JOIN orders o ON o.id = l.order_id
		WHERE l.org_id = o.org_id AND l.quantity = 1 AND l.net_amount = l.unit_price_amount
		AND l.payable_amount = o.payable_amount AND l.vat_amount = o.vat_amount
		AND l.commission_amount = 0 AND l.seller_net_amount = l.payable_amount`, 3)
	count(`SELECT count(*) FROM orders WHERE subtotal_amount = 10000 AND vat_amount = 2000 AND payable_amount = 12000`, 2)
	count(`SELECT count(*) FROM orders WHERE subtotal_amount = 25000 AND vat_amount = 5000 AND payable_amount = 30000`, 1)
	count(`SELECT count(*) FROM offerings f WHERE f.quantity_sold <>
		(SELECT coalesce(sum(l.quantity), 0) FROM order_lines l JOIN orders o ON o.id = l.order_id
		 WHERE l.offering_id = f.id AND o.status NOT IN ('expired','cancelled'))`, 0)
	count(`SELECT count(*) FROM entitlements e JOIN order_lines l ON l.id = e.source_id
		JOIN orders o ON o.id = l.order_id WHERE e.source_type = 'order_line' AND e.org_id = o.org_id
		AND e.quantity_total = 5 AND e.quantity_used = 0 AND e.activated_at IS NULL
		AND ((o.status = 'awaiting_payment' AND e.status = 'pending' AND l.fulfillment_status = 'not_applicable')
		 OR (o.status = 'expired' AND e.status = 'revoked' AND e.revoked_at = o.expires_at AND l.fulfillment_status = 'cancelled'))`, 2)
	for _, table := range []string{"collection_accounts", "payee_bank_accounts", "payments", "incoming_bank_transfers",
		"ledger_transactions", "ledger_entries", "invoices", "outbox"} {
		count("SELECT count(*) FROM "+table, 0)
	}

	snapshot := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, table := range []string{"legal_parties", "payees", "services", "offerings", "commerce_policies", "orders", "order_lines", "entitlements"} {
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
	reapply()
	count(`SELECT count(*) FROM order_lines WHERE unit_price_amount = 10000 AND title_snapshot = 'ÖRNEK Hedefli Gönderi Paketi'`, 2)
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
