package seed

import (
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/money"
)

// CommerceDemo identifies the synthetic Model A/C catalogue and unpaid
// orders. The collection account is deliberately left to its own setup:
// no payment, ledger entry, invoice or bank evidence is fabricated here.
type CommerceDemo struct {
	BuyerPartyID, PayeePartyID, PayeeID         uuid.UUID
	PackageOfferingID, ClubOfferingID           uuid.UUID
	PackageOrderID, ClubOrderID, ExpiredOrderID uuid.UUID
	PackageEntitlementID, ExpiredEntitlementID  uuid.UUID
	DirectOfferingID, DirectOrderID             uuid.UUID
}

// These are existing checksum-valid parties test vectors, not real tax
// registrations. The pricing examples are synthetic, not commercial terms.
const demoBuyerTaxNo = "1111111114"
const demoPayeeTaxNo = "4567890128"

func (b *builder) commerce() error {
	c := CommerceDemo{
		BuyerPartyID: b.id("party", 0), PayeePartyID: b.id("party", 1), PayeeID: b.id("payee", 0),
		PackageOfferingID: b.id("offering", 0), ClubOfferingID: b.id("offering", 1),
		PackageOrderID: b.id("order", 0), ClubOrderID: b.id("order", 1), ExpiredOrderID: b.id("order", 2),
		PackageEntitlementID: b.id("commerce_ent", 0), ExpiredEntitlementID: b.id("commerce_ent", 2),
		DirectOfferingID: b.id("offering", 2), DirectOrderID: b.id("order", 3),
	}
	b.p.Demo.Commerce = c
	created := b.now.AddDate(0, 0, -30)
	verified := created.Add(time.Hour)
	address := map[string]any{"line1": "ÖRNEK — sentetik demo adresi", "district": "Örnek", "city": "İstanbul", "country": "TR"}
	buyer := map[string]any{"title": "ÖRNEK Demo Yazılım A.Ş.", "tax_no": demoBuyerTaxNo,
		"tax_office": "Örnek Vergi Dairesi", "address": address, "email": CompanyEmail}
	// Verification mirrors parties.verificationReady: legal tax identifier
	// and a nonempty address/city. Bank-account verification is separate.
	b.p.add("legal_parties", "id", c.BuyerPartyID, "kind", "buyer_company", "legal_type", "company",
		"org_id", b.p.Demo.CompanyOrgID, "title", buyer["title"], "tax_no", demoBuyerTaxNo,
		"tax_office", buyer["tax_office"], "address", jsonb(address), "email", CompanyEmail,
		"vat_status", "vat_payer", "status", "verified", "created_by", b.companies[0].owner,
		"verified_by", b.admin2, "verified_at", verified, "created_at", created, "updated_at", verified)
	b.p.add("legal_parties", "id", c.PayeePartyID, "kind", "payee", "legal_type", "foundation",
		"title", "ÖRNEK Demo Eğitim Vakfı", "tax_no", demoPayeeTaxNo, "tax_office", "Örnek Vergi Dairesi",
		"address", jsonb(address), "email", "demo.vakif@example.test", "vat_status", "vat_payer",
		"status", "verified", "created_by", b.admin1, "verified_by", b.admin2,
		"verified_at", verified, "created_at", created, "updated_at", verified)
	b.p.add("payees", "id", c.PayeeID, "legal_party_id", c.PayeePartyID, "org_id", b.p.Demo.MainClubOrgID,
		"status", "active", "is_default", true, "payout_method", "bank_eft", "invoicing_capability", "can_issue",
		"created_by", b.admin1, "verified_by", b.admin2, "verified_at", verified, "valid_from", verified,
		"created_at", created, "updated_at", verified)
	for i, spec := range []struct {
		code, title, category, fulfillment string
		price                              int64
	}{
		{"demo_targeted_post", "ÖRNEK Hedefli Gönderi Paketi", "targeted_post_quota", "platform_native", 10000},
		{"demo_club_sponsorship", "ÖRNEK Kulüp Destek Paketi", "sponsorship", "org_bound", 25000},
	} {
		var template, payee any
		seller, creator, sellerKind, trigger := b.ref.PlatformOrgID, b.admin1, "platform", "on_payment"
		if i == 0 {
			template = jsonb(map[string]any{"feature": "targeted_post", "kind": "quota", "quantity_per_unit": 5, "valid_days": 90})
		} else {
			seller, creator, sellerKind, trigger = b.p.Demo.MainClubOrgID, b.clubs[0].admin.id, "org", "on_fulfillment"
			payee = c.PayeeID
		}
		svc, offer, policy := b.id("service", i), b.id("offering", i), b.id("policy", i)
		b.p.add("services", "id", svc, "code", spec.code, "name_tr", spec.title,
			"description_tr", "Sentetik demo hizmeti; gerçek ticari teklif değildir.", "category", spec.category,
			"fulfillment_type", spec.fulfillment, "entitlement_template", template, "default_vat_rate_bp", 2000,
			"invoice_description_tr", spec.title, "accounting_reviewed_at", verified,
			"accounting_reviewed_ref", "DEMO — yalnız sentetik örnek", "active", true, "created_at", created, "updated_at", verified)
		b.p.add("offerings", "id", offer, "service_id", svc, "seller_kind", sellerKind, "org_id", seller,
			"default_payee_id", payee, "title", spec.title, "deliverables", jsonb([]any{}),
			"unit_price_amount", spec.price, "vat_rate_bp", 2000, "quantity_total", 10, "quantity_sold", 1,
			"sales_start_at", verified, "sales_end_at", b.now.AddDate(0, 3, 0), "invoice_trigger", trigger,
			"status", "active", "submitted_at", verified, "created_by", creator, "approved_by", b.admin2,
			"approved_at", verified.Add(time.Hour), "created_at", created, "updated_at", verified.Add(time.Hour))
		// Offering scope avoids overriding an operator's bootstrap policy.
		b.p.add("commerce_policies", "id", policy, "scope", "offering", "scope_id", offer,
			"model_code", "A", "collection_method", "bank_transfer", "commission_rate_bp", 0,
			"commission_basis", "net_excl_vat", "commission_payer", "seller", "commission_vat_rate_bp", 2000,
			"invoice_automation", "manual", "dispute_window_days", 7, "payout_hold_days", 0,
			"valid_from", created, "notes", "Sentetik demo: sıfır komisyon, gerçek ticari koşul değildir.",
			"created_by", b.admin1, "created_at", created)
		if err := b.commerceOrder(i, i, spec.title, spec.price, seller, payee, buyer); err != nil {
			return err
		}
		if i == 0 {
			if err := b.commerceOrder(2, i, spec.title, spec.price, seller, payee, buyer); err != nil {
				return err
			}
		}
	}
	return b.commerceDirect(buyer)
}

