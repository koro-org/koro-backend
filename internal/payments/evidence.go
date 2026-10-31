package payments

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// Rejected evidence of the payments records (T-393, T-397; BE-154b). A
// receipt or an evidence file that the scan rejects after its upload stays
// referenced by its record (a financial record, BE-151; the old file row
// stays attached as its history): the record shows the state of the file
// (`*_file_state`), the people who upload it are told, and the role that
// uploaded it attaches a new file in its place with the cell of the first
// upload (no new action; 000163 koro_evidence_file_guard lets only a
// rejected file go). Nothing else of the record changes: no money, no
// status, no four-eyes column.

// EvidenceSlot names one evidence column of payments: what
// Store.ReplaceEvidence writes.
type EvidenceSlot string

// Evidence slots.
const (
	// SlotTransferReceipt: incoming_bank_transfers.receipt_file_id (any
	// status; its receivable collections follow, ON UPDATE CASCADE).
	SlotTransferReceipt EvidenceSlot = "transfer_receipt"
	// SlotReturnReceipt: incoming_bank_transfers.return_receipt_file_id
	// of a returned transfer.
	SlotReturnReceipt EvidenceSlot = "transfer_return_receipt"
	// SlotPaymentEvidence: payments.evidence_file_id (a model C direct
	// payment's transfer receipt).
	SlotPaymentEvidence EvidenceSlot = "payment_evidence"
	// SlotRefundReceipt: refunds.receipt_file_id of a completed refund.
	SlotRefundReceipt EvidenceSlot = "refund_receipt"
	// SlotRequestEvidence: refund_requests.evidence_file_id of a request
	// that waits for finance (open or in_review).
	SlotRequestEvidence EvidenceSlot = "refund_request_evidence"
)

// EvidenceService is the replacement of a rejected receipt or evidence
// file (BE-154b). Each use case is the cell of the first upload.
type EvidenceService interface {
	// AdminReplaceTransferReceipt is adminReplaceIncomingTransferReceipt
	// (payments.transfer.create).
	AdminReplaceTransferReceipt(ctx context.Context, a authz.Actor, transferID, fileID uuid.UUID) (IncomingTransfer, error)
	// AdminReplaceReturnReceipt is adminReplaceIncomingTransferReturnReceipt
	// (payments.transfer.mark_returned, R).
	AdminReplaceReturnReceipt(ctx context.Context, a authz.Actor, transferID, fileID uuid.UUID, proof identity.ReauthProof) (IncomingTransfer, error)
	// AdminReplacePaymentEvidence is adminReplacePaymentEvidence
	// (payments.transfer.match: the direct payment's confirmation).
	AdminReplacePaymentEvidence(ctx context.Context, a authz.Actor, paymentID, fileID uuid.UUID) (Payment, error)
	// AdminReplaceRefundReceipt is adminReplaceRefundReceipt
	// (payments.refund.complete).
	AdminReplaceRefundReceipt(ctx context.Context, a authz.Actor, refundID, fileID uuid.UUID) (Refund, error)
	// ReplaceRefundRequestEvidence is replaceRefundRequestEvidence
	// (payments.refund_request.create on the request).
	ReplaceRefundRequestEvidence(ctx context.Context, a authz.Actor, requestID, fileID uuid.UUID) (RefundRequestView, error)
	// EvidenceStates returns the states of the files of ids that exist
	// (one files read): the `*_file_state` members of the responses, read
	// after the use case (never replayed).
	EvidenceStates(ctx context.Context, ids ...*uuid.UUID) (map[uuid.UUID]files.State, error)
}

// replacement is one replace use case: the slot written, the purpose of
// the new file and the body field that names it.
type replacement struct {
	slot    EvidenceSlot
	purpose files.Purpose
	loc     string
}

