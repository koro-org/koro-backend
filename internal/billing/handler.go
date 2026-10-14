package billing

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

// Register adds the billing operations of 04 §2.18 to api. svc may be nil
// (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	reg(api, huma.Operation{
		OperationID: "listOrgInvoices", Method: http.MethodGet, Path: "/orgs/{org_id}/invoices",
		Summary: "List the invoices of an organization",
		Description: "The invoices the organization issues (direction=issued: a payee's service invoices) and receives " +
			"(direction=received: a company's invoices, Koro's commission invoices to a payee), newest first; both when " +
			"direction is omitted. Planned invoices are the documents the commerce model expects (02 §18.3).",
	}, op.Meta{Action: string(authz.BillingInvoiceList), Surface: op.SurfaceOrg}, h.listOrg)

	reg(api, huma.Operation{
		OperationID: "getInvoice", Method: http.MethodGet, Path: "/invoices/{invoice_id}",
		Summary:     "Get an invoice",
		Description: "The invoice with its lines, for its issuing and receiving organizations and finance.",
	}, op.Meta{Action: string(authz.BillingInvoiceRead), Surface: op.SurfaceResource}, h.get)

	reg(api, huma.Operation{
		OperationID: "downloadInvoicePdf", Method: http.MethodGet, Path: "/invoices/{invoice_id}/pdf",
		Summary: "Get a download URL of an invoice's PDF",
		Description: "A URL valid for 5 minutes. Only an issued invoice (409 billing.invalid_state); an uploaded PDF " +
			"waiting for verification only for its uploader side and finance. 409 files.not_clean until the virus scan " +
			"clears the file.",
	}, op.Meta{Action: string(authz.BillingInvoiceRead), Surface: op.SurfaceResource}, h.pdf)

	reg(api, huma.Operation{
		OperationID: "uploadInvoice", Method: http.MethodPost, Path: "/invoices/{invoice_id}/upload",
		Summary: "Upload the document of a planned invoice",
		Description: "The PDF (an invoice_pdf upload of the caller or of the issuing organization), the invoice number, " +
			"the issue date and the document type of a planned invoice without a document, or of one whose upload was " +
			"found not to match (409 billing.invalid_state otherwise). The issuing organization's admins upload its own " +
			"service invoices (03 k29); finance uploads any. Another finance admin verifies it.",
	}, op.Meta{Action: string(authz.BillingInvoiceUpload), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.upload)

	reg(api, huma.Operation{
		OperationID: "adminListInvoices", Method: http.MethodGet, Path: "/admin/invoices",
		Summary:     "List every invoice",
		Description: "Newest first; status narrows the queue (planned: to upload or verify, mismatch).",
	}, op.Meta{Action: string(authz.BillingInvoiceListAdmin), Surface: op.SurfaceAdmin}, h.adminList)

	reg(api, huma.Operation{
		OperationID: "adminVerifyInvoice", Method: http.MethodPost, Path: "/admin/invoices/{invoice_id}/verify",
		Summary: "Verify an uploaded invoice (second eye)",
		Description: "The number, date and amounts of the uploaded document match the planned invoice (issued: the PDF " +
			"is e-mailed to the recipient) or not (mismatch with a note: the uploader side may upload again). The " +
			"uploader cannot verify (403 authz.four_eyes_violation). Only an upload waiting for verification (409 " +
			"billing.invalid_state).",
	}, op.Meta{Action: string(authz.BillingInvoiceVerify), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.verify)

	reg(api, huma.Operation{
		OperationID: "adminCancelInvoice", Method: http.MethodPost, Path: "/admin/invoices/{invoice_id}/cancel",
		Summary: "Cancel an invoice",
		Description: "An unissued invoice is cancelled; an issued one is cancelled and a credit note of it is planned " +
			"(credit_note in the answer). A cancelled invoice or an issued credit note is final (409 billing.invalid_state).",
	}, op.Meta{Action: string(authz.BillingInvoiceCancel), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.cancel)

	reg(api, huma.Operation{
		OperationID: "adminListIntegrations", Method: http.MethodGet, Path: "/admin/integrations",
		Summary:     "List the integration connections",
		Description: "The account and the token state of each integration; secret values are never returned.",
	}, op.Meta{Action: string(authz.BillingIntegrationManage), Surface: op.SurfaceAdmin}, h.listIntegrations)

	reg(api, huma.Operation{
		OperationID: "adminPutIntegration", Method: http.MethodPut, Path: "/admin/integrations/{provider}",
		Summary: "Set the credentials of an integration",
		Description: "Requires re-authentication. The tokens are write-only: they are stored envelope-encrypted and " +
			"never returned (503 when no encryption key is configured).",
	}, op.Meta{Action: string(authz.BillingIntegrationManage), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth},
		h.putIntegration)
}

type handler struct{ svc Service }

// reg registers one billing operation with its Koro metadata.
func reg[I, O any](api huma.API, o huma.Operation, m op.Meta, fn func(context.Context, *I) (*O, error)) {
	o.Tags = []string{"billing"}
	huma.Register(api, op.Koro(o, m), fn)
}

// sortInvoices is the only order of the invoice lists (cursor fingerprint).
const sortInvoices = "-created_at"

// parseID parses a path ID; a malformed one becomes the nil UUID, which no
// row has: it answers like a missing row (404) after the surface's authz.
func parseID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil()
	}
	return id
}

