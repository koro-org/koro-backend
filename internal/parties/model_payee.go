package parties

import (
	"time"
	"uuid"

	"koro/internal/platform/iban"
)

// PayeeStatus is payees.status (02 §17.3).
type PayeeStatus string

// Payee statuses.
const (
	PayeeDraft               PayeeStatus = "draft"
	PayeePendingVerification PayeeStatus = "pending_verification"
	PayeeActive              PayeeStatus = "active"
	PayeeSuspended           PayeeStatus = "suspended"
	PayeeClosed              PayeeStatus = "closed" // soft delete: kept for the financial records
)

// Valid reports whether s is a status of payees_status_chk.
func (s PayeeStatus) Valid() bool {
	switch s {
	case PayeeDraft, PayeePendingVerification, PayeeActive, PayeeSuspended, PayeeClosed:
		return true
	}
	return false
}

// PayoutMethod is payees.payout_method (02 §17.3).
type PayoutMethod string

// Payout methods. iyzico is phase 2 (it needs a sub-merchant key): the API
// does not offer it.
const (
	PayoutBankEFT PayoutMethod = "bank_eft"
	PayoutIyzico  PayoutMethod = "iyzico"
	PayoutNone    PayoutMethod = "none"
)

// Valid reports whether m is a method of payees_payout_method_chk.
func (m PayoutMethod) Valid() bool { return m == PayoutBankEFT || m == PayoutIyzico || m == PayoutNone }

// InvoicingCapability is payees.invoicing_capability (02 §17.3): whether
// the payee can issue an invoice itself.
type InvoicingCapability string

// Invoicing capabilities.
const (
	InvoicingCanIssue    InvoicingCapability = "can_issue"
	InvoicingCannotIssue InvoicingCapability = "cannot_issue"
	InvoicingUnknown     InvoicingCapability = "unknown"
)

// Valid reports whether c is a value of payees_invoicing_capability_chk.
func (c InvoicingCapability) Valid() bool {
	return c == InvoicingCanIssue || c == InvoicingCannotIssue || c == InvoicingUnknown
}

