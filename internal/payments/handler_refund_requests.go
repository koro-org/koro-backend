package payments

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// registerRefundRequests adds the refund request operations of BE-132
// (Q-26): the buyer's (an order's refund options, the request, its list,
// the withdrawal), the seller's list, one request and its evidence (the
// parties) and finance's queue and decisions.
func registerRefundRequests(api huma.API, h handler) {
	const tag = "payments"
	op.Register(api, tag, huma.Operation{
		OperationID: "listOrderRefundRequests", Method: http.MethodGet, Path: "/orgs/{org_id}/orders/{order_id}/refund-requests",
		Summary: "List an order's refund options and requests",
		Description: "The buying company's members: every line of the order with what a refund request may still ask of " +
			"it (refundable_amount, kuruş: what Koro holds of the line — its buyer fee only in model C — less the refunds, " +
			"the open refunds and, within the order, the other lines' waiting requests), the request deadline (the " +
			"line's delivery + the refund request window; none while undelivered) and why a line cannot be asked " +
			"(blocked_reason), and the order's requests, newest first. Another org's order is 404.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestList), Surface: op.SurfaceOrg}, h.refundOptions)

	op.Register(api, tag, huma.Operation{
		OperationID: "createOrderRefundRequest", Method: http.MethodPost, Path: "/orgs/{org_id}/orders/{order_id}/refund-requests",
		Summary: "Ask for a refund of an order line", DefaultStatus: http.StatusCreated,
		Description: "A company admin or owner of the buying company asks for a refund of one line of a paid order " +
			"(the order is paid, in fulfilment, completed, disputed or partially refunded: 409 orders.order_not_paid or " +
			"orders.invalid_state otherwise; a cancelled line is refunded by finance without a request, 409). An " +
			"undelivered line may be asked any time, a delivered one within the refund request window after its delivery " +
			"(409 payments.refund_request_window_closed); one waiting request per line (409 payments.refund_request_open). " +
			"amount is at most the line's refundable_amount (422 orders.refund_exceeds_paid). target_iban is where the " +
			"money goes back (a TR IBAN; shown masked). evidence_file_id, optional, is a completed refund_request_evidence " +
			"upload of the buying company (422 otherwise; 409 files.rejected, files.not_clean or files.already_attached): " +
			"the seller's admins and finance see it. The request waits for finance (open); the seller's admins and " +
			"finance are told.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestCreate), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyRequired},
		h.createRefundRequest)

	op.Register(api, tag, huma.Operation{
		OperationID: "listOrgRefundRequests", Method: http.MethodGet, Path: "/orgs/{org_id}/refund-requests",
		Summary:     "List a buying company's refund requests",
		Description: "Newest first; filter by status, waiting ones (pending: open or in_review) or one order.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestList), Surface: op.SurfaceOrg}, h.listOrgRefundRequests)

	op.Register(api, tag, huma.Operation{
		OperationID: "listOrgSalesRefundRequests", Method: http.MethodGet, Path: "/orgs/{org_id}/sales/refund-requests",
		Summary: "List the refund requests on a seller's lines",
		Description: "The selling org's club admins, university representatives and career staff, and finance and " +
			"platform admins: the requests on the org's order lines, newest first (pending=true: the open ones; order_id: " +
			"one order). The seller's view carries neither the buyer's IBAN nor its user.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestListSales), Surface: op.SurfaceOrg}, h.listSalesRefundRequests)

	op.Register(api, tag, huma.Operation{
		OperationID: "getRefundRequest", Method: http.MethodGet, Path: "/refund-requests/{refund_request_id}",
		Summary: "Get a refund request",
		Description: "The buying company's members, the seller's staff (the seller's view: no IBAN, no buyer user) and " +
			"support, finance and platform admins; anyone else 404. refund_status is the status of the refund an " +
			"accepted request opened.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestRead), Surface: op.SurfaceResource}, h.getRefundRequest)

	op.Register(api, tag, huma.Operation{
		OperationID: "withdrawRefundRequest", Method: http.MethodPost, Path: "/refund-requests/{refund_request_id}/withdraw",
		Summary:     "Withdraw a refund request",
		Description: "The buying company's admin, while it waits (open or in_review; 409 payments.invalid_state once decided).",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestWithdraw), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.withdrawRefundRequest)

	op.Register(api, tag, huma.Operation{
		OperationID: "downloadRefundRequestEvidence", Method: http.MethodGet, Path: "/refund-requests/{refund_request_id}/evidence",
		Summary: "Get a download URL of a refund request's evidence",
		Description: "A URL valid for 5 minutes of the buyer's evidence file, for the parties only: the buying " +
			"company's admins, the seller's club admins and university representatives, finance and platform admins. A " +
			"request without evidence is 404; 409 files.not_clean until the virus scan clears the file. Every download " +
			"is audited.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestReadEvidence), Surface: op.SurfaceResource}, h.refundRequestEvidence)

	op.Register(api, tag, huma.Operation{
		OperationID: "adminListRefundRequests", Method: http.MethodGet, Path: "/admin/refund-requests",
		Summary:     "List the refund requests (finance's queue)",
		Description: "Every buyer's requests, oldest first; pending=true keeps the waiting ones (open or in_review).",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestListAdmin), Surface: op.SurfaceAdmin}, h.adminListRefundRequests)

	op.Register(api, tag, huma.Operation{
		OperationID: "adminGetRefundRequest", Method: http.MethodGet, Path: "/admin/refund-requests/{refund_request_id}",
		Summary: "Get a refund request with its money",
		Description: "The request, what its line may still give back now (line_refundable_amount) and the order's " +
			"payments that can fund the refund, with the refund an acceptance opened.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestListAdmin), Surface: op.SurfaceAdmin}, h.adminGetRefundRequest)

	op.Register(api, tag, huma.Operation{
		OperationID: "adminReviewRefundRequest", Method: http.MethodPost, Path: "/admin/refund-requests/{refund_request_id}/review",
		Summary: "Take a refund request into review",
		Description: "Only an open request (409 payments.invalid_state); never by its requester (403 " +
			"authz.four_eyes_violation). The buyer is told.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestReview), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.reviewRefundRequest)

	op.Register(api, tag, huma.Operation{
		OperationID: "adminAcceptRefundRequest", Method: http.MethodPost, Path: "/admin/refund-requests/{refund_request_id}/accept",
		Summary: "Accept a refund request",
		Description: "A waiting request (409 payments.invalid_state), never by its requester (403). In one transaction a " +
			"refund of the request's line and amount is requested (adminCreateRefund's rules: every limit again, 422 " +
			"orders.refund_exceeds_paid; a line in an open payout 409 payments.line_in_payout): it draws from payment_id " +
			"or, when empty, the order's oldest captured bank transfer that still holds the amount (422 when none does); " +
			"its IBAN is target_iban or the buyer's, its reason refund_reason or the buyer's. Another finance admin " +
			"approves the refund (four eyes). The buyer and the seller are told.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestAccept), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyRequired},
		h.acceptRefundRequest)

	op.Register(api, tag, huma.Operation{
		OperationID: "adminRejectRefundRequest", Method: http.MethodPost, Path: "/admin/refund-requests/{refund_request_id}/reject",
		Summary:     "Reject a refund request",
		Description: "A waiting request (409 payments.invalid_state), never by its requester (403); the reason is shown to the buyer and audited.",
	}, op.Meta{Action: string(authz.PaymentsRefundRequestReject), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.rejectRefundRequest)
}

// ---- field types whose bounds and enums come from the service's constants (BE-117)

type (
	// requestReason is a buyer's reason (RequestReasons).
	requestReason string
	// lineRefundReason is a refund reason of an order line (no overpayment).
	lineRefundReason string
	// requestStatus is a refund request status (RefundRequestStatuses).
	requestStatus string
	// requestExplanation is the buyer's explanation.
	requestExplanation string
	// requestDecision is the reason of a rejection.
	requestDecision string
	// refundNote is the note of the refund an acceptance opens.
	refundNote string
)

// TransformSchema implements huma.SchemaTransformer.
func (requestReason) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Enum = op.Enum(RequestReasons())
	return s
}

// lineRefundReasons are the refund reasons an accepted request's refund may take.
func lineRefundReasons() []string {
	return slices.DeleteFunc(RefundReasons(), func(r string) bool { return r == RefundReasonOverpayment })
}

// TransformSchema implements huma.SchemaTransformer.
func (lineRefundReason) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Enum = op.Enum(lineRefundReasons())
	return s
}

// TransformSchema implements huma.SchemaTransformer.
func (requestStatus) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Enum = op.Enum(RefundRequestStatuses())
	return s
}

// TransformSchema implements huma.SchemaTransformer.
func (requestExplanation) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.LengthBounds(s, minExplanation, maxExplanation)
}

// TransformSchema implements huma.SchemaTransformer.
func (requestDecision) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.LengthBounds(s, 1, maxRequestDecision)
}

// TransformSchema implements huma.SchemaTransformer.
func (refundNote) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.LengthBounds(s, 0, maxRefundNote)
}

// ---- DTOs

// RefundRequestPath is the refund_request_id path parameter.
type RefundRequestPath struct {
	RefundRequestID string `path:"refund_request_id" doc:"Refund request ID (an unknown or malformed ID is 404)"`
}

// RefundRequestDTO is a refund request as its reader's audience sees it:
// the seller's view has no target_iban_masked and no requested_by; the
// staff fields (reviewed_by, decided_by, refund_id) are staff only.
type RefundRequestDTO struct {
	RefundRequestID  string     `json:"refund_request_id" format:"uuid"`
	OrderID          string     `json:"order_id" format:"uuid"`
	OrderNo          string     `json:"order_no"`
	OrderLineID      string     `json:"order_line_id" format:"uuid"`
	LineTitle        string     `json:"line_title"`
	OrgID            string     `json:"org_id" format:"uuid" doc:"The buying company"`
	BuyerName        string     `json:"buyer_name"`
	SellerOrgID      string     `json:"seller_org_id" format:"uuid" doc:"The line's seller"`
	SellerName       string     `json:"seller_name"`
	Amount           int64      `json:"amount" doc:"Kuruş"`
	Currency         string     `json:"currency" enum:"TRY"`
	Reason           string     `json:"reason"`
	Explanation      string     `json:"explanation"`
	HasEvidence      bool       `json:"has_evidence" doc:"downloadRefundRequestEvidence serves it to the parties"`
	TargetIBANMasked string     `json:"target_iban_masked,omitempty" doc:"Where the money goes back, masked (the buyer's and staff's view)"`
	Status           string     `json:"status"`
	RequestedBy      string     `json:"requested_by,omitempty" format:"uuid" doc:"The buyer's admin who asked (the buyer's and staff's view)"`
	RequestedAt      time.Time  `json:"requested_at"`
	ReviewedBy       string     `json:"reviewed_by,omitempty" format:"uuid" doc:"Staff view"`
	ReviewedAt       *time.Time `json:"reviewed_at,omitempty"`
	DecidedBy        string     `json:"decided_by,omitempty" format:"uuid" doc:"Staff view"`
	DecidedAt        *time.Time `json:"decided_at,omitempty"`
	RejectionReason  string     `json:"rejection_reason,omitempty"`
	WithdrawnAt      *time.Time `json:"withdrawn_at,omitempty"`
	RefundID         string     `json:"refund_id,omitempty" format:"uuid" doc:"Staff view: the refund an acceptance opened"`
	RefundStatus     string     `json:"refund_status,omitempty" doc:"The status of that refund (one request's reads)"`
}

// TransformSchema implements huma.SchemaTransformer: the enums come from
// the service's lists.
func (RefundRequestDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	op.SetEnum(s, "reason", RequestReasons())
	op.SetEnum(s, "status", RefundRequestStatuses())
	return op.SetEnum(s, "refund_status", RefundStatuses())
}

func refundRequestDTO(v RefundRequestView) RefundRequestDTO {
	r := v.Request
	d := RefundRequestDTO{
		RefundRequestID: r.ID.String(), OrderID: r.OrderID.String(), OrderNo: r.OrderNo, OrderLineID: r.OrderLineID.String(),
		LineTitle: r.LineTitle, OrgID: r.OrgID.String(), BuyerName: v.BuyerName, SellerOrgID: r.SellerOrgID.String(),
		SellerName: v.SellerName, Amount: r.Amount, Currency: r.Currency, Reason: r.Reason, Explanation: r.Explanation,
		HasEvidence: r.EvidenceFileID != nil, Status: string(r.Status), RequestedAt: r.CreatedAt.UTC(),
		ReviewedAt: utc(r.ReviewedAt), DecidedAt: utc(r.DecidedAt), RejectionReason: str(r.RejectionReason),
		WithdrawnAt: utc(r.WithdrawnAt),
	}
	if v.Refund != nil {
		d.RefundStatus = string(v.Refund.Status)
	}
	if v.Audience != AudienceSeller {
		d.TargetIBANMasked, d.RequestedBy = r.MaskedIBAN(), r.RequestedBy.String()
	}
	if v.Audience == AudienceStaff {
		d.ReviewedBy, d.DecidedBy, d.RefundID = httpx.IDStr(r.ReviewedBy), httpx.IDStr(r.DecidedBy), httpx.IDStr(r.RefundID)
	}
	return d
}

func refundRequestDTOs(vs []RefundRequestView) []RefundRequestDTO {
	out := make([]RefundRequestDTO, len(vs))
	for i, v := range vs {
		out[i] = refundRequestDTO(v)
	}
	return out
}

// RefundRequestList is the body of the request lists.
type RefundRequestList struct {
	Items      []RefundRequestDTO `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// RefundOptionDTO is one line of listOrderRefundRequests.
type RefundOptionDTO struct {
	OrderLineID         string     `json:"order_line_id" format:"uuid"`
	Title               string     `json:"title"`
	SellerOrgID         string     `json:"seller_org_id" format:"uuid"`
	RefundableAmount    int64      `json:"refundable_amount" doc:"Kuruş a request may ask now (0 when blocked)"`
	RequestDeadline     *time.Time `json:"request_deadline,omitempty" doc:"The line's delivery + the refund request window; absent while undelivered"`
	OpenRefundRequestID string     `json:"open_refund_request_id,omitempty" format:"uuid" doc:"The line's waiting request"`
	Eligible            bool       `json:"eligible"`
	BlockedReason       string     `json:"blocked_reason,omitempty" doc:"Why no request may be opened now"`
}

// TransformSchema implements huma.SchemaTransformer.
func (RefundOptionDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.SetEnum(s, "blocked_reason", BlockedReasons())
}

// RefundOptionsDTO is the body of listOrderRefundRequests.
type RefundOptionsDTO struct {
	OrderID  string             `json:"order_id" format:"uuid"`
	OrderNo  string             `json:"order_no"`
	Status   string             `json:"status" doc:"The order's status"`
	Currency string             `json:"currency" enum:"TRY"`
	Lines    []RefundOptionDTO  `json:"lines"`
	Items    []RefundRequestDTO `json:"items" doc:"The order's refund requests, newest first"`
}

// RefundRequestDetailDTO is the body of adminGetRefundRequest.
type RefundRequestDetailDTO struct {
	RefundRequestDTO
	LineRefundableAmount int64               `json:"line_refundable_amount" doc:"Kuruş the line may still give back now (the request's own amount not counted)"`
	Payments             []FundingPaymentDTO `json:"payments" doc:"The order's payments that can fund the refund"`
	Refund               *RefundDTO          `json:"refund,omitempty" doc:"The refund an acceptance opened"`
}

// ---- handlers

// OrderRefundPath is the org_id and order_id path parameters of an order's
// refund requests.
type OrderRefundPath struct {
	OrgPath
	OrderID string `path:"order_id" doc:"Order ID (an unknown or malformed ID, or another org's order, is 404)"`
}

// OrgPath is the org_id path parameter.
type OrgPath struct {
	OrgID string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
}

type refundOptionsOutput struct{ Body RefundOptionsDTO }

func (h handler) refundOptions(ctx context.Context, in *OrderRefundPath) (*refundOptionsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	o, err := h.svc.RefundOptions(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.OrderID))
	if err != nil {
		return nil, err
	}
	out := RefundOptionsDTO{OrderID: o.OrderID.String(), OrderNo: o.OrderNo, Status: o.Status, Currency: o.Currency,
		Lines: make([]RefundOptionDTO, len(o.Lines)), Items: refundRequestDTOs(o.Requests)}
	for i, l := range o.Lines {
		out.Lines[i] = RefundOptionDTO{OrderLineID: l.LineID.String(), Title: l.Title, SellerOrgID: l.SellerOrgID.String(),
			RefundableAmount: l.Refundable, RequestDeadline: utc(l.Deadline), OpenRefundRequestID: httpx.IDStr(l.OpenRequest),
			Eligible: l.Blocked == "", BlockedReason: l.Blocked}
	}
	return &refundOptionsOutput{Body: out}, nil
}

type refundRequestOutput struct{ Body RefundRequestDTO }

type createRefundRequestInput struct {
	idempotency.RequiredHeader
	OrderRefundPath
	Body struct {
		OrderLineID    string             `json:"order_line_id" format:"uuid" doc:"The line of the order to refund"`
		Amount         int64              `json:"amount" minimum:"1" doc:"Kuruş, at most the line's refundable_amount"`
		Reason         requestReason      `json:"reason"`
		Explanation    requestExplanation `json:"explanation" doc:"What happened (the seller's admins and finance read it)"`
		TargetIBAN     string             `json:"target_iban" maxLength:"42" doc:"TR IBAN the money goes back to; spaces are ignored"`
		EvidenceFileID string             `json:"evidence_file_id,omitempty" format:"uuid" doc:"A refund_request_evidence upload of the buying company"`
	}
}

func (h handler) createRefundRequest(ctx context.Context, in *createRefundRequestInput) (*refundRequestOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	ri := RefundRequestInput{OrderLineID: httpx.ParseID(b.OrderLineID), Amount: b.Amount, Reason: string(b.Reason),
		Explanation: string(b.Explanation), TargetIBAN: b.TargetIBAN}
	if b.EvidenceFileID != "" {
		id := httpx.ParseID(b.EvidenceFileID)
		ri.EvidenceFileID = &id
	}
	v, err := h.svc.CreateRefundRequest(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.OrderID), ri)
	if err != nil {
		return nil, err
	}
	return &refundRequestOutput{Body: refundRequestDTO(v)}, nil
}

// RefundRequestListQuery is the query of the request lists.
type RefundRequestListQuery struct {
	page.Params
	Status  requestStatus `query:"status"`
	Pending bool          `query:"pending" doc:"Only the waiting requests (open or in_review)"`
	OrderID string        `query:"order_id" maxLength:"36" doc:"One order's requests"`
}

// query turns the input into the service's query, its cursor decoded in
// sort order with the list's fingerprint.
func (in RefundRequestListQuery) query(opID, sort string) (RefundRequestQuery, string) {
	fp := page.Fingerprint(opID, string(in.Status), boolText(in.Pending), in.OrderID)
	q := RefundRequestQuery{Status: RefundRequestStatus(in.Status), Pending: in.Pending, Limit: in.Limit}
	q.OrderID, q.invalid = httpx.OptID("query.order_id", in.OrderID)
	if q.invalid == nil {
		q.After, q.invalid = decodeSortedCursor(in.Cursor, sort, fp)
	}
	return q, fp
}

// boolText is the fingerprint text of a flag.
func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// sortOldest is the order of finance's refund request queue (cursor
// fingerprint).
const sortOldest = "created_at"

type refundRequestListOutput struct{ Body RefundRequestList }

func requestListOut(p RefundRequestPage, sort, fp string) *refundRequestListOutput {
	return &refundRequestListOutput{Body: RefundRequestList{Items: refundRequestDTOs(p.Items), NextCursor: encodeSortedCursor(p.Next, sort, fp)}}
}

type orgRefundRequestsInput struct {
	OrgPath
	RefundRequestListQuery
}

func (h handler) listOrgRefundRequests(ctx context.Context, in *orgRefundRequestsInput) (*refundRequestListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q, fp := in.query("listOrgRefundRequests", sortNewest)
	p, err := h.svc.ListOrgRefundRequests(ctx, a, httpx.ParseID(in.OrgID), q)
	if err != nil {
		return nil, err
	}
	return requestListOut(p, sortNewest, fp), nil
}

func (h handler) listSalesRefundRequests(ctx context.Context, in *orgRefundRequestsInput) (*refundRequestListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q, fp := in.query("listOrgSalesRefundRequests", sortNewest)
	p, err := h.svc.ListSalesRefundRequests(ctx, a, httpx.ParseID(in.OrgID), q)
	if err != nil {
		return nil, err
	}
	return requestListOut(p, sortNewest, fp), nil
}

func (h handler) adminListRefundRequests(ctx context.Context, in *RefundRequestListQuery) (*refundRequestListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q, fp := in.query("adminListRefundRequests", sortOldest)
	p, err := h.svc.AdminListRefundRequests(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return requestListOut(p, sortOldest, fp), nil
}

func (h handler) getRefundRequest(ctx context.Context, in *RefundRequestPath) (*refundRequestOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetRefundRequest(ctx, a, httpx.ParseID(in.RefundRequestID))
	if err != nil {
		return nil, err
	}
	return &refundRequestOutput{Body: refundRequestDTO(v)}, nil
}

type refundRequestStepInput struct {
	idempotency.OptionalHeader
	RefundRequestPath
}

func (h handler) withdrawRefundRequest(ctx context.Context, in *refundRequestStepInput) (*refundRequestOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.WithdrawRefundRequest(ctx, a, httpx.ParseID(in.RefundRequestID))
	if err != nil {
		return nil, err
	}
	return &refundRequestOutput{Body: refundRequestDTO(v)}, nil
}

func (h handler) refundRequestEvidence(ctx context.Context, in *RefundRequestPath) (*receiptOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.RefundRequestEvidence(ctx, a, httpx.ParseID(in.RefundRequestID))
	if err != nil {
		return nil, err
	}
	return &receiptOutput{CacheControl: "no-store", Body: ReceiptLinkDTO{URL: d.URL, ExpiresAt: d.ExpiresAt.UTC()}}, nil
}

type refundRequestDetailOutput struct{ Body RefundRequestDetailDTO }

func (h handler) adminGetRefundRequest(ctx context.Context, in *RefundRequestPath) (*refundRequestDetailOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.AdminGetRefundRequest(ctx, a, httpx.ParseID(in.RefundRequestID))
	if err != nil {
		return nil, err
	}
	out := RefundRequestDetailDTO{RefundRequestDTO: refundRequestDTO(d.RefundRequestView), LineRefundableAmount: d.LineRefundable,
		Payments: make([]FundingPaymentDTO, len(d.Payments))}
	for i, p := range d.Payments {
		out.Payments[i] = FundingPaymentDTO{PaymentID: p.PaymentID.String(), AvailableAmount: p.Available}
	}
	if d.Refund != nil {
		r := refundDTO(*d.Refund)
		out.Refund = &r
	}
	return &refundRequestDetailOutput{Body: out}, nil
}

func (h handler) reviewRefundRequest(ctx context.Context, in *refundRequestStepInput) (*refundRequestOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ReviewRefundRequest(ctx, a, httpx.ParseID(in.RefundRequestID))
	if err != nil {
		return nil, err
	}
	return &refundRequestOutput{Body: refundRequestDTO(v)}, nil
}

type acceptRefundRequestInput struct {
	idempotency.RequiredHeader
	RefundRequestPath
	Body struct {
		PaymentID    string           `json:"payment_id,omitempty" format:"uuid" doc:"The captured payment the refund draws from; empty: the order's oldest one that holds the amount"`
		TargetIBAN   string           `json:"target_iban,omitempty" maxLength:"42" doc:"Overrides the buyer's IBAN (TR); spaces are ignored"`
		RefundReason lineRefundReason `json:"refund_reason,omitempty" doc:"The refund's reason; empty: the buyer's"`
		Note         refundNote       `json:"note,omitempty" doc:"The refund's note (audited; no personal data)"`
	}
}

func (h handler) acceptRefundRequest(ctx context.Context, in *acceptRefundRequestInput) (*refundRequestOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	ai := AcceptInput{TargetIBAN: b.TargetIBAN, RefundReason: string(b.RefundReason), Note: string(b.Note)}
	if b.PaymentID != "" {
		id := httpx.ParseID(b.PaymentID)
		ai.PaymentID = &id
	}
	v, err := h.svc.AcceptRefundRequest(ctx, a, httpx.ParseID(in.RefundRequestID), ai)
	if err != nil {
		return nil, err
	}
	return &refundRequestOutput{Body: refundRequestDTO(v)}, nil
}

type rejectRefundRequestInput struct {
	idempotency.OptionalHeader
	RefundRequestPath
	Body struct {
		Reason requestDecision `json:"reason" doc:"Why the request is rejected (shown to the buyer, audited)"`
	}
}

func (h handler) rejectRefundRequest(ctx context.Context, in *rejectRefundRequestInput) (*refundRequestOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.RejectRefundRequest(ctx, a, httpx.ParseID(in.RefundRequestID), string(in.Body.Reason))
	if err != nil {
		return nil, err
	}
	return &refundRequestOutput{Body: refundRequestDTO(v)}, nil
}
