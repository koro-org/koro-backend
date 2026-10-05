package parties

import (
	"context"
	"fmt"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/authz"
	"koro/internal/platform/db"
)

// payeeConstraints maps the named constraints of 000096/000097 onto
// module errors (05 §6.6). The four-eyes CHECKs answer like authz (03 §6.1:
// the database is the second layer of the same rule).
var payeeConstraints = db.Constraints{
	"payees_org_party_uq":                        ErrPayeeExists,
	"payee_bank_accounts_pending_uq":             ErrBankAccountPending,
	"payee_bank_accounts_iban_chk":               ErrIBANInvalid,
	"payee_bank_accounts_approver_submitter_chk": authz.ErrFourEyes,
	"payee_bank_accounts_approver_verifier_chk":  authz.ErrFourEyes,
	"payee_bank_accounts_verifier_submitter_chk": authz.ErrFourEyes,
}

func takePayee(op string, q *gorm.DB) (Payee, error) {
	var p Payee
	err := q.Take(&p).Error
	switch {
	case db.IsNotFound(err):
		return Payee{}, ErrNotFound
	case err != nil:
		return Payee{}, fmt.Errorf("parties: %s: %w", op, err)
	}
	return p, nil
}

func lockIf(q *gorm.DB, lock bool) *gorm.DB {
	if lock {
		return q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	return q
}

// InsertPayee implements Store.
//
// cross-org: a new row carries its own org_id (the seller org the service
// resolved).
func (s gormStore) InsertPayee(ctx context.Context, p Payee) (Payee, error) {
	p.PartyKind = KindPayee
	if err := db.Conn(ctx, s.orm).Create(&p).Error; err != nil {
		return Payee{}, fmt.Errorf("parties: insert payee: %w", payeeConstraints.Translate(err))
	}
	return s.GetPayee(ctx, p.ID, false)
}

// GetPayee implements Store.
//
// cross-org: addressed by ID (cross-module reads, the admin surface, a row
// the service resolves within its org first).
func (s gormStore) GetPayee(ctx context.Context, id uuid.UUID, lock bool) (Payee, error) {
	return takePayee("get payee", lockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// OrgPayee implements Store.
func (s gormStore) OrgPayee(ctx context.Context, orgID, id uuid.UUID, lock bool) (Payee, error) {
	if orgID == uuid.Nil() {
		return Payee{}, db.ErrMissingOrgScope
	}
	return takePayee("org payee", lockIf(db.Conn(ctx, s.orm), lock).Scopes(db.ScopeOrg(orgID)).Where("id = ?", id))
}

// OrgPayees implements Store.
func (s gormStore) OrgPayees(ctx context.Context, orgID uuid.UUID) ([]Payee, error) {
	var out []Payee
	if err := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("parties: org payees: %w", err)
	}
	return out, nil
}

// DefaultPayee implements Store.
func (s gormStore) DefaultPayee(ctx context.Context, orgID uuid.UUID) (Payee, bool, error) {
	var out []Payee
	err := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("is_default AND status = 'active'").Limit(1).Find(&out).Error
	if err != nil {
		return Payee{}, false, fmt.Errorf("parties: default payee: %w", err)
	}
	if len(out) == 0 {
		return Payee{}, false, nil
	}
	return out[0], true, nil
}

// UpdatePayee implements Store: a targeted UPDATE of the mutable columns
// (legal_party_id, party_kind, org_id, created_by, valid_from and
// created_at never change; updated_at is the trigger's), RETURNING the row.
//
// cross-org: addressed by ID; the service has locked the row (admin
// surface) in the same transaction.
func (s gormStore) UpdatePayee(ctx context.Context, p Payee) (Payee, error) {
	var rows []Payee
	err := db.Conn(ctx, s.orm).Raw(`UPDATE payees SET
		status = @status, is_default = @is_default, payout_method = @payout_method,
		invoicing_capability = @invoicing_capability, university_protocol_ref = @university_protocol_ref,
		verified_at = @verified_at, verified_by = @verified_by, valid_to = @valid_to
		WHERE id = @id
		RETURNING *`, map[string]any{
		"id": p.ID, "status": p.Status, "is_default": p.IsDefault, "payout_method": p.PayoutMethod,
		"invoicing_capability": p.InvoicingCapability, "university_protocol_ref": p.UniversityProtocolRef,
		"verified_at": p.VerifiedAt, "verified_by": p.VerifiedBy, "valid_to": p.ValidTo,
	}).Scan(&rows).Error
	if err != nil {
		return Payee{}, fmt.Errorf("parties: update payee: %w", payeeConstraints.Translate(err))
	}
	if len(rows) == 0 {
		return Payee{}, ErrNotFound
	}
	return rows[0], nil
}

// ClearDefault implements Store.
func (s gormStore) ClearDefault(ctx context.Context, orgID, keep uuid.UUID) ([]uuid.UUID, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	var ids []uuid.UUID
	err := db.Conn(ctx, s.orm).Raw(`UPDATE payees SET is_default = false
		WHERE org_id = @org_id AND is_default AND status = 'active' AND id <> @keep
		RETURNING id`, map[string]any{"org_id": orgID, "keep": keep}).Scan(&ids).Error
	if err != nil {
		return nil, fmt.Errorf("parties: clear default payee: %w", err)
	}
	return ids, nil
}

// PayeesCreatedBy implements Store.
//
// cross-org: the KVKK export of one user.
func (s gormStore) PayeesCreatedBy(ctx context.Context, userID uuid.UUID) ([]Payee, error) {
	var out []Payee
	if err := db.Conn(ctx, s.orm).Where("created_by = ?", userID).Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("parties: payees created by: %w", err)
	}
	return out, nil
}

func takeAccount(op string, q *gorm.DB) (BankAccount, error) {
	var b BankAccount
	err := q.Take(&b).Error
	switch {
	case db.IsNotFound(err):
		return BankAccount{}, ErrNotFound
	case err != nil:
		return BankAccount{}, fmt.Errorf("parties: %s: %w", op, err)
	}
	return b, nil
}

// InsertBankAccount implements Store (bank_code is generated: read only).
//
// cross-org: the org_id of the new row is its payee's (the service read
// the payee; the composite FK (payee_id, org_id) holds them together).
func (s gormStore) InsertBankAccount(ctx context.Context, b BankAccount) (BankAccount, error) {
	if err := db.Conn(ctx, s.orm).Create(&b).Error; err != nil {
		return BankAccount{}, fmt.Errorf("parties: insert bank account: %w", payeeConstraints.Translate(err))
	}
	return s.GetBankAccount(ctx, b.ID, false)
}

// GetBankAccount implements Store.
//
// cross-org: addressed by ID (the admin surface, cross-module reads); the
// org surface compares its payee with the payee resolved within the org.
func (s gormStore) GetBankAccount(ctx context.Context, id uuid.UUID, lock bool) (BankAccount, error) {
	return takeAccount("get bank account", lockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// PayeeBankAccounts implements Store.
//
// cross-org: the accounts of a payee the service has resolved (within its
// org on the org surface).
func (s gormStore) PayeeBankAccounts(ctx context.Context, payeeID uuid.UUID) ([]BankAccount, error) {
	var out []BankAccount
	if err := db.Conn(ctx, s.orm).Where("payee_id = ?", payeeID).Order("id DESC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("parties: payee bank accounts: %w", err)
	}
	return out, nil
}

// ApprovedAccounts implements Store.
//
// cross-org: the accounts of payees the caller has resolved.
func (s gormStore) ApprovedAccounts(ctx context.Context, payeeIDs []uuid.UUID, lock bool) (map[uuid.UUID]BankAccount, error) {
	out := map[uuid.UUID]BankAccount{}
	if len(payeeIDs) == 0 {
		return out, nil
	}
	var rows []BankAccount
	q := lockIf(db.Conn(ctx, s.orm), lock).Where("payee_id IN ? AND status = 'approved'", payeeIDs).Order("id")
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("parties: approved bank accounts: %w", err)
	}
	for _, b := range rows {
		out[b.PayeeID] = b
	}
	return out, nil
}

// ListBankAccounts implements Store.
//
// cross-org: the four-eyes queue of finance and platform admins.
func (s gormStore) ListBankAccounts(ctx context.Context, statuses []BankAccountStatus, after *uuid.UUID, limit int) ([]BankAccount, error) {
	q := db.Conn(ctx, s.orm).Model(&BankAccount{})
	if len(statuses) > 0 {
		q = q.Where("status IN ?", statuses)
	}
	if after != nil {
		q = q.Where("id > ?", *after)
	}
	var out []BankAccount
	if err := q.Order("id").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("parties: list bank accounts: %w", err)
	}
	return out, nil
}

// TransitionBankAccount implements Store: a targeted UPDATE of the state
// columns guarded by the expected status (the frozen columns are never in
// the statement), RETURNING the row.
//
// cross-org: addressed by ID; the service has locked the row in the same
// transaction.
func (s gormStore) TransitionBankAccount(ctx context.Context, b BankAccount, from BankAccountStatus) (BankAccount, error) {
	var rows []BankAccount
	err := db.Conn(ctx, s.orm).Raw(`UPDATE payee_bank_accounts SET
		status = @status, verified_by = @verified_by, verified_at = @verified_at,
		approved_by = @approved_by, approved_at = @approved_at,
		rejected_by = @rejected_by, rejected_at = @rejected_at, rejection_reason = @rejection_reason,
		superseded_at = @superseded_at, superseded_by_id = @superseded_by_id,
		revoked_by = @revoked_by, revoked_at = @revoked_at, revoke_reason = @revoke_reason
		WHERE id = @id AND status = @from
		RETURNING *`, map[string]any{
		"id": b.ID, "from": from, "status": b.Status, "verified_by": b.VerifiedBy, "verified_at": b.VerifiedAt,
		"approved_by": b.ApprovedBy, "approved_at": b.ApprovedAt, "rejected_by": b.RejectedBy,
		"rejected_at": b.RejectedAt, "rejection_reason": b.RejectionReason, "superseded_at": b.SupersededAt,
		"superseded_by_id": b.SupersededByID, "revoked_by": b.RevokedBy, "revoked_at": b.RevokedAt,
		"revoke_reason": b.RevokeReason,
	}).Scan(&rows).Error
	if err != nil {
		return BankAccount{}, fmt.Errorf("parties: bank account %s: %w", b.Status, payeeConstraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetBankAccount(ctx, b.ID, false); err != nil {
			return BankAccount{}, err
		}
		return BankAccount{}, ErrInvalidState
	}
	return rows[0], nil
}

// BankAccountsSubmittedBy implements Store.
//
// cross-org: the KVKK export of one user.
func (s gormStore) BankAccountsSubmittedBy(ctx context.Context, userID uuid.UUID) ([]BankAccount, error) {
	var out []BankAccount
	if err := db.Conn(ctx, s.orm).Where("submitted_by = ?", userID).Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("parties: bank accounts submitted by: %w", err)
	}
	return out, nil
}
