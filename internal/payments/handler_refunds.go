package payments

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// registerRefunds adds the payment and refund operations of BE-043 (04
// §2.16; adminListRefundsDue is the queue of the money orders says waits
// for a refund, payments.refund.list).
func registerRefunds(api huma.API, h handler) {
	op.Register(api, "payments", huma.Operation{
		OperationID: "adminListPayments", Method: http.MethodGet, Path: "/admin/payments",
		Summary:     "List payments",
		Description: "Every captured payment, newest first. A bank transfer's amount is the whole transfer (an overpayment included).",
	}, op.Meta{Action: string(authz.PaymentsPaymentList), Surface: op.SurfaceAdmin}, h.listPayments)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminGetPayment", Method: http.MethodGet, Path: "/admin/payments/{payment_id}",
		Summary:     "Get a payment with its splits and refunds",
		Description: "The splits of the order it paid (seller and Koro shares per line) and the refunds drawn from it.",
	}, op.Meta{Action: string(authz.PaymentsPaymentRead), Surface: op.SurfaceAdmin}, h.getPayment)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminListRefunds", Method: http.MethodGet, Path: "/admin/refunds",
		Summary:     "List refunds",
		Description: "Newest first; filter by status, order or payment.",
	}, op.Meta{Action: string(authz.PaymentsRefundList), Surface: op.SurfaceAdmin}, h.listRefunds)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminListRefundsDue", Method: http.MethodGet, Path: "/admin/refunds/due",
		Summary: "List the money waiting for a refund",
		Description: "The cancelled lines of paid orders (offering cancelled, dispute resolved with a refund) and the orders " +
			"cancelled or expired with money paid, oldest first, minus what open refunds already cover; each with the " +
			"order's payments that can fund it. A finance or platform admin opens the refund (adminCreateRefund). At most " +
			"limit rows, no cursor.",
	}, op.Meta{Action: string(authz.PaymentsRefundList), Surface: op.SurfaceAdmin}, h.listRefundsDue)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminCreateRefund", Method: http.MethodPost, Path: "/admin/refunds",
		Summary: "Request a refund", DefaultStatus: http.StatusCreated,
		Description: "The refund waits for another finance admin's approval (requested; four eyes). It draws from one " +
			"captured bank-transfer payment: an overpayment (no order_line_id), the money of an order cancelled or expired " +
			"before it was paid in full (no order_line_id) or a line of a paid order (order_line_id required). Requested " +
			"and approved refunds count against every limit (422 orders.refund_exceeds_paid). A line in a payout waiting " +
			"for approval or for its transfer is 409 payments.line_in_payout; a line already paid out is refunded and its " +
			"seller part claimed back from the payee.",
	}, op.Meta{Action: string(authz.PaymentsRefundCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyRequired}, h.createRefund)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminApproveRefund", Method: http.MethodPost, Path: "/admin/refunds/{refund_id}/approve",
		Summary: "Approve a refund",
		Description: "Requires re-authentication. The requester never approves (403 authz.four_eyes_violation); only a " +
			"requested refund (409 payments.invalid_state). The approval covers the target IBAN.",
	}, op.Meta{Action: string(authz.PaymentsRefundApprove), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.approveRefund)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminRejectRefund", Method: http.MethodPost, Path: "/admin/refunds/{refund_id}/reject",
		Summary:     "Reject a refund request",
		Description: "Only a requested refund (409 payments.invalid_state); the requester may withdraw it this way. The note is audited.",
	}, op.Meta{Action: string(authz.PaymentsRefundReject), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.rejectRefund)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminCancelRefund", Method: http.MethodPost, Path: "/admin/refunds/{refund_id}/cancel",
		Summary:     "Cancel an approved refund before completion",
		Description: "Requires another finance admin and re-authentication. Only approved refunds; no ledger money moves.",
	}, op.Meta{Action: string(authz.PaymentsRefundCancel), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.cancelRefund)
	op.Register(api, "payments", huma.Operation{
		OperationID: "adminMarkRefundFailed", Method: http.MethodPost, Path: "/admin/refunds/{refund_id}/mark-failed",
		Summary:     "Record a failed refund before completion",
		Description: "Requires another finance admin and re-authentication. Only approved refunds; completed refunds cannot be reversed here.",
	}, op.Meta{Action: string(authz.PaymentsRefundMarkFailed), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.markRefundFailed)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminCompleteRefund", Method: http.MethodPost, Path: "/admin/refunds/{refund_id}/complete",
		Summary: "Record the EFT of an approved refund",
		Description: "Only an approved refund (409 payments.invalid_state); the receipt (a refund_receipt upload) and the " +
			"bank reference are required (422). One transaction: the refund completed, the payment's refunded amount, the " +
			"order (orders.ApplyRefund; not for an overpayment), the ledger (refund_completed) and payment.refunded; any " +
			"failure leaves the refund approved. Models A2 and B have no ledger template yet (422 ledger.model_not_enabled).",
	}, op.Meta{Action: string(authz.PaymentsRefundComplete), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.completeRefund)
}