// Replace use cases.
var (
	replaceTransferReceipt = replacement{SlotTransferReceipt, files.PurposeTransferReceipt, "body.receipt_file_id"}
	replaceReturnReceipt   = replacement{SlotReturnReceipt, files.PurposeRefundReceipt, "body.receipt_file_id"}
	replacePaymentEvidence = replacement{SlotPaymentEvidence, files.PurposeTransferReceipt, "body.evidence_file_id"}
	replaceRefundReceipt   = replacement{SlotRefundReceipt, files.PurposeRefundReceipt, "body.receipt_file_id"}
	replaceRequestEvidence = replacement{SlotRequestEvidence, files.PurposeRefundRequestEvidence, "body.evidence_file_id"}
)

// required is the 422 of a missing new file.
func (rp replacement) required(fileID uuid.UUID) error {
	if fileID == uuid.Nil() {
		return problem.Field(rp.loc, "is required: the new "+string(rp.purpose)+" upload")
	}
	return nil
}

// replace is the transaction of a replacement, with the Idempotency-Key:
// lock returns the file the locked row holds now (ErrInvalidState when the
// row is not in the slot's state) → files.Replace (409 files.not_rejected
// unless that file was rejected; Attach's 409s for the new one) → the
// targeted UPDATE of the slot → record writes the audit record with the
// replacement's metadata (the old and the new file, why the old one went).
func (s *service) replace(ctx context.Context, a authz.Actor, rp replacement, id, fileID uuid.UUID,
	lock func(ctx context.Context) (uuid.UUID, error), record func(ctx context.Context, meta map[string]any) error) error {
	if s.d.Files == nil {
		return ErrUnavailable
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Slot     EvidenceSlot
		ID, File uuid.UUID
	}{"replace_evidence", rp.slot, id, fileID})
	if err != nil {
		return err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := lock(ctx)
		if err != nil {
			return uuid.Nil(), err
		}
		meta, err := files.Replace(ctx, s.d.Files, a, cur, fileID, files.Owner{Module: moduleName, EntityID: id, Purpose: rp.purpose})
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Store.ReplaceEvidence(ctx, rp.slot, id, cur, fileID); err != nil {
			return uuid.Nil(), err
		}
		return id, record(ctx, meta)
	})
	return err
}

// heldFile returns the file a row holds in a slot, or ErrInvalidState
// without one.
func heldFile(id *uuid.UUID) (uuid.UUID, error) {
	if id == nil {
		return uuid.Nil(), ErrInvalidState
	}
	return *id, nil
}

// AdminReplaceTransferReceipt implements EvidenceService: authz (fin; 403
// before reading) → the new receipt (a transfer_receipt upload; 422) → in
// one transaction: the transfer locked (404), with a receipt (409
// payments.invalid_state) → the replacement (409 files.not_rejected …) →
// audit. The collections of the transfer take the new receipt with it
// (receivable_collections_source_fkey, ON UPDATE CASCADE).
func (s *service) AdminReplaceTransferReceipt(ctx context.Context, a authz.Actor, transferID, fileID uuid.UUID) (IncomingTransfer, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferCreate, adminSurface); err != nil {
		return IncomingTransfer{}, err
	}
	rp := replaceTransferReceipt
	if err := rp.required(fileID); err != nil {
		return IncomingTransfer{}, err
	}
	if err := s.checkFile(ctx, rp.loc, fileID, rp.purpose); err != nil {
		return IncomingTransfer{}, err
	}
	var cur IncomingTransfer
	err := s.replace(ctx, a, rp, transferID, fileID, func(ctx context.Context) (uuid.UUID, error) {
		var err error
		if cur, err = s.d.Store.GetTransfer(ctx, transferID, true); err != nil {
			return uuid.Nil(), err
		}
		return heldFile(cur.ReceiptFileID)
	}, func(ctx context.Context, meta map[string]any) error {
		return s.d.Audit.Log(ctx, transferEvent(a, authz.PaymentsTransferCreate, cur, "", meta))
	})
	if err != nil {
		return IncomingTransfer{}, err
	}
	return s.d.Store.GetTransfer(ctx, transferID, false)
}

