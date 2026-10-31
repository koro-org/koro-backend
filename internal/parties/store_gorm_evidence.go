package parties

import (
	"context"
	"fmt"
	"uuid"

	"koro/internal/platform/db"
)

// ReplaceDocumentFile implements Store with a targeted UPDATE of the file,
// its uploader and the verification (BE-154b).
//
// cross-org: addressed by ID; the service authorised the document.
func (s gormStore) ReplaceDocumentFile(ctx context.Context, id, cur, next, by uuid.UUID) (Document, error) {
	var rows []Document
	err := db.Conn(ctx, s.orm).Raw(`UPDATE legal_party_documents SET file_id = @next, uploaded_by = @by,
		verified_at = NULL, verified_by = NULL
		WHERE id = @id AND file_id = @cur RETURNING *`,
		map[string]any{"id": id, "cur": cur, "next": next, "by": by}).Scan(&rows).Error
	if err != nil {
		return Document{}, fmt.Errorf("parties: replace document file: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetDocument(ctx, id, false); err != nil {
			return Document{}, err
		}
		return Document{}, ErrInvalidState
	}
	return rows[0], nil
}

// ReplaceAccountEvidence implements Store with a targeted UPDATE of the
// evidence and the first eye (06 H-56: no Save over the frozen columns).
//
// cross-org: addressed by ID; the service authorised the account.
func (s gormStore) ReplaceAccountEvidence(ctx context.Context, b BankAccount, cur uuid.UUID, from BankAccountStatus) (BankAccount, error) {
	var rows []BankAccount
	err := db.Conn(ctx, s.orm).Raw(`UPDATE payee_bank_accounts SET evidence_file_id = @next, status = @status,
		verified_by = @verified_by, verified_at = @verified_at
		WHERE id = @id AND status = @from AND evidence_file_id = @cur RETURNING *`, map[string]any{
		"id": b.ID, "from": from, "cur": cur, "next": b.EvidenceFileID, "status": b.Status, "verified_by": b.VerifiedBy,
		"verified_at": b.VerifiedAt,
	}).Scan(&rows).Error
	if err != nil {
		return BankAccount{}, fmt.Errorf("parties: replace bank account evidence: %w", payeeConstraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetBankAccount(ctx, b.ID, false); err != nil {
			return BankAccount{}, err
		}
		return BankAccount{}, ErrInvalidState
	}
	return rows[0], nil
}