// ---- payments

// PaymentPath is the payment_id path parameter.
type PaymentPath struct {
	PaymentID string `path:"payment_id" doc:"Payment ID (an unknown or malformed ID is 404)"`
}

// PaymentDTO is 04 §2.16 PaymentDTO.
type PaymentDTO struct {
	PaymentID          string     `json:"payment_id" format:"uuid"`
	OrderID            string     `json:"order_id" format:"uuid"`
	OrgID              string     `json:"org_id" format:"uuid" doc:"The buying org"`
	Provider           string     `json:"provider" enum:"bank_transfer,iyzico,direct_confirmed,manual"`
	IncomingTransferID string     `json:"incoming_transfer_id,omitempty" format:"uuid"`
	Amount             int64      `json:"amount" doc:"Kuruş; a bank transfer's whole amount (an overpayment included)"`
	Currency           string     `json:"currency" enum:"TRY"`
	Status             string     `json:"status" enum:"initiated,captured,failed,partially_refunded,refunded,chargeback"`
	RefundedAmount     int64      `json:"refunded_amount" doc:"Kuruş of completed refunds"`
	CapturedAt         *time.Time `json:"captured_at,omitempty"`
	CapturedBy         string     `json:"captured_by,omitempty" format:"uuid" doc:"The finance admin who captured it by hand"`
	EvidenceFileID     string     `json:"evidence_file_id,omitempty" format:"uuid"`
	CreatedAt          time.Time  `json:"created_at"`
}

func paymentRowDTO(p Payment) PaymentDTO {
	return PaymentDTO{
		PaymentID: p.ID.String(), OrderID: p.OrderID.String(), OrgID: p.OrgID.String(), Provider: string(p.Provider),
		IncomingTransferID: httpx.IDStr(p.IncomingTransferID), Amount: p.Amount, Currency: p.Currency, Status: string(p.Status),
		RefundedAmount: p.RefundedAmount, CapturedAt: utc(p.CapturedAt), CapturedBy: httpx.IDStr(p.CapturedBy),
		EvidenceFileID: httpx.IDStr(p.EvidenceFileID), CreatedAt: p.CreatedAt.UTC(),
	}
}

// SplitDTO is one split of a payment (02 §20.4).
type SplitDTO struct {
	SplitID        string `json:"split_id" format:"uuid"`
	OrderLineID    string `json:"order_line_id" format:"uuid"`
	SellerAmount   int64  `json:"seller_amount" doc:"Kuruş: the payee's share"`
	PlatformAmount int64  `json:"platform_amount" doc:"Kuruş: Koro's commission, its VAT and the buyer fee"`
	Status         string `json:"status" enum:"held,approved,disapproved,released,refunded"`
}

