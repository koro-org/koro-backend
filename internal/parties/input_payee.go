package parties

import (
	"unicode/utf8"
	"uuid"

	"koro/internal/orgs"
	"koro/internal/platform/iban"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// PayeeInput is the body of adminCreatePayee (04 §2.13).
type PayeeInput struct {
	LegalPartyID          uuid.UUID // a payee party (kind payee), never a natural person (06 H-54)
	OrgID                 uuid.UUID // the seller org: a club or a university
	IsDefault             bool
	PayoutMethod          PayoutMethod        // "" → bank_eft; iyzico is phase 2
	InvoicingCapability   InvoicingCapability // "" → unknown
	UniversityProtocolRef string
}

// PayeePatch is the body of adminUpdatePayee: nil leaves a field as it is.
// Status takes suspended (an active payee stops being paid) or closed (the
// payee ends; kept for the financial records). A suspended payee becomes
// active again through adminVerifyPayee.
type PayeePatch struct {
	Status                *PayeeStatus
	IsDefault             *bool
	PayoutMethod          *PayoutMethod
	InvoicingCapability   *InvoicingCapability
	UniversityProtocolRef *string // "" clears it
}

func (p PayeePatch) empty() bool {
	return p.Status == nil && p.IsDefault == nil && p.PayoutMethod == nil && p.InvoicingCapability == nil &&
		p.UniversityProtocolRef == nil
}

// BankAccountInput is the body of submitPayeeBankAccount and
// adminSubmitBankAccount (the re-authentication proof travels apart).
type BankAccountInput struct {
	IBAN           string // any spacing and case; stored normalised ("TR" + 24 digits)
	HolderName     string // the account holder as the bank names it
	BankName       string
	EvidenceFileID uuid.UUID // a bank_account_evidence upload (account statement, bank letter)
}

// VerifyInput is the body of adminVerifyBankAccount: the first eye's
// checklist (T-19). A holder that is not the legal party's title is an
// exception that needs a note; the evidence must hold (otherwise the
// account is rejected, not verified).
type VerifyInput struct {
	HolderMatchesParty bool
	EvidenceOK         bool
	Note               string
}

// PayeeQuery narrows adminQueryPayees (BE-129). Zero fields do not filter.
type PayeeQuery struct {
	OrgID  *uuid.UUID
	Status PayeeStatus
	// Q is part of the legal party's title, or its VKN (10 digits): it may
	// name a sole proprietor, so it travels in the body.
	Q     string
	Limit int
	After *uuid.UUID
}

// PayeePage is a page of adminQueryPayees; Next is the keyset position of
// the next page (nil: last page).
type PayeePage struct {
	Items []AdminPayeeView
	Next  *uuid.UUID
}

// AdminPayeeView is a payee of the staff search with the org it is paid
// for (finance finds the club or university by the payee).
type AdminPayeeView struct {
	PayeeView
	Org orgs.OrgSummary
}

// BankAccountQuery narrows adminListBankAccounts. No status: both pending
// statuses (the four-eyes queue).
type BankAccountQuery struct {
	Status BankAccountStatus
	Limit  int
	After  *uuid.UUID
}

// BankAccountPage is a page of the bank account queue; Next is the keyset
// position of the next page (nil: last page).
type BankAccountPage struct {
	Items []BankAccountView
	Next  *uuid.UUID
}

// OwnershipCorrection is the body of adminCorrectLegalPartyOwnership
// (06 H-60): why the party does not own its tax ID, and the evidence (a
// party_document upload of the caller: the real owner's tax certificate).
type OwnershipCorrection struct {
	Reason         string
	EvidenceFileID uuid.UUID
}

// Field limits of 02 §17.3–17.4.
const (
	maxProtocolRef = 200
	maxBankName    = 120
	minReason      = 10 // a reason that explains something (ownership correction)
)

// cleanAccountInput validates a bank account submission: the IBAN (422
// parties.iban_invalid), the holder (2–300 characters) and the bank name.
func cleanAccountInput(in BankAccountInput) (number, holder string, bank *string, err error) {
	if number, err = iban.Parse(in.IBAN); err != nil {
		return "", "", nil, ErrIBANInvalid
	}
	if holder, err = cleanTitle("body.holder_name", in.HolderName); err != nil {
		return "", "", nil, err
	}
	if bank, err = cleanLine("body.bank_name", in.BankName, maxBankName); err != nil {
		return "", "", nil, err
	}
	if in.EvidenceFileID == uuid.Nil() {
		return "", "", nil, problem.Field("body.evidence_file_id", "is required")
	}
	return number, holder, bank, nil
}

// cleanReason checks a mandatory decision text (rejection, revocation):
// 1–500 characters after trimming.
func cleanReason(loc, s string, minRunes int) (string, error) {
	n, err := normalized(loc, s)
	if err != nil {
		return "", err
	}
	if c := utf8.RuneCountInString(n); c < minRunes || c > maxNote {
		if minRunes > 1 {
			return "", problem.Field(loc, "must be 10-500 characters")
		}
		return "", problem.Field(loc, "is required (at most 500 characters)")
	}
	return n, nil
}

// holderMatchesTitle compares the account holder with the party's title
// after Turkish-insensitive folding (letters and digits only): "ÖRNEK
// EĞİTİM VAKFI" equals "Örnek Eğitim Vakfı". It is advisory (02 §17.4:
// the first eye decides, T-19).
func holderMatchesTitle(holder, title string) bool {
	h, t := textx.Slug(holder, 0), textx.Slug(title, 0)
	return h != "" && h == t
}

// isSellerKind reports the org kinds a payee is paid for (02 §17.3: the
// seller org, a club or a university).
func isSellerKind(k orgs.Kind) bool { return k == orgs.KindClub || k == orgs.KindUniversity }
