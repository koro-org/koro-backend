package seed_test

import (
	"testing"

	"koro/db/seed"
)

func TestBuild_ModelCCommerce(t *testing.T) {
	t.Parallel()
	p := build(t, reference(), now)
	c := p.Demo.Commerce
	find := func(table string, id any) map[string]any {
		t.Helper()
		for _, r := range rows(p, table) {
			if r["id"] == id {
				return r
			}
		}
		t.Fatalf("missing %s row %v", table, id)
		return nil
	}
	check := func(r, want map[string]any) {
		t.Helper()
		for col, v := range want {
			if r[col] != v {
				t.Errorf("%s: %v = %v, want %v", r["id"], col, r[col], v)
			}
		}
	}
	order := find("orders", c.DirectOrderID)
	check(order, map[string]any{
		"order_no": "KRDEM00003", "model_code": "C", "collection_method": "direct_to_payee", "status": "awaiting_payment",
		"org_id": p.Demo.CompanyOrgID, "buyer_party_id": c.BuyerPartyID,
		"subtotal_amount": int64(10000), "vat_amount": int64(2000), "buyer_fee_amount": int64(600),
		"total_amount": int64(12600), "payable_amount": int64(12600), "koro_collect_amount": int64(600), "direct_to_payee_amount": int64(12000),
		"paid_at": nil, "direct_confirmed_at": nil, "collection_account_id": nil,
	})
	var line map[string]any
	for _, r := range rows(p, "order_lines") {
		if r["order_id"] == c.DirectOrderID {
			if line != nil {
				t.Fatal("Model C has more than one line")
			}
			line = r
		}
	}
	if line == nil {
		t.Fatal("Model C has no line")
	}
	check(line, map[string]any{"offering_id": c.DirectOfferingID, "payee_id": c.PayeeID,
		"seller_org_id": p.Demo.MainClubOrgID, "org_id": p.Demo.CompanyOrgID, "commission_payer": "buyer",
		"commission_rate_bp": int64(500), "commission_amount": int64(500), "commission_vat_amount": int64(100),
		"buyer_fee_amount": int64(600), "seller_net_amount": int64(12000), "payable_amount": int64(12600),
		"fulfillment_status": "pending", "payout_id": nil})
	check(find("commerce_policies", line["commerce_policy_id"]), map[string]any{
		"scope": "offering", "scope_id": c.DirectOfferingID, "model_code": "C", "collection_method": "direct_to_payee",
		"commission_payer": "buyer", "commission_rate_bp": line["commission_rate_bp"], "commission_vat_rate_bp": line["commission_vat_rate_bp"],
	})
	check(find("offerings", c.DirectOfferingID), map[string]any{
		"org_id": p.Demo.MainClubOrgID, "seller_kind": "org", "service_id": line["service_id"], "default_payee_id": c.PayeeID,
		"unit_price_amount": int64(10000), "quantity_sold": 1,
	})
	// The three pre-existing Model A examples retain their reference codes,
	// values and states; the new offering reuses a service, not their policy.
	for id, want := range map[any]map[string]any{
		c.PackageOrderID: {"order_no": "KRDEM00000", "status": "awaiting_payment", "payable_amount": int64(12000)},
		c.ClubOrderID:    {"order_no": "KRDEM00001", "status": "awaiting_payment", "payable_amount": int64(30000)},
		c.ExpiredOrderID: {"order_no": "KRDEM00002", "status": "expired", "payable_amount": int64(12000)},
	} {
		want["model_code"], want["collection_method"] = "A", "bank_transfer"
		check(find("orders", id), want)
	}
	for table, want := range map[string]int{"services": 2, "offerings": 3, "commerce_policies": 3, "orders": 4,
		"order_lines": 4, "student_profiles": seed.StudentCount, "collection_accounts": 1, "payee_bank_accounts": 0,
		"payments": 0, "ledger_transactions": 0, "ledger_entries": 0, "invoices": 0, "outbox": 0} {
		if got := p.Counts()[table]; got != want {
			t.Errorf("%s: %d rows, want %d", table, got, want)
		}
	}
}
