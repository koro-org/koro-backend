package payments

import (
	"time"
	"uuid"

	"koro/internal/orders"
	"koro/internal/platform/civil"
)

// Decision is the second eye's answer (adminApproveCollectionAccount,
// adminApproveIncomingTransferReturn).
type Decision string

// Decisions.
const (
	DecisionApproved Decision = "approved"
	DecisionRejected Decision = "rejected"
)

// Valid reports whether d is a decision.
func (d Decision) Valid() bool { return d == DecisionApproved || d == DecisionRejected }

// AccountInput is the body of adminCreateCollectionAccount (04 §2.16).
type AccountInput struct {
	Label        string
	LegalPartyID uuid.UUID // Koro's own party (kind platform)
	IBAN         string
	HolderName   string
	BankName     string
}

// TransferInput is the body of adminCreateIncomingTransfer (04 §2.16, §C.4).
type TransferInput struct {
	CollectionAccountID uuid.UUID
	ValueDate           civil.Date
	Amount              int64 // kuruş
	SenderName          string
	SenderIBAN          string
	Description         string
	BankReference       string
	ReceiptFileID       *uuid.UUID // a transfer_receipt upload
}

// ImportInput is the body of adminImportIncomingTransfers: the bank
// statement as CSV text (csvimport.go, T-294: files has no CSV purpose).
type ImportInput struct {
	CollectionAccountID uuid.UUID
	CSV                 string
}

// Import row outcomes.
const (
	RowImported  = "imported"
	RowDuplicate = "duplicate"
	RowRejected  = "rejected"
)

// ImportRow is the outcome of one data row (Line is the 1-based line of the
// CSV text, the header being line 1). Error is a fixed text, never a value.
type ImportRow struct {
	Line       int        `json:"line"`
	Outcome    string     `json:"outcome"`
	TransferID *uuid.UUID `json:"transfer_id,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// ImportResult is the answer of an import (04 §2.16: {imported, duplicates,
// rejected}, plus the outcome of every row). It survives a JSON round trip
// (Idempotency-Key replay).
type ImportResult struct {
	Imported   int         `json:"imported"`
	Duplicates int         `json:"duplicates"`
	Rejected   int         `json:"rejected"`
	Rows       []ImportRow `json:"rows"`
}

// TransferQuery narrows adminListIncomingTransfers.
type TransferQuery struct {
	Filter TransferFilter
	Limit  int
	After  *TransferCursor

	// invalid is a malformed query parameter or cursor found by the
	// handler; it is reported after authorisation (03 §7.2: 403 first).
	invalid error
}

// TransferPage is a page of transfers, newest value date first.
type TransferPage struct {
	Items []IncomingTransfer
	Next  *TransferCursor // nil on the last page
}

// Suggestion is an order a transfer may pay (04 §2.16: the reference code
// of its description and its amount). Score: 1 the amount is what the order
// waits for, 0.7 it is less (a partial payment), 0.5 it is more (an
// overpayment refund opens), 0 the order does not wait for a payment.
type Suggestion struct {
	OrderID     uuid.UUID
	OrderNo     string
	AmountDue   int64
	OrderStatus orders.Status
	Score       float64
}

// TransferView is a transfer with its suggestions (adminGetIncomingTransfer).
type TransferView struct {
	Transfer    IncomingTransfer
	Suggestions []Suggestion
}

// MatchInput is the body of adminMatchIncomingTransfer (04 §2.16, §C.4).
type MatchInput struct {
	OrderID         uuid.UUID
	ExpectedOrderNo string
	// RefundIBAN is where the overpayment goes back: the transfer's sender
	// IBAN when empty; required when the transfer pays more than the order
	// waits for and its sender IBAN is unknown.
	RefundIBAN string
}

// PaymentSummary is the payment part of MatchResult and DirectResult.
type PaymentSummary struct {
	PaymentID  uuid.UUID     `json:"payment_id"`
	Provider   Provider      `json:"provider"`
	Amount     int64         `json:"amount"`
	Status     PaymentStatus `json:"status"`
	CapturedAt time.Time     `json:"captured_at"`
}

// OrderSummary is the order part of MatchResult and DirectResult.
type OrderSummary struct {
	OrderID    uuid.UUID     `json:"order_id"`
	OrderNo    string        `json:"order_no"`
	Status     orders.Status `json:"status"`
	PaidAmount int64         `json:"paid_amount"`
	DueAmount  int64         `json:"due_amount"`
	Paid       bool          `json:"paid"` // this capture completed the order
}

// MatchResult is the answer of a match (04 §C.4 MatchResultDTO). It
// survives a JSON round trip (Idempotency-Key replay).
type MatchResult struct {
	TransferID          uuid.UUID      `json:"transfer_id"`
	Status              TransferStatus `json:"status"`
	Payment             PaymentSummary `json:"payment"`
	Order               OrderSummary   `json:"order"`
	AppliedAmount       int64          `json:"applied_amount"`
	OverpaymentRefundID *uuid.UUID     `json:"overpayment_refund_id"`
}

// ReturnRequest is the body of adminRequestIncomingTransferReturn.
type ReturnRequest struct {
	Reason     string
	TargetIBAN string // the sender's IBAN when empty
}

// ReturnReceipt is the body of adminMarkIncomingTransferReturned (04 §2.16).
type ReturnReceipt struct {
	BankReference string
	ReceiptFileID uuid.UUID // a refund_receipt upload
}

// DirectInput is the body of adminConfirmDirectPayment (model C).
type DirectInput struct {
	OrderID         uuid.UUID
	ExpectedOrderNo string
	EvidenceFileID  uuid.UUID // a transfer_receipt upload: the buyer's transfer to the payee
}

// DirectResult is the answer of a direct payment confirmation.
type DirectResult struct {
	Payment PaymentSummary `json:"payment"`
	Order   OrderSummary   `json:"order"`
}

func summaryOf(p Payment) PaymentSummary {
	s := PaymentSummary{PaymentID: p.ID, Provider: p.Provider, Amount: p.Amount, Status: p.Status}
	if p.CapturedAt != nil {
		s.CapturedAt = p.CapturedAt.UTC()
	}
	return s
}

func orderSummaryOf(o orders.PayableOrder, paid bool) OrderSummary {
	return OrderSummary{OrderID: o.ID, OrderNo: o.OrderNo, Status: o.Status, PaidAmount: o.PaidAmount, DueAmount: o.DueAmount, Paid: paid}
}
