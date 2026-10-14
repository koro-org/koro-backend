package ledger

import (
	"time"
	"uuid"

	"koro/internal/platform/iban"
)

// PayoutStatus is payouts.status (02 §21.4).
type PayoutStatus string

// Payout states: pending_approval → approved → paid (final) | failed;
// pending_approval or approved → cancelled. failed and cancelled release
// the lines (their items are deleted).
const (
	StatusPendingApproval PayoutStatus = "pending_approval"
	StatusApproved        PayoutStatus = "approved"
	StatusPaid            PayoutStatus = "paid"
	StatusFailed          PayoutStatus = "failed"
	StatusCancelled       PayoutStatus = "cancelled"
)

// Valid reports whether s is a known state.
func (s PayoutStatus) Valid() bool {
	switch s {
	case StatusPendingApproval, StatusApproved, StatusPaid, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Open reports whether the payout still holds its lines and may move money
// (pending_approval, approved): the closeOrg check and the pending amount.
func (s PayoutStatus) Open() bool { return s == StatusPendingApproval || s == StatusApproved }

// Live reports whether the payout keeps its items (every state but failed
// and cancelled, whose items are deleted).
func (s PayoutStatus) Live() bool { return s != StatusFailed && s != StatusCancelled }

// Payout is one payouts row (02 §21.4; 000113). The payee, the seller org,
// the account, the IBAN and holder snapshots, the amount and the creator
// are frozen at creation (payouts_frozen); the approver once set; a paid
// payout is final. Writes are targeted updates of the state columns (06
// H-56), never a Save of the row.
type Payout struct {
	ID             uuid.UUID    `gorm:"type:uuid;column:id;primaryKey"`
	PayeeID        uuid.UUID    `gorm:"type:uuid;column:payee_id"`
	OrgID          uuid.UUID    `gorm:"type:uuid;column:org_id"` // the seller org = payees.org_id (06 H-38)
	BankAccountID  uuid.UUID    `gorm:"type:uuid;column:bank_account_id"`
	IBANSnapshot   string       `gorm:"column:iban_snapshot" json:"-"`
	HolderSnapshot string       `gorm:"column:holder_snapshot"`
	Amount         int64        `gorm:"column:amount"`
	Currency       string       `gorm:"column:currency"`
	Status         PayoutStatus `gorm:"column:status"`
	CreatedBy      uuid.UUID    `gorm:"type:uuid;column:created_by"`
	ApprovedBy     *uuid.UUID   `gorm:"type:uuid;column:approved_by"`
	ApprovedAt     *time.Time   `gorm:"column:approved_at"`
	PaidBy         *uuid.UUID   `gorm:"type:uuid;column:paid_by"`
	PaidAt         *time.Time   `gorm:"column:paid_at"`
	BankReference  *string      `gorm:"column:bank_reference"`
	ReceiptFileID  *uuid.UUID   `gorm:"type:uuid;column:receipt_file_id"`
	FailureReason  *string      `gorm:"column:failure_reason"`
	CancelledAt    *time.Time   `gorm:"column:cancelled_at"`
	CancelReason   *string      `gorm:"column:cancel_reason"`
	CreatedAt      time.Time    `gorm:"column:created_at"`
	UpdatedAt      time.Time    `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Payout) TableName() string { return "payouts" }

// MaskedIBAN is the IBAN snapshot as the organisation sees it (04 §2.17
// listOrgPayouts: "IBAN maskeli").
func (p Payout) MaskedIBAN() string { return iban.Mask(p.IBANSnapshot) }

// PayoutItem is one payout_items row (02 §21.5; 000115): a line the payout
// pays and its amount (the line's seller_net_amount; T-291: a refunded line
// is never payable, so there is no refund share to subtract).
type PayoutItem struct {
	PayoutID    uuid.UUID `gorm:"type:uuid;column:payout_id;primaryKey"`
	OrderLineID uuid.UUID `gorm:"type:uuid;column:order_line_id;primaryKey"`
	PayeeID     uuid.UUID `gorm:"type:uuid;column:payee_id"`
	Amount      int64     `gorm:"column:amount"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (PayoutItem) TableName() string { return "payout_items" }

// PayoutFilter narrows a payout list.
type PayoutFilter struct {
	OrgID   *uuid.UUID   // the seller org (org surface); nil = every org (admin)
	PayeeID *uuid.UUID   // nil = every payee
	Status  PayoutStatus // "" = every state
}

// PayoutCursor is the keyset position of a payout page (newest first).
type PayoutCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// PayoutAmounts are the payout totals of one payee (getPayeeBalance).
type PayoutAmounts struct {
	Pending int64 // Σ amount of pending_approval and approved payouts
	PaidOut int64 // Σ amount of paid payouts
}

// PayoutItemView is one item of a payout as the operations return it.
type PayoutItemView struct {
	OrderLineID uuid.UUID
	OrderID     uuid.UUID
	OrderNo     string
	Amount      int64
}

// PayoutView is a payout with its items. ReceiptURL is set on the org
// surface for a paid payout (a 5-minute download URL of the receipt).
type PayoutView struct {
	Payout
	Items      []PayoutItemView
	ReceiptURL string
	ReceiptExp *time.Time
}

// PayoutPage is a page of payouts, newest first.
type PayoutPage struct {
	Items []Payout
	Next  *PayoutCursor // nil on the last page
}

// PayoutQuery narrows listOrgPayouts and adminListPayouts.
type PayoutQuery struct {
	Status PayoutStatus // admin only; "" = every state
	Limit  int
	After  *PayoutCursor

	// invalid is a malformed parameter or cursor found by the handler,
	// reported after authorisation (03 §7.2).
	invalid error
}

// Balance is getPayeeBalance (04 §2.17): what Koro owes the payee now
// (Payable, the ledger's payee balance: allocated seller shares minus paid
// payouts, open payouts included), the part of it in open payouts
// (PendingPayout) and what was paid out so far (PaidOut).
type Balance struct {
	PayeeID       uuid.UUID
	Payable       int64
	PendingPayout int64
	PaidOut       int64
}

// PayableLineView is a line a payout may take (adminListPayables).
type PayableLineView struct {
	OrderLineID uuid.UUID
	OrderID     uuid.UUID
	OrderNo     string
	Amount      int64 // the line's seller_net_amount
	FulfilledAt time.Time
}

// PayableGroup is the payable lines of one payee.
type PayableGroup struct {
	PayeeID uuid.UUID
	OrgID   uuid.UUID
	Lines   []PayableLineView
	Total   int64
}

// PayablesQuery narrows adminListPayables.
type PayablesQuery struct {
	PayeeID *uuid.UUID // nil = every payee Koro owes money to
	AsOf    *time.Time // nil = now: the payout hold is measured at this time

	invalid error
}

// CreatePayoutInput is adminCreatePayout's body.
type CreatePayoutInput struct {
	PayeeID uuid.UUID
	// LineIDs, when given, are the lines to pay (each must be payable);
	// empty takes every payable line of the payee.
	LineIDs []uuid.UUID
}

// MarkPaidInput is adminMarkPayoutPaid's body.
type MarkPaidInput struct {
	BankReference string
	ReceiptFileID *uuid.UUID // required (422 when missing)
	PaidAt        *time.Time // nil = now
}
