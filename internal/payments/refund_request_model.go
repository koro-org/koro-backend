package payments

import (
	"context"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/iban"
)

// ---- refund_requests (BE-132, Q-26; 000156)

// RefundRequestStatus is refund_requests.status.
type RefundRequestStatus string

// Refund request statuses: a buying company's admin opens a request
// (open); a finance or platform admin takes it (in_review) and accepts it
// (accepted: a refund is opened, its four eyes and EFT follow) or rejects
// it with a reason (rejected); the buyer may withdraw a waiting request
// (withdrawn). accepted, rejected and withdrawn are final.
const (
	RequestOpen      RefundRequestStatus = "open"
	RequestInReview  RefundRequestStatus = "in_review"
	RequestAccepted  RefundRequestStatus = "accepted"
	RequestRejected  RefundRequestStatus = "rejected"
	RequestWithdrawn RefundRequestStatus = "withdrawn"
)

// RefundRequestStatuses lists every status in 000156 order.
func RefundRequestStatuses() []RefundRequestStatus {
	return []RefundRequestStatus{RequestOpen, RequestInReview, RequestAccepted, RequestRejected, RequestWithdrawn}
}

// Valid reports whether s is a refund_requests.status value.
func (s RefundRequestStatus) Valid() bool { return slices.Contains(RefundRequestStatuses(), s) }

// Pending reports a request that waits for finance (open or in_review):
// it holds its amount (refund_requests_line_pending_uq: one per line).
func (s RefundRequestStatus) Pending() bool { return s == RequestOpen || s == RequestInReview }

// RequestReasons are the reasons a buyer gives (refund_requests_reason_chk):
// the refund reasons that are the buyer's to state (not an overpayment,
// which finance finds itself, nor goodwill, which is Koro's gesture).
func RequestReasons() []string {
	return []string{RefundReasonNotFulfilled, RefundReasonEventCancelled, RefundReasonOfferingCancelled,
		RefundReasonDuplicatePayment, RefundReasonOther}
}

// Limits of a refund request (000156 CHECKs; the OpenAPI bounds come from
// them, refund_request_limits.go).
const (
	minExplanation     = 10
	maxExplanation     = 2000
	maxRequestDecision = 1000 // rejection_reason
)

// DefaultRefundRequestWindow is how long after a line's delivery its buyer
// may ask for a refund (PAYMENTS_REFUND_REQUEST_WINDOW; BE-132 default, a
// user question): 30 days. An undelivered line has no deadline.
const DefaultRefundRequestWindow = 30 * 24 * time.Hour

