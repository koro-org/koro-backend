package payments

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/parties"
	"koro/internal/platform/civil"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the payments operations of BE-041 (04 §2.16: collection
// accounts, incoming transfers, the match; 06 H-56 / T-79 / T-228: the
// three return steps; T-18: the model C direct confirmation) and of BE-043
// (the payments, the refunds and the refunds due; handler_refunds.go) to
// api. svc may be nil (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminListCollectionAccounts", Method: http.MethodGet, Path: "/admin/collection-accounts",
		Summary: "List Koro's collection accounts",
		Description: "Every account, newest first. The default active account is the IBAN of every payment instruction. " +
			"The IBAN is masked (BE-129): the second eye who checks it against the bank reveals it with " +
			"adminRevealCollectionAccountIban.",
	}, op.Meta{Action: string(authz.PaymentsCollectionAccountList), Surface: op.SurfaceAdmin}, h.listAccounts)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminRevealCollectionAccountIban", Method: http.MethodGet, Path: "/admin/collection-accounts/{account_id}/iban",
		Summary:     "Reveal the full IBAN of a collection account",
		Description: "Koro's own account (iban_masked's full form), for the check against the bank before an approval. " + parties.RevealNote,
	}, op.Meta{Action: string(authz.PaymentsIbanReveal), Surface: op.SurfaceAdmin, RateLimit: op.RateBreakGlass}, h.revealAccount)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminCreateCollectionAccount", Method: http.MethodPost, Path: "/admin/collection-accounts",
		Summary: "Create a collection account", DefaultStatus: http.StatusCreated,
		Description: "Requires re-authentication. The account belongs to Koro's own legal party (kind platform) and waits " +
			"for another admin's approval (pending_approval, four eyes). An IBAN that another pending or active account " +
			"has is 409 payments.iban_in_use.",
	}, op.Meta{Action: string(authz.PaymentsCollectionAccountCreate), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.createAccount)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminApproveCollectionAccount", Method: http.MethodPost, Path: "/admin/collection-accounts/{account_id}/approve",
		Summary: "Approve or reject a collection account",
		Description: "Requires re-authentication. The creator never decides (403 authz.four_eyes_violation). An approval " +
			"sends a security notice to every platform and finance admin; it does not make the account the default.",
	}, op.Meta{Action: string(authz.PaymentsCollectionAccountApprove), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.approveAccount)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminRetireCollectionAccount", Method: http.MethodPost, Path: "/admin/collection-accounts/{account_id}/retire",
		Summary: "Retire a collection account",
		Description: "Requires re-authentication. Only an active account (409 payments.invalid_state). A retired default " +
			"leaves no default: payment instructions answer 409 payments.no_active_collection_account until another " +
			"account is made the default.",
	}, op.Meta{Action: string(authz.PaymentsCollectionAccountRetire), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.retireAccount)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminMakeDefaultCollectionAccount", Method: http.MethodPost, Path: "/admin/collection-accounts/{account_id}/make-default",
		Summary: "Make a collection account the default",
		Description: "Requires re-authentication. Only an active account (409 payments.invalid_state). New payment " +
			"instructions show its IBAN; the previous default stays active.",
	}, op.Meta{Action: string(authz.PaymentsCollectionAccountRetire), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.makeDefault)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminQueryIncomingTransfers", Method: http.MethodPost, Path: "/admin/incoming-transfers/search",
		Summary: "Search incoming bank transfers",
		Description: "Read-only: the criteria are in the body so that a sender's name never reaches a URL or an " +
			"access log (BE-128). Newest value date first. q matches part of the description, the sender's name or " +
			"the bank reference, or a reference code exactly. next_cursor goes back in the body.",
	}, op.Meta{Action: string(authz.PaymentsTransferList), Surface: op.SurfaceAdmin, RateLimit: op.RateReadDefault,
		Idempotency: op.IdempotencyOptional}, h.queryTransfers)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminGetIncomingTransfer", Method: http.MethodGet, Path: "/admin/incoming-transfers/{transfer_id}",
		Summary: "Get an incoming bank transfer with match suggestions",
		Description: "An unmatched transfer suggests the order of the reference code in its description, rated by its " +
			"amount: 1 exactly what the order waits for, 0.7 less (partial), 0.5 more (an overpayment refund opens), 0 " +
			"the order does not wait for a payment.",
	}, op.Meta{Action: string(authz.PaymentsTransferRead), Surface: op.SurfaceAdmin}, h.getTransfer)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminRevealIncomingTransferIban", Method: http.MethodGet, Path: "/admin/incoming-transfers/{transfer_id}/iban",
		Summary: "Reveal a full IBAN of an incoming transfer",
		Description: "field sender is sender_iban_masked's full form (the sender's account, maybe a natural person's), " +
			"return_target the return's target (return.target_iban_masked), for the return EFT. An IBAN the transfer " +
			"does not hold is 404. " + parties.RevealNote,
	}, op.Meta{Action: string(authz.PaymentsIbanReveal), Surface: op.SurfaceAdmin, RateLimit: op.RateBreakGlass}, h.revealTransfer)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminDownloadIncomingTransferReceipt", Method: http.MethodGet,
		Path:    "/admin/incoming-transfers/{transfer_id}/receipt",
		Summary: "Get a download URL of a transfer's bank receipt",
		Description: "BE-129: the receipt recorded with the transfer (receipt_file_id; a transfer_receipt upload), for the " +
			"check of a receivable collection or a refund: a URL valid for 5 minutes, every download audited. A transfer " +
			"without a receipt is 404; 409 files.not_clean until the virus scan clears the file.",
	}, op.Meta{Action: string(authz.PaymentsTransferRead), Surface: op.SurfaceAdmin}, h.transferReceipt)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminCreateIncomingTransfer", Method: http.MethodPost, Path: "/admin/incoming-transfers",
		Summary: "Record an incoming bank transfer", DefaultStatus: http.StatusCreated,
		Description: "The transfer is recorded unmatched on an approved collection account; the reference code of its " +
			"description is extracted for the suggestions. A bank reference the account already has is 409 " +
			"payments.duplicate_bank_reference.",
	}, op.Meta{Action: string(authz.PaymentsTransferCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.createTransfer)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminImportIncomingTransfers", Method: http.MethodPost, Path: "/admin/incoming-transfers/import",
		Summary: "Import a bank statement (CSV)", DefaultStatus: http.StatusAccepted, MaxBodyBytes: 2 * MaxImportBytes,
		Description: "csv is the statement text (UTF-8, ';' or ',' separated, header line first; columns value_date, " +
			"amount, description, bank_reference, optional sender_name, sender_iban; at most 1 MiB and 2000 rows). Each " +
			"row is imported, skipped as a duplicate (its bank reference is already recorded on the account) or rejected " +
			"with a reason; the answer counts them and lists every row.",
	}, op.Meta{Action: string(authz.PaymentsTransferCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyRequired}, h.importTransfers)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminMatchIncomingTransfer", Method: http.MethodPost, Path: "/admin/incoming-transfers/{transfer_id}/match",
		Summary: "Match a transfer to an order (payment capture)",
		Description: "One transaction: the transfer is matched, the payment captured, the order paid as far as the money " +
			"goes (partial payments accumulate), the ledger posted and payment.captured emitted; any failure leaves the " +
			"transfer unmatched. expected_order_no must be the order's reference code (422 " +
			"payments.order_reference_mismatch). Money beyond what the order waits for opens an overpayment refund " +
			"request to refund_iban (default: the sender's IBAN); its approval takes four eyes.",
	}, op.Meta{Action: string(authz.PaymentsTransferMatch), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyRequired}, h.match)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminIgnoreIncomingTransfer", Method: http.MethodPost, Path: "/admin/incoming-transfers/{transfer_id}/ignore",
		Summary:     "Ignore an unmatched transfer",
		Description: "Only an unmatched transfer (409). The reason is audited.",
	}, op.Meta{Action: string(authz.PaymentsTransferIgnore), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.ignore)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminRequestIncomingTransferReturn", Method: http.MethodPost, Path: "/admin/incoming-transfers/{transfer_id}/request-return",
		Summary: "Request the return of a transfer to its sender",
		Description: "Requires re-authentication. Step 1 of 3 (06 H-56): an unmatched or ignored transfer waits for another " +
			"finance admin's approval (return_requested). target_iban defaults to the sender's IBAN.",
	}, op.Meta{Action: string(authz.PaymentsTransferMarkReturned), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.requestReturn)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminApproveIncomingTransferReturn", Method: http.MethodPost, Path: "/admin/incoming-transfers/{transfer_id}/approve-return",
		Summary: "Approve or reject the return of a transfer",
		Description: "Requires re-authentication. Step 2 of 3: the requester never decides (403 " +
			"authz.four_eyes_violation). An approval freezes the target IBAN (return_approved); a rejection makes the " +
			"transfer unmatched again.",
	}, op.Meta{Action: string(authz.PaymentsTransferMarkReturned), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.approveReturn)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminMarkIncomingTransferReturned", Method: http.MethodPost, Path: "/admin/incoming-transfers/{transfer_id}/mark-returned",
		Summary: "Record the EFT that returned a transfer",
		Description: "Requires re-authentication. Step 3 of 3: only an approved return (409 payments.invalid_state); the " +
			"bank reference and the receipt (a refund_receipt upload) close it (returned).",
	}, op.Meta{Action: string(authz.PaymentsTransferMarkReturned), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.markReturned)

	op.Register(api, "payments", huma.Operation{
		OperationID: "adminConfirmDirectPayment", Method: http.MethodPost, Path: "/admin/payments/direct-confirmations",
		Summary: "Confirm a model C order's direct payment to its payees",
		Description: "Model C (the buyer pays the payees directly, Koro collects its fee): with the receipt of the " +
			"buyer's transfer (a transfer_receipt upload) a finance admin records a direct_confirmed payment and the " +
			"order is paid once Koro's fee is also complete. Fee capture and direct confirmation may arrive in either " +
			"order. Confirmation is recorded once on an awaiting-payment order (409 orders.invalid_state otherwise). " +
			"The direct amount never enters Koro's ledger; final settlement allocates only its fee and fee VAT.",
	}, op.Meta{Action: string(authz.PaymentsTransferMatch), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyRequired}, h.confirmDirect)

	registerRefunds(api, h)
	registerCollections(api, h)
}