// PaymentDetailDTO is the body of adminGetPayment.
type PaymentDetailDTO struct {
	PaymentDTO
	OrderNo string      `json:"order_no"`
	Splits  []SplitDTO  `json:"splits"`
	Refunds []RefundDTO `json:"refunds"`
}

// PaymentList is the body of adminListPayments.
type PaymentList struct {
	Items      []PaymentDTO `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// sortNewest is the only order of the payment and refund lists (cursor
// fingerprint).
const sortNewest = "-created_at"

type listPaymentsInput struct {
	page.Params
	OrderID string `query:"order_id" maxLength:"36" doc:"One order's payments"`
	Status  string `query:"status" enum:"initiated,captured,failed,partially_refunded,refunded,chargeback"`
}

type paymentListOutput struct{ Body PaymentList }

func (h handler) listPayments(ctx context.Context, in *listPaymentsInput) (*paymentListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListPayments", in.OrderID, in.Status)
	q := PaymentQuery{Filter: PaymentFilter{Status: PaymentStatus(in.Status)}, Limit: in.Limit}
	q.Filter.OrderID, q.invalid = httpx.OptID("query.order_id", in.OrderID)
	if q.invalid == nil {
		q.After, q.invalid = decodeTimeCursor(in.Cursor, fp)
	}
	p, err := h.svc.ListPayments(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &paymentListOutput{Body: PaymentList{Items: make([]PaymentDTO, len(p.Items))}}
	for i, x := range p.Items {
		out.Body.Items[i] = paymentRowDTO(x)
	}
	out.Body.NextCursor = encodeTimeCursor(p.Next, fp)
	return out, nil
}

type paymentDetailOutput struct{ Body PaymentDetailDTO }

func (h handler) getPayment(ctx context.Context, in *PaymentPath) (*paymentDetailOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetPaymentView(ctx, a, httpx.ParseID(in.PaymentID))
	if err != nil {
		return nil, err
	}
	d := PaymentDetailDTO{PaymentDTO: paymentRowDTO(v.Payment), OrderNo: v.OrderNo, Splits: make([]SplitDTO, len(v.Splits)),
		Refunds: make([]RefundDTO, len(v.Refunds))}
	for i, s := range v.Splits {
		d.Splits[i] = SplitDTO{SplitID: s.ID.String(), OrderLineID: s.OrderLineID.String(), SellerAmount: s.SellerAmount,
			PlatformAmount: s.PlatformAmount, Status: s.Status}
	}
	for i, r := range v.Refunds {
		d.Refunds[i] = refundDTO(r)
	}
	return &paymentDetailOutput{Body: d}, nil
}

// ---- refunds

// RefundPath is the refund_id path parameter.
type RefundPath struct {
	RefundID string `path:"refund_id" doc:"Refund ID (an unknown or malformed ID is 404)"`
}

// RefundDTO is 04 §2.16 RefundDTO. The target IBAN is shown in full to
// finance and platform admins: the second eye approves the destination
// (it may be a natural person's: an overpayment goes back to its sender).
type RefundDTO struct {
	RefundID        string     `json:"refund_id" format:"uuid"`
	PaymentID       string     `json:"payment_id" format:"uuid"`
	OrderID         string     `json:"order_id" format:"uuid"`
	OrderLineID     string     `json:"order_line_id,omitempty" format:"uuid"`
	Amount          int64      `json:"amount" doc:"Kuruş"`
	Currency        string     `json:"currency" enum:"TRY"`
	Reason          string     `json:"reason" enum:"event_cancelled,offering_cancelled,not_fulfilled,duplicate_payment,overpayment,goodwill,other"`
	ReasonNote      string     `json:"reason_note,omitempty"`
	Status          string     `json:"status" enum:"requested,approved,processing,completed,failed,rejected,cancelled"`
	Method          string     `json:"method" enum:"bank_eft,iyzico"`
	TargetIBAN      string     `json:"target_iban,omitempty"`
	RequestedBy     string     `json:"requested_by" format:"uuid"`
	RequestedAt     time.Time  `json:"requested_at"`
	ApprovedBy      string     `json:"approved_by,omitempty" format:"uuid"`
	ApprovedAt      *time.Time `json:"approved_at,omitempty"`
	RejectedBy      string     `json:"rejected_by,omitempty" format:"uuid"`
	RejectedAt      *time.Time `json:"rejected_at,omitempty"`
	RejectionReason string     `json:"rejection_reason,omitempty"`
	BankReference   string     `json:"bank_reference,omitempty"`
	ReceiptFileID   string     `json:"receipt_file_id,omitempty" format:"uuid"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
	ClosedBy        string     `json:"closed_by,omitempty" format:"uuid"`
	ClosedAt        *time.Time `json:"closed_at,omitempty"`
	CancelReason    string     `json:"cancel_reason,omitempty"`
	FailureReason   string     `json:"failure_reason,omitempty"`
}

