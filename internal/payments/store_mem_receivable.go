package payments

import (
	"context"
	"slices"
	"uuid"

	"koro/internal/authz"
)

func checkCollection(c ReceivableCollection) error {
	if !c.Status.Valid() || c.Amount <= 0 || c.ReceiptFileID == uuid.Nil() || c.BankReference == "" {
		return ErrInvalidState
	}
	if c.ApprovedBy != nil && *c.ApprovedBy == c.RequestedBy {
		return authz.ErrFourEyes
	}
	collected, rejected := c.Status == CollectionCollected, c.Status == CollectionRejected
	if collected != (c.ApprovedBy != nil) || collected != (c.ApprovedAt != nil) || collected != (c.LedgerTransactionID != nil) ||
		rejected != (c.RejectedBy != nil) || rejected != (c.RejectedAt != nil) || rejected != (c.RejectionReason != nil) {
		return ErrInvalidState
	}
	return nil
}
func (m *MemStore) InsertCollection(_ context.Context, c ReceivableCollection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.Status != CollectionRequested {
		return ErrInvalidState
	}
	if err := checkCollection(c); err != nil {
		return err
	}
	for _, old := range m.collections {
		if old.ID == c.ID || (old.IncomingTransferID == c.IncomingTransferID || old.ReceiptFileID == c.ReceiptFileID) && old.Status != CollectionRejected {
			return ErrInvalidState
		}
	}
	if m.collections == nil {
		m.collections = map[uuid.UUID]ReceivableCollection{}
	}
	t := m.stamp()
	c.CreatedAt, c.UpdatedAt = t, t // the store's, as the column defaults (BE-069)
	m.collections[c.ID] = c
	return nil
}
func (m *MemStore) GetCollection(_ context.Context, id uuid.UUID, _ bool) (ReceivableCollection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.collections[id]
	if !ok {
		return ReceivableCollection{}, ErrNotFound
	}
	return c, nil
}
func (m *MemStore) UpdateCollection(_ context.Context, c ReceivableCollection) (ReceivableCollection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.collections[c.ID]
	if !ok {
		return ReceivableCollection{}, ErrNotFound
	}
	if old.Status != CollectionRequested {
		return ReceivableCollection{}, ErrInvalidState
	}
	next := old
	next.Status, next.ApprovedBy, next.ApprovedAt = c.Status, c.ApprovedBy, c.ApprovedAt
	next.RejectedBy, next.RejectedAt, next.RejectionReason = c.RejectedBy, c.RejectedAt, c.RejectionReason
	next.LedgerTransactionID, next.UpdatedAt = c.LedgerTransactionID, m.touch(old.UpdatedAt)
	if err := checkCollection(next); err != nil {
		return ReceivableCollection{}, err
	}
	m.collections[c.ID] = next
	return next, nil
}
func (m *MemStore) ListCollections(_ context.Context, f CollectionFilter, after *TimeCursor, limit int) ([]ReceivableCollection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := []ReceivableCollection{}
	for _, c := range m.collections {
		if f.Status != "" && f.Status != c.Status || f.PayeeID != nil && *f.PayeeID != c.PayeeID || after != nil && compareTime(c.CreatedAt, c.ID, after.CreatedAt, after.ID) >= 0 {
			continue
		}
		rows = append(rows, c)
	}
	slices.SortFunc(rows, func(a, b ReceivableCollection) int { return compareTime(b.CreatedAt, b.ID, a.CreatedAt, a.ID) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}
func (m *MemStore) PendingCollections(_ context.Context, lineID, self uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for _, c := range m.collections {
		if c.OrderLineID == lineID && c.ID != self && c.Status == CollectionRequested {
			total += c.Amount
		}
	}
	return total, nil
}
func (m *MemStore) CollectionsOfUser(_ context.Context, id uuid.UUID) ([]ReceivableCollection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := []ReceivableCollection{}
	for _, c := range m.collections {
		if c.RequestedBy == id || c.ApprovedBy != nil && *c.ApprovedBy == id || c.RejectedBy != nil && *c.RejectedBy == id {
			rows = append(rows, c)
		}
	}
	slices.SortFunc(rows, func(a, b ReceivableCollection) int { return compareUUID(a.ID, b.ID) })
	return rows, nil
}
