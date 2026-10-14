package billing

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// Kind is invoices.kind (02 §22.1).
type Kind string

// Invoice kinds (02 §18.3 table): the payee's service invoice to the buyer,
// Koro's commission invoice to the payee (commission_payer seller), Koro's
// platform fee invoice to the buyer (commission_payer buyer; model C's fee),
// the payee's invoice to Koro (model B) and a credit note of an issued
// invoice. A platform-native line (Koro's own feature) is a service_sale
// Koro issues.
const (
	KindServiceSale Kind = "service_sale"
	KindCommission  Kind = "commission"
	KindPlatformFee Kind = "platform_fee"
	KindPayeeToKoro Kind = "payee_to_koro"
	KindCreditNote  Kind = "credit_note"
)

// Valid reports whether k is an invoices.kind value.
func (k Kind) Valid() bool {
	switch k {
	case KindServiceSale, KindCommission, KindPlatformFee, KindPayeeToKoro, KindCreditNote:
		return true
	}
	return false
}

// Status is invoices.status.
type Status string

// Invoice states (02 §22.1). Manual (MVP): planned → (uploaded: planned with
// uploaded_by) → issued | mismatch (→ uploaded again: planned) → cancelled.
// The provider flow (BE-053): queued → provider_pending → issued | failed.
const (
	StatusPlanned         Status = "planned"
	StatusQueued          Status = "queued"
	StatusProviderPending Status = "provider_pending"
	StatusIssued          Status = "issued"
	StatusFailed          Status = "failed"
	StatusMismatch        Status = "mismatch"
	StatusCancelled       Status = "cancelled"
)

// Valid reports whether s is an invoices.status value.
func (s Status) Valid() bool {
	switch s {
	case StatusPlanned, StatusQueued, StatusProviderPending, StatusIssued, StatusFailed, StatusMismatch, StatusCancelled:
		return true
	}
	return false
}

// IssuingMode is invoices.issuing_mode.
type IssuingMode string

// Issuing modes: Koro's invoice through the provider, Koro's invoice
// uploaded by finance (manual_upload), the payee's invoice uploaded by its
// org or finance (seller_uploaded), Koro issuing on the payee's behalf
// (model A2, T-11).
const (
	ModeProvider       IssuingMode = "provider"
	ModeManualUpload   IssuingMode = "manual_upload"
	ModeSellerUploaded IssuingMode = "seller_uploaded"
	ModeKoroOnBehalf   IssuingMode = "koro_on_behalf"
)

// Uploaded reports whether invoices of the mode are uploaded documents.
func (m IssuingMode) Uploaded() bool { return m == ModeManualUpload || m == ModeSellerUploaded }

// DocumentType is invoices.document_type.
type DocumentType string

// Document types.
const (
	DocEFatura DocumentType = "e_fatura"
	DocEArsiv  DocumentType = "e_arsiv"
	DocPaper   DocumentType = "paper"
	DocUnknown DocumentType = "unknown"
)

// Valid reports whether d is a document type an upload may declare.
func (d DocumentType) Valid() bool { return d == DocEFatura || d == DocEArsiv || d == DocPaper }

// Trigger is invoices.trigger: the event that planned the invoice.
type Trigger string

// Triggers.
const (
	TriggerOnPayment     Trigger = "on_payment"
	TriggerOnFulfillment Trigger = "on_fulfillment"
	TriggerOnPayout      Trigger = "on_payout"
	TriggerManual        Trigger = "manual"
)

// Providers of invoices.provider.
const (
	ProviderNone    = "none"
	ProviderParasut = "parasut"
)

// Currency is the only currency (02: currency = 'TRY').
const Currency = "TRY"

// Snapshot is invoices.issuer_snapshot / recipient_snapshot: the legal
// party as the invoice names it (title, tax office, VKN, address, invoice
// e-mail). Never a national ID (06 H-60).
type Snapshot struct {
	Title     string          `json:"title"`
	TaxOffice string          `json:"tax_office,omitempty"`
	TaxNo     string          `json:"tax_no,omitempty"`
	Address   SnapshotAddress `json:"address,omitzero"`
	Email     string          `json:"email,omitempty"`
}

