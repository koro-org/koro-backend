package seed

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// CollectionIBAN is the shared ibantest vector with nonexistent bank code
// 99999. It is checksum-valid but never a destination for real payments.
const CollectionIBAN = "TR809999900000000000000001"

type collectionPlan struct {
	rows                  []Row // optional platform party, then pending account
	id, creator, approver uuid.UUID
	at                    time.Time
}

func (b *builder) collectionAccount() {
	party, account := b.id("party", 2), b.id("collection", 0)
	creator, approver := b.id("user", 3), b.id("user", 4) // FinanceEmail1/2
	at := b.now.AddDate(0, 0, -30)
	var p Plan
	p.add("legal_parties", "id", party, "kind", "platform", "legal_type", "company", "org_id", b.ref.PlatformOrgID,
		"title", "ÖRNEK Koro Demo Şirketi", "tax_no", "2223334447", "tax_office", "Örnek Vergi Dairesi",
		"address", jsonb(map[string]string{"line1": "ÖRNEK sentetik adres", "city": "İstanbul", "country": "TR"}),
		"email", "demo.tahsilat@example.test", "vat_status", "vat_payer", "status", "verified",
		"created_by", creator, "verified_by", approver, "verified_at", at, "created_at", at, "updated_at", at)
	p.add("collection_accounts", "id", account, "label", "ÖRNEK demo tahsilat hesabı", "legal_party_id", party,
		"iban", CollectionIBAN, "holder_name", "ÖRNEK Koro Demo Şirketi — gerçek hesap değildir",
		"bank_name", "ÖRNEK sentetik banka (99999)", "status", "pending_approval", "is_default", false,
		"created_by", creator, "created_at", at, "updated_at", at)
	b.p.collection = &collectionPlan{rows: p.Rows, id: account, creator: creator, approver: approver, at: at}
}

// apply preserves every pre-existing account and platform party. The table
// lock also serializes this small setup with admin account writes, which do
// not take the demo advisory lock. Only an account inserted in this call is
// approved. Any existing account means setup belongs to the operator,
// including a deliberate absence of an active default.
func (p *collectionPlan) apply(ctx context.Context, tx pgx.Tx, res *Result) error {
	if _, err := tx.Exec(ctx, `LOCK TABLE collection_accounts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("seed: collection account lock: %w", err)
	}
	var protected bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM collection_accounts)`).Scan(&protected); err != nil {
		return fmt.Errorf("seed: existing collection account: %w", err)
	}
	if protected {
		// Every protected account already has a platform legal party (FK).
		res.Existing["legal_parties"]++
		res.Existing["collection_accounts"]++
		return nil
	}
	var staff int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT u.id) FROM users u JOIN platform_role_assignments r ON r.user_id = u.id
		WHERE u.id IN ($1, $2) AND u.status = 'active' AND r.status IN ('active','revoke_pending')
		AND r.role IN ('finance_admin','platform_admin')`, p.creator, p.approver).Scan(&staff); err != nil {
		return fmt.Errorf("seed: collection account approvers: %w", err)
	}
	if staff != 2 {
		return errors.New("seed: collection account requires two active finance demo accounts")
	}
	// A platform party is globally unique. ON CONFLICT preserves an
	// operator's party unchanged; the account references its actual ID.
	sql, args := insertSQL(p.rows[0])
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("seed: collection platform party: %w", err)
	}
	if tag.RowsAffected() == 1 {
		res.Inserted["legal_parties"]++
	} else {
		res.Existing["legal_parties"]++
	}
	var party uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM legal_parties WHERE kind = 'platform'`).Scan(&party); err != nil {
		return fmt.Errorf("seed: platform party: %w", err)
	}
	sql, args = insertSQL(p.rows[1])
	for i, col := range p.rows[1].Cols {
		if col == "legal_party_id" {
			args[i] = party
		}
	}
	tag, err = tx.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("seed: pending collection account: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("seed: collection account was not inserted")
	}
	// Respect both the initial-status trigger and the distinct-eye CHECK.
	if _, err := tx.Exec(ctx, `UPDATE collection_accounts SET status = 'active', is_default = true,
		approved_by = $2, approved_at = $3 WHERE id = $1 AND status = 'pending_approval'`, p.id, p.approver, p.at); err != nil {
		return fmt.Errorf("seed: approve collection account: %w", err)
	}
	res.Inserted["collection_accounts"]++
	return nil
}
