package catalog

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// ---- services (02 §18.1)

// Category is services.category.
type Category string

// Service categories.
const (
	CategorySponsorship         Category = "sponsorship"
	CategoryBooth               Category = "booth"
	CategoryLogoPlacement       Category = "logo_placement"
	CategoryTalkSlot            Category = "talk_slot"
	CategorySocialMediaPost     Category = "social_media_post"
	CategoryWorkshop            Category = "workshop"
	CategoryTargetedPostQuota   Category = "targeted_post_quota"
	CategoryContactRequestQuota Category = "contact_request_quota"
	CategoryDiscoveryAccess     Category = "discovery_access"
	CategoryOther               Category = "other"
)

// Categories lists every category in 02 order.
func Categories() []Category {
	return []Category{CategorySponsorship, CategoryBooth, CategoryLogoPlacement, CategoryTalkSlot, CategorySocialMediaPost,
		CategoryWorkshop, CategoryTargetedPostQuota, CategoryContactRequestQuota, CategoryDiscoveryAccess, CategoryOther}
}

// Valid reports whether c is a services.category value.
func (c Category) Valid() bool { return slices.Contains(Categories(), c) }

// FulfillmentType is services.fulfillment_type: what the buyer gets.
type FulfillmentType string

// Fulfillment types.
const (
	// FulfillEventBound: delivered at an event of the seller (a booth, a
	// talk slot): the offering is bound to the event (06 H-55).
	FulfillEventBound FulfillmentType = "event_bound"
	// FulfillOrgBound: delivered by the seller org outside an event (a
	// social media post).
	FulfillOrgBound FulfillmentType = "org_bound"
	// FulfillPlatformNative: a Koro feature right (entitlement_template;
	// the order grants it, BE-040). Sold by the platform only.
	FulfillPlatformNative FulfillmentType = "platform_native"
)

// Valid reports whether f is a services.fulfillment_type value.
func (f FulfillmentType) Valid() bool {
	return f == FulfillEventBound || f == FulfillOrgBound || f == FulfillPlatformNative
}

// VATClass is services.vat_class (the accountant's VAT profile).
type VATClass string

// VAT classes.
const (
	VATStandard     VATClass = "standard"
	VATExempt       VATClass = "exempt"        // vat_exemption_code required
	VATOutsideScope VATClass = "outside_scope" // not subject to VAT
	VATWithheld     VATClass = "withheld"      // tevkifat: vat_withholding_num/den
)

// Valid reports whether c is a services.vat_class value.
func (c VATClass) Valid() bool {
	return c == VATStandard || c == VATExempt || c == VATOutsideScope || c == VATWithheld
}

