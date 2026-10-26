package orders

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"
)

// ---- orders (02 §19.1)

// Status is orders.status.
type Status string

// Order statuses (02 §19.1). awaiting_payment → paid (the transfer is
// matched in full, payments BE-041) → in_fulfilment (a line is marked or
// delivered) → completed (every line is fulfilled or cancelled); side
// states: cancelled (before payment), expired (the payment window ended),
// disputed (a line is disputed), partially_refunded / refunded (refunds,
// BE-043).
const (
	StatusAwaitingPayment   Status = "awaiting_payment"
	StatusPaid              Status = "paid"
	StatusInFulfilment      Status = "in_fulfilment"
	StatusCompleted         Status = "completed"
	StatusCancelled         Status = "cancelled"
	StatusExpired           Status = "expired"
	StatusPartiallyRefunded Status = "partially_refunded"
	StatusRefunded          Status = "refunded"
	StatusDisputed          Status = "disputed"
)

// Statuses lists every order status in 02 order.
func Statuses() []Status {
	return []Status{StatusAwaitingPayment, StatusPaid, StatusInFulfilment, StatusCompleted, StatusCancelled, StatusExpired,
		StatusPartiallyRefunded, StatusRefunded, StatusDisputed}
}

// Valid reports whether s is an orders.status value.
func (s Status) Valid() bool {
	for _, v := range Statuses() {
		if v == s {
			return true
		}
	}
	return false
}

// PaidState reports whether the order has been paid in full (02
// orders_paid_chk: these states carry paid_at).
func (s Status) PaidState() bool {
	switch s {
	case StatusPaid, StatusInFulfilment, StatusCompleted, StatusPartiallyRefunded, StatusRefunded, StatusDisputed:
		return true
	}
	return false
}

// Open reports whether the order is still running (not cancelled,
// expired, completed or fully refunded): it blocks closing its buyer org
// (hooks.openCommerce) and holds its units (catalog per_buyer_limit).
func (s Status) Open() bool {
	switch s {
	case StatusAwaitingPayment, StatusPaid, StatusInFulfilment, StatusDisputed, StatusPartiallyRefunded:
		return true
	}
	return false
}

// Model codes (02 §18.3; the catalog's commerce policy decides).
const (
	ModelA  = "A"
	ModelA2 = "A2"
	ModelB  = "B"
	ModelC  = "C"
)

// Collection methods (02 §18.3).
const (
	CollectBankTransfer  = "bank_transfer"
	CollectIyzico        = "iyzico"
	CollectDirectToPayee = "direct_to_payee"
)

