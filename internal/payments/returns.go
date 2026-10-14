package payments

import (
	"context"
	"log/slog"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/idempotency"
)

// The return of a transfer to its sender (06 H-18, H-56, T-60, T-228): a
// money exit, so four eyes in three steps, all under
// payments.transfer.mark_returned (fin·R):
//
//	1. request  (adminRequestIncomingTransferReturn) unmatched/ignored → return_requested:
//	            the reason and the target IBAN (the sender's by default);
//	2. decision (adminApproveIncomingTransferReturn) return_requested → return_approved,
//	            by another finance admin (403 authz.four_eyes_violation), or rejected →
//	            unmatched again (the request columns are cleared; the audit log keeps them);
//	            the target IBAN is frozen once approved (incoming_bank_transfers_return_frozen);
//	3. receipt  (adminMarkIncomingTransferReturned) return_approved → returned: the EFT's
//	            bank reference and receipt (any finance admin, as a payout's mark_paid).
//
// The ledger posts nothing: an unmatched transfer was never posted.

// Limits of a return (000105 CHECKs).
const maxReturnReason = 500

// returnGate is the "403 before reading" check of a return step.
func returnGate(ctx context.Context, a authz.Actor) error {
	return authz.Can(ctx, a, authz.PaymentsTransferMarkReturned, unreadTransfer)
}