// ServiceItem is one row of the Koro service catalogue (services; 02
// §18.1): what a club, a university or Koro may sell, with its invoice
// wording and VAT profile. A service is sellable once it is active and the
// accountant has reviewed it (accounting_reviewed_at; 03 k23).
type ServiceItem struct {
	ID                    uuid.UUID       `gorm:"type:uuid;column:id;primaryKey"`
	Code                  string          `gorm:"column:code"`
	NameTR                string          `gorm:"column:name_tr"`
	DescriptionTR         *string         `gorm:"column:description_tr"`
	Category              Category        `gorm:"column:category"`
	FulfillmentType       FulfillmentType `gorm:"column:fulfillment_type"`
	EntitlementTemplate   JSON            `gorm:"column:entitlement_template;type:jsonb"`
	DefaultVATRateBP      int             `gorm:"column:default_vat_rate_bp"`
	VATClass              VATClass        `gorm:"column:vat_class"`
	VATExemptionCode      *string         `gorm:"column:vat_exemption_code"`
	VATWithholdingCode    *string         `gorm:"column:vat_withholding_code"`
	VATWithholdingNum     *int16          `gorm:"column:vat_withholding_num"`
	VATWithholdingDen     *int16          `gorm:"column:vat_withholding_den"`
	InvoiceDescriptionTR  *string         `gorm:"column:invoice_description_tr"`
	AccountingReviewedAt  *time.Time      `gorm:"column:accounting_reviewed_at"`
	AccountingReviewedRef *string         `gorm:"column:accounting_reviewed_ref"`
	Active                bool            `gorm:"column:active"`
	CreatedAt             time.Time       `gorm:"column:created_at;->"`
	UpdatedAt             time.Time       `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (ServiceItem) TableName() string { return "services" }

// Sellable reports whether offerings of the service may go on sale: active
// and reviewed by the accountant (02 §18.1 "NULL ⇒ satışa açılamaz").
func (s ServiceItem) Sellable() bool { return s.Active && s.AccountingReviewedAt != nil }

// ---- offerings (02 §18.2)

// SellerKind is offerings.seller_kind.
type SellerKind string

// Seller kinds.
const (
	SellerOrg      SellerKind = "org"      // a club or a university; paid to its payee
	SellerPlatform SellerKind = "platform" // Koro itself (the platform org); no payee
)

// OfferingStatus is offerings.status.
type OfferingStatus string

// Offering statuses (02 §18.2). draft → pending_approval (submit) →
// active (platform approval, and the university's when required) →
// sold_out (the stock is gone) / closed / cancelled. A rejection sends it
// back to draft with the reason.
const (
	OfferingDraft           OfferingStatus = "draft"
	OfferingPendingApproval OfferingStatus = "pending_approval"
	OfferingActive          OfferingStatus = "active"
	OfferingSoldOut         OfferingStatus = "sold_out"
	OfferingClosed          OfferingStatus = "closed"
	OfferingCancelled       OfferingStatus = "cancelled"
)

// Valid reports whether s is an offerings.status value.
func (s OfferingStatus) Valid() bool {
	switch s {
	case OfferingDraft, OfferingPendingApproval, OfferingActive, OfferingSoldOut, OfferingClosed, OfferingCancelled:
		return true
	}
	return false
}

// InvoiceTrigger is offerings.invoice_trigger: when billing plans the
// service invoice (BE-042).
type InvoiceTrigger string

// Invoice triggers.
const (
	InvoiceOnPayment     InvoiceTrigger = "on_payment"
	InvoiceOnFulfillment InvoiceTrigger = "on_fulfillment"
)

// Valid reports whether t is an offerings.invoice_trigger value.
func (t InvoiceTrigger) Valid() bool { return t == InvoiceOnPayment || t == InvoiceOnFulfillment }

// UniversityApproval is offerings.university_approval_status (T-21).
type UniversityApproval string

// University approval states.
const (
	UniNotRequired UniversityApproval = "not_required"
	UniPending     UniversityApproval = "pending"
	UniApproved    UniversityApproval = "approved"
	UniRejected    UniversityApproval = "rejected"
)

// SponsorFlag is one value of offerings.blocked_sponsor_flags: a sponsor
// category the seller refuses (02 §18.2).
type SponsorFlag string

// Sponsor flags.
const (
	SponsorAlcohol   SponsorFlag = "alcohol"
	SponsorTobacco   SponsorFlag = "tobacco"
	SponsorGambling  SponsorFlag = "gambling"
	SponsorPolitical SponsorFlag = "political"
)

// SponsorFlags lists the flags in 02 order.
func SponsorFlags() []SponsorFlag {
	return []SponsorFlag{SponsorAlcohol, SponsorTobacco, SponsorGambling, SponsorPolitical}
}

// Offering is one offerings row: a package (v1: package = offering, 01
// §2.1) of a seller org or of the platform — a priced service, optionally
// bound to an event of the seller, with its stock and approval state.
// Amounts are kuruş, rates basis points (ADR-0014).
type Offering struct {
	ID                         uuid.UUID          `gorm:"type:uuid;column:id;primaryKey"`
	ServiceID                  uuid.UUID          `gorm:"type:uuid;column:service_id"`
	SellerKind                 SellerKind         `gorm:"column:seller_kind"`
	OrgID                      uuid.UUID          `gorm:"type:uuid;column:org_id"`
	EventID                    *uuid.UUID         `gorm:"type:uuid;column:event_id"`
	DefaultPayeeID             *uuid.UUID         `gorm:"type:uuid;column:default_payee_id"`
	Title                      string             `gorm:"column:title"`
	DescriptionJSON            JSON               `gorm:"column:description_json;type:jsonb"`
	Deliverables               Deliverables       `gorm:"column:deliverables;type:jsonb"`
	UnitPriceAmount            int64              `gorm:"column:unit_price_amount"`
	Currency                   string             `gorm:"column:currency"`
	VATRateBP                  int                `gorm:"column:vat_rate_bp"`
	CompensationType           string             `gorm:"column:compensation_type"`
	QuantityTotal              *int               `gorm:"column:quantity_total"`
	QuantitySold               int                `gorm:"column:quantity_sold"`
	PerBuyerLimit              *int               `gorm:"column:per_buyer_limit"`
	SalesStartAt               *time.Time         `gorm:"column:sales_start_at"`
	SalesEndAt                 *time.Time         `gorm:"column:sales_end_at"`
	FulfilBy                   *time.Time         `gorm:"column:fulfil_by"`
	InvoiceTrigger             InvoiceTrigger     `gorm:"column:invoice_trigger"`
	RequiresUniversityApproval bool               `gorm:"column:requires_university_approval"`
	UniversityApprovalStatus   UniversityApproval `gorm:"column:university_approval_status"`
	UniversityApprovedBy       *uuid.UUID         `gorm:"type:uuid;column:university_approved_by"`
	UniversityApprovedAt       *time.Time         `gorm:"column:university_approved_at"`
	BlockedSponsorFlags        db.TextArray       `gorm:"column:blocked_sponsor_flags;type:text[]"`
	Status                     OfferingStatus     `gorm:"column:status"`
	SubmittedAt                *time.Time         `gorm:"column:submitted_at"`
	ApprovedBy                 *uuid.UUID         `gorm:"type:uuid;column:approved_by"`
	ApprovedAt                 *time.Time         `gorm:"column:approved_at"`
	RejectionReason            *string            `gorm:"column:rejection_reason"`
	CancelledAt                *time.Time         `gorm:"column:cancelled_at"`
	CancelReason               *string            `gorm:"column:cancel_reason"`
	TermsVersion               *string            `gorm:"column:terms_version"`
	CreatedBy                  uuid.UUID          `gorm:"type:uuid;column:created_by"`
	CreatedAt                  time.Time          `gorm:"column:created_at;->"`
	UpdatedAt                  time.Time          `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Offering) TableName() string { return "offerings" }