func optID(s, loc string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, fieldErr(loc, "must be a UUID")
	}
	return &id, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// ---- DTOs

// InvoiceAddressDTO is the address of a party snapshot.
type InvoiceAddressDTO struct {
	Line1      string `json:"line1,omitempty"`
	District   string `json:"district,omitempty"`
	City       string `json:"city,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

// InvoicePartyDTO is an issuer or recipient as the invoice names it.
type InvoicePartyDTO struct {
	Title     string             `json:"title"`
	TaxOffice string             `json:"tax_office,omitempty"`
	TaxNo     string             `json:"tax_no,omitempty" doc:"VKN"`
	Address   *InvoiceAddressDTO `json:"address,omitempty"`
	Email     string             `json:"email,omitempty" doc:"The invoice e-mail address"`
}

// InvoiceLineDTO is one line of an invoice (kuruş; VAT in basis points).
type InvoiceLineDTO struct {
	Position           int    `json:"position"`
	OrderLineID        string `json:"order_line_id,omitempty" format:"uuid"`
	Description        string `json:"description"`
	Quantity           int    `json:"quantity"`
	UnitPriceAmount    int64  `json:"unit_price_amount"`
	NetAmount          int64  `json:"net_amount"`
	VATRateBP          int    `json:"vat_rate_bp"`
	VATAmount          int64  `json:"vat_amount"`
	VATExemptionCode   string `json:"vat_exemption_code,omitempty"`
	VATWithholdingCode string `json:"vat_withholding_code,omitempty"`
	VATWithheldAmount  int64  `json:"vat_withheld_amount"`
	TotalAmount        int64  `json:"total_amount"`
}

// InvoiceDTO is an invoice (04 §2.18).
type InvoiceDTO struct {
	InvoiceID         string           `json:"invoice_id" format:"uuid"`
	Kind              string           `json:"kind" enum:"service_sale,commission,platform_fee,payee_to_koro,credit_note"`
	Status            string           `json:"status" enum:"planned,queued,provider_pending,issued,failed,mismatch,cancelled"`
	IssuingMode       string           `json:"issuing_mode" enum:"provider,manual_upload,seller_uploaded,koro_on_behalf"`
	DocumentType      string           `json:"document_type" enum:"e_fatura,e_arsiv,paper,unknown"`
	Trigger           string           `json:"trigger" enum:"on_payment,on_fulfillment,on_payout,manual"`
	OrderID           string           `json:"order_id,omitempty" format:"uuid"`
	PayoutID          string           `json:"payout_id,omitempty" format:"uuid"`
	IssuerPartyID     string           `json:"issuer_party_id" format:"uuid"`
	RecipientPartyID  string           `json:"recipient_party_id" format:"uuid"`
	IssuerOrgID       string           `json:"issuer_org_id,omitempty" format:"uuid"`
	RecipientOrgID    string           `json:"recipient_org_id,omitempty" format:"uuid"`
	IssuerSnapshot    InvoicePartyDTO  `json:"issuer_snapshot"`
	RecipientSnapshot InvoicePartyDTO  `json:"recipient_snapshot"`
	InvoiceNo         string           `json:"invoice_no,omitempty"`
	IssueDate         string           `json:"issue_date,omitempty" format:"date"`
	ETTN              string           `json:"ettn,omitempty" format:"uuid"`
	Currency          string           `json:"currency" enum:"TRY"`
	NetAmount         int64            `json:"net_amount" doc:"Kuruş"`
	VATAmount         int64            `json:"vat_amount" doc:"Kuruş"`
	VATWithheldAmount int64            `json:"vat_withheld_amount" doc:"Kuruş (tevkifat)"`
	TotalAmount       int64            `json:"total_amount" doc:"Kuruş: net + VAT"`
	PayableAmount     int64            `json:"payable_amount" doc:"Kuruş: total − withheld VAT"`
	RefersToInvoiceID string           `json:"refers_to_invoice_id,omitempty" format:"uuid" doc:"A credit note's invoice"`
	HasPDF            bool             `json:"has_pdf"`
	UploadedBy        string           `json:"uploaded_by,omitempty" format:"uuid"`
	MismatchNote      string           `json:"mismatch_note,omitempty" doc:"Why the upload did not match (status mismatch)"`
	IssuedAt          *time.Time       `json:"issued_at,omitempty"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
	Lines             []InvoiceLineDTO `json:"lines,omitempty" doc:"getInvoice and the admin writes"`
}