// RefundRequest is one refund_requests row. The request (order, line,
// buyer, seller, amount, reason, explanation, IBAN, evidence, requester)
// never changes (refund_requests_frozen); the review, the decision and the
// withdrawal once set are final. org_id is the order's buyer and
// seller_org_id the line's seller (06 H-38: from the rows, never the
// request); order_no and line_title are copies of frozen values. The
// target IBAN may be a sole trader's (personal data): responses show it
// masked; the refund an acceptance opens carries it on.
type RefundRequest struct {
	ID              uuid.UUID           `gorm:"type:uuid;column:id;primaryKey"`
	OrderID         uuid.UUID           `gorm:"type:uuid;column:order_id"`
	OrderLineID     uuid.UUID           `gorm:"type:uuid;column:order_line_id"`
	OrgID           uuid.UUID           `gorm:"type:uuid;column:org_id"`
	SellerOrgID     uuid.UUID           `gorm:"type:uuid;column:seller_org_id"`
	OrderNo         string              `gorm:"column:order_no"`
	LineTitle       string              `gorm:"column:line_title"`
	Amount          int64               `gorm:"column:amount"`
	Currency        string              `gorm:"column:currency"`
	Reason          string              `gorm:"column:reason"`
	Explanation     string              `gorm:"column:explanation"`
	TargetIBAN      string              `gorm:"column:target_iban"`
	EvidenceFileID  *uuid.UUID          `gorm:"type:uuid;column:evidence_file_id"`
	Status          RefundRequestStatus `gorm:"column:status"`
	RequestedBy     uuid.UUID           `gorm:"type:uuid;column:requested_by"`
	ReviewedBy      *uuid.UUID          `gorm:"type:uuid;column:reviewed_by"`
	ReviewedAt      *time.Time          `gorm:"column:reviewed_at"`
	DecidedBy       *uuid.UUID          `gorm:"type:uuid;column:decided_by"`
	DecidedAt       *time.Time          `gorm:"column:decided_at"`
	RejectionReason *string             `gorm:"column:rejection_reason"`
	RefundID        *uuid.UUID          `gorm:"type:uuid;column:refund_id"`
	WithdrawnBy     *uuid.UUID          `gorm:"type:uuid;column:withdrawn_by"`
	WithdrawnAt     *time.Time          `gorm:"column:withdrawn_at"`
	CreatedAt       time.Time           `gorm:"column:created_at;->"`
	UpdatedAt       time.Time           `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (RefundRequest) TableName() string { return "refund_requests" }

// MaskedIBAN is the target IBAN as responses and audit records show it.
func (r RefundRequest) MaskedIBAN() string { return iban.Mask(r.TargetIBAN) }

// requestResource is the authz view of a request (BE-132): the buyer, the
// line's seller, the status and the requester (the decisions' four eyes).
func requestResource(r RefundRequest) authz.Resource {
	org := r.OrgID
	return authz.Resource{Type: authz.TypeRefundRequest, ID: r.ID, OrgID: &org, Attrs: map[string]string{
		authz.AttrStatus: string(r.Status), authz.AttrOrderLineSellerOrgID: r.SellerOrgID.String(),
		authz.AttrRequestedBy: r.RequestedBy.String(),
	}}
}

// requestOwner is the files owner of a request's evidence.
func requestOwner(id uuid.UUID) files.Owner {
	return files.Owner{Module: moduleName, EntityID: id, Purpose: files.PurposeRefundRequestEvidence}
}

// RefundRequestStore is the persistence of the refund requests (an
// aggregate of its own: Store embeds it, 05 §2.2).
type RefundRequestStore interface {
	// InsertRefundRequest writes a new request (open). A second waiting
	// request of the line is ErrRefundRequestOpen
	// (refund_requests_line_pending_uq).
	InsertRefundRequest(ctx context.Context, r RefundRequest) error
	// GetRefundRequest returns a request (FOR UPDATE when lock) or ErrNotFound.
	GetRefundRequest(ctx context.Context, id uuid.UUID, lock bool) (RefundRequest, error)
	// ListRefundRequests returns a page of requests after the cursor row,
	// newest first (created_at DESC, id DESC) or, with f.Oldest, oldest
	// first (finance's queue).
	ListRefundRequests(ctx context.Context, f RefundRequestFilter, after *TimeCursor, limit int) ([]RefundRequest, error)
	// RefundRequestsOfOrder returns every request of an order (any status),
	// oldest first: the amounts its pending requests hold.
	RefundRequestsOfOrder(ctx context.Context, orderID uuid.UUID) ([]RefundRequest, error)
	// UpdateRefundRequest writes the running columns of next (status, the
	// review, the decision, the refund, the withdrawal) when the row is
	// still in status from; otherwise ErrInvalidState (ErrNotFound for a
	// missing row). The eyes CHECKs answer authz.ErrFourEyes.
	UpdateRefundRequest(ctx context.Context, next RefundRequest, from RefundRequestStatus) (RefundRequest, error)
	// RefundRequestsOfUser returns the requests a user opened, reviewed,
	// decided or withdrew (KVKK export).
	RefundRequestsOfUser(ctx context.Context, userID uuid.UUID) ([]RefundRequest, error)
}

// RefundRequestFilter narrows ListRefundRequests. Zero fields do not filter.
type RefundRequestFilter struct {
	OrgID       *uuid.UUID // the buyer
	SellerOrgID *uuid.UUID // the line's seller
	OrderID     *uuid.UUID
	Status      RefundRequestStatus
	Pending     bool // open or in_review
	Oldest      bool // created_at ASC, id ASC (finance's queue)
}

// ---- the service's views

// RequestAudience says whose view of a request a reader gets.
type RequestAudience string

// Request audiences: the buying company's members, the selling org's staff
// (no IBAN, no buyer user: 03 k26's spirit) and platform staff.
const (
	AudienceBuyer  RequestAudience = "buyer"
	AudienceSeller RequestAudience = "seller"
	AudienceStaff  RequestAudience = "staff"
)

// RefundRequestView is a request with the names of its orgs, as one
// audience sees it.
type RefundRequestView struct {
	Request    RefundRequest
	BuyerName  string
	SellerName string
	Audience   RequestAudience
	// Refund is the refund an accepted request opened (the detail reads).
	Refund *Refund
}

// RefundRequestPage is a page of requests.
type RefundRequestPage struct {
	Items []RefundRequestView
	Next  *TimeCursor // nil on the last page
}

// RefundRequestQuery is a list's query; invalid carries a malformed
// parameter (422 after authz).
type RefundRequestQuery struct {
	Status  RefundRequestStatus
	Pending bool
	OrderID *uuid.UUID
	Limit   int
	After   *TimeCursor

	invalid error
}

// RefundRequestDetail is adminGetRefundRequest: the request, what the line
// still holds for a refund now and the order's payments that can fund it.
type RefundRequestDetail struct {
	RefundRequestView
	LineRefundable int64
	Payments       []FundingPayment
}

// Line eligibility codes (RefundOption.Blocked; "" is eligible).
const (
	BlockedOrderState  = "order_state"    // the order is not paid, or refunded in full
	BlockedLineState   = "line_cancelled" // the line is cancelled: finance refunds it (refunds due)
	BlockedWindow      = "window_closed"  // the request window after delivery has passed
	BlockedOpenRequest = "open_request"   // the line has a waiting request
	BlockedNothingLeft = "nothing_left"   // Koro holds nothing more of the line
)

// BlockedReasons lists every eligibility code.
func BlockedReasons() []string {
	return []string{BlockedOrderState, BlockedLineState, BlockedWindow, BlockedOpenRequest, BlockedNothingLeft}
}

// RefundOption is what the buyer may still ask of one line of an order.
type RefundOption struct {
	LineID      uuid.UUID
	Title       string
	SellerOrgID uuid.UUID
	// Refundable is the most a request may ask (kuruş): what Koro holds of
	// the line (its buyer fee only in model C) minus what was refunded or
	// is promised by an open refund, within what the order still holds
	// less its other waiting requests.
	Refundable int64
	// Deadline is the end of the request window (the line's delivery + the
	// window); nil while the line is undelivered.
	Deadline    *time.Time
	OpenRequest *uuid.UUID
	Blocked     string
}

// RefundOptions is listOrderRefundRequests: one order's lines as refund
// options and its requests.
type RefundOptions struct {
	OrderID  uuid.UUID
	OrderNo  string
	Status   string // the order's status
	Currency string
	Lines    []RefundOption
	Requests []RefundRequestView
}

// RefundRequestInput is the body of createOrderRefundRequest.
type RefundRequestInput struct {
	OrderLineID    uuid.UUID
	Amount         int64 // kuruş
	Reason         string
	Explanation    string
	TargetIBAN     string
	EvidenceFileID *uuid.UUID
}

// AcceptInput is the body of adminAcceptRefundRequest: the refund it opens.
// Every field may stay empty: the payment is the order's first captured
// transfer that still holds the amount, the IBAN the buyer's, the reason
// the buyer's.
type AcceptInput struct {
	PaymentID    *uuid.UUID
	TargetIBAN   string
	RefundReason string
	Note         string
}

// RefundRequestService is the refund request part of Service (BE-132).
type RefundRequestService interface {
	// RefundOptions is listOrderRefundRequests (payments.refund_request.list).
	RefundOptions(ctx context.Context, a authz.Actor, orgID, orderID uuid.UUID) (RefundOptions, error)
	// CreateRefundRequest is createOrderRefundRequest (Idempotency-Key).
	CreateRefundRequest(ctx context.Context, a authz.Actor, orgID, orderID uuid.UUID, in RefundRequestInput) (RefundRequestView, error)
	// ListOrgRefundRequests is listOrgRefundRequests (the buyer's).
	ListOrgRefundRequests(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RefundRequestQuery) (RefundRequestPage, error)
	// ListSalesRefundRequests is listOrgSalesRefundRequests (the seller's).
	ListSalesRefundRequests(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RefundRequestQuery) (RefundRequestPage, error)
	// GetRefundRequest is getRefundRequest (the reader's audience).
	GetRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID) (RefundRequestView, error)
	// WithdrawRefundRequest is withdrawRefundRequest.
	WithdrawRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID) (RefundRequestView, error)
	// RefundRequestEvidence is downloadRefundRequestEvidence: a 5-minute URL.
	RefundRequestEvidence(ctx context.Context, a authz.Actor, id uuid.UUID) (files.Download, error)
	// AdminListRefundRequests is adminListRefundRequests (finance's queue).
	AdminListRefundRequests(ctx context.Context, a authz.Actor, q RefundRequestQuery) (RefundRequestPage, error)
	// AdminGetRefundRequest is adminGetRefundRequest.
	AdminGetRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID) (RefundRequestDetail, error)
	// ReviewRefundRequest is adminReviewRefundRequest (open → in_review).
	ReviewRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID) (RefundRequestView, error)
	// AcceptRefundRequest is adminAcceptRefundRequest (Idempotency-Key): the
	// refund is opened in the same transaction.
	AcceptRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID, in AcceptInput) (RefundRequestView, error)
	// RejectRefundRequest is adminRejectRefundRequest.
	RejectRefundRequest(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (RefundRequestView, error)
}
