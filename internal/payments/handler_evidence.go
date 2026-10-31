package payments

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/files"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
)

// replaceNote is the common part of the replace operations' descriptions.
const replaceNote = " Only when the file was rejected after its upload (found infected or not processable: its *_file_state " +
	"is rejected; 409 files.not_rejected otherwise). The new file is a completed upload of the same purpose (422 " +
	"otherwise; 409 files.rejected, files.not_clean or files.already_attached). Nothing else of the record changes; " +
	"the rejected file stays on record (audit operation replace_rejected_file with both file IDs). Idempotency-Key optional."

// registerEvidence adds the replace operations of the rejected receipts and
// evidence (BE-154b, T-397): the cell of the first upload, no new action.
func registerEvidence(api huma.API, h handler) {
	op.Register(api, "payments", huma.Operation{
		OperationID: "adminReplaceIncomingTransferReceipt", Method: http.MethodPost,
		Path: "/admin/incoming-transfers/{transfer_id}/replace-receipt", Summary: "Upload a transfer's bank receipt again",
		Description: "The people who record a transfer (adminCreateIncomingTransfer). receipt_file_id is a new transfer_receipt " +
			"upload; a transfer without a receipt is 409 payments.invalid_state. Its receivable collections take the new " +
			"receipt with it." + replaceNote,
	}, op.Meta{Action: string(authz.PaymentsTransferCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.replaceTransferReceipt)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminReplaceIncomingTransferReturnReceipt", Method: http.MethodPost,
		Path: "/admin/incoming-transfers/{transfer_id}/replace-return-receipt", Summary: "Upload a return's EFT receipt again",
		Description: "Requires re-authentication, as the return's receipt step (adminMarkIncomingTransferReturned). Only a " +
			"returned transfer (409 payments.invalid_state); receipt_file_id is a new refund_receipt upload. The return's " +
			"four eyes and bank reference do not change." + replaceNote,
	}, op.Meta{Action: string(authz.PaymentsTransferMarkReturned), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.replaceReturnReceipt)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminReplacePaymentEvidence", Method: http.MethodPost, Path: "/admin/payments/{payment_id}/replace-evidence",
		Summary: "Upload a direct payment's evidence again",
		Description: "The people who confirm a direct payment (adminConfirmDirectPayment). Only a payment with evidence (a model " +
			"C direct payment; 409 payments.invalid_state otherwise); evidence_file_id is a new transfer_receipt upload." +
			replaceNote,
	}, op.Meta{Action: string(authz.PaymentsTransferMatch), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.replacePaymentEvidence)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminReplaceRefundReceipt", Method: http.MethodPost, Path: "/admin/refunds/{refund_id}/replace-receipt",
		Summary: "Upload a completed refund's EFT receipt again",
		Description: "The people who complete a refund (adminCompleteRefund). Only a completed refund (409 " +
			"payments.invalid_state); receipt_file_id is a new refund_receipt upload. The refund, its bank reference, the " +
			"ledger and the order do not change." + replaceNote,
	}, op.Meta{Action: string(authz.PaymentsRefundComplete), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.replaceRefundReceipt)

	op.Register(api, "payments", huma.Operation{
		OperationID: "replaceRefundRequestEvidence", Method: http.MethodPost,
		Path: "/refund-requests/{refund_request_id}/replace-evidence", Summary: "Upload a refund request's evidence again",
		Description: "The buying company's admins (createOrderRefundRequest's cell, on the request; anyone who does not see " +
			"the request gets 404). Only while the request waits for finance (open or in_review) and holds evidence (409 " +
			"payments.invalid_state); evidence_file_id is a new refund_request_evidence upload of the buying company." +
			replaceNote,
	}, op.Meta{Action: string(authz.PaymentsRefundRequestCreate), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.replaceRequestEvidence)
}

// ---- the states of the files of a response (BE-154b)

// statesOf reads the states of the files fileIDs names in rows, in one
// files read, after the use case (never replayed).
func statesOf[T any](ctx context.Context, h handler, rows []T, fileIDs func(T) []*uuid.UUID) (map[uuid.UUID]files.State, error) {
	var ids []*uuid.UUID
	for _, r := range rows {
		ids = append(ids, fileIDs(r)...)
	}
	return h.svc.EvidenceStates(ctx, ids...)
}

func transferFiles(t IncomingTransfer) []*uuid.UUID {
	return []*uuid.UUID{t.ReceiptFileID, t.ReturnReceiptFileID}
}

func paymentFiles(p Payment) []*uuid.UUID { return []*uuid.UUID{p.EvidenceFileID} }

