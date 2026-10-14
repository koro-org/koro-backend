package orders

import (
	"time"
	"uuid"
)

// ---- inputs of the HTTP use cases

// CreateInput is the body of createOrder (04 §2.15): what to buy, never a
// price (06 H-6: amounts are the server's).
type CreateInput struct {
	BuyerPartyID uuid.UUID
	Lines        []LineInput
}

// LineInput is one line of CreateInput.
type LineInput struct {
	OfferingID uuid.UUID
	Quantity   int
}

// ListQuery narrows listOrgOrders and adminListOrders (the admin filters
// OrgID, OrderNo and the placement window are ignored on the org list).
type ListQuery struct {
	Status      Status
	OrgID       *uuid.UUID
	OrderNo     string
	PlacedFrom  *time.Time
	PlacedUntil *time.Time
	Limit       int
	After       *TimeKey
}

// SalesQuery narrows listOrgSales.
type SalesQuery struct {
	Fulfillment Fulfillment
	Limit       int
	After       *TimeKey
}

// MarkInput is the body of markOrderLineFulfilled.
type MarkInput struct {
	Note           string
	EvidenceFileID *uuid.UUID
}

// Dispute resolutions (04 §2.15 adminResolveOrderLineDispute).
const (
	OutcomeFulfilled = "fulfilled" // the seller delivered: the line is fulfilled (admin)
	OutcomeRefund    = "refund"    // the line is cancelled; its money waits for a refund (BE-043)
)

// ResolveInput is the body of adminResolveOrderLineDispute.
type ResolveInput struct {
	Outcome      string
	Note         string
	RefundAmount *int64 // refund only: at most the line's refundable amount (the whole of it when nil)
}

// ---- views

// OrderView is one order with its lines for getOrder and createOrder.
// SellerView is 03 k26: the caller acts for a seller org, so Lines holds
// only that seller's lines and the DTO shows the buyer's title, never the
// order totals or the payment.
type OrderView struct {
	Order      Order
	Lines      []Line
	Sellers    map[uuid.UUID]string // seller org → name
	SellerView bool
}

// OrderPage is a page of orders.
type OrderPage struct {
	Items []Order
	Next  *TimeKey
}

// Instructions are the payment instructions of an order (04 §C.3).
type Instructions struct {
	ReferenceCode string
	AmountDue     int64
	Currency      string
	IBAN          string
	HolderName    string
	BankName      string
	ExpiresAt     time.Time
}

// SalesItem is one sold line of listOrgSales with its order's reference
// and the buyer's title.
type SalesItem struct {
	Line        Line
	OrderNo     string
	BuyerTitle  string
	OrderStatus Status
}

// SalesPage is a page of sold lines.
type SalesPage struct {
	Items []SalesItem
	Next  *TimeKey
}

// LineView is one line after a delivery action.
type LineView struct {
	Line        Line
	OrderNo     string
	OrderStatus Status
}

// ---- the contract of payments, ledger and billing

// PayableOrder is what payments needs to match money to an order.
type PayableOrder struct {
	ID                uuid.UUID
	OrderNo           string
	OrgID             uuid.UUID
	Status            Status
	ModelCode         string
	CollectionMethod  string
	Currency          string
	KoroCollectAmount int64 // what Koro collects in total
	PaidAmount        int64 // captured so far
	DueAmount         int64 // KoroCollectAmount − PaidAmount
	RefundedAmount    int64
	ExpiresAt         time.Time
}

func payableOf(o Order) PayableOrder {
	return PayableOrder{
		ID: o.ID, OrderNo: o.OrderNo, OrgID: o.OrgID, Status: o.Status, ModelCode: o.ModelCode,
		CollectionMethod: o.CollectionMethod, Currency: o.Currency, KoroCollectAmount: o.KoroCollectAmount,
		PaidAmount: o.PaidAmount, DueAmount: o.DueAmount(), RefundedAmount: o.RefundedAmount, ExpiresAt: o.ExpiresAt,
	}
}

// PaymentEffect is the result of ApplyPayment and ConfirmDirectPayment.
// Lines is set once the order is paid (Paid): payments writes one
// payment_splits row per line and ledger allocates the order
// (order_allocated) from them.
type PaymentEffect struct {
	OrderID    uuid.UUID
	OrgID      uuid.UUID
	ModelCode  string
	PaidAmount int64 // after this payment
	DueAmount  int64 // still due after this payment
	Paid       bool  // this payment completed the order
	Lines      []PaidLine
}

// PaidLine is the split of one line of a paid order (02 §20.4 seller /
// platform amounts; 02 §21.3 order_allocated).
type PaidLine struct {
	LineID              uuid.UUID
	OfferingID          uuid.UUID
	SellerOrgID         uuid.UUID
	PayeeID             *uuid.UUID // nil: the platform's own line
	CommissionPayer     string
	PayableAmount       int64 // what the buyer paid for the line
	SellerNetAmount     int64 // the payee's share (hakediş)
	CommissionAmount    int64
	CommissionVATAmount int64
	BuyerFeeAmount      int64 // commission + its VAT when the buyer pays it
	VATWithheldAmount   int64
}

func paidLineOf(l Line) PaidLine {
	return PaidLine{
		LineID: l.ID, OfferingID: l.OfferingID, SellerOrgID: l.SellerOrgID, PayeeID: l.PayeeID, CommissionPayer: l.CommissionPayer,
		PayableAmount: l.PayableAmount, SellerNetAmount: l.SellerNetAmount, CommissionAmount: l.CommissionAmount,
		CommissionVATAmount: l.CommissionVATAmount, BuyerFeeAmount: l.BuyerFeeAmount, VATWithheldAmount: l.VATWithheldAmount,
	}
}

// PayableLine is a line a payout may take (ledger, BE-044).
type PayableLine struct {
	LineID          uuid.UUID
	OrderID         uuid.UUID
	OrderNo         string
	PayeeID         uuid.UUID
	SellerOrgID     uuid.UUID
	SellerNetAmount int64
	FulfilledAt     time.Time
}