func (b *builder) commerceOrder(n, offering int, title string, price int64, seller uuid.UUID, payee any, buyer map[string]any) error {
	vat, err := money.VAT(price, 2000)
	if err != nil {
		return fmt.Errorf("seed: commerce VAT: %w", err)
	}
	gross, err := money.Add(price, vat)
	if err != nil {
		return fmt.Errorf("seed: commerce total: %w", err)
	}
	placed, status, fulfillment := b.now.Add(-time.Hour), "awaiting_payment", "pending"
	if offering == 0 {
		fulfillment = "not_applicable"
	}
	if n == 2 {
		placed, status, fulfillment = b.now.AddDate(0, 0, -14), "expired", "cancelled"
	}
	expires := placed.AddDate(0, 0, 7)
	order, line := b.id("order", n), b.id("line", n)
	b.p.add("orders", "id", order, "order_no", fmt.Sprintf("KRDEM0000%d", n), "org_id", b.p.Demo.CompanyOrgID,
		"placed_by_user_id", b.companies[0].owner, "buyer_party_id", b.p.Demo.Commerce.BuyerPartyID,
		"buyer_snapshot", jsonb(buyer), "model_code", "A", "collection_method", "bank_transfer", "status", status,
		"subtotal_amount", price, "vat_amount", vat, "total_amount", gross, "payable_amount", gross,
		"koro_collect_amount", gross, "expires_at", expires, "placed_at", placed, "created_at", placed, "updated_at", placed)
	b.p.add("order_lines", "id", line, "order_id", order, "org_id", b.p.Demo.CompanyOrgID,
		"seller_org_id", seller, "payee_id", payee, "offering_id", b.id("offering", offering), "service_id", b.id("service", offering),
		"commerce_policy_id", b.id("policy", offering), "title_snapshot", title, "deliverables_snapshot", jsonb([]any{}),
		"quantity", 1, "unit_price_amount", price, "net_amount", price, "vat_rate_bp", 2000, "vat_amount", vat,
		"gross_amount", gross, "commission_rate_bp", 0, "commission_basis", "net_excl_vat", "commission_payer", "seller",
		"commission_amount", int64(0), "commission_vat_rate_bp", 2000, "commission_vat_amount", int64(0),
		"buyer_fee_amount", int64(0), "seller_net_amount", gross, "payable_amount", gross, "fulfillment_status", fulfillment,
		"created_at", placed, "updated_at", placed)
	if offering == 0 {
		rightStatus := "pending"
		var revoked any
		var reason any
		if n == 2 {
			rightStatus, revoked, reason = "revoked", expires, "Order expired"
		}
		b.p.add("entitlements", "id", b.id("commerce_ent", n), "org_id", b.p.Demo.CompanyOrgID,
			"feature", "targeted_post", "kind", "quota", "quantity_total", 5, "quantity_used", 0,
			"valid_from", placed, "valid_to", expires.AddDate(0, 0, 90), "status", rightStatus,
			"source_type", "order_line", "source_id", line, "revoked_at", revoked, "revoke_reason", reason,
			"created_at", placed, "updated_at", placed)
	}
	return nil
}