func refundDTO(r Refund) RefundDTO {
	return RefundDTO{
		RefundID: r.ID.String(), PaymentID: r.PaymentID.String(), OrderID: r.OrderID.String(), OrderLineID: httpx.IDStr(r.OrderLineID),
		Amount: r.Amount, Currency: Currency, Reason: r.Reason, ReasonNote: str(r.ReasonNote), Status: string(r.Status),
		Method: r.Method, TargetIBAN: str(r.TargetIBAN), RequestedBy: r.RequestedBy.String(), RequestedAt: r.CreatedAt.UTC(),
		ApprovedBy: httpx.IDStr(r.ApprovedBy), ApprovedAt: utc(r.ApprovedAt), RejectedBy: httpx.IDStr(r.RejectedBy), RejectedAt: utc(r.RejectedAt),
		RejectionReason: str(r.RejectionReason), BankReference: str(r.BankReference), ReceiptFileID: httpx.IDStr(r.ReceiptFileID),
		CompletedAt: utc(r.CompletedAt), ClosedBy: httpx.IDStr(r.ClosedBy), ClosedAt: utc(r.ClosedAt),
		CancelReason: str(r.CancelReason), FailureReason: str(r.FailureReason),
	}
}

// RefundList is the body of adminListRefunds.
type RefundList struct {
	Items      []RefundDTO `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

type listRefundsInput struct {
	page.Params
	Status    string `query:"status" enum:"requested,approved,processing,completed,failed,rejected,cancelled"`
	OrderID   string `query:"order_id" maxLength:"36" doc:"One order's refunds"`
	PaymentID string `query:"payment_id" maxLength:"36" doc:"One payment's refunds"`
}

type refundListOutput struct{ Body RefundList }

func (h handler) listRefunds(ctx context.Context, in *listRefundsInput) (*refundListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListRefunds", in.Status, in.OrderID, in.PaymentID)
	q := RefundQuery{Filter: RefundFilter{Status: RefundStatus(in.Status)}, Limit: in.Limit}
	q.Filter.OrderID, q.invalid = httpx.OptID("query.order_id", in.OrderID)
	if q.invalid == nil {
		q.Filter.PaymentID, q.invalid = httpx.OptID("query.payment_id", in.PaymentID)
	}
	if q.invalid == nil {
		q.After, q.invalid = decodeTimeCursor(in.Cursor, fp)
	}
	p, err := h.svc.ListRefunds(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &refundListOutput{Body: RefundList{Items: make([]RefundDTO, len(p.Items))}}
	for i, r := range p.Items {
		out.Body.Items[i] = refundDTO(r)
	}
	out.Body.NextCursor = encodeTimeCursor(p.Next, fp)
	return out, nil
}

// FundingPaymentDTO is a payment that can fund a refund due.
type FundingPaymentDTO struct {
	PaymentID       string `json:"payment_id" format:"uuid"`
	AvailableAmount int64  `json:"available_amount" doc:"Kuruş neither refunded nor held by an open refund"`
}

// RefundDueDTO is one row of adminListRefundsDue.
type RefundDueDTO struct {
	OrderID     string              `json:"order_id" format:"uuid"`
	OrderNo     string              `json:"order_no"`
	OrgID       string              `json:"org_id" format:"uuid" doc:"The buying org"`
	OrderLineID string              `json:"order_line_id,omitempty" format:"uuid" doc:"The cancelled line; empty: the whole order's paid money"`
	Amount      int64               `json:"amount" doc:"Kuruş still to request"`
	Since       time.Time           `json:"since"`
	Payments    []FundingPaymentDTO `json:"payments"`
}

// RefundDueList is the body of adminListRefundsDue.
type RefundDueList struct {
	Items []RefundDueDTO `json:"items"`
}

type listRefundsDueInput struct {
	Limit int `query:"limit" minimum:"1" maximum:"100" default:"25" doc:"Rows (the oldest first)"`
}

type refundDueOutput struct{ Body RefundDueList }

func (h handler) listRefundsDue(ctx context.Context, in *listRefundsDueInput) (*refundDueOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	dues, err := h.svc.ListRefundsDue(ctx, a, in.Limit)
	if err != nil {
		return nil, err
	}
	out := &refundDueOutput{Body: RefundDueList{Items: make([]RefundDueDTO, len(dues))}}
	for i, d := range dues {
		item := RefundDueDTO{OrderID: d.OrderID.String(), OrderNo: d.OrderNo, OrgID: d.OrgID.String(), OrderLineID: httpx.IDStr(d.LineID),
			Amount: d.Amount, Since: d.Since.UTC(), Payments: make([]FundingPaymentDTO, len(d.Payments))}
		for j, p := range d.Payments {
			item.Payments[j] = FundingPaymentDTO{PaymentID: p.PaymentID.String(), AvailableAmount: p.Available}
		}
		out.Body.Items[i] = item
	}
	return out, nil
}

type refundOutput struct{ Body RefundDTO }

type createRefundInput struct {
	idempotency.RequiredHeader
	Body struct {
		PaymentID   string `json:"payment_id" format:"uuid" doc:"The captured bank-transfer payment the money goes back from"`
		OrderLineID string `json:"order_line_id,omitempty" format:"uuid" doc:"A line of a paid order; empty for an overpayment or an unpaid order's money"`
		Amount      int64  `json:"amount" minimum:"1" doc:"Kuruş"`
		Reason      string `json:"reason" enum:"event_cancelled,offering_cancelled,not_fulfilled,duplicate_payment,overpayment,goodwill,other"`
		ReasonNote  string `json:"reason_note,omitempty" maxLength:"1000" doc:"Audited; no personal data"`
		Method      string `json:"method,omitempty" enum:"bank_eft" default:"bank_eft"`
		TargetIBAN  string `json:"target_iban" maxLength:"42" doc:"TR IBAN the EFT goes to; spaces are ignored"`
	}
}

func (h handler) createRefund(ctx context.Context, in *createRefundInput) (*refundOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	ri := RefundInput{PaymentID: httpx.ParseID(b.PaymentID), Amount: b.Amount, Reason: b.Reason, ReasonNote: b.ReasonNote, Method: b.Method,
		TargetIBAN: b.TargetIBAN}
	if b.OrderLineID != "" {
		id := httpx.ParseID(b.OrderLineID)
		ri.OrderLineID = &id
	}
	r, err := h.svc.CreateRefund(ctx, a, ri)
	if err != nil {
		return nil, err
	}
	return &refundOutput{Body: refundDTO(r)}, nil
}

type approveRefundInput struct {
	idempotency.OptionalHeader
	RefundPath
	Body struct {
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) approveRefund(ctx context.Context, in *approveRefundInput) (*refundOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.ApproveRefund(ctx, a, httpx.ParseID(in.RefundID), proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &refundOutput{Body: refundDTO(r)}, nil
}

type rejectRefundInput struct {
	idempotency.OptionalHeader
	RefundPath
	Body struct {
		Note string `json:"note" minLength:"1" maxLength:"1000" doc:"Why the refund is rejected (audited; no personal data)"`
	}
}

func (h handler) rejectRefund(ctx context.Context, in *rejectRefundInput) (*refundOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.RejectRefund(ctx, a, httpx.ParseID(in.RefundID), in.Body.Note)
	if err != nil {
		return nil, err
	}
	return &refundOutput{Body: refundDTO(r)}, nil
}

// CompletedRefundDTO is the body of adminCompleteRefund: the refund and how
// the ledger took the money back.
type CompletedRefundDTO struct {
	RefundDTO
	LedgerTransactionID string `json:"ledger_transaction_id" format:"uuid" doc:"The refund_completed transaction"`
	UnallocatedAmount   int64  `json:"unallocated_amount" doc:"Kuruş back from buyer_advances (overpayment, unpaid order)"`
	ClawbackAmount      int64  `json:"clawback_amount" doc:"Kuruş claimed back from the payee of a line already paid out (06 H-59)"`
}

type completeRefundInput struct {
	idempotency.OptionalHeader
	RefundPath
	Body struct {
		BankReference string `json:"bank_reference" minLength:"1" maxLength:"100" doc:"The EFT's bank reference"`
		ReceiptFileID string `json:"receipt_file_id" format:"uuid" doc:"A refund_receipt upload (required)"`
	}
}

type completedRefundOutput struct{ Body CompletedRefundDTO }

func (h handler) completeRefund(ctx context.Context, in *completeRefundInput) (*completedRefundOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.CompleteRefund(ctx, a, httpx.ParseID(in.RefundID), RefundReceipt{BankReference: in.Body.BankReference,
		ReceiptFileID: httpx.ParseID(in.Body.ReceiptFileID)})
	if err != nil {
		return nil, err
	}
	return &completedRefundOutput{Body: CompletedRefundDTO{RefundDTO: refundDTO(r.Refund), LedgerTransactionID: r.TransactionID.String(),
		UnallocatedAmount: r.Unallocated, ClawbackAmount: r.Clawback}}, nil
}

// ---- query helpers

// decodeTimeCursor decodes a (created_at, id) cursor of the lists.
func decodeTimeCursor(raw, fp string) (*TimeCursor, error) {
	c, err := page.Decode(raw, sortNewest, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &TimeCursor{CreatedAt: at, ID: id}, nil
}

// encodeTimeCursor is the next_cursor of a page ("" on the last page).
func encodeTimeCursor(next *TimeCursor, fp string) string {
	if next == nil {
		return ""
	}
	return page.Next(sortNewest, fp, page.TimeVal(next.CreatedAt), page.UUIDVal(next.ID))
}

type cancelRefundInput struct {
	idempotency.OptionalHeader
	RefundPath
	Body struct {
		CancelReason string             `json:"cancel_reason" minLength:"1" maxLength:"500" doc:"Audited; no personal data"`
		Reauth       *consent.ReauthDTO `json:"reauth,omitempty"`
	}
}

func (h handler) cancelRefund(ctx context.Context, in *cancelRefundInput) (*refundOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.CloseRefund(ctx, a, httpx.ParseID(in.RefundID), false, in.Body.CancelReason, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &refundOutput{Body: refundDTO(r)}, nil
}

type failRefundInput struct {
	idempotency.OptionalHeader
	RefundPath
	Body struct {
		FailureReason string             `json:"failure_reason" minLength:"1" maxLength:"500" doc:"Audited; no personal data"`
		Reauth        *consent.ReauthDTO `json:"reauth,omitempty"`
	}
}

func (h handler) markRefundFailed(ctx context.Context, in *failRefundInput) (*refundOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.CloseRefund(ctx, a, httpx.ParseID(in.RefundID), true, in.Body.FailureReason, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &refundOutput{Body: refundDTO(r)}, nil
}
