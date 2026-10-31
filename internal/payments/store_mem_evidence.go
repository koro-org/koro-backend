package payments

import (
	"context"
	"fmt"
	"uuid"
)

// ReplaceEvidence implements Store (the transfer's collections follow its
// receipt, as receivable_collections_source_fkey's ON UPDATE CASCADE).
func (m *MemStore) ReplaceEvidence(_ context.Context, slot EvidenceSlot, id, cur, next uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// swap writes next into col when it holds cur and the row is in state.
	swap := func(found, state bool, col **uuid.UUID) error {
		switch {
		case !found:
			return ErrNotFound
		case !state || *col == nil || **col != cur:
			return ErrInvalidState
		}
		n := next
		*col = &n
		return nil
	}
	switch slot {
	case SlotTransferReceipt, SlotReturnReceipt:
		t, ok := m.transfers[id]
		col, state := &t.ReceiptFileID, true
		if slot == SlotReturnReceipt {
			col, state = &t.ReturnReceiptFileID, t.Status == TransferReturned
		}
		if err := swap(ok, state, col); err != nil {
			return err
		}
		t.UpdatedAt = m.touch(t.UpdatedAt)
		m.transfers[id] = t
		if slot == SlotTransferReceipt {
			for cid, c := range m.collections {
				if c.IncomingTransferID == id && c.ReceiptFileID == cur {
					c.ReceiptFileID = next
					m.collections[cid] = c
				}
			}
		}
	case SlotPaymentEvidence:
		p, ok := m.payments[id]
		if err := swap(ok, true, &p.EvidenceFileID); err != nil {
			return err
		}
		p.UpdatedAt = m.touch(p.UpdatedAt)
		m.payments[id] = p
	case SlotRefundReceipt:
		r, ok := m.refunds[id]
		if err := swap(ok, r.Status == RefundStatusCompleted, &r.ReceiptFileID); err != nil {
			return err
		}
		r.UpdatedAt = m.touch(r.UpdatedAt)
		m.refunds[id] = r
	case SlotRequestEvidence:
		r, ok := m.requests[id]
		if err := swap(ok, r.Status.Pending(), &r.EvidenceFileID); err != nil {
			return err
		}
		r.UpdatedAt = m.touch(r.UpdatedAt)
		m.requests[id] = r
	default:
		return fmt.Errorf("payments: replace evidence: unknown slot %q", slot)
	}
	return nil
}