// Payee is one payees row (02 §17.3): the legal party (kind payee) paid on
// behalf of a seller org (a club or a university). The seller org is never
// the party's org_id (R-003 §4.7): a foundation is the payee of several
// clubs through one row per club.
type Payee struct {
	ID                    uuid.UUID           `gorm:"type:uuid;column:id;primaryKey"`
	LegalPartyID          uuid.UUID           `gorm:"type:uuid;column:legal_party_id"`
	PartyKind             Kind                `gorm:"column:party_kind"` // always payee (composite FK to legal_parties)
	OrgID                 uuid.UUID           `gorm:"type:uuid;column:org_id"`
	Status                PayeeStatus         `gorm:"column:status"`
	IsDefault             bool                `gorm:"column:is_default"`
	PayoutMethod          PayoutMethod        `gorm:"column:payout_method"`
	InvoicingCapability   InvoicingCapability `gorm:"column:invoicing_capability"`
	UniversityProtocolRef *string             `gorm:"column:university_protocol_ref"`
	VerifiedAt            *time.Time          `gorm:"column:verified_at"`
	VerifiedBy            *uuid.UUID          `gorm:"type:uuid;column:verified_by"`
	ValidFrom             time.Time           `gorm:"column:valid_from"`
	ValidTo               *time.Time          `gorm:"column:valid_to"`
	CreatedBy             uuid.UUID           `gorm:"type:uuid;column:created_by"`
	CreatedAt             time.Time           `gorm:"column:created_at"`
	UpdatedAt             time.Time           `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Payee) TableName() string { return "payees" }

// ActiveAt reports whether the payee may be paid at t: status active and t
// within [valid_from, valid_to).
func (p Payee) ActiveAt(t time.Time) bool {
	return p.Status == PayeeActive && !t.Before(p.ValidFrom) && (p.ValidTo == nil || t.Before(*p.ValidTo))
}

// BankAccountStatus is payee_bank_accounts.status (02 §17.4).
type BankAccountStatus string

// Bank account statuses: an org submission starts pending_verification
// (first eye: verify), an admin submission pending_approval (the submitter
// is the first eye); a different admin approves. A change is a new row:
// approving it supersedes the approved one in the same transaction.
const (
	AccountPendingVerification BankAccountStatus = "pending_verification"
	AccountPendingApproval     BankAccountStatus = "pending_approval"
	AccountApproved            BankAccountStatus = "approved"
	AccountRejected            BankAccountStatus = "rejected"
	AccountSuperseded          BankAccountStatus = "superseded"
	AccountRevoked             BankAccountStatus = "revoked"
)

// Valid reports whether s is a status of payee_bank_accounts_status_chk.
func (s BankAccountStatus) Valid() bool {
	switch s {
	case AccountPendingVerification, AccountPendingApproval, AccountApproved, AccountRejected, AccountSuperseded,
		AccountRevoked:
		return true
	}
	return false
}

// Pending reports a status that waits for an eye (the 4-eyes queue).
func (s BankAccountStatus) Pending() bool {
	return s == AccountPendingVerification || s == AccountPendingApproval
}

// BankAccount is one payee_bank_accounts row (02 §17.4). The IBAN is plain
// text (06 H-54: payees are legal entities in the MVP) and checked by
// payee_bank_accounts_iban_chk; organisation roles only ever see it masked,
// a full read by finance or platform admins is audited (03 §6.5). The IBAN,
// the holder, the payee, the evidence and the submitter never change
// (payee_bank_accounts_frozen): a change is a new row through four eyes.
// The IBAN is never logged (05 §11.2).
type BankAccount struct {
	ID               uuid.UUID         `gorm:"type:uuid;column:id;primaryKey"`
	PayeeID          uuid.UUID         `gorm:"type:uuid;column:payee_id"`
	OrgID            uuid.UUID         `gorm:"type:uuid;column:org_id"`
	IBAN             string            `gorm:"column:iban" json:"-"`
	HolderName       string            `gorm:"column:holder_name"`
	BankCode         string            `gorm:"column:bank_code;->"` // generated: substr(iban, 5, 5)
	BankName         *string           `gorm:"column:bank_name"`
	EvidenceFileID   uuid.UUID         `gorm:"type:uuid;column:evidence_file_id"`
	Status           BankAccountStatus `gorm:"column:status"`
	SubmittedBy      uuid.UUID         `gorm:"type:uuid;column:submitted_by"`
	SubmittedByAdmin bool              `gorm:"column:submitted_by_admin"`
	SubmittedAt      time.Time         `gorm:"column:submitted_at"`
	VerifiedBy       *uuid.UUID        `gorm:"type:uuid;column:verified_by"`
	VerifiedAt       *time.Time        `gorm:"column:verified_at"`
	ApprovedBy       *uuid.UUID        `gorm:"type:uuid;column:approved_by"`
	ApprovedAt       *time.Time        `gorm:"column:approved_at"`
	RejectedBy       *uuid.UUID        `gorm:"type:uuid;column:rejected_by"`
	RejectedAt       *time.Time        `gorm:"column:rejected_at"`
	RejectionReason  *string           `gorm:"column:rejection_reason"`
	SupersededAt     *time.Time        `gorm:"column:superseded_at"`
	SupersededByID   *uuid.UUID        `gorm:"type:uuid;column:superseded_by_id"`
	RevokedBy        *uuid.UUID        `gorm:"type:uuid;column:revoked_by"`
	RevokedAt        *time.Time        `gorm:"column:revoked_at"`
	RevokeReason     *string           `gorm:"column:revoke_reason"`
	CreatedAt        time.Time         `gorm:"column:created_at"`
	UpdatedAt        time.Time         `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (BankAccount) TableName() string { return "payee_bank_accounts" }

// MaskedIBAN is the IBAN as organisation roles see it
// ("TR33 **** **** **** **** 8413 26", 04 §2.13).
func (b BankAccount) MaskedIBAN() string { return iban.Mask(b.IBAN) }

// PayeeView is a payee as the HTTP operations return it: its legal party
// (title, legal type, status) and, when there is one, its approved bank
// account (always masked here: a full IBAN is read through the audited
// bank account operations).
type PayeeView struct {
	Payee    Payee
	Party    LegalParty // without the encrypted national ID and its index
	Approved *BankAccount
	ETag     string
}

// BankAccountView is a bank account as the HTTP operations return it.
// FullIBAN is true for finance and platform admins only (the read is
// audited); PartyTitle and HolderMatchesTitle are the first eye's check
// (T-19): the legal party's invoice title and whether the holder name
// equals it after Turkish-insensitive normalisation (advisory: the eye
// decides).
type BankAccountView struct {
	Account            BankAccount
	FullIBAN           bool
	PartyTitle         string
	HolderMatchesTitle bool
}
