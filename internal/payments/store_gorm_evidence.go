package payments

import (
	"context"
	"fmt"
	"uuid"

	"koro/internal/platform/db"
)

// evidenceUpdate is a slot's targeted UPDATE (fixed SQL: the file column
// alone, guarded by the file it holds and the slot's state) and its table.
type evidenceUpdate struct {
	table, sql string
}

// evidenceUpdates are the slots of ReplaceEvidence.
var evidenceUpdates = map[EvidenceSlot]evidenceUpdate{
	SlotTransferReceipt: {"incoming_bank_transfers",
		`UPDATE incoming_bank_transfers SET receipt_file_id = @next WHERE id = @id AND receipt_file_id = @cur`},
	SlotReturnReceipt: {"incoming_bank_transfers", `UPDATE incoming_bank_transfers SET return_receipt_file_id = @next
		WHERE id = @id AND return_receipt_file_id = @cur AND status = 'returned'`},
	SlotPaymentEvidence: {"payments", `UPDATE payments SET evidence_file_id = @next WHERE id = @id AND evidence_file_id = @cur`},
	SlotRefundReceipt: {"refunds",
		`UPDATE refunds SET receipt_file_id = @next WHERE id = @id AND receipt_file_id = @cur AND status = 'completed'`},
	SlotRequestEvidence: {"refund_requests", `UPDATE refund_requests SET evidence_file_id = @next
		WHERE id = @id AND evidence_file_id = @cur AND status IN ('open','in_review')`},
}

// ReplaceEvidence implements Store with a targeted UPDATE of the file
// column alone (06 H-56), guarded by the file it holds and the slot's
// state; the database's koro_evidence_file_guard refuses a file that was
// not rejected.
//
// cross-org: addressed by ID; the service authorised the record.
func (s gormStore) ReplaceEvidence(ctx context.Context, slot EvidenceSlot, id, cur, next uuid.UUID) error {
	u, ok := evidenceUpdates[slot]
	if !ok {
		return fmt.Errorf("payments: replace evidence: unknown slot %q", slot)
	}
	conn := db.Conn(ctx, s.orm)
	res := conn.Exec(u.sql, map[string]any{"id": id, "cur": cur, "next": next})
	if res.Error != nil {
		return fmt.Errorf("payments: replace %s: %w", slot, constraints.Translate(res.Error))
	}
	if res.RowsAffected == 1 {
		return nil
	}
	var n int64
	if err := conn.Table(u.table).Where("id = ?", id).Count(&n).Error; err != nil {
		return fmt.Errorf("payments: replace %s: %w", slot, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return ErrInvalidState
}
