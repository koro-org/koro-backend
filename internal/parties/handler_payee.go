package parties

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// registerPayees adds the payee and bank account operations of BE-037
// (04 §2.13) and the admin legal party operations of 06 H-60 and T-234.
func registerPayees(api huma.API, h handler) {
	reg(api, huma.Operation{
		OperationID: "listOrgPayees", Method: http.MethodGet, Path: "/orgs/{org_id}/payees",
		Summary: "List the payees of an organization",
		Description: "The legal parties paid on behalf of a club or university, with their approved bank account " +
			"(the IBAN masked for everyone here).",
	}, op.Meta{Action: string(authz.PartiesPayeeList), Surface: op.SurfaceOrg}, h.listPayees)

	reg(api, huma.Operation{
		OperationID: "getOrgPayee", Method: http.MethodGet, Path: "/orgs/{org_id}/payees/{payee_id}",
		Summary:     "Get a payee of an organization",
		Description: "The ETag is the If-Match value of adminUpdatePayee. A payee of another organization is 404.",
	}, op.Meta{Action: string(authz.PartiesPayeeRead), Surface: op.SurfaceOrg}, h.getPayee)

	reg(api, huma.Operation{
		OperationID: "listPayeeBankAccounts", Method: http.MethodGet, Path: "/orgs/{org_id}/payees/{payee_id}/bank-accounts",
		Summary: "List the bank accounts of a payee",
		Description: "Newest first. Organization roles see the IBAN masked; finance and platform admins see it in full " +
			"and the read is audited.",
	}, op.Meta{Action: string(authz.PartiesBankAccountList), Surface: op.SurfaceOrg}, h.listAccounts)

	reg(api, huma.Operation{
		OperationID: "submitPayeeBankAccount", Method: http.MethodPost, Path: "/orgs/{org_id}/payees/{payee_id}/bank-accounts",
		Summary: "Submit a bank account (IBAN) of a payee", DefaultStatus: http.StatusCreated,
		Description: "Requires re-authentication (reauth). The account waits for four eyes (pending_verification: a " +
			"finance or platform admin checks the evidence and the holder, a different one approves); no money goes to it " +
			"before. An invalid IBAN is 422 parties.iban_invalid, a pending submission of the payee 409 " +
			"parties.bank_account_pending. Every admin of the organization and every finance admin is notified.",
	}, op.Meta{Action: string(authz.PartiesBankAccountSubmit), Surface: op.SurfaceOrg, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.submitAccount)

	reg(api, huma.Operation{
		OperationID: "adminCreatePayee", Method: http.MethodPost, Path: "/admin/payees",
		Summary: "Create a payee", DefaultStatus: http.StatusCreated,
		Description: "Relates a payee legal party (kind payee) to the club or university it is paid for (draft until " +
			"adminVerifyPayee). A natural person is 422 parties.natural_person_payee (06 H-54); a second open payee of " +
			"the same party for the organization 409 parties.payee_exists.",
	}, op.Meta{Action: string(authz.PartiesPayeeManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminCreatePayee)

	reg(api, huma.Operation{
		OperationID: "adminUpdatePayee", Method: http.MethodPatch, Path: "/admin/payees/{payee_id}",
		Summary: "Update a payee",
		Description: "If-Match is required (428/412). status suspended stops payouts, closed ends the payee (kept for " +
			"the financial records). Making an active payee the default moves the organization's default to it.",
	}, op.Meta{Action: string(authz.PartiesPayeeManage), Surface: op.SurfaceAdmin}, h.adminUpdatePayee)

	reg(api, huma.Operation{
		OperationID: "adminVerifyPayee", Method: http.MethodPost, Path: "/admin/payees/{payee_id}/verify",
		Summary: "Verify (activate) a payee",
		Description: "draft, pending_verification or suspended → active. The legal party must be verified (422 " +
			"parties.verification_incomplete); an active or closed payee is 409 parties.invalid_state.",
	}, op.Meta{Action: string(authz.PartiesPayeeVerify), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminVerifyPayee)

	reg(api, huma.Operation{
		OperationID: "adminListBankAccounts", Method: http.MethodGet, Path: "/admin/bank-accounts",
		Summary: "List payee bank accounts (four-eyes queue)",
		Description: "Oldest first; without status the accounts waiting for an eye. Full IBANs: the read is audited. " +
			"holder_matches_title compares the holder with the legal party's title (advisory; the first eye decides).",
	}, op.Meta{Action: string(authz.PartiesBankAccountList), Surface: op.SurfaceAdmin}, h.adminListAccounts)

	reg(api, huma.Operation{
		OperationID: "adminSubmitBankAccount", Method: http.MethodPost, Path: "/admin/payees/{payee_id}/bank-accounts",
		Summary: "Submit a bank account (IBAN) of a payee as an admin", DefaultStatus: http.StatusCreated,
		Description: "Requires re-authentication. The submitting admin is the first eye: the account starts " +
			"pending_approval and a different admin approves it.",
	}, op.Meta{Action: string(authz.PartiesBankAccountSubmit), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.adminSubmitAccount)

	reg(api, huma.Operation{
		OperationID: "adminVerifyBankAccount", Method: http.MethodPost, Path: "/admin/bank-accounts/{bank_account_id}/verify",
		Summary: "Verify a bank account (first eye)",
		Description: "The submitter never verifies (403 authz.four_eyes_violation). The evidence must hold (otherwise " +
			"reject the account); a holder that is not the legal party's title needs a note (T-19). Only " +
			"pending_verification (409 parties.invalid_state); an evidence file that is not scanned clean is 409 files.not_clean.",
	}, op.Meta{Action: string(authz.PartiesBankAccountVerify), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminVerifyAccount)

	reg(api, huma.Operation{
		OperationID: "adminApproveBankAccount", Method: http.MethodPost, Path: "/admin/bank-accounts/{bank_account_id}/approve",
		Summary: "Approve a bank account (second eye)",
		Description: "Requires re-authentication. Neither the submitter nor the first eye approves (403 " +
			"authz.four_eyes_violation). The evidence must be scanned clean (409 files.not_clean). The payee's approved " +
			"account is superseded in the same transaction; the organization's admins and every finance admin get a " +
			"security notice.",
	}, op.Meta{Action: string(authz.PartiesBankAccountApprove), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.adminApproveAccount)

	reg(api, huma.Operation{
		OperationID: "adminRejectBankAccount", Method: http.MethodPost, Path: "/admin/bank-accounts/{bank_account_id}/reject",
		Summary:     "Reject a bank account",
		Description: "Only an account waiting for an eye (409 parties.invalid_state otherwise); the submitter is notified.",
	}, op.Meta{Action: string(authz.PartiesBankAccountReject), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminRejectAccount)

	reg(api, huma.Operation{
		OperationID: "adminRevokeBankAccount", Method: http.MethodPost, Path: "/admin/bank-accounts/{bank_account_id}/revoke",
		Summary: "Revoke an approved bank account",
		Description: "Requires re-authentication. No payout can be approved to a revoked account; payouts created " +
			"before must be cancelled. Only an approved account (409 parties.invalid_state).",
	}, op.Meta{Action: string(authz.PartiesBankAccountRevoke), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.adminRevokeAccount)

	reg(api, huma.Operation{
		OperationID: "adminDownloadBankAccountEvidence", Method: http.MethodGet,
		Path:    "/admin/bank-accounts/{bank_account_id}/evidence/download",
		Summary: "Download the evidence of a bank account",
		Description: "The account statement or bank letter the eyes check: a URL valid for at most 5 minutes, every " +
			"download audited. A file that is not scanned clean is 409 files.not_clean.",
	}, op.Meta{Action: string(authz.PartiesBankAccountList), Surface: op.SurfaceAdmin}, h.adminDownloadEvidence)

	reg(api, huma.Operation{
		OperationID: "adminGetLegalParty", Method: http.MethodGet, Path: "/admin/legal-parties/{party_id}",
		Summary:     "Get a legal party (finance and platform admins)",
		Description: "With its documents and the ETag that adminUpdateLegalParty takes as If-Match. Tax numbers are not masked.",
	}, op.Meta{Action: string(authz.PartiesLegalPartyRead), Surface: op.SurfaceAdmin}, h.adminGetParty)

	reg(api, huma.Operation{
		OperationID: "adminUploadLegalPartyDocument", Method: http.MethodPost, Path: "/admin/legal-parties/{party_id}/documents",
		Summary: "Add a document to any legal party", DefaultStatus: http.StatusCreated,
		Description: "file_id is a party_document upload of the caller. For parties no organization surface reaches " +
			"(a foundation before its payee, Koro's own party). Another admin verifies it.",
	}, op.Meta{Action: string(authz.PartiesDocumentUpload), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminUploadDocument)

	reg(api, huma.Operation{
		OperationID: "adminDownloadLegalPartyDocument", Method: http.MethodGet,
		Path:        "/admin/legal-party-documents/{document_id}/download",
		Summary:     "Download a legal party document (finance and platform admins)",
		Description: "A URL valid for at most 5 minutes; every download is audited. A file that is not scanned clean is 409 files.not_clean.",
	}, op.Meta{Action: string(authz.PartiesDocumentRead), Surface: op.SurfaceAdmin}, h.adminDownloadDocument)

	reg(api, huma.Operation{
		OperationID: "adminCorrectLegalPartyOwnership", Method: http.MethodPost,
		Path:    "/admin/legal-parties/{party_id}/ownership-correction",
		Summary: "Correct the ownership of a tax ID (platform admins)",
		Description: "06 H-60: the party holds a tax ID it does not own (the real owner gets 409 parties.tax_id_taken). " +
			"It is retired (inactive: out of the tax ID uniqueness) with the reason and the evidence, which is kept as " +
			"a document of the party. Platform admins only; orders keep their buyer snapshot.",
	}, op.Meta{Action: string(authz.PartiesLegalPartyVerify), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminCorrectOwnership)
}

// proofOf turns the reauth body part into identity's proof.
func proofOf(r *consent.ReauthDTO) identity.ReauthProof {
	if r == nil {
		return identity.ReauthProof{}
	}
	return identity.ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
}

// ---- DTOs

// PartySummaryDTO is the legal side of a payee in PayeeDTO.
type PartySummaryDTO struct {
	PartyID   string `json:"party_id" format:"uuid"`
	Title     string `json:"title" doc:"Invoice title (unvan)"`
	LegalType string `json:"legal_type" enum:"company,university,foundation,association,sole_prop,individual,public_body,other"`
	Status    string `json:"status" enum:"draft,pending_verification,verified,rejected,inactive"`
}

// AccountSummaryDTO is the approved bank account of a payee in PayeeDTO
// (always masked).
type AccountSummaryDTO struct {
	BankAccountID string     `json:"bank_account_id" format:"uuid"`
	IBANMasked    string     `json:"iban_masked" doc:"TR12 **** **** **** **** 1234 56"`
	BankName      string     `json:"bank_name,omitempty"`
	ApprovedAt    *time.Time `json:"approved_at,omitempty"`
}

// PayeeDTO is 04 §2.13 PayeeDTO.
type PayeeDTO struct {
	PayeeID               string             `json:"payee_id" format:"uuid"`
	OrgID                 string             `json:"org_id" format:"uuid" doc:"The club or university paid on behalf of"`
	LegalParty            PartySummaryDTO    `json:"legal_party"`
	Status                string             `json:"status" enum:"draft,pending_verification,active,suspended,closed"`
	IsDefault             bool               `json:"is_default"`
	PayoutMethod          string             `json:"payout_method" enum:"bank_eft,iyzico,none"`
	InvoicingCapability   string             `json:"invoicing_capability" enum:"can_issue,cannot_issue,unknown"`
	UniversityProtocolRef string             `json:"university_protocol_ref,omitempty"`
	VerifiedAt            *time.Time         `json:"verified_at,omitempty"`
	ValidFrom             time.Time          `json:"valid_from"`
	ValidTo               *time.Time         `json:"valid_to,omitempty"`
	ApprovedAccount       *AccountSummaryDTO `json:"approved_account,omitempty"`
	CreatedAt             time.Time          `json:"created_at"`
	UpdatedAt             time.Time          `json:"updated_at"`
}

func payeeDTO(v PayeeView) PayeeDTO {
	p := v.Payee
	out := PayeeDTO{
		PayeeID: p.ID.String(), OrgID: p.OrgID.String(), Status: string(p.Status), IsDefault: p.IsDefault,
		LegalParty: PartySummaryDTO{PartyID: v.Party.ID.String(), Title: v.Party.Title, LegalType: string(v.Party.LegalType),
			Status: string(v.Party.Status)},
		PayoutMethod: string(p.PayoutMethod), InvoicingCapability: string(p.InvoicingCapability),
		UniversityProtocolRef: str(p.UniversityProtocolRef), VerifiedAt: utc(p.VerifiedAt), ValidFrom: p.ValidFrom.UTC(),
		ValidTo: utc(p.ValidTo), CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
	}
	if b := v.Approved; b != nil {
		out.ApprovedAccount = &AccountSummaryDTO{BankAccountID: b.ID.String(), IBANMasked: b.MaskedIBAN(),
			BankName: str(b.BankName), ApprovedAt: utc(b.ApprovedAt)}
	}
	return out
}

// BankAccountDTO is 04 §2.13 BankAccountDTO (+ the decision columns). iban
// is set for finance and platform admins only; everyone sees iban_masked.
type BankAccountDTO struct {
	BankAccountID   string     `json:"bank_account_id" format:"uuid"`
	PayeeID         string     `json:"payee_id" format:"uuid"`
	IBANMasked      string     `json:"iban_masked" doc:"TR12 **** **** **** **** 1234 56"`
	IBAN            string     `json:"iban,omitempty" doc:"Finance and platform admins only (the read is audited)"`
	HolderName      string     `json:"holder_name"`
	BankCode        string     `json:"bank_code" doc:"Characters 5-9 of the IBAN"`
	BankName        string     `json:"bank_name,omitempty"`
	Status          string     `json:"status" enum:"pending_verification,pending_approval,approved,rejected,superseded,revoked"`
	SubmittedAt     time.Time  `json:"submitted_at"`
	VerifiedAt      *time.Time `json:"verified_at,omitempty"`
	ApprovedAt      *time.Time `json:"approved_at,omitempty"`
	RejectedAt      *time.Time `json:"rejected_at,omitempty"`
	RejectionReason string     `json:"rejection_reason,omitempty"`
	SupersededAt    *time.Time `json:"superseded_at,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	RevokeReason    string     `json:"revoke_reason,omitempty"`
}

func accountDTO(v BankAccountView) BankAccountDTO {
	b := v.Account
	out := BankAccountDTO{
		BankAccountID: b.ID.String(), PayeeID: b.PayeeID.String(), IBANMasked: b.MaskedIBAN(), HolderName: b.HolderName,
		BankCode: b.BankCode, BankName: str(b.BankName), Status: string(b.Status), SubmittedAt: b.SubmittedAt.UTC(),
		VerifiedAt: utc(b.VerifiedAt), ApprovedAt: utc(b.ApprovedAt), RejectedAt: utc(b.RejectedAt),
		RejectionReason: str(b.RejectionReason), SupersededAt: utc(b.SupersededAt), RevokedAt: utc(b.RevokedAt),
		RevokeReason: str(b.RevokeReason),
	}
	if v.FullIBAN {
		out.IBAN = b.IBAN
	}
	return out
}

// BankAccountAdminDTO is 04 §2.13 BankAccountAdminDTO: the full IBAN, the
// eyes and what the first eye compares (T-19).
type BankAccountAdminDTO struct {
	BankAccountDTO
	OrgID              string `json:"org_id" format:"uuid"`
	EvidenceFileID     string `json:"evidence_file_id" format:"uuid" doc:"adminDownloadBankAccountEvidence"`
	SubmittedBy        string `json:"submitted_by" format:"uuid"`
	SubmittedByAdmin   bool   `json:"submitted_by_admin" doc:"An admin submission starts pending_approval (the submitter is the first eye)"`
	VerifiedBy         string `json:"verified_by,omitempty" format:"uuid"`
	ApprovedBy         string `json:"approved_by,omitempty" format:"uuid"`
	RejectedBy         string `json:"rejected_by,omitempty" format:"uuid"`
	RevokedBy          string `json:"revoked_by,omitempty" format:"uuid"`
	SupersededByID     string `json:"superseded_by_id,omitempty" format:"uuid"`
	PartyTitle         string `json:"party_title" doc:"The legal party's invoice title the holder is checked against"`
	HolderMatchesTitle bool   `json:"holder_matches_title" doc:"Holder equals the title after Turkish-insensitive folding (advisory)"`
}

func idStr(p *uuid.UUID) string {
	if p == nil {
		return ""
	}
	return p.String()
}

func accountAdminDTO(v BankAccountView) BankAccountAdminDTO {
	b := v.Account
	v.FullIBAN = true
	return BankAccountAdminDTO{
		BankAccountDTO: accountDTO(v), OrgID: b.OrgID.String(), EvidenceFileID: b.EvidenceFileID.String(),
		SubmittedBy: b.SubmittedBy.String(), SubmittedByAdmin: b.SubmittedByAdmin, VerifiedBy: idStr(b.VerifiedBy),
		ApprovedBy: idStr(b.ApprovedBy), RejectedBy: idStr(b.RejectedBy), RevokedBy: idStr(b.RevokedBy),
		SupersededByID: idStr(b.SupersededByID), PartyTitle: v.PartyTitle, HolderMatchesTitle: v.HolderMatchesTitle,
	}
}

// ---- org surface

// PayeePath is the payee_id path parameter (exported: huma reads the
// fields of an embedded struct only when its type is exported).
type PayeePath struct {
	PayeeID string `path:"payee_id" doc:"Payee ID (an unknown, malformed or other organization's ID is 404)"`
}

type orgPayeePath struct {
	OrgID string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	PayeePath
}

type payeeListOutput struct {
	Body struct {
		Items []PayeeDTO `json:"items"`
	}
}

func (h handler) listPayees(ctx context.Context, in *orgPath) (*payeeListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	vs, err := h.svc.ListOrgPayees(ctx, a, parseID(in.OrgID))
	if err != nil {
		return nil, err
	}
	out := &payeeListOutput{}
	out.Body.Items = make([]PayeeDTO, len(vs))
	for i, v := range vs {
		out.Body.Items[i] = payeeDTO(v)
	}
	return out, nil
}

type payeeOutput struct {
	ETag string `header:"ETag"`
	Body PayeeDTO
}

func payeeOut(v PayeeView) *payeeOutput { return &payeeOutput{ETag: v.ETag, Body: payeeDTO(v)} }

func (h handler) getPayee(ctx context.Context, in *orgPayeePath) (*payeeOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetOrgPayee(ctx, a, parseID(in.OrgID), parseID(in.PayeeID))
	if err != nil {
		return nil, err
	}
	return payeeOut(v), nil
}

type accountListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items []BankAccountDTO `json:"items"`
	}
}

func (h handler) listAccounts(ctx context.Context, in *orgPayeePath) (*accountListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	vs, err := h.svc.ListPayeeBankAccounts(ctx, a, parseID(in.OrgID), parseID(in.PayeeID))
	if err != nil {
		return nil, err
	}
	out := &accountListOutput{CacheControl: "no-store"}
	out.Body.Items = make([]BankAccountDTO, len(vs))
	for i, v := range vs {
		out.Body.Items[i] = accountDTO(v)
	}
	return out, nil
}

// SubmitAccountBody is the body of submitPayeeBankAccount and
// adminSubmitBankAccount.
type SubmitAccountBody struct {
	IBAN           string             `json:"iban" minLength:"15" maxLength:"42" doc:"TR + 24 digits; spaces allowed"`
	HolderName     string             `json:"holder_name" minLength:"2" maxLength:"300" doc:"The account holder as the bank names it"`
	BankName       string             `json:"bank_name,omitempty" maxLength:"120"`
	EvidenceFileID string             `json:"evidence_file_id" format:"uuid" doc:"A bank_account_evidence upload (account statement or bank letter)"`
	Reauth         *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
}

func (b SubmitAccountBody) input() (BankAccountInput, error) {
	file, err := uuid.Parse(b.EvidenceFileID)
	if err != nil {
		return BankAccountInput{}, fieldErr("body.evidence_file_id", "must be a UUID")
	}
	return BankAccountInput{IBAN: b.IBAN, HolderName: b.HolderName, BankName: b.BankName, EvidenceFileID: file}, nil
}

type submitAccountInput struct {
	idempotency.OptionalHeader
	OrgID string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	PayeePath
	Body SubmitAccountBody
}

type accountOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         BankAccountDTO
}

func (h handler) submitAccount(ctx context.Context, in *submitAccountInput) (*accountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	bi, err := in.Body.input()
	if err != nil {
		return nil, err
	}
	v, err := h.svc.SubmitPayeeBankAccount(ctx, a, parseID(in.OrgID), parseID(in.PayeeID), bi, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &accountOutput{CacheControl: "no-store", Body: accountDTO(v)}, nil
}

// ---- admin surface: payees

type adminCreatePayeeInput struct {
	idempotency.OptionalHeader
	Body struct {
		LegalPartyID          string `json:"legal_party_id" format:"uuid" doc:"A payee legal party (adminCreateLegalParty kind payee)"`
		OrgID                 string `json:"org_id" format:"uuid" doc:"The club or university paid on behalf of"`
		IsDefault             bool   `json:"is_default,omitempty" doc:"The organization's default payee once active"`
		PayoutMethod          string `json:"payout_method,omitempty" enum:"bank_eft,none" doc:"bank_eft when omitted"`
		InvoicingCapability   string `json:"invoicing_capability,omitempty" enum:"can_issue,cannot_issue,unknown" doc:"unknown when omitted"`
		UniversityProtocolRef string `json:"university_protocol_ref,omitempty" maxLength:"200"`
	}
}

func (h handler) adminCreatePayee(ctx context.Context, in *adminCreatePayeeInput) (*payeeOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	party, err := uuid.Parse(b.LegalPartyID)
	if err != nil {
		return nil, fieldErr("body.legal_party_id", "must be a UUID")
	}
	org, err := uuid.Parse(b.OrgID)
	if err != nil {
		return nil, fieldErr("body.org_id", "must be a UUID")
	}
	v, err := h.svc.AdminCreatePayee(ctx, a, PayeeInput{
		LegalPartyID: party, OrgID: org, IsDefault: b.IsDefault, PayoutMethod: PayoutMethod(b.PayoutMethod),
		InvoicingCapability: InvoicingCapability(b.InvoicingCapability), UniversityProtocolRef: b.UniversityProtocolRef,
	})
	if err != nil {
		return nil, err
	}
	return payeeOut(v), nil
}

type adminUpdatePayeeInput struct {
	PayeePath
	IfMatch string `header:"If-Match" doc:"ETag of the payee (required: missing → 428, stale → 412)"`
	Body    struct {
		Status                string  `json:"status,omitempty" enum:"suspended,closed" doc:"adminVerifyPayee activates a suspended payee again"`
		IsDefault             *bool   `json:"is_default,omitempty"`
		PayoutMethod          *string `json:"payout_method,omitempty" enum:"bank_eft,none"`
		InvoicingCapability   *string `json:"invoicing_capability,omitempty" enum:"can_issue,cannot_issue,unknown"`
		UniversityProtocolRef *string `json:"university_protocol_ref,omitempty" maxLength:"200" doc:"Empty string clears it"`
	}
}

func (h handler) adminUpdatePayee(ctx context.Context, in *adminUpdatePayeeInput) (*payeeOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	pt := PayeePatch{IsDefault: b.IsDefault, UniversityProtocolRef: b.UniversityProtocolRef}
	if b.Status != "" {
		st := PayeeStatus(b.Status)
		pt.Status = &st
	}
	if b.PayoutMethod != nil {
		m := PayoutMethod(*b.PayoutMethod)
		pt.PayoutMethod = &m
	}
	if b.InvoicingCapability != nil {
		c := InvoicingCapability(*b.InvoicingCapability)
		pt.InvoicingCapability = &c
	}
	v, err := h.svc.AdminUpdatePayee(ctx, a, parseID(in.PayeeID), in.IfMatch, pt)
	if err != nil {
		return nil, err
	}
	return payeeOut(v), nil
}

type adminPayeeActionInput struct {
	idempotency.OptionalHeader
	PayeePath
}

func (h handler) adminVerifyPayee(ctx context.Context, in *adminPayeeActionInput) (*payeeOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminVerifyPayee(ctx, a, parseID(in.PayeeID))
	if err != nil {
		return nil, err
	}
	return payeeOut(v), nil
}

// ---- admin surface: bank accounts

// AccountPath is the bank_account_id path parameter.
type AccountPath struct {
	BankAccountID string `path:"bank_account_id" doc:"Bank account ID (an unknown or malformed ID is 404)"`
}

type adminListAccountsInput struct {
	page.Params
	Status string `query:"status" enum:"pending_verification,pending_approval,approved,rejected,superseded,revoked" doc:"Both pending statuses when omitted"`
}

type adminAccountListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []BankAccountAdminDTO `json:"items"`
		NextCursor string                `json:"next_cursor,omitempty"`
	}
}

const sortQueue = "id"

func (h handler) adminListAccounts(ctx context.Context, in *adminListAccountsInput) (*adminAccountListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListBankAccounts", in.Status)
	var after *uuid.UUID
	c, err := page.Decode(in.Cursor, sortQueue, fp, 1)
	if err != nil {
		return nil, err
	}
	if c != nil {
		id, err := c.UUID(0)
		if err != nil {
			return nil, err
		}
		after = &id
	}
	p, err := h.svc.AdminListBankAccounts(ctx, a, BankAccountQuery{Status: BankAccountStatus(in.Status), Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &adminAccountListOutput{CacheControl: "no-store"}
	out.Body.Items = make([]BankAccountAdminDTO, len(p.Items))
	for i, v := range p.Items {
		out.Body.Items[i] = accountAdminDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortQueue, fp, page.UUIDVal(*p.Next))
	}
	return out, nil
}

type adminAccountOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         BankAccountAdminDTO
}

func adminAccountOut(v BankAccountView) *adminAccountOutput {
	return &adminAccountOutput{CacheControl: "no-store", Body: accountAdminDTO(v)}
}

type adminSubmitAccountInput struct {
	idempotency.OptionalHeader
	PayeePath
	Body SubmitAccountBody
}

func (h handler) adminSubmitAccount(ctx context.Context, in *adminSubmitAccountInput) (*adminAccountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	bi, err := in.Body.input()
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminSubmitBankAccount(ctx, a, parseID(in.PayeeID), bi, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return adminAccountOut(v), nil
}

// VerifyChecklist is the first eye's checklist of adminVerifyBankAccount (T-19).
type VerifyChecklist struct {
	HolderMatchesParty bool `json:"holder_matches_party" doc:"The holder is the legal party's title (T-19)"`
	EvidenceOK         bool `json:"evidence_ok" doc:"The evidence shows this IBAN and holder; false → reject instead"`
}

type adminVerifyAccountInput struct {
	idempotency.OptionalHeader
	AccountPath
	Body struct {
		Checklist VerifyChecklist `json:"checklist"`
		Note      string          `json:"note,omitempty" maxLength:"500" doc:"Required when the holder is not the legal party's title (audited; no personal data)"`
	}
}

func (h handler) adminVerifyAccount(ctx context.Context, in *adminVerifyAccountInput) (*adminAccountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	v, err := h.svc.AdminVerifyBankAccount(ctx, a, parseID(in.BankAccountID), VerifyInput{
		HolderMatchesParty: b.Checklist.HolderMatchesParty, EvidenceOK: b.Checklist.EvidenceOK, Note: b.Note,
	})
	if err != nil {
		return nil, err
	}
	return adminAccountOut(v), nil
}

type adminApproveAccountInput struct {
	idempotency.OptionalHeader
	AccountPath
	Body struct {
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) adminApproveAccount(ctx context.Context, in *adminApproveAccountInput) (*adminAccountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminApproveBankAccount(ctx, a, parseID(in.BankAccountID), proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return adminAccountOut(v), nil
}

type adminRejectAccountInput struct {
	idempotency.OptionalHeader
	AccountPath
	Body struct {
		RejectionReason string `json:"rejection_reason" minLength:"1" maxLength:"500" doc:"Shown to the organization (no personal data)"`
	}
}

func (h handler) adminRejectAccount(ctx context.Context, in *adminRejectAccountInput) (*adminAccountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminRejectBankAccount(ctx, a, parseID(in.BankAccountID), in.Body.RejectionReason)
	if err != nil {
		return nil, err
	}
	return adminAccountOut(v), nil
}

type adminRevokeAccountInput struct {
	idempotency.OptionalHeader
	AccountPath
	Body struct {
		RevokeReason string             `json:"revoke_reason" minLength:"1" maxLength:"500" doc:"Shown to the organization (no personal data)"`
		Reauth       *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) adminRevokeAccount(ctx context.Context, in *adminRevokeAccountInput) (*adminAccountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminRevokeBankAccount(ctx, a, parseID(in.BankAccountID), in.Body.RevokeReason, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return adminAccountOut(v), nil
}

func (h handler) adminDownloadEvidence(ctx context.Context, in *AccountPath) (*downloadOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	l, err := h.svc.AdminDownloadBankAccountEvidence(ctx, a, parseID(in.BankAccountID))
	if err != nil {
		return nil, err
	}
	return &downloadOutput{CacheControl: "no-store", Body: DownloadDTO{URL: l.URL, ExpiresAt: l.ExpiresAt.UTC()}}, nil
}

// ---- admin surface: legal parties (06 H-60, T-234)

func (h handler) adminGetParty(ctx context.Context, in *AdminPartyPath) (*partyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminGetParty(ctx, a, parseID(in.PartyID))
	if err != nil {
		return nil, err
	}
	return partyOut(v), nil
}

type adminUploadDocumentInput struct {
	idempotency.OptionalHeader
	AdminPartyPath
	Body struct {
		Kind   string `json:"kind" enum:"tax_certificate,signature_circular,foundation_deed,university_protocol,authorization_letter,other"`
		FileID string `json:"file_id" format:"uuid" doc:"A party_document upload of the caller (createUpload, completeUpload)"`
	}
}

func (h handler) adminUploadDocument(ctx context.Context, in *adminUploadDocumentInput) (*documentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fileID, err := uuid.Parse(in.Body.FileID)
	if err != nil {
		return nil, fieldErr("body.file_id", "must be a UUID")
	}
	d, err := h.svc.AdminUploadDocument(ctx, a, parseID(in.PartyID), DocumentInput{Kind: DocumentKind(in.Body.Kind), FileID: fileID})
	if err != nil {
		return nil, err
	}
	return &documentOutput{Body: documentDTO(d, true)}, nil
}

type adminDocumentPath struct {
	DocumentID string `path:"document_id" doc:"Document ID (an unknown or malformed ID is 404)"`
}

func (h handler) adminDownloadDocument(ctx context.Context, in *adminDocumentPath) (*downloadOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	l, err := h.svc.AdminDownloadDocument(ctx, a, parseID(in.DocumentID))
	if err != nil {
		return nil, err
	}
	return &downloadOutput{CacheControl: "no-store", Body: DownloadDTO{URL: l.URL, ExpiresAt: l.ExpiresAt.UTC()}}, nil
}

type adminCorrectOwnershipInput struct {
	idempotency.OptionalHeader
	AdminPartyPath
	Body struct {
		Reason         string `json:"reason" minLength:"10" maxLength:"500" doc:"Why the party does not own its tax ID (audited)"`
		EvidenceFileID string `json:"evidence_file_id" format:"uuid" doc:"A party_document upload of the caller: the real owner's evidence"`
	}
}

func (h handler) adminCorrectOwnership(ctx context.Context, in *adminCorrectOwnershipInput) (*partyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	file, err := uuid.Parse(in.Body.EvidenceFileID)
	if err != nil {
		return nil, fieldErr("body.evidence_file_id", "must be a UUID")
	}
	v, err := h.svc.AdminCorrectOwnership(ctx, a, parseID(in.PartyID), OwnershipCorrection{Reason: in.Body.Reason, EvidenceFileID: file})
	if err != nil {
		return nil, err
	}
	return partyOut(v), nil
}