// RequestReturn implements Service (step 1): the gate (403) → body (422) →
// the transfer (404) → the target IBAN (the body's, else the sender's; 422
// without one) → the re-authentication proof (403) → in one transaction:
// unmatched or ignored only (409) → return_requested → audit.
func (s *service) RequestReturn(ctx context.Context, a authz.Actor, id uuid.UUID, in ReturnRequest, proof identity.ReauthProof) (IncomingTransfer, error) {
	if err := returnGate(ctx, a); err != nil {
		return IncomingTransfer{}, err
	}
	why, err := cleanText("body.return_reason", in.Reason, 1, maxReturnReason)
	if err != nil {
		return IncomingTransfer{}, err
	}
	var target string
	if in.TargetIBAN != "" {
		if target, err = cleanIBAN("body.target_iban", in.TargetIBAN); err != nil {
			return IncomingTransfer{}, err
		}
	}
	t, err := s.d.Store.GetTransfer(ctx, id, false)
	if err != nil {
		return IncomingTransfer{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsTransferMarkReturned, transferResource(t, false)); err != nil {
		return IncomingTransfer{}, err
	}
	if target == "" {
		if t.SenderIBAN == nil {
			return IncomingTransfer{}, fieldErr("body.target_iban", "is required: the sender's IBAN is unknown")
		}
		target = *t.SenderIBAN
	}
	kind, err := s.reauth(ctx, a, proof)
	if err != nil {
		return IncomingTransfer{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op, Reason, Target string
		Transfer           uuid.UUID
	}{"request_return", why, target, id})
	if err != nil {
		return IncomingTransfer{}, err
	}
	return s.transitionTransfer(ctx, hash, id, func(ctx context.Context, cur IncomingTransfer) (IncomingTransfer, error) {
		if cur.Status != TransferUnmatched && cur.Status != TransferIgnored {
			return IncomingTransfer{}, stateErr(cur)
		}
		now, by := s.now(), a.UserID
		next := cur
		next.Status, next.IgnoreReason = TransferReturnRequested, nil
		next.ReturnRequestedBy, next.ReturnRequestedAt, next.ReturnReason, next.ReturnTargetIBAN = &by, &now, &why, &target
		stored, err := s.d.Store.UpdateTransfer(ctx, next, cur.Status)
		if err != nil {
			return IncomingTransfer{}, err
		}
		meta := map[string]any{"step": "request", "from": string(cur.Status), audit.MetaReauth: kind,
			"target_is_sender": cur.SenderIBAN != nil && *cur.SenderIBAN == target}
		return stored, s.d.Audit.Log(ctx, transferEvent(a, authz.PaymentsTransferMarkReturned, stored, why, meta))
	})
}

// DecideReturn implements Service (step 2, the second eye): the gate (403)
// → the decision (422) → the transfer (404) → the eyes (403: never the
// requester) → the re-authentication proof (403) → in one transaction:
// return_requested only (409) → return_approved, or unmatched again on a
// rejection → audit.
func (s *service) DecideReturn(ctx context.Context, a authz.Actor, id uuid.UUID, d Decision, proof identity.ReauthProof) (IncomingTransfer, error) {
	if err := returnGate(ctx, a); err != nil {
		return IncomingTransfer{}, err
	}
	if !d.Valid() {
		return IncomingTransfer{}, fieldErr("body.decision", "must be approved or rejected")
	}
	t, err := s.d.Store.GetTransfer(ctx, id, false)
	if err != nil {
		return IncomingTransfer{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsTransferMarkReturned, transferResource(t, true)); err != nil {
		return IncomingTransfer{}, err
	}
	if t.Status != TransferReturnRequested {
		return IncomingTransfer{}, stateErr(t)
	}
	kind, err := s.reauth(ctx, a, proof)
	if err != nil {
		return IncomingTransfer{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Transfer uuid.UUID
		Decision Decision
	}{"decide_return", id, d})
	if err != nil {
		return IncomingTransfer{}, err
	}
	return s.transitionTransfer(ctx, hash, id, func(ctx context.Context, cur IncomingTransfer) (IncomingTransfer, error) {
		if cur.Status != TransferReturnRequested {
			return IncomingTransfer{}, stateErr(cur)
		}
		if err := authz.Can(ctx, a, authz.PaymentsTransferMarkReturned, transferResource(cur, true)); err != nil {
			return IncomingTransfer{}, err
		}
		now, by := s.now(), a.UserID
		next := cur
		if d == DecisionApproved {
			next.Status, next.ReturnApprovedBy, next.ReturnApprovedAt = TransferReturnApproved, &by, &now
		} else {
			next.Status = TransferUnmatched
			next.clearReturn()
		}
		stored, err := s.d.Store.UpdateTransfer(ctx, next, TransferReturnRequested)
		if err != nil {
			return IncomingTransfer{}, err
		}
		meta := map[string]any{"step": "decision", "decision": string(d), "requested_by": cur.ReturnRequestedBy.String(),
			audit.MetaReauth: kind}
		return stored, s.d.Audit.Log(ctx, transferEvent(a, authz.PaymentsTransferMarkReturned, stored, "", meta))
	})
}

// MarkReturned implements Service (step 3): the gate (403) → body (422; the
// receipt: a refund_receipt upload) → the transfer (404) → the
// re-authentication proof (403) → in one transaction: return_approved only
// (409) → returned with the bank reference and the receipt (attached) →
// audit.
func (s *service) MarkReturned(ctx context.Context, a authz.Actor, id uuid.UUID, in ReturnReceipt, proof identity.ReauthProof) (IncomingTransfer, error) {
	if err := returnGate(ctx, a); err != nil {
		return IncomingTransfer{}, err
	}
	ref, err := cleanText("body.bank_reference", in.BankReference, 1, maxBankReference)
	if err != nil {
		return IncomingTransfer{}, err
	}
	if err := s.checkFile(ctx, "body.receipt_file_id", in.ReceiptFileID, files.PurposeRefundReceipt); err != nil {
		return IncomingTransfer{}, err
	}
	t, err := s.d.Store.GetTransfer(ctx, id, false)
	if err != nil {
		return IncomingTransfer{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsTransferMarkReturned, transferResource(t, false)); err != nil {
		return IncomingTransfer{}, err
	}
	kind, err := s.reauth(ctx, a, proof)
	if err != nil {
		return IncomingTransfer{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op, Ref           string
		Transfer, Receipt uuid.UUID
	}{"mark_returned", ref, id, in.ReceiptFileID})
	if err != nil {
		return IncomingTransfer{}, err
	}
	return s.transitionTransfer(ctx, hash, id, func(ctx context.Context, cur IncomingTransfer) (IncomingTransfer, error) {
		if cur.Status != TransferReturnApproved {
			return IncomingTransfer{}, stateErr(cur)
		}
		now, receipt := s.now(), in.ReceiptFileID
		next := cur
		next.Status, next.ReturnedAt, next.ReturnBankReference, next.ReturnReceiptFileID = TransferReturned, &now, &ref, &receipt
		stored, err := s.d.Store.UpdateTransfer(ctx, next, TransferReturnApproved)
		if err != nil {
			return IncomingTransfer{}, err
		}
		if err := s.attach(ctx, a, receipt, stored.ID, files.PurposeRefundReceipt); err != nil {
			return IncomingTransfer{}, err
		}
		s.logInfo(ctx, "incoming transfer returned", slog.String("transfer_id", stored.ID.String()), slog.Int64("amount", stored.Amount))
		return stored, s.d.Audit.Log(ctx, transferEvent(a, authz.PaymentsTransferMarkReturned, stored, "",
			map[string]any{"step": "receipt", "receipt_file_id": receipt.String(), audit.MetaReauth: kind}))
	})
}

// transitionTransfer runs apply on the locked transfer in one transaction
// with the Idempotency-Key, then reads the result back.
func (s *service) transitionTransfer(ctx context.Context, hash []byte, id uuid.UUID,
	apply func(ctx context.Context, cur IncomingTransfer) (IncomingTransfer, error)) (IncomingTransfer, error) {
	_, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.GetTransfer(ctx, id, true)
		if err != nil {
			return uuid.Nil(), err
		}
		stored, err := apply(ctx, cur)
		return stored.ID, err
	})
	if err != nil {
		return IncomingTransfer{}, err
	}
	return s.d.Store.GetTransfer(ctx, id, false)
}