// AdminReplaceReturnReceipt implements EvidenceService (R, the people of
// the return's receipt step): the gate (403) → the new receipt (a
// refund_receipt upload; 422) → the transfer (404) → the
// re-authentication proof (403) → in one transaction: returned with a
// receipt (409 payments.invalid_state) → the replacement → audit. The
// return's four eyes and bank reference do not change.
func (s *service) AdminReplaceReturnReceipt(ctx context.Context, a authz.Actor, transferID, fileID uuid.UUID, proof identity.ReauthProof) (IncomingTransfer, error) {
	if err := returnGate(ctx, a); err != nil {
		return IncomingTransfer{}, err
	}
	rp := replaceReturnReceipt
	if err := rp.required(fileID); err != nil {
		return IncomingTransfer{}, err
	}
	if err := s.checkFile(ctx, rp.loc, fileID, rp.purpose); err != nil {
		return IncomingTransfer{}, err
	}
	t, err := s.d.Store.GetTransfer(ctx, transferID, false)
	if err != nil {
		return IncomingTransfer{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsTransferMarkReturned, transferResource(t, false)); err != nil {
		return IncomingTransfer{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.PaymentsTransferMarkReturned, authz.TypeIncomingTransfer, &t.ID, nil,
		map[string]any{"step": "replace_receipt"}))
	if err != nil {
		return IncomingTransfer{}, err
	}
	var cur IncomingTransfer
	err = s.replace(ctx, a, rp, transferID, fileID, func(ctx context.Context) (uuid.UUID, error) {
		var err error
		if cur, err = s.d.Store.GetTransfer(ctx, transferID, true); err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != TransferReturned {
			return uuid.Nil(), ErrInvalidState
		}
		return heldFile(cur.ReturnReceiptFileID)
	}, func(ctx context.Context, meta map[string]any) error {
		meta["step"], meta[audit.MetaReauth] = "replace_receipt", kind
		return s.d.Audit.Log(ctx, transferEvent(a, authz.PaymentsTransferMarkReturned, cur, "", meta))
	})
	if err != nil {
		return IncomingTransfer{}, err
	}
	return s.d.Store.GetTransfer(ctx, transferID, false)
}

// AdminReplacePaymentEvidence implements EvidenceService: authz (fin; 403
// before reading) → the new evidence (a transfer_receipt upload; 422) → in
// one transaction: the payment locked (404), with evidence (a direct
// payment; 409 payments.invalid_state otherwise) → the replacement → audit.
func (s *service) AdminReplacePaymentEvidence(ctx context.Context, a authz.Actor, paymentID, fileID uuid.UUID) (Payment, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferMatch, adminSurface); err != nil {
		return Payment{}, err
	}
	rp := replacePaymentEvidence
	if err := rp.required(fileID); err != nil {
		return Payment{}, err
	}
	if err := s.checkFile(ctx, rp.loc, fileID, rp.purpose); err != nil {
		return Payment{}, err
	}
	var cur Payment
	err := s.replace(ctx, a, rp, paymentID, fileID, func(ctx context.Context) (uuid.UUID, error) {
		var err error
		if cur, err = s.d.Store.GetPayment(ctx, paymentID, true); err != nil {
			return uuid.Nil(), err
		}
		return heldFile(cur.EvidenceFileID)
	}, func(ctx context.Context, meta map[string]any) error {
		id := cur.ID
		meta["order_id"], meta["provider"] = cur.OrderID.String(), string(cur.Provider)
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.PaymentsTransferMatch), Outcome: audit.Success, ResourceType: authz.TypePayment, ResourceID: &id,
			Metadata: meta,
		}.By(a))
	})
	if err != nil {
		return Payment{}, err
	}
	return s.d.Store.GetPayment(ctx, paymentID, false)
}