type handler struct{ svc Service }

// proofOf turns the reauth body part into identity's proof.
func proofOf(r *consent.ReauthDTO) identity.ReauthProof {
	if r == nil {
		return identity.ReauthProof{}
	}
	return identity.ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- collection accounts

// AccountPath is the account_id path parameter.
type AccountPath struct {
	AccountID string `path:"account_id" doc:"Collection account ID (an unknown or malformed ID is 404)"`
}

// CollectionAccountDTO is 04 §2.16 CollectionAccountDTO. The IBAN is
// masked like every IBAN of a finance response (BE-129; a payment
// instruction shows the default account's in full to its buyer).
type CollectionAccountDTO struct {
	AccountID    string     `json:"account_id" format:"uuid"`
	Label        string     `json:"label"`
	LegalPartyID string     `json:"legal_party_id" format:"uuid"`
	IBANMasked   string     `json:"iban_masked" doc:"TR12 **** **** **** **** 1234 56 (adminRevealCollectionAccountIban)"`
	HolderName   string     `json:"holder_name"`
	BankName     string     `json:"bank_name"`
	Status       string     `json:"status" enum:"pending_approval,active,rejected,retired"`
	IsDefault    bool       `json:"is_default" doc:"The account of every new payment instruction"`
	CreatedBy    string     `json:"created_by" format:"uuid"`
	ApprovedBy   string     `json:"approved_by,omitempty" format:"uuid" doc:"The admin who decided it (approved or rejected)"`
	ApprovedAt   *time.Time `json:"approved_at,omitempty"`
	RetiredAt    *time.Time `json:"retired_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func accountDTO(c CollectionAccount) CollectionAccountDTO {
	return CollectionAccountDTO{
		AccountID: c.ID.String(), Label: c.Label, LegalPartyID: c.LegalPartyID.String(), IBANMasked: c.MaskedIBAN(), HolderName: c.HolderName,
		BankName: c.BankName, Status: string(c.Status), IsDefault: c.IsDefault, CreatedBy: c.CreatedBy.String(),
		ApprovedBy: httpx.IDStr(c.ApprovedBy), ApprovedAt: utc(c.ApprovedAt), RetiredAt: utc(c.RetiredAt), CreatedAt: c.CreatedAt.UTC(),
	}
}

// CollectionAccountList is the body of adminListCollectionAccounts.
type CollectionAccountList struct {
	Items []CollectionAccountDTO `json:"items"`
}

type accountListOutput struct{ Body CollectionAccountList }

func (h handler) listAccounts(ctx context.Context, _ *struct{}) (*accountListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := h.svc.ListCollectionAccounts(ctx, a)
	if err != nil {
		return nil, err
	}
	out := &accountListOutput{Body: CollectionAccountList{Items: make([]CollectionAccountDTO, len(cs))}}
	for i, c := range cs {
		out.Body.Items[i] = accountDTO(c)
	}
	return out, nil
}

type accountOutput struct{ Body CollectionAccountDTO }

type createAccountInput struct {
	idempotency.OptionalHeader
	Body struct {
		Label        string             `json:"label" minLength:"1" maxLength:"100" doc:"Shown to finance only"`
		LegalPartyID string             `json:"legal_party_id" format:"uuid" doc:"Koro's own legal party (kind platform)"`
		IBAN         string             `json:"iban" maxLength:"42" doc:"TR IBAN; spaces are ignored"`
		HolderName   string             `json:"holder_name" minLength:"2" maxLength:"300"`
		BankName     string             `json:"bank_name" minLength:"2" maxLength:"120"`
		Reauth       *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) createAccount(ctx context.Context, in *createAccountInput) (*accountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	c, err := h.svc.CreateCollectionAccount(ctx, a, AccountInput{Label: b.Label, LegalPartyID: httpx.ParseID(b.LegalPartyID), IBAN: b.IBAN,
		HolderName: b.HolderName, BankName: b.BankName}, proofOf(b.Reauth))
	if err != nil {
		return nil, err
	}
	return &accountOutput{Body: accountDTO(c)}, nil
}

type approveAccountInput struct {
	idempotency.OptionalHeader
	AccountPath
	Body struct {
		Decision string             `json:"decision" enum:"approved,rejected"`
		Reauth   *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) approveAccount(ctx context.Context, in *approveAccountInput) (*accountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, err := h.svc.DecideCollectionAccount(ctx, a, httpx.ParseID(in.AccountID), Decision(in.Body.Decision), proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &accountOutput{Body: accountDTO(c)}, nil
}

type reauthAccountInput struct {
	idempotency.OptionalHeader
	AccountPath
	Body struct {
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) retireAccount(ctx context.Context, in *reauthAccountInput) (*accountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, err := h.svc.RetireCollectionAccount(ctx, a, httpx.ParseID(in.AccountID), proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &accountOutput{Body: accountDTO(c)}, nil
}

func (h handler) makeDefault(ctx context.Context, in *reauthAccountInput) (*accountOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	c, err := h.svc.MakeDefaultCollectionAccount(ctx, a, httpx.ParseID(in.AccountID), proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &accountOutput{Body: accountDTO(c)}, nil
}

type revealAccountInput struct {
	AccountPath
	httpx.AccessReasonHeader
}

func (h handler) revealAccount(ctx context.Context, in *revealAccountInput) (*parties.RevealOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	n, err := h.svc.RevealCollectionAccountIBAN(ctx, a, httpx.ParseID(in.AccountID), in.AccessReason.String())
	if err != nil {
		return nil, err
	}
	return parties.RevealOut(n), nil
}

// ---- incoming transfers

// TransferPath is the transfer_id path parameter.
type TransferPath struct {
	TransferID string `path:"transfer_id" doc:"Incoming transfer ID (an unknown or malformed ID is 404)"`
}

// TransferReturnDTO is the return of a transfer to its sender (06 H-18).
type TransferReturnDTO struct {
	RequestedBy      string     `json:"requested_by,omitempty" format:"uuid"`
	RequestedAt      *time.Time `json:"requested_at,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	TargetIBANMasked string     `json:"target_iban_masked,omitempty" doc:"Where the return EFT goes, masked (adminRevealIncomingTransferIban field return_target)"`
	ApprovedBy       string     `json:"approved_by,omitempty" format:"uuid"`
	ApprovedAt       *time.Time `json:"approved_at,omitempty"`
	ReturnedAt       *time.Time `json:"returned_at,omitempty"`
	BankReference    string     `json:"bank_reference,omitempty"`
	ReceiptFileID    string     `json:"receipt_file_id,omitempty" format:"uuid"`
}

// IncomingTransferDTO is 04 §2.16 IncomingTransferDTO. The sender's name and
// IBAN may be a natural person's: finance and platform admins only, the
// IBAN masked (BE-129).
type IncomingTransferDTO struct {
	TransferID          string             `json:"transfer_id" format:"uuid"`
	CollectionAccountID string             `json:"collection_account_id" format:"uuid"`
	ValueDate           string             `json:"value_date" format:"date"`
	Amount              int64              `json:"amount" doc:"Kuruş"`
	Currency            string             `json:"currency" enum:"TRY"`
	SenderName          string             `json:"sender_name,omitempty"`
	SenderIBANMasked    string             `json:"sender_iban_masked,omitempty" doc:"Masked (adminRevealIncomingTransferIban field sender)"`
	Description         string             `json:"description"`
	BankReference       string             `json:"bank_reference"`
	ExtractedOrderNo    string             `json:"extracted_order_no,omitempty" doc:"The reference code found in the description"`
	Status              string             `json:"status" enum:"unmatched,matched,ignored,return_requested,return_approved,returned,collection_requested,collected"`
	MatchedOrderID      string             `json:"matched_order_id,omitempty" format:"uuid"`
	MatchedBy           string             `json:"matched_by,omitempty" format:"uuid"`
	MatchedAt           *time.Time         `json:"matched_at,omitempty"`
	IgnoreReason        string             `json:"ignore_reason,omitempty"`
	ReceiptFileID       string             `json:"receipt_file_id,omitempty" format:"uuid"`
	EntryMethod         string             `json:"entry_method" enum:"manual,csv_import,bank_api"`
	EnteredBy           string             `json:"entered_by" format:"uuid"`
	Return              *TransferReturnDTO `json:"return,omitempty"`
	CreatedAt           time.Time          `json:"created_at"`
}

func transferDTO(t IncomingTransfer) IncomingTransferDTO {
	d := IncomingTransferDTO{
		TransferID: t.ID.String(), CollectionAccountID: t.CollectionAccountID.String(), ValueDate: t.ValueDate.String(),
		Amount: t.Amount, Currency: t.Currency, SenderName: str(t.SenderName), SenderIBANMasked: masked(t.SenderIBAN),
		Description: t.Description, BankReference: t.BankReference, ExtractedOrderNo: str(t.ExtractedOrderNo),
		Status: string(t.Status), MatchedOrderID: httpx.IDStr(t.MatchedOrderID), MatchedBy: httpx.IDStr(t.MatchedBy), MatchedAt: utc(t.MatchedAt),
		IgnoreReason: str(t.IgnoreReason), ReceiptFileID: httpx.IDStr(t.ReceiptFileID), EntryMethod: string(t.EntryMethod),
		EnteredBy: t.EnteredBy.String(), CreatedAt: t.CreatedAt.UTC(),
	}
	if t.ReturnRequestedBy != nil {
		d.Return = &TransferReturnDTO{
			RequestedBy: httpx.IDStr(t.ReturnRequestedBy), RequestedAt: utc(t.ReturnRequestedAt), Reason: str(t.ReturnReason),
			TargetIBANMasked: masked(t.ReturnTargetIBAN), ApprovedBy: httpx.IDStr(t.ReturnApprovedBy), ApprovedAt: utc(t.ReturnApprovedAt),
			ReturnedAt: utc(t.ReturnedAt), BankReference: str(t.ReturnBankReference), ReceiptFileID: httpx.IDStr(t.ReturnReceiptFileID),
		}
	}
	return d
}

// SuggestionDTO is one suggestion of adminGetIncomingTransfer (04 §2.16).
type SuggestionDTO struct {
	OrderID         string  `json:"order_id" format:"uuid"`
	OrderNo         string  `json:"order_no"`
	AmountDueAmount int64   `json:"amount_due_amount" doc:"Kuruş the order still waits for"`
	OrderStatus     string  `json:"order_status"`
	MatchConfidence float64 `json:"match_confidence" minimum:"0" maximum:"1" doc:"How well the transfer matches the order: 1, 0.7, 0.5 or 0"`
}

// IncomingTransferDetailDTO is the body of adminGetIncomingTransfer.
type IncomingTransferDetailDTO struct {
	IncomingTransferDTO
	Suggestions []SuggestionDTO `json:"suggestions"`
}

// IncomingTransferList is the body of adminQueryIncomingTransfers.
type IncomingTransferList struct {
	Items      []IncomingTransferDTO `json:"items"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

// sortTransfers is the only order of the transfer list (cursor fingerprint).
const sortTransfers = "-value_date"

type queryTransfersInput struct {
	Body struct {
		Status        string `json:"status,omitempty" enum:"unmatched,matched,ignored,return_requested,return_approved,returned,collection_requested,collected"`
		ValueDateFrom string `json:"value_date_from,omitempty" maxLength:"10" doc:"YYYY-MM-DD (inclusive)"`
		ValueDateTo   string `json:"value_date_to,omitempty" maxLength:"10" doc:"YYYY-MM-DD (inclusive)"`
		Q             string `json:"q,omitempty" maxLength:"100" doc:"Part of the description, the sender's name or the bank reference; or a reference code"`
		page.BodyParams
	}
}

type transferListOutput struct{ Body IncomingTransferList }

func (h handler) queryTransfers(ctx context.Context, in *queryTransfersInput) (*transferListOutput, error) {
	b := in.Body
	return h.transferPage(ctx, transferArgs{opID: "adminQueryIncomingTransfers", status: b.Status,
		from: b.ValueDateFrom, to: b.ValueDateTo, q: b.Q, cursor: b.Cursor, limit: b.Limit})
}

// transferArgs are the criteria of one page of the transfer list as the
// client sent them in the body.
type transferArgs struct {
	opID, status, from, to, q, cursor string
	limit                             int
}

func (h handler) transferPage(ctx context.Context, args transferArgs) (*transferListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint(args.opID, args.status, args.from, args.to, args.q)
	p, err := h.svc.ListTransfers(ctx, a, args.query(fp))
	if err != nil {
		return nil, err
	}
	out := &transferListOutput{Body: IncomingTransferList{Items: make([]IncomingTransferDTO, len(p.Items))}}
	for i, t := range p.Items {
		out.Body.Items[i] = transferDTO(t)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortTransfers, fp, p.Next.ValueDate.String(), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

// query parses the criteria; a malformed one is reported by the service
// after authorisation (TransferQuery.invalid).
func (args transferArgs) query(fp string) TransferQuery {
	q := TransferQuery{Filter: TransferFilter{Status: TransferStatus(args.status), Q: args.q}, Limit: args.limit}
	for _, b := range []struct {
		raw, loc string
		dst      **civil.Date
	}{{args.from, "body.value_date_from", &q.Filter.ValueFrom}, {args.to, "body.value_date_to", &q.Filter.ValueTo}} {
		if b.raw == "" {
			continue
		}
		d, err := civil.Parse(b.raw)
		if err != nil {
			q.invalid = problem.Field(b.loc, "must be a date (YYYY-MM-DD)")
			return q
		}
		*b.dst = &d
	}
	c, err := page.Decode(args.cursor, sortTransfers, fp, 2)
	if err != nil {
		q.invalid = err
		return q
	}
	if c != nil {
		d, err := civil.Parse(c.Vals[0])
		if err != nil {
			q.invalid = page.ErrInvalidCursor
			return q
		}
		id, err := c.UUID(1)
		if err != nil {
			q.invalid = err
			return q
		}
		q.After = &TransferCursor{ValueDate: d, ID: id}
	}
	return q
}

type transferDetailOutput struct{ Body IncomingTransferDetailDTO }

func (h handler) getTransfer(ctx context.Context, in *TransferPath) (*transferDetailOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetTransfer(ctx, a, httpx.ParseID(in.TransferID))
	if err != nil {
		return nil, err
	}
	out := &transferDetailOutput{Body: IncomingTransferDetailDTO{IncomingTransferDTO: transferDTO(v.Transfer),
		Suggestions: make([]SuggestionDTO, len(v.Suggestions))}}
	for i, s := range v.Suggestions {
		out.Body.Suggestions[i] = SuggestionDTO{OrderID: s.OrderID.String(), OrderNo: s.OrderNo, AmountDueAmount: s.AmountDue,
			OrderStatus: string(s.OrderStatus), MatchConfidence: s.MatchConfidence}
	}
	return out, nil
}

// transferIBANParam is the field of adminRevealIncomingTransferIban; its
// enum is TransferIBANs (BE-117: the contract cannot drift from the check).
type transferIBANParam string

// TransformSchema implements huma.SchemaTransformer.
func (transferIBANParam) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Enum = op.Enum(TransferIBANs())
	return s
}

type revealTransferInput struct {
	TransferPath
	Field transferIBANParam `query:"field" required:"true" doc:"sender or return_target"`
	httpx.AccessReasonHeader
}

func (h handler) revealTransfer(ctx context.Context, in *revealTransferInput) (*parties.RevealOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	n, err := h.svc.RevealTransferIBAN(ctx, a, httpx.ParseID(in.TransferID), TransferIBAN(in.Field), in.AccessReason.String())
	if err != nil {
		return nil, err
	}
	return parties.RevealOut(n), nil
}

// ReceiptLinkDTO is the body of adminDownloadIncomingTransferReceipt (04
// §1.14 signed URL).
type ReceiptLinkDTO struct {
	URL       string    `json:"url" doc:"Valid for at most 5 minutes"`
	ExpiresAt time.Time `json:"expires_at"`
}

type receiptOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ReceiptLinkDTO
}

func (h handler) transferReceipt(ctx context.Context, in *TransferPath) (*receiptOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.TransferReceipt(ctx, a, httpx.ParseID(in.TransferID))
	if err != nil {
		return nil, err
	}
	return &receiptOutput{CacheControl: "no-store", Body: ReceiptLinkDTO{URL: d.URL, ExpiresAt: d.ExpiresAt.UTC()}}, nil
}

type transferOutput struct{ Body IncomingTransferDTO }

type createTransferInput struct {
	idempotency.OptionalHeader
	Body struct {
		CollectionAccountID string `json:"collection_account_id" format:"uuid"`
		ValueDate           string `json:"value_date" format:"date" doc:"The bank's value date (not in the future)"`
		Amount              int64  `json:"amount" minimum:"1" doc:"Kuruş"`
		SenderName          string `json:"sender_name,omitempty" maxLength:"300"`
		SenderIBAN          string `json:"sender_iban,omitempty" maxLength:"42"`
		Description         string `json:"description" minLength:"1" maxLength:"500" doc:"The transfer's description as the bank shows it"`
		BankReference       string `json:"bank_reference" minLength:"1" maxLength:"100" doc:"The bank's receipt or transaction number"`
		ReceiptFileID       string `json:"receipt_file_id,omitempty" format:"uuid" doc:"A transfer_receipt upload"`
	}
}

func (h handler) createTransfer(ctx context.Context, in *createTransferInput) (*transferOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	ti := TransferInput{CollectionAccountID: httpx.ParseID(b.CollectionAccountID), Amount: b.Amount, SenderName: b.SenderName,
		SenderIBAN: b.SenderIBAN, Description: b.Description, BankReference: b.BankReference}
	if d, err := civil.Parse(b.ValueDate); err == nil {
		ti.ValueDate = d
	}
	if b.ReceiptFileID != "" {
		id := httpx.ParseID(b.ReceiptFileID)
		ti.ReceiptFileID = &id
	}
	t, err := h.svc.CreateTransfer(ctx, a, ti)
	if err != nil {
		return nil, err
	}
	return &transferOutput{Body: transferDTO(t)}, nil
}

// ImportResultDTO is the body of adminImportIncomingTransfers (04 §2.16).
type ImportResultDTO struct {
	Imported   int            `json:"imported"`
	Duplicates int            `json:"duplicates"`
	Rejected   int            `json:"rejected"`
	Rows       []ImportRowDTO `json:"rows"`
}

// ImportRowDTO is the outcome of one statement row.
type ImportRowDTO struct {
	Line       int    `json:"line" doc:"Line of the CSV text (the header is line 1)"`
	Outcome    string `json:"outcome" enum:"imported,duplicate,rejected"`
	TransferID string `json:"transfer_id,omitempty" format:"uuid"`
	Error      string `json:"error,omitempty" doc:"Why the row was rejected (never a value of the row)"`
}

type importInput struct {
	idempotency.RequiredHeader
	Body struct {
		CollectionAccountID string `json:"collection_account_id" format:"uuid"`
		CSV                 string `json:"csv" minLength:"1" maxLength:"1048576" doc:"The bank statement as CSV text"`
	}
}

type importOutput struct{ Body ImportResultDTO }

func (h handler) importTransfers(ctx context.Context, in *importInput) (*importOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.ImportTransfers(ctx, a, ImportInput{CollectionAccountID: httpx.ParseID(in.Body.CollectionAccountID), CSV: in.Body.CSV})
	if err != nil {
		return nil, err
	}
	out := &importOutput{Body: ImportResultDTO{Imported: r.Imported, Duplicates: r.Duplicates, Rejected: r.Rejected,
		Rows: make([]ImportRowDTO, len(r.Rows))}}
	for i, row := range r.Rows {
		out.Body.Rows[i] = ImportRowDTO{Line: row.Line, Outcome: row.Outcome, TransferID: httpx.IDStr(row.TransferID), Error: row.Error}
	}
	return out, nil
}

// PaymentSummaryDTO is the payment of MatchResultDTO and DirectResultDTO.
type PaymentSummaryDTO struct {
	PaymentID  string    `json:"payment_id" format:"uuid"`
	Provider   string    `json:"provider" enum:"bank_transfer,iyzico,direct_confirmed,manual"`
	Amount     int64     `json:"amount" doc:"Kuruş"`
	Status     string    `json:"status"`
	CapturedAt time.Time `json:"captured_at"`
}

// OrderSummaryDTO is the order of MatchResultDTO and DirectResultDTO.
type OrderSummaryDTO struct {
	OrderID    string `json:"order_id" format:"uuid"`
	OrderNo    string `json:"order_no"`
	Status     string `json:"status"`
	PaidAmount int64  `json:"paid_amount" doc:"Kuruş Koro has collected"`
	DueAmount  int64  `json:"due_amount" doc:"Kuruş Koro still waits for"`
	Paid       bool   `json:"paid" doc:"This capture completed the order"`
}

// MatchResultDTO is 04 §C.4 MatchResultDTO.
type MatchResultDTO struct {
	TransferID          string            `json:"transfer_id" format:"uuid"`
	Status              string            `json:"status"`
	Payment             PaymentSummaryDTO `json:"payment"`
	Order               OrderSummaryDTO   `json:"order"`
	AppliedAmount       int64             `json:"applied_amount" doc:"Kuruş applied to the order"`
	OverpaymentRefundID *string           `json:"overpayment_refund_id" format:"uuid" doc:"The overpayment refund request, or null"`
}

func paymentDTO(p PaymentSummary) PaymentSummaryDTO {
	return PaymentSummaryDTO{PaymentID: p.PaymentID.String(), Provider: string(p.Provider), Amount: p.Amount, Status: string(p.Status),
		CapturedAt: p.CapturedAt.UTC()}
}

func orderDTO(o OrderSummary) OrderSummaryDTO {
	return OrderSummaryDTO{OrderID: o.OrderID.String(), OrderNo: o.OrderNo, Status: string(o.Status), PaidAmount: o.PaidAmount,
		DueAmount: o.DueAmount, Paid: o.Paid}
}

type matchInput struct {
	idempotency.RequiredHeader
	TransferPath
	Body struct {
		OrderID         string `json:"order_id" format:"uuid"`
		ExpectedOrderNo string `json:"expected_order_no" minLength:"10" maxLength:"20" doc:"The order's reference code, as finance reads it"`
		RefundIBAN      string `json:"refund_iban,omitempty" maxLength:"42" doc:"Where an overpayment goes back (default: the sender's IBAN)"`
	}
}

type matchOutput struct{ Body MatchResultDTO }

func (h handler) match(ctx context.Context, in *matchInput) (*matchOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.MatchTransfer(ctx, a, httpx.ParseID(in.TransferID), MatchInput{OrderID: httpx.ParseID(in.Body.OrderID),
		ExpectedOrderNo: in.Body.ExpectedOrderNo, RefundIBAN: in.Body.RefundIBAN})
	if err != nil {
		return nil, err
	}
	d := MatchResultDTO{TransferID: r.TransferID.String(), Status: string(r.Status), Payment: paymentDTO(r.Payment),
		Order: orderDTO(r.Order), AppliedAmount: r.AppliedAmount}
	if r.OverpaymentRefundID != nil {
		s := r.OverpaymentRefundID.String()
		d.OverpaymentRefundID = &s
	}
	return &matchOutput{Body: d}, nil
}

type ignoreInput struct {
	idempotency.OptionalHeader
	TransferPath
	Body struct {
		IgnoreReason string `json:"ignore_reason" minLength:"1" maxLength:"500" doc:"Audited; no personal data"`
	}
}

func (h handler) ignore(ctx context.Context, in *ignoreInput) (*transferOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.IgnoreTransfer(ctx, a, httpx.ParseID(in.TransferID), in.Body.IgnoreReason)
	if err != nil {
		return nil, err
	}
	return &transferOutput{Body: transferDTO(t)}, nil
}

type requestReturnInput struct {
	idempotency.OptionalHeader
	TransferPath
	Body struct {
		ReturnReason string             `json:"return_reason" minLength:"1" maxLength:"500" doc:"Audited; no personal data"`
		TargetIBAN   string             `json:"target_iban,omitempty" maxLength:"42" doc:"Default: the sender's IBAN"`
		Reauth       *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) requestReturn(ctx context.Context, in *requestReturnInput) (*transferOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.RequestReturn(ctx, a, httpx.ParseID(in.TransferID), ReturnRequest{Reason: in.Body.ReturnReason, TargetIBAN: in.Body.TargetIBAN},
		proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &transferOutput{Body: transferDTO(t)}, nil
}

type approveReturnInput struct {
	idempotency.OptionalHeader
	TransferPath
	Body struct {
		Decision string             `json:"decision" enum:"approved,rejected"`
		Reauth   *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) approveReturn(ctx context.Context, in *approveReturnInput) (*transferOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.DecideReturn(ctx, a, httpx.ParseID(in.TransferID), Decision(in.Body.Decision), proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &transferOutput{Body: transferDTO(t)}, nil
}

type markReturnedInput struct {
	idempotency.OptionalHeader
	TransferPath
	Body struct {
		BankReference string             `json:"bank_reference" minLength:"1" maxLength:"100" doc:"The EFT's bank reference"`
		ReceiptFileID string             `json:"receipt_file_id" format:"uuid" doc:"A refund_receipt upload"`
		Reauth        *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) markReturned(ctx context.Context, in *markReturnedInput) (*transferOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.MarkReturned(ctx, a, httpx.ParseID(in.TransferID), ReturnReceipt{BankReference: in.Body.BankReference,
		ReceiptFileID: httpx.ParseID(in.Body.ReceiptFileID)}, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &transferOutput{Body: transferDTO(t)}, nil
}

// DirectResultDTO is the body of adminConfirmDirectPayment.
type DirectResultDTO struct {
	Payment PaymentSummaryDTO `json:"payment"`
	Order   OrderSummaryDTO   `json:"order"`
}

type directInput struct {
	idempotency.RequiredHeader
	Body struct {
		OrderID         string `json:"order_id" format:"uuid"`
		ExpectedOrderNo string `json:"expected_order_no" minLength:"10" maxLength:"20" doc:"The order's reference code"`
		EvidenceFileID  string `json:"evidence_file_id" format:"uuid" doc:"A transfer_receipt upload: the buyer's transfer to the payee"`
	}
}

type directOutput struct{ Body DirectResultDTO }

func (h handler) confirmDirect(ctx context.Context, in *directInput) (*directOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.ConfirmDirectPayment(ctx, a, DirectInput{OrderID: httpx.ParseID(in.Body.OrderID), ExpectedOrderNo: in.Body.ExpectedOrderNo,
		EvidenceFileID: httpx.ParseID(in.Body.EvidenceFileID)})
	if err != nil {
		return nil, err
	}
	return &directOutput{Body: DirectResultDTO{Payment: paymentDTO(r.Payment), Order: orderDTO(r.Order)}}, nil
}
