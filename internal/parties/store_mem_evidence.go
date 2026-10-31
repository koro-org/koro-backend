package parties

import (
	"context"
	"uuid"
)

// ReplaceDocumentFile implements Store.
func (s *MemStore) ReplaceDocumentFile(_ context.Context, id, cur, next, by uuid.UUID) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[id]
	switch {
	case !ok:
		return Document{}, ErrNotFound
	case d.FileID != cur:
		return Document{}, ErrInvalidState
	}
	d.FileID, d.UploadedBy, d.VerifiedAt, d.VerifiedBy = next, by, nil, nil
	if err := s.checkDocument(d); err != nil {
		return Document{}, err
	}
	s.docs[id] = d
	return d, nil
}

// ReplaceAccountEvidence implements Store.
func (s *MemStore) ReplaceAccountEvidence(_ context.Context, b BankAccount, cur uuid.UUID, from BankAccountStatus) (BankAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.accounts[b.ID]
	switch {
	case !ok:
		return BankAccount{}, ErrNotFound
	case row.Status != from || row.EvidenceFileID != cur:
		return BankAccount{}, ErrInvalidState
	}
	next := row
	next.EvidenceFileID, next.Status, next.VerifiedBy, next.VerifiedAt = b.EvidenceFileID, b.Status, b.VerifiedBy, b.VerifiedAt
	if err := checkAccount(next); err != nil {
		return BankAccount{}, err
	}
	next.UpdatedAt = s.stamp(row.UpdatedAt)
	s.accounts[next.ID] = next
	return next, nil
}