// AdminReplaceRefundReceipt implements EvidenceService: authz (fin; 403
// before reading) → the new receipt (a refund_receipt upload; 422) → in
// one transaction: the refund locked (404), completed with a receipt (409
// payments.invalid_state) → the replacement → audit. The refund, its bank
// reference, the ledger and the order do not change.
func (s *service) AdminReplaceRefundReceipt(ctx context.Context, a authz.Actor, refundID, fileID uuid.UUID) (Refund, error) {
	if err := authz.Can(ctx, a, authz.PaymentsRefundComplete, adminSurface); err != nil {
		return Refund{}, err
	}
	rp := replaceRefundReceipt
	if err := rp.required(fileID); err != nil {
		return Refund{}, err
	}
	if err := s.checkFile(ctx, rp.loc, fileID, rp.purpose); err != nil {
		return Refund{}, err
	}
	var cur Refund
	err := s.replace(ctx, a, rp, refundID, fileID, func(ctx context.Context) (uuid.UUID, error) {
		var err error
		if cur, err = s.d.Store.GetRefund(ctx, refundID, true); err != nil {
			return uuid.Nil(), err
		}
		if cur.Status != RefundStatusCompleted {
			return uuid.Nil(), ErrInvalidState
		}
		return heldFile(cur.ReceiptFileID)
	}, func(ctx context.Context, meta map[string]any) error {
		return s.d.Audit.Log(ctx, refundAudit(a, authz.PaymentsRefundComplete, cur, "", meta))
	})
	if err != nil {
		return Refund{}, err
	}
	return s.d.Store.GetRefund(ctx, refundID, false)
}

// ReplaceRefundRequestEvidence implements EvidenceService: the request
// (404) → authz on it (the buying company's admins, the cell of
// createOrderRefundRequest; 404 for whoever does not see it) → the new
// evidence (a completed refund_request_evidence upload of the buying
// company; 422) → in one transaction: the request locked, waiting for
// finance with evidence (409 payments.invalid_state) → the replacement →
// audit.
func (s *service) ReplaceRefundRequestEvidence(ctx context.Context, a authz.Actor, requestID, fileID uuid.UUID) (RefundRequestView, error) {
	r, err := s.d.Store.GetRefundRequest(ctx, requestID, false)
	if err != nil {
		return RefundRequestView{}, err
	}
	if err := authz.Can(ctx, a, authz.PaymentsRefundRequestCreate, requestResource(r)); err != nil {
		return RefundRequestView{}, err
	}
	rp := replaceRequestEvidence
	if err := rp.required(fileID); err != nil {
		return RefundRequestView{}, err
	}
	if err := s.checkRequestEvidence(ctx, r.OrgID, fileID); err != nil {
		return RefundRequestView{}, err
	}
	var cur RefundRequest
	err = s.replace(ctx, a, rp, requestID, fileID, func(ctx context.Context) (uuid.UUID, error) {
		var err error
		if cur, err = s.d.Store.GetRefundRequest(ctx, requestID, true); err != nil {
			return uuid.Nil(), err
		}
		if !cur.Status.Pending() {
			return uuid.Nil(), ErrInvalidState
		}
		return heldFile(cur.EvidenceFileID)
	}, func(ctx context.Context, meta map[string]any) error {
		return s.d.Audit.Log(ctx, requestAudit(a, authz.PaymentsRefundRequestCreate, cur, "", meta))
	})
	if err != nil {
		return RefundRequestView{}, err
	}
	return s.view(ctx, requestID, AudienceBuyer)
}

// EvidenceStates implements EvidenceService (no files port: no states).
func (s *service) EvidenceStates(ctx context.Context, ids ...*uuid.UUID) (map[uuid.UUID]files.State, error) {
	if s.d.Files == nil {
		return map[uuid.UUID]files.State{}, nil
	}
	return s.d.Files.States(ctx, files.IDs(ids...))
}

// ---- file.rejected

// NotifyRejectedEvidenceTaskName is the file.rejected subscription of the
// payments records' receipts and evidence (BE-154b, T-397).
const NotifyRejectedEvidenceTaskName = "payments.notify_rejected_evidence"

// Templates of the rejected evidence notices (in-app; transactional,
// preference orders_payments).
const (
	// tmplTransferReceiptRejected: finance admins; a recorded transfer's or
	// a direct payment's receipt.
	tmplTransferReceiptRejected = "payments.transfer_receipt_rejected"
	// tmplRefundReceiptRejected: finance admins; a completed refund's or a
	// returned transfer's receipt.
	tmplRefundReceiptRejected = "payments.refund_receipt_rejected"
	// tmplRequestEvidenceRejected: the buying company's admins and finance
	// admins; the evidence of a request that waits for finance.
	tmplRequestEvidenceRejected = "payments.refund_request_evidence_rejected"
)