// SnapshotAddress is the address of a Snapshot.
type SnapshotAddress struct {
	Line1      string `json:"line1,omitempty"`
	District   string `json:"district,omitempty"`
	City       string `json:"city,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

// Value implements driver.Valuer (jsonb object).
func (s Snapshot) Value() (driver.Value, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("billing: snapshot: %w", err)
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (s *Snapshot) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = bytes.Clone(v)
	case string:
		raw = []byte(v)
	case nil:
		*s = Snapshot{}
		return nil
	default:
		return fmt.Errorf("billing: snapshot: cannot scan %T", src)
	}
	var out Snapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("billing: snapshot: %w", err)
	}
	*s = out
	return nil
}

// Invoice is one invoices row (02 §22.1; 000116). The source (order or
// payout), the parties, their snapshots, the kind and the amounts are
// written when the invoice is planned and never change; the upload, the
// verification and the state are the running columns. Once issued the
// document columns freeze too (invoices_frozen): a correction is a credit
// note. Writes are targeted updates (06 H-56), never a Save.
type Invoice struct {
	ID                uuid.UUID    `gorm:"type:uuid;column:id;primaryKey"`
	OrderID           *uuid.UUID   `gorm:"type:uuid;column:order_id"`
	PayoutID          *uuid.UUID   `gorm:"type:uuid;column:payout_id"`
	Kind              Kind         `gorm:"column:kind"`
	IssuerPartyID     uuid.UUID    `gorm:"type:uuid;column:issuer_party_id"`
	RecipientPartyID  uuid.UUID    `gorm:"type:uuid;column:recipient_party_id"`
	IssuerOrgID       *uuid.UUID   `gorm:"type:uuid;column:issuer_org_id"`    // read scope: the payee's org; nil for Koro
	RecipientOrgID    *uuid.UUID   `gorm:"type:uuid;column:recipient_org_id"` // read scope: the buyer or the payee's org
	IssuerSnapshot    Snapshot     `gorm:"column:issuer_snapshot;type:jsonb"`
	RecipientSnapshot Snapshot     `gorm:"column:recipient_snapshot;type:jsonb"`
	IssuingMode       IssuingMode  `gorm:"column:issuing_mode"`
	DocumentType      DocumentType `gorm:"column:document_type"`
	Provider          string       `gorm:"column:provider"`
	Status            Status       `gorm:"column:status"`
	Trigger           Trigger      `gorm:"column:trigger"`
	InvoiceNo         *string      `gorm:"column:invoice_no"`
	ETTN              *uuid.UUID   `gorm:"type:uuid;column:ettn"`
	IssueDate         *civil.Date  `gorm:"column:issue_date"`
	Currency          string       `gorm:"column:currency"`
	NetAmount         int64        `gorm:"column:net_amount"`
	VATAmount         int64        `gorm:"column:vat_amount"`
	VATWithheldAmount int64        `gorm:"column:vat_withheld_amount"`
	TotalAmount       int64        `gorm:"column:total_amount"`
	PayableAmount     int64        `gorm:"column:payable_amount"`
	RefersToInvoiceID *uuid.UUID   `gorm:"type:uuid;column:refers_to_invoice_id"`
	PDFFileID         *uuid.UUID   `gorm:"type:uuid;column:pdf_file_id"`
	PDFSHA256         []byte       `gorm:"column:pdf_sha256"`
	Attempts          int          `gorm:"column:attempts"`
	LastError         *string      `gorm:"column:last_error"` // the mismatch note (manual) or the provider's last error
	IdempotencyKey    string       `gorm:"column:idempotency_key"`
	UploadedBy        *uuid.UUID   `gorm:"type:uuid;column:uploaded_by"`
	VerifiedBy        *uuid.UUID   `gorm:"type:uuid;column:verified_by"`
	VerifiedAt        *time.Time   `gorm:"column:verified_at"`
	IssuedAt          *time.Time   `gorm:"column:issued_at"`
	CreatedAt         time.Time    `gorm:"column:created_at"`
	UpdatedAt         time.Time    `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Invoice) TableName() string { return "invoices" }

// Uploaded reports whether a document waits for (or went through) the
// verification.
func (i Invoice) Uploaded() bool { return i.UploadedBy != nil && i.PDFFileID != nil }

// AwaitsVerification reports whether finance may verify the invoice now:
// a planned invoice with an uploaded document.
func (i Invoice) AwaitsVerification() bool { return i.Status == StatusPlanned && i.Uploaded() }

// Uploadable reports whether a document may be uploaded now: a planned
// invoice without one, or one whose upload was found not to match.
func (i Invoice) Uploadable() bool {
	return (i.Status == StatusPlanned && i.UploadedBy == nil) || i.Status == StatusMismatch
}

// WasIssued reports whether the invoice was issued (also when cancelled
// later: issued_at stays).
func (i Invoice) WasIssued() bool { return i.IssuedAt != nil }

// Source is the order or the payout the invoice belongs to.
func (i Invoice) Source() uuid.UUID {
	if i.OrderID != nil {
		return *i.OrderID
	}
	if i.PayoutID != nil {
		return *i.PayoutID
	}
	return uuid.Nil()
}

// Line is one invoice_lines row (02 §22.2; 000117).
type Line struct {
	ID                 uuid.UUID  `gorm:"type:uuid;column:id;primaryKey"`
	InvoiceID          uuid.UUID  `gorm:"type:uuid;column:invoice_id"`
	Position           int16      `gorm:"column:position"`
	OrderLineID        *uuid.UUID `gorm:"type:uuid;column:order_line_id"`
	Description        string     `gorm:"column:description"`
	Quantity           int        `gorm:"column:quantity"`
	UnitPriceAmount    int64      `gorm:"column:unit_price_amount"`
	NetAmount          int64      `gorm:"column:net_amount"`
	VATRateBP          int        `gorm:"column:vat_rate_bp"`
	VATAmount          int64      `gorm:"column:vat_amount"`
	VATExemptionCode   *string    `gorm:"column:vat_exemption_code"`
	VATWithholdingCode *string    `gorm:"column:vat_withholding_code"`
	VATWithheldAmount  int64      `gorm:"column:vat_withheld_amount"`
	TotalAmount        int64      `gorm:"column:total_amount"`
}

// TableName implements gorm's Tabler.
func (Line) TableName() string { return "invoice_lines" }

// Credential is one integration_credentials row (02 §22.5; 000120): the
// OAuth tokens of an integration, envelope-encrypted. The encrypted values
// never leave the module.
type Credential struct {
	Provider        string     `gorm:"column:provider;primaryKey"`
	ExternalAccount *string    `gorm:"column:external_account"`
	AccessTokenEnc  []byte     `gorm:"column:access_token_enc" json:"-"`
	RefreshTokenEnc []byte     `gorm:"column:refresh_token_enc" json:"-"`
	ExpiresAt       *time.Time `gorm:"column:expires_at"`
	RotatedAt       *time.Time `gorm:"column:rotated_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Credential) TableName() string { return "integration_credentials" }

// Integration providers of integration_credentials_provider_chk.
var integrationProviders = []string{ProviderParasut, "iyzico", "iys", "email"}

// stamp is the database's precision (microseconds).
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