// InvoiceAdminDTO is an invoice as finance sees it.
type InvoiceAdminDTO struct {
	InvoiceDTO
	PDFFileID      string     `json:"pdf_file_id,omitempty" format:"uuid"`
	VerifiedBy     string     `json:"verified_by,omitempty" format:"uuid"`
	VerifiedAt     *time.Time `json:"verified_at,omitempty"`
	LastError      string     `json:"last_error,omitempty" doc:"The mismatch note or the provider's last error"`
	IdempotencyKey string     `json:"idempotency_key" doc:"The planned document's key (kind:source:line:seq)"`
}

func snapshotDTO(s Snapshot) InvoicePartyDTO {
	d := InvoicePartyDTO{Title: s.Title, TaxOffice: s.TaxOffice, TaxNo: s.TaxNo, Email: s.Email}
	if s.Address != (SnapshotAddress{}) {
		a := InvoiceAddressDTO(s.Address)
		d.Address = &a
	}
	return d
}

func invoiceDTO(inv Invoice, lines []Line) InvoiceDTO {
	d := InvoiceDTO{
		InvoiceID: inv.ID.String(), Kind: string(inv.Kind), Status: string(inv.Status), IssuingMode: string(inv.IssuingMode),
		DocumentType: string(inv.DocumentType), Trigger: string(inv.Trigger), OrderID: idStr(inv.OrderID),
		PayoutID: idStr(inv.PayoutID), IssuerPartyID: inv.IssuerPartyID.String(), RecipientPartyID: inv.RecipientPartyID.String(),
		IssuerOrgID: idStr(inv.IssuerOrgID), RecipientOrgID: idStr(inv.RecipientOrgID),
		IssuerSnapshot: snapshotDTO(inv.IssuerSnapshot), RecipientSnapshot: snapshotDTO(inv.RecipientSnapshot),
		ETTN: idStr(inv.ETTN), Currency: inv.Currency, NetAmount: inv.NetAmount, VATAmount: inv.VATAmount,
		VATWithheldAmount: inv.VATWithheldAmount, TotalAmount: inv.TotalAmount, PayableAmount: inv.PayableAmount,
		RefersToInvoiceID: idStr(inv.RefersToInvoiceID), HasPDF: inv.PDFFileID != nil, UploadedBy: idStr(inv.UploadedBy),
		IssuedAt: utcPtr(inv.IssuedAt), CreatedAt: inv.CreatedAt.UTC(), UpdatedAt: inv.UpdatedAt.UTC(),
	}
	if d.Currency == "" {
		d.Currency = Currency
	}
	if inv.InvoiceNo != nil {
		d.InvoiceNo = *inv.InvoiceNo
	}
	if inv.IssueDate != nil {
		d.IssueDate = inv.IssueDate.String()
	}
	if inv.Status == StatusMismatch && inv.LastError != nil {
		d.MismatchNote = *inv.LastError
	}
	for _, l := range lines {
		ld := InvoiceLineDTO{
			Position: int(l.Position), OrderLineID: idStr(l.OrderLineID), Description: l.Description, Quantity: l.Quantity,
			UnitPriceAmount: l.UnitPriceAmount, NetAmount: l.NetAmount, VATRateBP: l.VATRateBP, VATAmount: l.VATAmount,
			VATWithheldAmount: l.VATWithheldAmount, TotalAmount: l.TotalAmount,
		}
		if l.VATExemptionCode != nil {
			ld.VATExemptionCode = *l.VATExemptionCode
		}
		if l.VATWithholdingCode != nil {
			ld.VATWithholdingCode = *l.VATWithholdingCode
		}
		d.Lines = append(d.Lines, ld)
	}
	return d
}