// notifyRejectedEvidence tells the people who upload a payments record's
// rejected receipt or evidence again (the record keeps the reference): the
// finance admins for a transfer, a direct payment, a refund and a return;
// the buying company's admins and the finance admins for the evidence of a
// refund request that still waits for finance (a decided request is
// silent). A record that holds another file by now (replaced) is a no-op;
// the dedupe keys (the file and the recipient) make a repeated delivery one
// notice. The uploader hears from files as before.
func (s *service) notifyRejectedEvidence(ctx context.Context, r files.Rejection) error {
	key := r.FileID.String()
	finance := func(tmpl string, link notify.Link, ids map[string]any) error {
		fins, err := s.financeAdmins(ctx)
		if err != nil {
			return err
		}
		return s.notifyInApp(ctx, tmpl, key, fins, link, ids, nil)
	}
	switch r.Purpose {
	case files.PurposeTransferReceipt:
		t, err := s.d.Store.GetTransfer(ctx, r.EntityID, false)
		switch {
		case err == nil && holds(t.ReceiptFileID, r.FileID):
			return finance(tmplTransferReceiptRejected, notify.AdminIncomingTransferLink(t.ID),
				map[string]any{"incoming_transfer_id": t.ID.String()})
		case err != nil && !isNotFound(err):
			return err
		}
		p, err := s.d.Store.GetPayment(ctx, r.EntityID, false)
		switch {
		case isNotFound(err):
			return nil
		case err != nil:
			return err
		case holds(p.EvidenceFileID, r.FileID):
			return finance(tmplTransferReceiptRejected, notify.AdminPaymentLink(p.ID),
				map[string]any{"payment_id": p.ID.String(), "order_id": p.OrderID.String()})
		}
	case files.PurposeRefundReceipt:
		ref, err := s.d.Store.GetRefund(ctx, r.EntityID, false)
		switch {
		case err == nil && holds(ref.ReceiptFileID, r.FileID):
			return finance(tmplRefundReceiptRejected, notify.AdminRefundLink(ref.ID), map[string]any{"refund_id": ref.ID.String()})
		case err != nil && !isNotFound(err):
			return err
		}
		t, err := s.d.Store.GetTransfer(ctx, r.EntityID, false)
		switch {
		case isNotFound(err):
			return nil
		case err != nil:
			return err
		case holds(t.ReturnReceiptFileID, r.FileID):
			return finance(tmplRefundReceiptRejected, notify.AdminIncomingTransferLink(t.ID),
				map[string]any{"incoming_transfer_id": t.ID.String()})
		}
	case files.PurposeRefundRequestEvidence:
		req, err := s.d.Store.GetRefundRequest(ctx, r.EntityID, false)
		switch {
		case isNotFound(err):
			return nil
		case err != nil:
			return err
		case !holds(req.EvidenceFileID, r.FileID) || !req.Status.Pending():
			return nil
		}
		if s.d.Orgs == nil {
			return errors.New("payments: the org directory is not wired")
		}
		buyers, err := s.membersWith(ctx, req.OrgID, authz.MemberCompanyAdmin, authz.MemberCompanyOwner)
		if err != nil {
			return err
		}
		ids := map[string]any{"refund_request_id": req.ID.String(), "order_id": req.OrderID.String()}
		return errors.Join(
			finance(tmplRequestEvidenceRejected, notify.AdminRefundRequestLink(req.ID), ids),
			s.notifyInApp(ctx, tmplRequestEvidenceRejected, key, buyers, notify.OrgRefundRequestLink(req.OrgID, req.ID), ids, nil))
	}
	return nil
}

// holds reports whether a record's file column holds file.
func holds(col *uuid.UUID, file uuid.UUID) bool { return col != nil && *col == file }