func collectionFiles(c ReceivableCollection) []*uuid.UUID { return []*uuid.UUID{&c.ReceiptFileID} }

// refundFiles returns the receipts of rs.
func refundFiles(rs ...Refund) []*uuid.UUID {
	out := make([]*uuid.UUID, len(rs))
	for i := range rs {
		out[i] = rs[i].ReceiptFileID
	}
	return out
}

func (h handler) transferOut(ctx context.Context, t IncomingTransfer) (*transferOutput, error) {
	st, err := statesOf(ctx, h, []IncomingTransfer{t}, transferFiles)
	if err != nil {
		return nil, err
	}
	return &transferOutput{Body: transferDTO(t, st)}, nil
}

func (h handler) refundOut(ctx context.Context, r Refund) (*refundOutput, error) {
	st, err := h.svc.EvidenceStates(ctx, refundFiles(r)...)
	if err != nil {
		return nil, err
	}
	return &refundOutput{Body: refundDTO(r, st)}, nil
}

func (h handler) collectionOut(ctx context.Context, c ReceivableCollection) (*collectionOutput, error) {
	st, err := statesOf(ctx, h, []ReceivableCollection{c}, collectionFiles)
	if err != nil {
		return nil, err
	}
	return &collectionOutput{Body: collectionDTO(c, st)}, nil
}

// ---- the replace operations

type replaceReceiptBody struct {
	ReceiptFileID string `json:"receipt_file_id" format:"uuid" doc:"The new receipt: an upload of the record's purpose"`
}

type replaceTransferReceiptInput struct {
	idempotency.OptionalHeader
	TransferPath
	Body replaceReceiptBody
}

func (h handler) replaceTransferReceipt(ctx context.Context, in *replaceTransferReceiptInput) (*transferOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.AdminReplaceTransferReceipt(ctx, a, httpx.ParseID(in.TransferID), httpx.ParseID(in.Body.ReceiptFileID))
	if err != nil {
		return nil, err
	}
	return h.transferOut(ctx, t)
}

type replaceReturnReceiptInput struct {
	idempotency.OptionalHeader
	TransferPath
	Body struct {
		ReceiptFileID string             `json:"receipt_file_id" format:"uuid" doc:"The new EFT receipt: a refund_receipt upload"`
		Reauth        *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) replaceReturnReceipt(ctx context.Context, in *replaceReturnReceiptInput) (*transferOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.AdminReplaceReturnReceipt(ctx, a, httpx.ParseID(in.TransferID), httpx.ParseID(in.Body.ReceiptFileID),
		proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return h.transferOut(ctx, t)
}

type replacePaymentEvidenceInput struct {
	idempotency.OptionalHeader
	PaymentPath
	Body struct {
		EvidenceFileID string `json:"evidence_file_id" format:"uuid" doc:"The new evidence: a transfer_receipt upload"`
	}
}

type paymentOutput struct{ Body PaymentDTO }

func (h handler) replacePaymentEvidence(ctx context.Context, in *replacePaymentEvidenceInput) (*paymentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminReplacePaymentEvidence(ctx, a, httpx.ParseID(in.PaymentID), httpx.ParseID(in.Body.EvidenceFileID))
	if err != nil {
		return nil, err
	}
	st, err := statesOf(ctx, h, []Payment{p}, paymentFiles)
	if err != nil {
		return nil, err
	}
	return &paymentOutput{Body: paymentRowDTO(p, st)}, nil
}

type replaceRefundReceiptInput struct {
	idempotency.OptionalHeader
	RefundPath
	Body replaceReceiptBody
}

func (h handler) replaceRefundReceipt(ctx context.Context, in *replaceRefundReceiptInput) (*refundOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.AdminReplaceRefundReceipt(ctx, a, httpx.ParseID(in.RefundID), httpx.ParseID(in.Body.ReceiptFileID))
	if err != nil {
		return nil, err
	}
	return h.refundOut(ctx, r)
}

type replaceRequestEvidenceInput struct {
	idempotency.OptionalHeader
	RefundRequestPath
	Body struct {
		EvidenceFileID string `json:"evidence_file_id" format:"uuid" doc:"The new evidence: a refund_request_evidence upload of the buying company"`
	}
}

func (h handler) replaceRequestEvidence(ctx context.Context, in *replaceRequestEvidenceInput) (*refundRequestOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ReplaceRefundRequestEvidence(ctx, a, httpx.ParseID(in.RefundRequestID), httpx.ParseID(in.Body.EvidenceFileID))
	if err != nil {
		return nil, err
	}
	return &refundRequestOutput{Body: refundRequestDTO(v)}, nil
}