func invoiceAdminDTO(inv Invoice, lines []Line) InvoiceAdminDTO {
	d := InvoiceAdminDTO{InvoiceDTO: invoiceDTO(inv, lines), PDFFileID: idStr(inv.PDFFileID), VerifiedBy: idStr(inv.VerifiedBy),
		VerifiedAt: utcPtr(inv.VerifiedAt), IdempotencyKey: inv.IdempotencyKey}
	if inv.LastError != nil {
		d.LastError = *inv.LastError
	}
	return d
}

// ---- lists

// InvoiceList is the body of listOrgInvoices.
type InvoiceList struct {
	Items      []InvoiceDTO `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type invoiceListOutput struct{ Body InvoiceList }

type listOrgInput struct {
	page.Params
	OrgID     string `path:"org_id" format:"uuid"`
	Direction string `query:"direction" enum:"issued,received" doc:"issued: the org issues them; received: the org receives them; both when omitted"`
	Kind      string `query:"kind" enum:"service_sale,commission,platform_fee,payee_to_koro,credit_note"`
	Status    string `query:"status" enum:"planned,queued,provider_pending,issued,failed,mismatch,cancelled"`
}

func (h handler) listOrg(ctx context.Context, in *listOrgInput) (*invoiceListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listOrgInvoices", in.OrgID, in.Direction, in.Kind, in.Status)
	q := ListQuery{Direction: Direction(in.Direction), Kind: Kind(in.Kind), Status: Status(in.Status), Limit: in.Limit}
	q.After, q.invalid = invoiceAfter(in.Cursor, fp)
	p, err := h.svc.ListOrgInvoices(ctx, a, parseID(in.OrgID), q)
	if err != nil {
		return nil, err
	}
	out := &invoiceListOutput{Body: InvoiceList{Items: make([]InvoiceDTO, len(p.Items))}}
	for i, inv := range p.Items {
		out.Body.Items[i] = invoiceDTO(inv, nil)
	}
	out.Body.NextCursor = invoiceNext(p.Next, fp)
	return out, nil
}

// InvoiceAdminList is the body of adminListInvoices.
type InvoiceAdminList struct {
	Items      []InvoiceAdminDTO `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type invoiceAdminListOutput struct{ Body InvoiceAdminList }

type adminListInput struct {
	page.Params
	Status   string `query:"status" enum:"planned,queued,provider_pending,issued,failed,mismatch,cancelled"`
	Kind     string `query:"kind" enum:"service_sale,commission,platform_fee,payee_to_koro,credit_note"`
	OrderID  string `query:"order_id" format:"uuid"`
	PayoutID string `query:"payout_id" format:"uuid"`
}

func (h handler) adminList(ctx context.Context, in *adminListInput) (*invoiceAdminListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListInvoices", in.Status, in.Kind, in.OrderID, in.PayoutID)
	q := ListQuery{Kind: Kind(in.Kind), Status: Status(in.Status), Limit: in.Limit}
	q.After, q.invalid = invoiceAfter(in.Cursor, fp)
	if q.invalid == nil {
		q.OrderID, q.invalid = optID(in.OrderID, "query.order_id")
	}
	if q.invalid == nil {
		q.PayoutID, q.invalid = optID(in.PayoutID, "query.payout_id")
	}
	p, err := h.svc.AdminListInvoices(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &invoiceAdminListOutput{Body: InvoiceAdminList{Items: make([]InvoiceAdminDTO, len(p.Items))}}
	for i, inv := range p.Items {
		out.Body.Items[i] = invoiceAdminDTO(inv, nil)
	}
	out.Body.NextCursor = invoiceNext(p.Next, fp)
	return out, nil
}

// invoiceAfter decodes an invoice cursor (created_at, id).
func invoiceAfter(cursor, fp string) (*Cursor, error) {
	c, err := page.Decode(cursor, sortInvoices, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &Cursor{CreatedAt: at, ID: id}, nil
}

func invoiceNext(next *Cursor, fp string) string {
	if next == nil {
		return ""
	}
	return page.Next(sortInvoices, fp, page.TimeVal(next.CreatedAt), page.UUIDVal(next.ID))
}

// ---- one invoice

// InvoicePath is the path of the /invoices/{invoice_id} operations.
type InvoicePath struct {
	InvoiceID string `path:"invoice_id" format:"uuid"`
}

type getInput struct{ InvoicePath }

type invoiceOutput struct{ Body InvoiceDTO }

func (h handler) get(ctx context.Context, in *getInput) (*invoiceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetInvoice(ctx, a, parseID(in.InvoiceID))
	if err != nil {
		return nil, err
	}
	return &invoiceOutput{Body: invoiceDTO(v.Invoice, v.Lines)}, nil
}

// InvoicePDFLinkDTO is the body of downloadInvoicePdf (04 §1.11 signed URL).
type InvoicePDFLinkDTO struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type pdfOutput struct{ Body InvoicePDFLinkDTO }

func (h handler) pdf(ctx context.Context, in *getInput) (*pdfOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.InvoicePDF(ctx, a, parseID(in.InvoiceID))
	if err != nil {
		return nil, err
	}
	return &pdfOutput{Body: InvoicePDFLinkDTO{URL: d.URL, ExpiresAt: d.ExpiresAt.UTC()}}, nil
}

type uploadInput struct {
	idempotency.OptionalHeader
	InvoicePath
	Body struct {
		InvoiceNo    string `json:"invoice_no" minLength:"1" maxLength:"30" doc:"The number on the document"`
		IssueDate    string `json:"issue_date" format:"date" doc:"The issue date on the document (not in the future)"`
		DocumentType string `json:"document_type" enum:"e_fatura,e_arsiv,paper"`
		PDFFileID    string `json:"pdf_file_id" format:"uuid" doc:"An invoice_pdf upload (application/pdf)"`
		ETTN         string `json:"ettn,omitempty" format:"uuid" doc:"The e-document's ETTN"`
	}
}

func (h handler) upload(ctx context.Context, in *uploadInput) (*invoiceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ui := UploadInput{InvoiceNo: in.Body.InvoiceNo, IssueDate: in.Body.IssueDate, DocumentType: DocumentType(in.Body.DocumentType),
		PDFFileID: parseID(in.Body.PDFFileID)}
	if in.Body.ETTN != "" {
		e := parseID(in.Body.ETTN)
		ui.ETTN = &e
	}
	v, err := h.svc.UploadInvoice(ctx, a, parseID(in.InvoiceID), ui)
	if err != nil {
		return nil, err
	}
	return &invoiceOutput{Body: invoiceDTO(v.Invoice, v.Lines)}, nil
}

type invoiceAdminOutput struct{ Body InvoiceAdminDTO }

type verifyInput struct {
	idempotency.OptionalHeader
	InvoicePath
	Body struct {
		Result string `json:"result" enum:"issued,mismatch"`
		Note   string `json:"note,omitempty" maxLength:"500" doc:"What does not match (required for mismatch; no personal data)"`
	}
}

func (h handler) verify(ctx context.Context, in *verifyInput) (*invoiceAdminOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminVerifyInvoice(ctx, a, parseID(in.InvoiceID), VerifyInput{Result: Status(in.Body.Result), Note: in.Body.Note})
	if err != nil {
		return nil, err
	}
	return &invoiceAdminOutput{Body: invoiceAdminDTO(v.Invoice, v.Lines)}, nil
}

// CancelledInvoiceDTO is the body of adminCancelInvoice.
type CancelledInvoiceDTO struct {
	InvoiceAdminDTO
	CreditNote *InvoiceAdminDTO `json:"credit_note,omitempty" doc:"The credit note planned for an issued invoice"`
}

type cancelOutput struct{ Body CancelledInvoiceDTO }

type cancelInput struct {
	idempotency.OptionalHeader
	InvoicePath
	Body struct {
		Reason string `json:"reason" minLength:"1" maxLength:"500" doc:"Why the invoice is cancelled (no personal data)"`
	}
}

func (h handler) cancel(ctx context.Context, in *cancelInput) (*cancelOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.AdminCancelInvoice(ctx, a, parseID(in.InvoiceID), in.Body.Reason)
	if err != nil {
		return nil, err
	}
	out := &cancelOutput{Body: CancelledInvoiceDTO{InvoiceAdminDTO: invoiceAdminDTO(r.Invoice, nil)}}
	if r.CreditNote != nil {
		cn := invoiceAdminDTO(*r.CreditNote, nil)
		out.Body.CreditNote = &cn
	}
	return out, nil
}

// ---- integrations

// IntegrationDTO is an integration's public state (no secret value).
type IntegrationDTO struct {
	Provider        string     `json:"provider" enum:"parasut,iyzico,iys,email"`
	ExternalAccount string     `json:"external_account,omitempty" doc:"The account at the provider (Paraşüt company ID)"`
	HasAccessToken  bool       `json:"has_access_token"`
	HasRefreshToken bool       `json:"has_refresh_token"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	RotatedAt       *time.Time `json:"rotated_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func integrationDTO(i Integration) IntegrationDTO {
	return IntegrationDTO{Provider: i.Provider, ExternalAccount: i.ExternalAccount, HasAccessToken: i.HasAccessToken,
		HasRefreshToken: i.HasRefreshToken, ExpiresAt: utcPtr(i.ExpiresAt), RotatedAt: utcPtr(i.RotatedAt),
		UpdatedAt: i.UpdatedAt.UTC()}
}

// IntegrationList is the body of adminListIntegrations.
type IntegrationList struct {
	Items []IntegrationDTO `json:"items"`
}

type integrationListOutput struct{ Body IntegrationList }

func (h handler) listIntegrations(ctx context.Context, _ *struct{}) (*integrationListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	is, err := h.svc.AdminListIntegrations(ctx, a)
	if err != nil {
		return nil, err
	}
	out := &integrationListOutput{Body: IntegrationList{Items: make([]IntegrationDTO, len(is))}}
	for i, it := range is {
		out.Body.Items[i] = integrationDTO(it)
	}
	return out, nil
}

// IntegrationCredentialsDTO is the write-only token part of adminPutIntegration.
type IntegrationCredentialsDTO struct {
	AccessToken  string     `json:"access_token,omitempty" maxLength:"8192" writeOnly:"true" doc:"Never returned"`
	RefreshToken string     `json:"refresh_token,omitempty" maxLength:"8192" writeOnly:"true" doc:"Never returned"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty" doc:"When the access token expires"`
}

type putIntegrationInput struct {
	Provider string `path:"provider" enum:"parasut,iyzico,iys,email"`
	Body     struct {
		ExternalAccount string                    `json:"external_account,omitempty" maxLength:"100"`
		Credentials     IntegrationCredentialsDTO `json:"credentials"`
		Reauth          *consent.ReauthDTO        `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

type integrationOutput struct{ Body IntegrationDTO }

func (h handler) putIntegration(ctx context.Context, in *putIntegrationInput) (*integrationOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var proof identity.ReauthProof
	if r := in.Body.Reauth; r != nil {
		proof = identity.ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
	}
	c := in.Body.Credentials
	it, err := h.svc.AdminPutIntegration(ctx, a, in.Provider, IntegrationInput{ExternalAccount: in.Body.ExternalAccount,
		AccessToken: c.AccessToken, RefreshToken: c.RefreshToken, ExpiresAt: c.ExpiresAt}, proof)
	if err != nil {
		return nil, err
	}
	return &integrationOutput{Body: integrationDTO(it)}, nil
}