// Published reports whether the offering has been on the marketplace:
// every signed-in user may read it (authz AttrOfferingPublished).
func (o Offering) Published() bool {
	switch o.Status {
	case OfferingActive, OfferingSoldOut, OfferingClosed:
		return true
	case OfferingCancelled:
		return o.ApprovedAt != nil
	}
	return false
}

// Remaining is the unsold stock (nil: unlimited).
func (o Offering) Remaining() *int {
	if o.QuantityTotal == nil {
		return nil
	}
	n := *o.QuantityTotal - o.QuantitySold
	return &n
}

// UniversityReview reports whether the offering waits for its university's
// decision (T-21).
func (o Offering) UniversityReview() bool {
	return o.RequiresUniversityApproval && o.Status == OfferingPendingApproval
}

// OnSaleAt reports whether a buyer may reserve the offering at t: active
// and within its sales window (sold_out has no stock).
func (o Offering) OnSaleAt(t time.Time) bool {
	return o.Status == OfferingActive && (o.SalesStartAt == nil || !t.Before(*o.SalesStartAt)) &&
		(o.SalesEndAt == nil || t.Before(*o.SalesEndAt))
}

// Deliverable is one item of offerings.deliverables: what the buyer gets
// (a booth, the logo on the banner...). Items of a package (several
// service items sold together, 01 §2.1) are the deliverables of one
// offering; orders snapshot them (order_lines.deliverables_snapshot) and
// track their fulfilment (BE-040).
type Deliverable struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Deliverables is the jsonb array of offerings.deliverables (never NULL).
type Deliverables []Deliverable

// Value implements driver.Valuer: an empty list is '[]'.
func (d Deliverables) Value() (driver.Value, error) {
	if d == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]Deliverable(d))
	if err != nil {
		return nil, fmt.Errorf("catalog: deliverables: %w", err)
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (d *Deliverables) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case nil:
		*d = Deliverables{}
		return nil
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("catalog: deliverables: cannot scan %T", src)
	}
	var out []Deliverable
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("catalog: deliverables: %w", err)
	}
	if out == nil {
		out = []Deliverable{}
	}
	*d = out
	return nil
}

// JSON is a jsonb column; nil is SQL NULL. Scan copies the driver's bytes.
type JSON []byte

// Value implements driver.Valuer.
func (j JSON) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return string(j), nil
}

// Scan implements sql.Scanner.
func (j *JSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = bytes.Clone(v)
	case string:
		*j = JSON(v)
	default:
		return errors.New("catalog: unsupported JSON source")
	}
	return nil
}

// ---- commerce policies (02 §18.3; ADR-0014)

// PolicyScope is commerce_policies.scope; resolution order offering >
// service > seller_org > default (02 §18.3).
type PolicyScope string

// Policy scopes.
const (
	ScopeDefault   PolicyScope = "default"
	ScopeSellerOrg PolicyScope = "seller_org"
	ScopeService   PolicyScope = "service"
	ScopeOffering  PolicyScope = "offering"
)

// Valid reports whether s is a commerce_policies.scope value.
func (s PolicyScope) Valid() bool {
	return s == ScopeDefault || s == ScopeSellerOrg || s == ScopeService || s == ScopeOffering
}

// rank is the resolution precedence (lower wins).
func (s PolicyScope) rank() int {
	switch s {
	case ScopeOffering:
		return 0
	case ScopeService:
		return 1
	case ScopeSellerOrg:
		return 2
	}
	return 3
}

