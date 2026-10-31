package payments

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/files"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

func registerCollections(api huma.API, h handler) {
	op.Register(api, "payments", huma.Operation{OperationID: "adminListReceivableCollections", Method: http.MethodGet, Path: "/admin/receivable-collections", Summary: "List manual receivable collections"},
		op.Meta{Action: string(authz.PaymentsReceivableList), Surface: op.SurfaceAdmin}, h.listCollections)
	op.Register(api, "payments", huma.Operation{OperationID: "adminCreateReceivableCollection", Method: http.MethodPost, Path: "/admin/receivable-collections", DefaultStatus: http.StatusCreated, Summary: "Request collection of a payee receivable",
		Description: "Claims one whole unmatched incoming transfer with a bank reference and receipt against a paid-out line's outstanding debt. Financial fields come from the transfer and line. A different finance admin approves it. No automatic payout offset."},
		op.Meta{Action: string(authz.PaymentsReceivableCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyRequired}, h.createCollection)
	op.Register(api, "payments", huma.Operation{OperationID: "adminApproveReceivableCollection", Method: http.MethodPost, Path: "/admin/receivable-collections/{collection_id}/approve", Summary: "Approve a receivable collection",
		Description: "A different finance admin re-authenticates; the collection, bank receipt and balanced cash/debt ledger entries commit together."},
		op.Meta{Action: string(authz.PaymentsReceivableApprove), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional, Reauth: true, RateLimit: op.RateReauth}, h.approveCollection)
	op.Register(api, "payments", huma.Operation{OperationID: "adminRejectReceivableCollection", Method: http.MethodPost, Path: "/admin/receivable-collections/{collection_id}/reject", Summary: "Reject a receivable collection request",
		Description: "Only requested collections. Releases the transfer for a corrected request or another finance operation; no ledger money moves."},
		op.Meta{Action: string(authz.PaymentsReceivableReject), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.rejectCollection)
}

// ReceivableCollectionDTO contains the frozen evidence and decision trail.
type ReceivableCollectionDTO struct {
	CollectionID        string     `json:"collection_id" format:"uuid"`
	IncomingTransferID  string     `json:"incoming_transfer_id" format:"uuid"`
	OrderLineID         string     `json:"order_line_id" format:"uuid"`
	OrderID             string     `json:"order_id" format:"uuid"`
	PayeeID             string     `json:"payee_id" format:"uuid"`
	OrgID               string     `json:"org_id" format:"uuid"`
	CollectionAccountID string     `json:"collection_account_id" format:"uuid"`
	Amount              int64      `json:"amount" doc:"Kuruş; the source transfer's whole amount"`
	Currency            string     `json:"currency" enum:"TRY"`
	BankReference       string     `json:"bank_reference"`
	ReceiptFileID       string     `json:"receipt_file_id" format:"uuid"`
	ReceiptFileState    string     `json:"receipt_file_state,omitempty" doc:"The state of the source transfer's receipt (BE-154b). rejected: finance uploads it again on the transfer (adminReplaceIncomingTransferReceipt); the collection follows"`
	Status              string     `json:"status" enum:"requested,collected,rejected"`
	RequestedBy         string     `json:"requested_by" format:"uuid"`
	ApprovedBy          string     `json:"approved_by,omitempty" format:"uuid"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	RejectedBy          string     `json:"rejected_by,omitempty" format:"uuid"`
	RejectedAt          *time.Time `json:"rejected_at,omitempty"`
	RejectionReason     string     `json:"rejection_reason,omitempty"`
	LedgerTransactionID string     `json:"ledger_transaction_id,omitempty" format:"uuid"`
	CreatedAt           time.Time  `json:"created_at"`
}

// TransformSchema implements huma.SchemaTransformer (BE-154b).
func (ReceivableCollectionDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.SetEnum(s, "receipt_file_state", files.StateValues())
}

// collectionDTO maps a collection with the state of its receipt (st).
func collectionDTO(c ReceivableCollection, st map[uuid.UUID]files.State) ReceivableCollectionDTO {
	return ReceivableCollectionDTO{
		CollectionID: c.ID.String(), IncomingTransferID: c.IncomingTransferID.String(), OrderLineID: c.OrderLineID.String(), OrderID: c.OrderID.String(),
		PayeeID: c.PayeeID.String(), OrgID: c.OrgID.String(), CollectionAccountID: c.CollectionAccountID.String(), Amount: c.Amount, Currency: Currency,
		BankReference: c.BankReference, ReceiptFileID: c.ReceiptFileID.String(),
		ReceiptFileState: string(files.StateOf(st, &c.ReceiptFileID)), Status: string(c.Status), RequestedBy: c.RequestedBy.String(),
		ApprovedBy: httpx.IDStr(c.ApprovedBy), ApprovedAt: utc(c.ApprovedAt), RejectedBy: httpx.IDStr(c.RejectedBy), RejectedAt: utc(c.RejectedAt),
		RejectionReason: str(c.RejectionReason), LedgerTransactionID: httpx.IDStr(c.LedgerTransactionID), CreatedAt: c.CreatedAt.UTC(),
	}
}

type collectionOutput struct{ Body ReceivableCollectionDTO }
type listCollectionsInput struct {
	page.Params
	Status  string `query:"status" enum:"requested,collected,rejected"`
	PayeeID string `query:"payee_id" maxLength:"36"`
}
type collectionListOutput struct {
	Body struct {
		Items      []ReceivableCollectionDTO `json:"items"`
		NextCursor string                    `json:"next_cursor,omitempty"`
	}
}

func (h handler) listCollections(ctx context.Context, in *listCollectionsInput) (*collectionListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListReceivableCollections", in.Status, in.PayeeID)
	q := CollectionQuery{Filter: CollectionFilter{Status: CollectionStatus(in.Status)}, Limit: in.Limit}
	q.Filter.PayeeID, q.invalid = httpx.OptID("query.payee_id", in.PayeeID)
	if q.invalid == nil {
		q.After, q.invalid = decodeTimeCursor(in.Cursor, fp)
	}
	p, err := h.svc.ListCollections(ctx, a, q)
	if err != nil {
		return nil, err
	}
	st, err := statesOf(ctx, h, p.Items, collectionFiles)
	if err != nil {
		return nil, err
	}
	out := &collectionListOutput{}
	out.Body.Items = make([]ReceivableCollectionDTO, len(p.Items))
	for i, c := range p.Items {
		out.Body.Items[i] = collectionDTO(c, st)
	}
	out.Body.NextCursor = encodeTimeCursor(p.Next, fp)
	return out, nil
}

type createCollectionInput struct {
	idempotency.RequiredHeader
	Body struct {
		IncomingTransferID string `json:"incoming_transfer_id" format:"uuid"`
		OrderLineID        string `json:"order_line_id" format:"uuid"`
	}
}

func (h handler) createCollection(ctx context.Context, in *createCollectionInput) (*collectionOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, err := h.svc.CreateCollection(ctx, a, CollectionInput{IncomingTransferID: httpx.ParseID(in.Body.IncomingTransferID), OrderLineID: httpx.ParseID(in.Body.OrderLineID)})
	if err != nil {
		return nil, err
	}
	return h.collectionOut(ctx, c)
}

// CollectionPath identifies one manual receivable collection.
type CollectionPath struct {
	CollectionID string `path:"collection_id" doc:"Collection ID; an unknown or malformed ID is 404"`
}
type approveCollectionInput struct {
	idempotency.OptionalHeader
	CollectionPath
	Body struct {
		Reauth *consent.ReauthDTO `json:"reauth,omitempty"`
	}
}

func (h handler) approveCollection(ctx context.Context, in *approveCollectionInput) (*collectionOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, err := h.svc.ApproveCollection(ctx, a, httpx.ParseID(in.CollectionID), proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return h.collectionOut(ctx, c)
}

type rejectCollectionInput struct {
	idempotency.OptionalHeader
	CollectionPath
	Body struct {
		Note string `json:"note" minLength:"1" maxLength:"500" doc:"Audited; no personal data"`
	}
}

func (h handler) rejectCollection(ctx context.Context, in *rejectCollectionInput) (*collectionOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, err := h.svc.RejectCollection(ctx, a, httpx.ParseID(in.CollectionID), in.Body.Note)
	if err != nil {
		return nil, err
	}
	return h.collectionOut(ctx, c)
}
