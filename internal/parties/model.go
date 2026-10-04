package parties

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"
)

// Kind is legal_parties.kind (02 §17.1).
type Kind string

// Party kinds.
const (
	KindPlatform     Kind = "platform"      // Koro itself: one row, seeded as draft (T-31)
	KindBuyerCompany Kind = "buyer_company" // the invoice title of a buying company (org_id = the company)
	KindPayee        Kind = "payee"         // the legal side of a payee: foundation, university, association …
)

// Valid reports whether k is a kind of the CHECK constraint.
func (k Kind) Valid() bool { return k == KindPlatform || k == KindBuyerCompany || k == KindPayee }

// LegalType is legal_parties.legal_type (02 §17.1).
type LegalType string

// Legal types.
const (
	LegalCompany     LegalType = "company"
	LegalUniversity  LegalType = "university"
	LegalFoundation  LegalType = "foundation"
	LegalAssociation LegalType = "association"
	LegalSoleProp    LegalType = "sole_prop"  // şahıs şirketi: a natural person merchant
	LegalIndividual  LegalType = "individual" // a natural person: TCKN instead of VKN
	LegalPublicBody  LegalType = "public_body"
	LegalOther       LegalType = "other"
)

// legalTypes lists the legal types in CHECK order.
var legalTypes = []LegalType{LegalCompany, LegalUniversity, LegalFoundation, LegalAssociation, LegalSoleProp,
	LegalIndividual, LegalPublicBody, LegalOther}

// Valid reports whether t is a legal type of the CHECK constraint.
func (t LegalType) Valid() bool { return slices.Contains(legalTypes, t) }

// NaturalPerson reports a legal type that is a real person (gerçek kişi):
// an individual or a sole proprietorship. 06 H-54: no payee of this type in
// the MVP.
func (t LegalType) NaturalPerson() bool { return t == LegalIndividual || t == LegalSoleProp }

// Status is legal_parties.status (02 §17.1).
type Status string

// Party statuses.
const (
	StatusDraft               Status = "draft" // the seeded platform party until it is filled in (T-31)
	StatusPendingVerification Status = "pending_verification"
	StatusVerified            Status = "verified"
	StatusRejected            Status = "rejected"
	StatusInactive            Status = "inactive" // retired: kept for the financial records, out of the tax ID uniqueness
)

// Valid reports whether s is a status of the CHECK constraint.
func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusPendingVerification, StatusVerified, StatusRejected, StatusInactive:
		return true
	}
	return false
}

// VATStatus is legal_parties.vat_status (02 §17.1).
type VATStatus string

// VAT statuses.
const (
	VATPayer    VATStatus = "vat_payer"
	VATExempt   VATStatus = "vat_exempt"
	VATNotPayer VATStatus = "not_vat_payer"
	VATUnknown  VATStatus = "unknown"
)

// Valid reports whether v is a VAT status of the CHECK constraint.
func (v VATStatus) Valid() bool {
	return v == VATPayer || v == VATExempt || v == VATNotPayer || v == VATUnknown
}

// DocumentKind is legal_party_documents.kind (02 §17.2).
type DocumentKind string

// Document kinds.
const (
	DocTaxCertificate      DocumentKind = "tax_certificate"      // vergi levhası
	DocSignatureCircular   DocumentKind = "signature_circular"   // imza sirküleri
	DocFoundationDeed      DocumentKind = "foundation_deed"      // vakıf senedi
	DocUniversityProtocol  DocumentKind = "university_protocol"  // üniversite protokolü
	DocAuthorizationLetter DocumentKind = "authorization_letter" // yetki yazısı
	DocOther               DocumentKind = "other"
)

// Valid reports whether k is a document kind of the CHECK constraint.
func (k DocumentKind) Valid() bool {
	switch k {
	case DocTaxCertificate, DocSignatureCircular, DocFoundationDeed, DocUniversityProtocol, DocAuthorizationLetter, DocOther:
		return true
	}
	return false
}

