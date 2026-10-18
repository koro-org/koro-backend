package seed

import (
	"fmt"
	"time"

	"koro/internal/platform/money"
)

// commerceDirect appends a Model C example without changing the existing
// Model A rows or ID ordinals. Only the illustrative buyer fee belongs to
// Koro; the seller's gross amount goes directly to the payee. The bankless
// demo payee needs an approved account before payment instructions can be
// used. No account, payment evidence or confirmation is invented by seeding.
func (b *builder) commerceDirect(buyer map[string]any) error {
	const (
		price     int64 = 10000
		feeRateBP int64 = 500 // synthetic arithmetic example, not commercial terms
		vatRateBP int64 = 2000
		title           = "ÖRNEK Doğrudan Kulüp Destek Paketi"
	)
	vat, err := money.VAT(price, vatRateBP)
	if err != nil {
		return fmt.Errorf("seed: direct sale VAT: %w", err)
	}
	direct, err := money.Add(price, vat)
	if err != nil {
		return fmt.Errorf("seed: direct sale total: %w", err)
	}
	fee, err := money.Commission(price, feeRateBP, nil, nil)
	if err != nil {
		return fmt.Errorf("seed: direct sale fee: %w", err)
	}
	feeVAT, err := money.VAT(fee, vatRateBP)
	if err != nil {
		return fmt.Errorf("seed: direct sale fee VAT: %w", err)
	}
	collected, err := money.Add(fee, feeVAT)
	if err != nil {
		return fmt.Errorf("seed: direct sale collection: %w", err)
	}
	payable, err := money.Add(direct, collected)
	if err != nil {
		return fmt.Errorf("seed: direct sale payable: %w", err)
	}
	c := b.p.Demo.Commerce
	service, policy, line := b.id("service", 1), b.id("policy", 2), b.id("line", 3)
	created, placed := b.now.AddDate(0, 0, -30), b.now.Add(-time.Hour)
	approved := created.Add(2 * time.Hour)
	b.p.add("offerings", "id", c.DirectOfferingID, "service_id", service, "seller_kind", "org",
		"org_id", b.p.Demo.MainClubOrgID, "default_payee_id", c.PayeeID, "title", title,
		"deliverables", jsonb([]any{}), "unit_price_amount", price, "vat_rate_bp", vatRateBP,
		"quantity_total", 10, "quantity_sold", 1, "sales_start_at", approved,
		"sales_end_at", b.now.AddDate(0, 3, 0), "invoice_trigger", "on_fulfillment", "status", "active",
		"submitted_at", created.Add(time.Hour), "created_by", b.clubs[0].admin.id, "approved_by", b.admin2,
		"approved_at", approved, "created_at", created, "updated_at", approved)
	// Production policy changes go through the admin route. This direct
	// insert belongs only to the existing dev/staging demo command gate.
	b.p.add("commerce_policies", "id", policy, "scope", "offering", "scope_id", c.DirectOfferingID,
		"model_code", "C", "collection_method", "direct_to_payee", "commission_rate_bp", feeRateBP,
		"commission_basis", "net_excl_vat", "commission_payer", "buyer", "commission_vat_rate_bp", vatRateBP,
		"invoice_automation", "manual", "dispute_window_days", 7, "payout_hold_days", 0, "valid_from", created,
		"notes", "Sentetik Model C örneği; gerçek ticari koşul değildir.", "created_by", b.admin1, "created_at", created)
	b.p.add("orders", "id", c.DirectOrderID, "order_no", "KRDEM00003", "org_id", b.p.Demo.CompanyOrgID,
		"placed_by_user_id", b.companies[0].owner, "buyer_party_id", c.BuyerPartyID, "buyer_snapshot", jsonb(buyer),
		"model_code", "C", "collection_method", "direct_to_payee", "status", "awaiting_payment",
		"subtotal_amount", price, "vat_amount", vat, "buyer_fee_amount", collected,
		"total_amount", payable, "payable_amount", payable, "koro_collect_amount", collected,
		"direct_to_payee_amount", direct, "expires_at", placed.AddDate(0, 0, 7), "placed_at", placed,
		"created_at", placed, "updated_at", placed)
	b.p.add("order_lines", "id", line, "order_id", c.DirectOrderID, "org_id", b.p.Demo.CompanyOrgID,
		"seller_org_id", b.p.Demo.MainClubOrgID, "payee_id", c.PayeeID, "offering_id", c.DirectOfferingID,
		"service_id", service, "commerce_policy_id", policy, "title_snapshot", title, "deliverables_snapshot", jsonb([]any{}),
		"quantity", 1, "unit_price_amount", price, "net_amount", price, "vat_rate_bp", vatRateBP, "vat_amount", vat,
		"gross_amount", direct, "commission_rate_bp", feeRateBP, "commission_basis", "net_excl_vat",
		"commission_payer", "buyer", "commission_amount", fee, "commission_vat_rate_bp", vatRateBP,
		"commission_vat_amount", feeVAT, "buyer_fee_amount", collected, "seller_net_amount", direct,
		"payable_amount", payable, "fulfillment_status", "pending", "created_at", placed, "updated_at", placed)
	return nil
}