// Order is one orders row: a buyer company's purchase of one or more lines
// under one money model (T-12). Every amount is kuruş, computed per line
// by platform/money and frozen at placement (orders_frozen); only the
// running state (status, paid/refunded amounts and their times) changes.
type Order struct {
	ID                  uuid.UUID     `gorm:"type:uuid;column:id;primaryKey"`
	OrderNo             string        `gorm:"column:order_no"` // = the bank-transfer reference (KR + 8 Crockford)
	OrgID               uuid.UUID     `gorm:"type:uuid;column:org_id"`
	OrgKind             string        `gorm:"column:org_kind"`
	PlacedByUserID      uuid.UUID     `gorm:"type:uuid;column:placed_by_user_id"`
	BuyerPartyID        uuid.UUID     `gorm:"type:uuid;column:buyer_party_id"`
	BuyerSnapshot       BuyerSnapshot `gorm:"column:buyer_snapshot;type:jsonb"`
	ModelCode           string        `gorm:"column:model_code"`
	CollectionMethod    string        `gorm:"column:collection_method"`
	CollectionAccountID *uuid.UUID    `gorm:"type:uuid;column:collection_account_id"`
	Status              Status        `gorm:"column:status"`
	Currency            string        `gorm:"column:currency"`
	SubtotalAmount      int64         `gorm:"column:subtotal_amount"`
	VATAmount           int64         `gorm:"column:vat_amount"`
	VATWithheldAmount   int64         `gorm:"column:vat_withheld_amount"`
	BuyerFeeAmount      int64         `gorm:"column:buyer_fee_amount"`
	TotalAmount         int64         `gorm:"column:total_amount"`
	PayableAmount       int64         `gorm:"column:payable_amount"`
	KoroCollectAmount   int64         `gorm:"column:koro_collect_amount"`
	DirectToPayeeAmount int64         `gorm:"column:direct_to_payee_amount"`
	PaidAmount          int64         `gorm:"column:paid_amount"`
	RefundedAmount      int64         `gorm:"column:refunded_amount"`
	ExpiresAt           time.Time     `gorm:"column:expires_at"`
	PlacedAt            time.Time     `gorm:"column:placed_at"`
	DirectConfirmedAt   *time.Time    `gorm:"column:direct_confirmed_at"`
	PaidAt              *time.Time    `gorm:"column:paid_at"`
	CompletedAt         *time.Time    `gorm:"column:completed_at"`
	CancelledAt         *time.Time    `gorm:"column:cancelled_at"`
	CancelReason        *string       `gorm:"column:cancel_reason"`
	CreatedAt           time.Time     `gorm:"column:created_at;->"`
	UpdatedAt           time.Time     `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Order) TableName() string { return "orders" }

// DueAmount is what Koro still waits for (koro_collect_amount − paid_amount).
func (o Order) DueAmount() int64 { return o.KoroCollectAmount - o.PaidAmount }

// DirectToPayee reports a model C order (02 §18.3): the buyer pays the
// payees directly and Koro collects only its fee, so Koro holds no seller
// money of it — no payout, and a refund through Koro covers the fee only.
func (o Order) DirectToPayee() bool { return o.CollectionMethod == CollectDirectToPayee }

// KoroHeld is what Koro collected for line l of order o: its payable
// amount, or only its buyer fee when the buyer paid the payee directly
// (model C). A refund through Koro never exceeds it.
func (o Order) KoroHeld(l Line) int64 {
	if o.DirectToPayee() {
		return l.BuyerFeeAmount
	}
	return l.PayableAmount
}

// Refundable is what may still be refunded through Koro on line l of o.
func (o Order) Refundable(l Line) int64 { return o.KoroHeld(l) - l.RefundedAmount }

// BuyerSnapshot is orders.buyer_snapshot: the billed party as it was at
// placement (04 §2.13: orders keep their own copy; a later change of the
// party never touches an order). Legal entity data (title, tax office,
// VKN, address, invoice e-mail), never a national ID.
type BuyerSnapshot struct {
	Title     string          `json:"title"`
	TaxOffice string          `json:"tax_office,omitempty"`
	TaxNo     string          `json:"tax_no,omitempty"`
	Address   SnapshotAddress `json:"address,omitzero"`
	Email     string          `json:"email,omitempty"`
}

// SnapshotAddress is the address of BuyerSnapshot.
type SnapshotAddress struct {
	Line1      string `json:"line1,omitempty"`
	District   string `json:"district,omitempty"`
	City       string `json:"city,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

// Value implements driver.Valuer (jsonb object).
func (b BuyerSnapshot) Value() (driver.Value, error) {
	out, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("orders: buyer snapshot: %w", err)
	}
	return string(out), nil
}

// Scan implements sql.Scanner.
func (b *BuyerSnapshot) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	case nil:
		*b = BuyerSnapshot{}
		return nil
	default:
		return fmt.Errorf("orders: buyer snapshot: cannot scan %T", src)
	}
	var out BuyerSnapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("orders: buyer snapshot: %w", err)
	}
	*b = out
	return nil
}

// ---- order lines (02 §19.2)

// Fulfillment is order_lines.fulfillment_status.
type Fulfillment string