// Address is legal_parties.address (02 §17.1: {"line1", "district", "city",
// "postal_code", "country": "TR"}), stored as a JSON object. An individual's
// address is personal data: never logged.
type Address struct {
	Line1      string `json:"line1,omitempty"`
	District   string `json:"district,omitempty"`
	City       string `json:"city,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

// Empty reports an address without any field (the seeded platform party).
func (a Address) Empty() bool { return a == Address{} }

// Value implements driver.Valuer (jsonb).
func (a Address) Value() (driver.Value, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner (jsonb arrives as text or bytes).
func (a *Address) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case nil:
		*a = Address{}
		return nil
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("parties: address: unexpected %T", src)
	}
	var out Address
	if err := json.Unmarshal(b, &out); err != nil {
		return errors.Join(errors.New("parties: address is not an object"), err)
	}
	*a = out
	return nil
}

// LegalParty is one legal_parties row (02 §17.1). TCKNEnc (an individual's
// national ID, envelope-encrypted) and TCKNBidx (its blind index) never leave
// the module: the cross-module reads (Parties) clear them, and no DTO maps
// them (03 k21, 04 §2.13). Tax numbers are never logged (05 §11.2).
type LegalParty struct {
	ID                uuid.UUID  `gorm:"type:uuid;column:id;primaryKey"`
	Kind              Kind       `gorm:"column:kind"`
	LegalType         LegalType  `gorm:"column:legal_type"`
	OrgID             *uuid.UUID `gorm:"type:uuid;column:org_id"`
	Title             string     `gorm:"column:title"`
	TaxOffice         *string    `gorm:"column:tax_office"`
	TaxNo             *string    `gorm:"column:tax_no"` // VKN
	TCKNEnc           []byte     `gorm:"column:tckn_enc" json:"-"`
	TCKNBidx          []byte     `gorm:"column:tckn_bidx" json:"-"`
	MersisNo          *string    `gorm:"column:mersis_no"`
	Address           Address    `gorm:"column:address;type:jsonb"`
	Email             *string    `gorm:"column:email"`
	Phone             *string    `gorm:"column:phone"`
	VATStatus         VATStatus  `gorm:"column:vat_status"`
	EInvoiceAlias     *string    `gorm:"column:e_invoice_alias"`
	EInvoiceCheckedAt *time.Time `gorm:"column:e_invoice_checked_at"`
	Status            Status     `gorm:"column:status"`
	VerifiedAt        *time.Time `gorm:"column:verified_at"`
	VerifiedBy        *uuid.UUID `gorm:"type:uuid;column:verified_by"`
	CreatedBy         *uuid.UUID `gorm:"type:uuid;column:created_by"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (LegalParty) TableName() string { return "legal_parties" }

// HasTCKN reports whether the party carries an encrypted national ID.
func (p LegalParty) HasTCKN() bool { return len(p.TCKNEnc) > 0 }

// withoutSecrets returns p without the encrypted national ID and its blind
// index (what other modules see).
func (p LegalParty) withoutSecrets() LegalParty {
	p.TCKNEnc, p.TCKNBidx = nil, nil
	return p
}

// Document is one legal_party_documents row (02 §17.2). The file lives in
// files (purpose party_document), attached to the document row.
type Document struct {
	ID           uuid.UUID    `gorm:"type:uuid;column:id;primaryKey"`
	LegalPartyID uuid.UUID    `gorm:"type:uuid;column:legal_party_id"`
	Kind         DocumentKind `gorm:"column:kind"`
	FileID       uuid.UUID    `gorm:"type:uuid;column:file_id"`
	UploadedBy   uuid.UUID    `gorm:"type:uuid;column:uploaded_by"`
	VerifiedAt   *time.Time   `gorm:"column:verified_at"`
	VerifiedBy   *uuid.UUID   `gorm:"type:uuid;column:verified_by"`
	CreatedAt    time.Time    `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Document) TableName() string { return "legal_party_documents" }

// Scope is how a party relates to the org of an org-surface request (03
// k21/k22): the org's own party, or the legal side of one of its payees.
type Scope string

// Scopes (the Attrs "scope" values of authz.TypeLegalParty).
const (
	ScopeOrg   Scope = "org"
	ScopePayee Scope = "payee"
)

// OrgParty is a party as an org surface sees it.
type OrgParty struct {
	Party LegalParty
	Scope Scope
}

// PartyView is a party with its documents and ETag (04 §1.13), as the HTTP
// operations return it. FullTaxIDs is true for finance and platform admins
// only: every org role sees the VKN and MERSİS number masked.
type PartyView struct {
	Party      LegalParty // without the encrypted national ID and its index
	Documents  []Document
	ETag       string
	FullTaxIDs bool
	// TCKNOnFile reports an individual's stored national ID (shown to
	// finance and platform admins as a flag; the value never leaves).
	TCKNOnFile bool
}

// DocumentLink is a short-lived download URL of a document (04 §1.14).
type DocumentLink struct {
	URL       string
	ExpiresAt time.Time
}
