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
	// DirectStatus is set only for a reader who may see the direct
	// transfers (getOrder, BE-109): an unpaid, unconfirmed model C order
	// read by an MFA-verified buyer administrator. Empty for every other
	// reader and order. The transfers themselves are only in the payment
	// instructions (BE-127).
	DirectStatus DirectStatus
}

// DirectStatus tells the buyer's administrators whether getOrder could
// list the direct transfers of an unpaid model C order (BE-109). The
// order itself is readable either way; the payment instructions keep
// refusing (409) while the transfers are unavailable.
type DirectStatus string

const (
	// DirectAvailable: the payment instructions list every transfer.
	DirectAvailable DirectStatus = "available"
	// DirectUnavailable: a payee of the order has no approved bank account
	// now (revoked or never approved; parties.no_approved_account), or may
	// not be paid directly (06 H-54). The instructions answer 409: no payee
	// account is disclosed, not even the ones that are approved, so the
	// buyer never pays a part of the direct amount on partial instructions.
	DirectUnavailable DirectStatus = "unavailable"
)

// OrderPage is a page of orders.
type OrderPage struct {
	Items []Order
	Next  *TimeKey
}

// Instructions are the payment instructions of an order (04 §C.3).
// DirectInstruction is a distinct transfer to one frozen payee. It is
// returned only to the buyer's administrators while direct payment is due.
type DirectInstruction struct {
	PayeeID    uuid.UUID
	Title      string
	Amount     int64
	IBAN       string
	HolderName string
	BankName   string
}

type Instructions struct {
	ReferenceCode  string
	AmountDue      int64
	Currency       string
	IBAN           string
	HolderName     string
	BankName       string
	ExpiresAt      time.Time
	DirectPayments []DirectInstruction
}

// SalesItem is one sold line of listOrgSales with its order's reference
// and the buyer's title.
type SalesItem struct {
	Line        Line
	OrderNo     string
	BuyerTitle  string
	OrderStatus Status
	Currency    string
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
	ID                  uuid.UUID
	OrderNo             string
	OrgID               uuid.UUID
	Status              Status
	ModelCode           string
	CollectionMethod    string
	Currency            string
	DirectToPayeeAmount int64
	KoroCollectAmount   int64 // what Koro collects in total
	PaidAmount          int64 // captured so far
	DueAmount           int64 // KoroCollectAmount − PaidAmount
	RefundedAmount      int64
	ExpiresAt           time.Time
}

// KoroHeld is Order.KoroHeld for payments' refund limits: what Koro
// collected of line l (its buyer fee only in model C; nothing of a dropped
// line, BE-134).
func (o PayableOrder) KoroHeld(l Line) int64 {
	return koroHeld(o.CollectionMethod == CollectDirectToPayee, l)
}

func payableOf(o Order) PayableOrder {
	return PayableOrder{
		ID: o.ID, OrderNo: o.OrderNo, OrgID: o.OrgID, Status: o.Status, ModelCode: o.ModelCode,
		DirectToPayeeAmount: o.DirectToPayeeAmount, CollectionMethod: o.CollectionMethod, Currency: o.Currency, KoroCollectAmount: o.KoroCollectAmount,
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
// platform amounts; 02 §21.3 order_allocated). NetAmount and VATAmount are
// the sale of Koro's own platform-native line (its ledger revenue and VAT,
// BE-067); a payee line's sale is the payee's.
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
	NetAmount           int64 // the sale excluding VAT
	VATAmount           int64 // the sale's VAT
	VATWithheldAmount   int64
}

func paidLineOf(l Line) PaidLine {
	return PaidLine{
		LineID: l.ID, OfferingID: l.OfferingID, SellerOrgID: l.SellerOrgID, PayeeID: l.PayeeID, CommissionPayer: l.CommissionPayer,
		PayableAmount: l.PayableAmount, SellerNetAmount: l.SellerNetAmount, CommissionAmount: l.CommissionAmount,
		CommissionVATAmount: l.CommissionVATAmount, BuyerFeeAmount: l.BuyerFeeAmount, NetAmount: l.NetAmount, VATAmount: l.VATAmount,
		VATWithheldAmount: l.VATWithheldAmount,
	}
}

// Paid returns the frozen money of l as a line of a paid order (payments'
// refund of a line, BE-067).
func (l Line) Paid() PaidLine { return paidLineOf(l) }

// CollectedLine is a line of an order Koro collected in full: paid (paid_at
// set), not a direct-to-payee (model C) order. The ledger allocates every
// one of them (02 §21.3 order_allocated; ledger.integrity_check and the
// late recognition of Koro's own lines, BE-067).
type CollectedLine struct {
	PaidLine
	OrderID        uuid.UUID
	ModelCode      string
	RefundedAmount int64
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