// ModelCode is commerce_policies.model_code: the money and invoice model
// (ADR-0014; ModelFlows).
type ModelCode string

// Models.
const (
	ModelA  ModelCode = "A"  // marketplace (MVP: bank transfer to Koro, payout to the payee)
	ModelA2 ModelCode = "A2" // A, Koro invoices the buyer on the payee's behalf
	ModelB  ModelCode = "B"  // Koro is the single seller; the payee invoices Koro
	ModelC  ModelCode = "C"  // the buyer pays the payee directly; Koro charges its fee
)

// Valid reports whether m is a commerce_policies.model_code value.
func (m ModelCode) Valid() bool { return m == ModelA || m == ModelA2 || m == ModelB || m == ModelC }

// CollectionMethod is commerce_policies.collection_method.
type CollectionMethod string

// Collection methods.
const (
	CollectBankTransfer  CollectionMethod = "bank_transfer"   // to Koro's IBAN (MVP)
	CollectIyzico        CollectionMethod = "iyzico"          // phase 2
	CollectDirectToPayee CollectionMethod = "direct_to_payee" // model C
)

// Valid reports whether c is a commerce_policies.collection_method value.
func (c CollectionMethod) Valid() bool {
	return c == CollectBankTransfer || c == CollectIyzico || c == CollectDirectToPayee
}

// CommissionBasis is commerce_policies.commission_basis.
type CommissionBasis string

// Commission bases.
const (
	BasisNetExclVAT   CommissionBasis = "net_excl_vat"
	BasisGrossInclVAT CommissionBasis = "gross_incl_vat"
)

// CommissionPayer is commerce_policies.commission_payer.
type CommissionPayer string

// Commission payers.
const (
	PayerSeller CommissionPayer = "seller" // deducted from the payee's share
	PayerBuyer  CommissionPayer = "buyer"  // added to the buyer's total (buyer_fee_amount)
)

// InvoiceAutomation is commerce_policies.invoice_automation.
type InvoiceAutomation string

// Invoice automation modes.
const (
	InvoiceManual   InvoiceAutomation = "manual"   // MVP: invoices are uploaded (BE-042)
	InvoiceProvider InvoiceAutomation = "provider" // Paraşüt (BE-053)
)

// CommercePolicy is one commerce_policies row: the money model and the
// commission of a scope over a validity period [valid_from, valid_to).
// Rows never change except closing (valid_to NULL → value,
// koro_policy_close_only); orders copy the row in effect onto each line
// (BE-040), so a later change never touches an order.
type CommercePolicy struct {
	ID                  uuid.UUID         `gorm:"type:uuid;column:id;primaryKey"`
	Scope               PolicyScope       `gorm:"column:scope"`
	ScopeID             *uuid.UUID        `gorm:"type:uuid;column:scope_id"`
	ModelCode           ModelCode         `gorm:"column:model_code"`
	CollectionMethod    CollectionMethod  `gorm:"column:collection_method"`
	CommissionRateBP    int               `gorm:"column:commission_rate_bp"`
	CommissionMinAmount *int64            `gorm:"column:commission_min_amount"`
	CommissionMaxAmount *int64            `gorm:"column:commission_max_amount"`
	CommissionBasis     CommissionBasis   `gorm:"column:commission_basis"`
	CommissionPayer     CommissionPayer   `gorm:"column:commission_payer"`
	CommissionVATRateBP int               `gorm:"column:commission_vat_rate_bp"`
	InvoiceAutomation   InvoiceAutomation `gorm:"column:invoice_automation"`
	DisputeWindowDays   int16             `gorm:"column:dispute_window_days"`
	PayoutHoldDays      int16             `gorm:"column:payout_hold_days"`
	ValidFrom           time.Time         `gorm:"column:valid_from"`
	ValidTo             *time.Time        `gorm:"column:valid_to"`
	Notes               *string           `gorm:"column:notes"`
	CreatedBy           uuid.UUID         `gorm:"type:uuid;column:created_by"`
	CreatedAt           time.Time         `gorm:"column:created_at;->"`
}

// TableName implements gorm's Tabler.
func (CommercePolicy) TableName() string { return "commerce_policies" }

// ValidAt reports whether the policy is in effect at t ([valid_from, valid_to)).
func (p CommercePolicy) ValidAt(t time.Time) bool {
	return !t.Before(p.ValidFrom) && (p.ValidTo == nil || t.Before(*p.ValidTo))
}

// Open reports whether the policy has no end yet.
func (p CommercePolicy) Open() bool { return p.ValidTo == nil }