// Fulfilment states (02 §19.2). A line of the seller: pending → marked (the
// seller says it is delivered; the buyer's dispute window opens) →
// fulfilled (the buyer confirms, the window elapses or an admin resolves a
// dispute) or disputed. A platform-native line (a Koro feature right) is
// not_applicable until payment and fulfilled (auto_platform) at payment. A
// line of a cancelled offering (or a dispute resolved with a refund) is
// cancelled: its money waits for a refund (RefundsDue, BE-043).
const (
	FulfillNotApplicable Fulfillment = "not_applicable"
	FulfillPending       Fulfillment = "pending"
	FulfillMarked        Fulfillment = "marked"
	FulfillFulfilled     Fulfillment = "fulfilled"
	FulfillDisputed      Fulfillment = "disputed"
	FulfillCancelled     Fulfillment = "cancelled"
)

// Fulfillments lists every fulfilment state in 02 order.
func Fulfillments() []Fulfillment {
	return []Fulfillment{FulfillNotApplicable, FulfillPending, FulfillMarked, FulfillFulfilled, FulfillDisputed, FulfillCancelled}
}

// Valid reports whether f is an order_lines.fulfillment_status value.
func (f Fulfillment) Valid() bool {
	for _, v := range Fulfillments() {
		if v == f {
			return true
		}
	}
	return false
}

// Done reports whether nothing is left to deliver on the line.
func (f Fulfillment) Done() bool { return f == FulfillFulfilled || f == FulfillCancelled }

// FulfilledVia is order_lines.fulfilled_via.
type FulfilledVia string

// How a line was fulfilled.
const (
	ViaBuyerConfirmed FulfilledVia = "buyer_confirmed"
	ViaWindowElapsed  FulfilledVia = "window_elapsed"
	ViaAdmin          FulfilledVia = "admin"
	ViaAutoPlatform   FulfilledVia = "auto_platform"
)

// FulfilledVias lists every order_lines.fulfilled_via value.
func FulfilledVias() []FulfilledVia {
	return []FulfilledVia{ViaBuyerConfirmed, ViaWindowElapsed, ViaAdmin, ViaAutoPlatform}
}

// ModelCodes lists every orders.model_code value (02 §18.3).
func ModelCodes() []string { return []string{ModelA, ModelA2, ModelB, ModelC} }

