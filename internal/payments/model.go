package payments

import (
	"time"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/iban"
)

// Currency is the only currency of the MVP (02 §20: CHECK currency = 'TRY').
const Currency = "TRY"

// ---- collection_accounts (02 §20.1; 000103)

// AccountStatus is collection_accounts.status.
type AccountStatus string

// Collection account statuses: a finance admin creates an account
// (pending_approval); another admin decides it (active or rejected); an
// active account may be made the default (where buyers pay) and is retired
// when it is no longer used.
const (
	AccountPending  AccountStatus = "pending_approval"
	AccountActive   AccountStatus = "active"
	AccountRejected AccountStatus = "rejected"
	AccountRetired  AccountStatus = "retired"
)

// Valid reports whether s is a collection_accounts.status value.
func (s AccountStatus) Valid() bool {
	switch s {
	case AccountPending, AccountActive, AccountRejected, AccountRetired:
		return true
	}
	return false
}

// PartyKindPlatform is collection_accounts.party_kind: the account is Koro's
// own (legal_parties.kind = 'platform').
const PartyKindPlatform = "platform"

// CollectionAccount is one collection_accounts row: a Koro bank account
// buyers pay into. The IBAN, the holder, the party and the creator never
// change (collection_accounts_frozen); a new IBAN is a new account.
type CollectionAccount struct {
	ID           uuid.UUID     `gorm:"type:uuid;column:id;primaryKey"`
	Label        string        `gorm:"column:label"`
	LegalPartyID uuid.UUID     `gorm:"type:uuid;column:legal_party_id"`
	PartyKind    string        `gorm:"column:party_kind"`
	IBAN         string        `gorm:"column:iban"`
	HolderName   string        `gorm:"column:holder_name"`
	BankName     string        `gorm:"column:bank_name"`
	Status       AccountStatus `gorm:"column:status"`
	IsDefault    bool          `gorm:"column:is_default"`
	CreatedBy    uuid.UUID     `gorm:"type:uuid;column:created_by"`
	ApprovedBy   *uuid.UUID    `gorm:"type:uuid;column:approved_by"` // the deciding admin (approve or reject)
	ApprovedAt   *time.Time    `gorm:"column:approved_at"`
	RetiredAt    *time.Time    `gorm:"column:retired_at"`
	CreatedAt    time.Time     `gorm:"column:created_at"`
	UpdatedAt    time.Time     `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (CollectionAccount) TableName() string { return "collection_accounts" }

// MaskedIBAN is the IBAN as audit records and security notices show it.
func (c CollectionAccount) MaskedIBAN() string { return iban.Mask(c.IBAN) }

// ---- incoming_bank_transfers (02 §20.2; 000105, 06 H-18)

// TransferStatus is incoming_bank_transfers.status.
type TransferStatus string

// Transfer statuses (06 H-18, H-56): a transfer is recorded unmatched; a
// finance admin matches it to an order (the payment capture) or ignores it;
// an unmatched (or ignored) transfer is sent back to its sender in three
// steps — one finance admin requests the return (return_requested),
// another approves it (return_approved), the EFT receipt closes it
// (returned). A rejected return request makes the transfer unmatched again.
const (
	TransferUnmatched       TransferStatus = "unmatched"
	TransferMatched         TransferStatus = "matched"
	TransferIgnored         TransferStatus = "ignored"
	TransferReturnRequested TransferStatus = "return_requested"
	TransferReturnApproved  TransferStatus = "return_approved"
	TransferReturned        TransferStatus = "returned"
)

// TransferStatuses lists every status in 02 order.
func TransferStatuses() []TransferStatus {
	return []TransferStatus{TransferUnmatched, TransferMatched, TransferIgnored, TransferReturnRequested,
		TransferReturnApproved, TransferReturned}
}

// Valid reports whether s is an incoming_bank_transfers.status value.
func (s TransferStatus) Valid() bool {
	for _, v := range TransferStatuses() {
		if v == s {
			return true
		}
	}
	return false
}

// EntryMethod is incoming_bank_transfers.entry_method.
type EntryMethod string

// Entry methods.
const (
	EntryManual    EntryMethod = "manual"
	EntryCSVImport EntryMethod = "csv_import"
	EntryBankAPI   EntryMethod = "bank_api"
)

// IncomingTransfer is one incoming_bank_transfers row: money that arrived
// at a Koro collection account. The bank data (account, value date,
// amount, reference, entry) never change (incoming_bank_transfers_frozen).
// The sender's name and IBAN may be a natural person's (02 §20.2).
type IncomingTransfer struct {
	ID                  uuid.UUID      `gorm:"type:uuid;column:id;primaryKey"`
	CollectionAccountID uuid.UUID      `gorm:"type:uuid;column:collection_account_id"`
	ValueDate           civil.Date     `gorm:"column:value_date;type:date"`
	Amount              int64          `gorm:"column:amount"`
	Currency            string         `gorm:"column:currency"`
	SenderName          *string        `gorm:"column:sender_name"`
	SenderIBAN          *string        `gorm:"column:sender_iban"`
	Description         string         `gorm:"column:description"`
	BankReference       string         `gorm:"column:bank_reference"`
	ExtractedOrderNo    *string        `gorm:"column:extracted_order_no"`
	Status              TransferStatus `gorm:"column:status"`
	MatchedOrderID      *uuid.UUID     `gorm:"type:uuid;column:matched_order_id"`
	MatchedBy           *uuid.UUID     `gorm:"type:uuid;column:matched_by"`
	MatchedAt           *time.Time     `gorm:"column:matched_at"`
	IgnoreReason        *string        `gorm:"column:ignore_reason"`
	ReceiptFileID       *uuid.UUID     `gorm:"type:uuid;column:receipt_file_id"`
	EntryMethod         EntryMethod    `gorm:"column:entry_method"`
	EnteredBy           uuid.UUID      `gorm:"type:uuid;column:entered_by"`
	ReturnRequestedBy   *uuid.UUID     `gorm:"type:uuid;column:return_requested_by"`
	ReturnRequestedAt   *time.Time     `gorm:"column:return_requested_at"`
	ReturnReason        *string        `gorm:"column:return_reason"`
	ReturnTargetIBAN    *string        `gorm:"column:return_target_iban"`
	ReturnApprovedBy    *uuid.UUID     `gorm:"type:uuid;column:return_approved_by"`
	ReturnApprovedAt    *time.Time     `gorm:"column:return_approved_at"`
	ReturnedAt          *time.Time     `gorm:"column:returned_at"`
	ReturnBankReference *string        `gorm:"column:return_bank_reference"`
	ReturnReceiptFileID *uuid.UUID     `gorm:"type:uuid;column:return_receipt_file_id"`
	CreatedAt           time.Time      `gorm:"column:created_at"`
	UpdatedAt           time.Time      `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (IncomingTransfer) TableName() string { return "incoming_bank_transfers" }

// clearReturn removes the return request of t (a rejected request, 000105:
// the audit log keeps it).
func (t *IncomingTransfer) clearReturn() {
	t.ReturnRequestedBy, t.ReturnRequestedAt, t.ReturnReason, t.ReturnTargetIBAN = nil, nil, nil, nil
	t.ReturnApprovedBy, t.ReturnApprovedAt = nil, nil
}

// ---- payments (02 §20.3; 000106)

// Provider is payments.provider.
type Provider string

// Payment providers. bank_transfer: a matched transfer to a Koro account
// (MVP); direct_confirmed: a finance admin's confirmation that a model C
// buyer paid the payees directly (it never reaches Koro's account);
// iyzico: phase 2.
const (
	ProviderBankTransfer    Provider = "bank_transfer"
	ProviderIyzico          Provider = "iyzico"
	ProviderDirectConfirmed Provider = "direct_confirmed"
	ProviderManual          Provider = "manual"
)

// PaymentStatus is payments.status.
type PaymentStatus string

// Payment statuses (refunds move captured on to partially_refunded /
// refunded; BE-043).
const (
	PaymentInitiated         PaymentStatus = "initiated"
	PaymentCaptured          PaymentStatus = "captured"
	PaymentFailed            PaymentStatus = "failed"
	PaymentPartiallyRefunded PaymentStatus = "partially_refunded"
	PaymentRefunded          PaymentStatus = "refunded"
	PaymentChargeback        PaymentStatus = "chargeback"
)

// Payment is one payments row: money captured for an order. The order, the
// provider, the transfer and the amount never change (payments_frozen).
type Payment struct {
	ID                 uuid.UUID     `gorm:"type:uuid;column:id;primaryKey"`
	OrderID            uuid.UUID     `gorm:"type:uuid;column:order_id"`
	OrgID              uuid.UUID     `gorm:"type:uuid;column:org_id"` // the buyer (= orders.org_id)
	Provider           Provider      `gorm:"column:provider"`
	IncomingTransferID *uuid.UUID    `gorm:"type:uuid;column:incoming_transfer_id"`
	Amount             int64         `gorm:"column:amount"`
	Currency           string        `gorm:"column:currency"`
	Status             PaymentStatus `gorm:"column:status"`
	PSPFeeAmount       int64         `gorm:"column:psp_fee_amount"`
	RefundedAmount     int64         `gorm:"column:refunded_amount"`
	CapturedAt         *time.Time    `gorm:"column:captured_at"`
	CapturedBy         *uuid.UUID    `gorm:"type:uuid;column:captured_by"`
	EvidenceFileID     *uuid.UUID    `gorm:"type:uuid;column:evidence_file_id"`
	CreatedAt          time.Time     `gorm:"column:created_at"`
	UpdatedAt          time.Time     `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Payment) TableName() string { return "payments" }

// ---- payment_splits (02 §20.4; 000107)

// SplitReleased is the status of a split of the manual flow: written when
// the order is fully paid (02 §20.4; iyzico's held → approved is phase 2).
const SplitReleased = "released"

// Split is one payment_splits row: how a paid order line divides between
// its payee (seller_amount) and Koro (platform_amount: commission, its VAT
// and the buyer fee; the whole line for Koro's own platform-native lines).
type Split struct {
	ID             uuid.UUID `gorm:"type:uuid;column:id;primaryKey"`
	PaymentID      uuid.UUID `gorm:"type:uuid;column:payment_id"`
	OrderID        uuid.UUID `gorm:"type:uuid;column:order_id"`
	OrderLineID    uuid.UUID `gorm:"type:uuid;column:order_line_id"`
	SellerAmount   int64     `gorm:"column:seller_amount"`
	PlatformAmount int64     `gorm:"column:platform_amount"`
	Status         string    `gorm:"column:status"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Split) TableName() string { return "payment_splits" }

// ---- refunds (02 §20.5; 000108) — BE-041 opens overpayment refunds only

// Refund is the part of a refunds row this ticket writes: the request of an
// overpayment refund opened by a match (03 k27, T-14). Approval, the EFT and
// the receipt are BE-043's (four eyes).
type Refund struct {
	ID          uuid.UUID  `gorm:"type:uuid;column:id;primaryKey"`
	PaymentID   uuid.UUID  `gorm:"type:uuid;column:payment_id"`
	OrderID     uuid.UUID  `gorm:"type:uuid;column:order_id"`
	OrderLineID *uuid.UUID `gorm:"type:uuid;column:order_line_id"`
	Amount      int64      `gorm:"column:amount"`
	Reason      string     `gorm:"column:reason"`
	ReasonNote  *string    `gorm:"column:reason_note"`
	Status      string     `gorm:"column:status"`
	RequestedBy uuid.UUID  `gorm:"type:uuid;column:requested_by"`
	Method      string     `gorm:"column:method"`
	TargetIBAN  *string    `gorm:"column:target_iban"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Refund) TableName() string { return "refunds" }

// Refund values of an overpayment refund (02 §20.5).
const (
	RefundReasonOverpayment = "overpayment"
	RefundStatusRequested   = "requested"
	RefundMethodBankEFT     = "bank_eft"
)