// Line is one order_lines row: an offering bought in a quantity, with the
// payee, the commerce policy (its copy) and every amount frozen at
// placement (order_lines_frozen). The running columns are the fulfilment
// (with the seller's delivery evidence), the refunded amount and the payout.
type Line struct {
	ID                   uuid.UUID     `gorm:"type:uuid;column:id;primaryKey"`
	OrderID              uuid.UUID     `gorm:"type:uuid;column:order_id"`
	OrgID                uuid.UUID     `gorm:"type:uuid;column:org_id"`        // the buyer (= orders.org_id)
	SellerOrgID          uuid.UUID     `gorm:"type:uuid;column:seller_org_id"` // = offerings.org_id
	PayeeID              *uuid.UUID    `gorm:"type:uuid;column:payee_id"`      // nil for the platform's own lines
	OfferingID           uuid.UUID     `gorm:"type:uuid;column:offering_id"`
	ServiceID            uuid.UUID     `gorm:"type:uuid;column:service_id"`
	EventID              *uuid.UUID    `gorm:"type:uuid;column:event_id"`
	CommercePolicyID     uuid.UUID     `gorm:"type:uuid;column:commerce_policy_id"`
	TitleSnapshot        string        `gorm:"column:title_snapshot"`
	DeliverablesSnapshot Deliverables  `gorm:"column:deliverables_snapshot;type:jsonb"`
	Quantity             int           `gorm:"column:quantity"`
	UnitPriceAmount      int64         `gorm:"column:unit_price_amount"`
	NetAmount            int64         `gorm:"column:net_amount"`
	VATRateBP            int           `gorm:"column:vat_rate_bp"`
	VATAmount            int64         `gorm:"column:vat_amount"`
	VATWithholdingNum    *int16        `gorm:"column:vat_withholding_num"`
	VATWithholdingDen    *int16        `gorm:"column:vat_withholding_den"`
	VATWithheldAmount    int64         `gorm:"column:vat_withheld_amount"`
	GrossAmount          int64         `gorm:"column:gross_amount"`
	CommissionRateBP     int           `gorm:"column:commission_rate_bp"`
	CommissionBasis      string        `gorm:"column:commission_basis"`
	CommissionPayer      string        `gorm:"column:commission_payer"`
	CommissionAmount     int64         `gorm:"column:commission_amount"`
	CommissionVATRateBP  int           `gorm:"column:commission_vat_rate_bp"`
	CommissionVATAmount  int64         `gorm:"column:commission_vat_amount"`
	DisputeWindowDays    int16         `gorm:"column:dispute_window_days"`
	PayoutHoldDays       int16         `gorm:"column:payout_hold_days"`
	BuyerFeeAmount       int64         `gorm:"column:buyer_fee_amount"`
	SellerNetAmount      int64         `gorm:"column:seller_net_amount"`
	PayableAmount        int64         `gorm:"column:payable_amount"`
	FulfillmentStatus    Fulfillment   `gorm:"column:fulfillment_status"`
	FulfillmentMarkedAt  *time.Time    `gorm:"column:fulfillment_marked_at"`
	FulfillmentMarkedBy  *uuid.UUID    `gorm:"type:uuid;column:fulfillment_marked_by"`
	DisputeWindowEndsAt  *time.Time    `gorm:"column:dispute_window_ends_at"`
	FulfilledAt          *time.Time    `gorm:"column:fulfilled_at"`
	FulfilledVia         *FulfilledVia `gorm:"column:fulfilled_via"`
	DisputeReason        *string       `gorm:"column:dispute_reason"`
	RefundedAmount       int64         `gorm:"column:refunded_amount"`
	PayoutID             *uuid.UUID    `gorm:"type:uuid;column:payout_id"`
	PaidOutAt            *time.Time    `gorm:"column:paid_out_at"`
	CreatedAt            time.Time     `gorm:"column:created_at;->"`
	UpdatedAt            time.Time     `gorm:"column:updated_at;->"`

	// FulfillmentEvidenceFileID is the seller's delivery evidence (a
	// delivery_evidence file of the seller org, attached when the line is
	// marked; BE-093). Once set it never changes (order_lines_evidence_frozen).
	FulfillmentEvidenceFileID *uuid.UUID `gorm:"type:uuid;column:fulfillment_evidence_file_id"`
}

// TableName implements gorm's Tabler.
func (Line) TableName() string { return "order_lines" }

// PlatformNative reports whether the line is a Koro feature right (no
// seller delivery: not_applicable until payment).
func (l Line) PlatformNative() bool {
	return l.PayeeID == nil && l.FulfillmentStatus == FulfillNotApplicable
}

// Deliverable is one item of order_lines.deliverables_snapshot.
type Deliverable struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Deliverables is the jsonb array of order_lines.deliverables_snapshot
// (never NULL).
type Deliverables []Deliverable

// Value implements driver.Valuer: an empty list is '[]'.
func (d Deliverables) Value() (driver.Value, error) {
	if d == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]Deliverable(d))
	if err != nil {
		return nil, fmt.Errorf("orders: deliverables: %w", err)
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (d *Deliverables) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case nil:
		*d = Deliverables{}
		return nil
	case []byte:
		raw = bytes.Clone(v)
	case string:
		raw = []byte(v)
	default:
		return errors.New("orders: deliverables: unsupported source")
	}
	var out []Deliverable
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("orders: deliverables: %w", err)
	}
	if out == nil {
		out = []Deliverable{}
	}
	*d = out
	return nil
}
