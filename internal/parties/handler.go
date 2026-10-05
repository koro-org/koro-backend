package parties

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// Register adds the parties operations (04 §2.13) to api: legal parties
// and their documents (BE-036), payees and their bank accounts, the admin
// party reads and the ownership correction (BE-037). svc may be nil (`koro
// openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	reg(api, huma.Operation{
		OperationID: "listOrgLegalParties", Method: http.MethodGet, Path: "/orgs/{org_id}/legal-parties",
		Summary: "List the legal parties of an organization",
		Description: "A company sees its buyer parties (invoice titles); a club or a university sees the legal side of " +
			"its payees (03 k21: the foundation paid on its behalf). No national ID (TCKN) is ever returned; tax number " +
			"and MERSİS number are masked for organization roles.",
	}, op.Meta{Action: string(authz.PartiesLegalPartyRead), Surface: op.SurfaceOrg}, h.list)

	reg(api, huma.Operation{
		OperationID: "getOrgLegalParty", Method: http.MethodGet, Path: "/orgs/{org_id}/legal-parties/{party_id}",
		Summary:     "Get a legal party of an organization",
		Description: "The ETag is the If-Match value of updateLegalParty. A party of another organization is 404.",
	}, op.Meta{Action: string(authz.PartiesLegalPartyRead), Surface: op.SurfaceOrg}, h.get)

	reg(api, huma.Operation{
		OperationID: "createBuyerLegalParty", Method: http.MethodPost, Path: "/orgs/{org_id}/legal-parties",
		Summary: "Create the invoice title of a company", DefaultStatus: http.StatusCreated,
		Description: "Creates a buyer_company party of the company (pending_verification until finance verifies it). " +
			"Only companies have buyer parties (422 parties.org_not_company); the tax number (VKN) is checked with its " +
			"check digit and is unique among active buyer parties (409 parties.tax_id_taken).",
	}, op.Meta{Action: string(authz.PartiesLegalPartyCreate), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.create)

	reg(api, huma.Operation{
		OperationID: "updateLegalParty", Method: http.MethodPatch, Path: "/orgs/{org_id}/legal-parties/{party_id}",
		Summary: "Update the invoice title of a company",
		Description: "If-Match with the ETag of getOrgLegalParty is required: missing → 428, stale → 412. A change to a " +
			"verified (or rejected) party sends it back to pending_verification; orders keep their own snapshot.",
	}, op.Meta{Action: string(authz.PartiesLegalPartyUpdate), Surface: op.SurfaceOrg}, h.update)

	reg(api, huma.Operation{
		OperationID: "uploadLegalPartyDocument", Method: http.MethodPost, Path: "/orgs/{org_id}/legal-parties/{party_id}/documents",
		Summary: "Add a document to a legal party", DefaultStatus: http.StatusCreated,
		Description: "file_id is a party_document upload (createUpload, then completeUpload) of the caller or of this " +
			"organization; any other file is 422. Finance verifies the document later.",
	}, op.Meta{Action: string(authz.PartiesDocumentUpload), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.upload)

	reg(api, huma.Operation{
		OperationID: "downloadLegalPartyDocument", Method: http.MethodGet,
		Path:    "/orgs/{org_id}/legal-parties/{party_id}/documents/{document_id}/download",
		Summary: "Download a legal party document",
		Description: "Returns a download URL valid for at most 5 minutes (Content-Disposition: attachment). Every download " +
			"is audited. A file that is not scanned clean is 409 files.not_clean.",
	}, op.Meta{Action: string(authz.PartiesDocumentRead), Surface: op.SurfaceOrg}, h.download)

	reg(api, huma.Operation{
		OperationID: "adminListLegalParties", Method: http.MethodGet, Path: "/admin/legal-parties",
		Summary:     "List legal parties (finance and platform admins)",
		Description: "Newest first. q matches part of the title, or a 10-digit tax number exactly. Tax numbers are not masked here.",
	}, op.Meta{Action: string(authz.PartiesLegalPartyRead), Surface: op.SurfaceAdmin}, h.adminList)

	reg(api, huma.Operation{
		OperationID: "adminCreateLegalParty", Method: http.MethodPost, Path: "/admin/legal-parties",
		Summary: "Create a payee or platform legal party", DefaultStatus: http.StatusCreated,
		Description: "kind payee: the legal side of a payee (foundation, university, association, institution). In the " +
			"MVP a payee is a legal entity: an individual or a sole proprietorship is 422 parties.natural_person_payee " +
			"(06 H-54). kind platform: Koro's own party (one row; 409 parties.platform_exists). The national ID (tckn, " +
			"individuals only) is stored envelope-encrypted with a blind index and never returned.",
	}, op.Meta{Action: string(authz.PartiesLegalPartyCreate), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminCreate)

	reg(api, huma.Operation{
		OperationID: "adminUpdateLegalParty", Method: http.MethodPatch, Path: "/admin/legal-parties/{party_id}",
		Summary: "Update a legal party",
		Description: "If-Match is required (428/412). A change to a verified (or rejected) party sends it back to " +
			"pending_verification. status inactive retires a party (kept for the financial records).",
	}, op.Meta{Action: string(authz.PartiesLegalPartyUpdate), Surface: op.SurfaceAdmin}, h.adminUpdate)

	reg(api, huma.Operation{
		OperationID: "adminVerifyLegalParty", Method: http.MethodPost, Path: "/admin/legal-parties/{party_id}/verification",
		Summary: "Verify or reject a legal party",
		Description: "verified needs the tax number and an address (422 parties.verification_incomplete); a party that " +
			"is already decided or inactive is 409 parties.invalid_state.",
	}, op.Meta{Action: string(authz.PartiesLegalPartyVerify), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminVerify)

	reg(api, huma.Operation{
		OperationID: "adminVerifyLegalPartyDocument", Method: http.MethodPost, Path: "/admin/legal-party-documents/{document_id}/verify",
		Summary: "Verify a legal party document",
		Description: "Never by the document's uploader (403 authz.four_eyes_violation). A verified document is 409 " +
			"parties.invalid_state; a file that is not scanned clean is 409 files.not_clean.",
	}, op.Meta{Action: string(authz.PartiesDocumentVerify), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.adminVerifyDocument)

	registerPayees(api, h)
}

// reg registers one parties operation with its Koro metadata.
func reg[I, O any](api huma.API, o huma.Operation, m op.Meta, fn func(context.Context, *I) (*O, error)) {
	o.Tags = []string{"parties"}
	huma.Register(api, op.Koro(o, m), fn)
}

type handler struct{ svc Service }

// parseID parses a path ID; a malformed one becomes the nil UUID, which no
// row has: it answers like a missing row (404) after the surface's authz.
func parseID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil()
	}
	return id
}

// ---- DTOs

// AddressDTO is legal_parties.address (02 §17.1).
type AddressDTO struct {
	Line1      string `json:"line1" maxLength:"200" doc:"Street address"`
	District   string `json:"district,omitempty" maxLength:"100"`
	City       string `json:"city" maxLength:"60"`
	PostalCode string `json:"postal_code,omitempty" maxLength:"5" doc:"5 digits"`
	Country    string `json:"country,omitempty" maxLength:"2" doc:"ISO 3166-1 alpha-2; TR when omitted"`
}

func (a *AddressDTO) model() *Address {
	if a == nil {
		return nil
	}
	return &Address{Line1: a.Line1, District: a.District, City: a.City, PostalCode: a.PostalCode, Country: a.Country}
}

func addressDTO(a Address) *AddressDTO {
	if a.Empty() {
		return nil
	}
	return &AddressDTO{Line1: a.Line1, District: a.District, City: a.City, PostalCode: a.PostalCode, Country: a.Country}
}

// DocumentDTO is one legal_party_documents row.
type DocumentDTO struct {
	DocumentID string     `json:"document_id" format:"uuid"`
	Kind       string     `json:"kind" enum:"tax_certificate,signature_circular,foundation_deed,university_protocol,authorization_letter,other"`
	UploadedAt time.Time  `json:"uploaded_at"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	UploadedBy string     `json:"uploaded_by,omitempty" format:"uuid" doc:"Finance and platform admins only"`
	VerifiedBy string     `json:"verified_by,omitempty" format:"uuid" doc:"Finance and platform admins only"`
}

func documentDTO(d Document, staff bool) DocumentDTO {
	out := DocumentDTO{DocumentID: d.ID.String(), Kind: string(d.Kind), UploadedAt: d.CreatedAt.UTC(), VerifiedAt: utc(d.VerifiedAt)}
	if staff {
		out.UploadedBy = d.UploadedBy.String()
		if d.VerifiedBy != nil {
			out.VerifiedBy = d.VerifiedBy.String()
		}
	}
	return out
}

// LegalPartyDTO is 04 §2.13 LegalPartyDTO (+ documents, org_id,
// timestamps). It never carries a national ID (TCKN): not even masked.
type LegalPartyDTO struct {
	PartyID   string `json:"party_id" format:"uuid"`
	Kind      string `json:"kind" enum:"platform,buyer_company,payee"`
	LegalType string `json:"legal_type" enum:"company,university,foundation,association,sole_prop,individual,public_body,other"`
	OrgID     string `json:"org_id,omitempty" format:"uuid" doc:"The company of a buyer party, the university of a university's own party"`
	Title     string `json:"title" doc:"Invoice title (unvan)"`
	TaxOffice string `json:"tax_office,omitempty"`
	TaxNo     string `json:"tax_no,omitempty" doc:"Tax number (VKN); masked to its last 4 digits for organization roles"`
	MersisNo  string `json:"mersis_no,omitempty" doc:"MERSİS number; masked to its last 4 digits for organization roles"`
	// TaxIDsMasked tells whether tax_no and mersis_no are masked.
	TaxIDsMasked  bool          `json:"tax_ids_masked"`
	TCKNOnFile    bool          `json:"tckn_on_file,omitempty" doc:"Individuals, finance and platform admins only: a national ID is stored (encrypted); its value is never returned"`
	Address       *AddressDTO   `json:"address,omitempty"`
	Email         string        `json:"email,omitempty"`
	Phone         string        `json:"phone,omitempty"`
	VATStatus     string        `json:"vat_status" enum:"vat_payer,vat_exempt,not_vat_payer,unknown"`
	EInvoiceAlias string        `json:"e_invoice_alias,omitempty" doc:"Finance and platform admins only"`
	Status        string        `json:"status" enum:"draft,pending_verification,verified,rejected,inactive"`
	VerifiedAt    *time.Time    `json:"verified_at,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	Documents     []DocumentDTO `json:"documents"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// toDTO maps a view: VKN and MERSİS masked unless v.FullTaxIDs, the TCKN
// never (only whether one is on file, for staff).
func toDTO(v PartyView) LegalPartyDTO {
	p := v.Party
	out := LegalPartyDTO{
		PartyID: p.ID.String(), Kind: string(p.Kind), LegalType: string(p.LegalType), Title: p.Title,
		TaxOffice: str(p.TaxOffice), TaxNo: str(p.TaxNo), MersisNo: str(p.MersisNo), TaxIDsMasked: !v.FullTaxIDs,
		Address: addressDTO(p.Address), Email: str(p.Email), Phone: str(p.Phone), VATStatus: string(p.VATStatus),
		Status: string(p.Status), VerifiedAt: utc(p.VerifiedAt), CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
		Documents: make([]DocumentDTO, len(v.Documents)),
	}
	if p.OrgID != nil {
		out.OrgID = p.OrgID.String()
	}
	if !v.FullTaxIDs {
		if out.TaxNo != "" {
			out.TaxNo = maskTail(out.TaxNo, maskedDigits)
		}
		if out.MersisNo != "" {
			out.MersisNo = maskTail(out.MersisNo, maskedDigits)
		}
	} else {
		out.TCKNOnFile = v.TCKNOnFile
		out.EInvoiceAlias = str(p.EInvoiceAlias)
	}
	for i, d := range v.Documents {
		out.Documents[i] = documentDTO(d, v.FullTaxIDs)
	}
	return out
}

// LegalPartyList is a list of LegalPartyDTO.
type LegalPartyList struct {
	Items      []LegalPartyDTO `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// DownloadDTO is a short-lived download URL (04 §1.11).
type DownloadDTO struct {
	URL       string    `json:"url" doc:"Valid for at most 5 minutes"`
	ExpiresAt time.Time `json:"expires_at"`
}

type partyOutput struct {
	ETag string `header:"ETag"`
	Body LegalPartyDTO
}

func partyOut(v PartyView) *partyOutput { return &partyOutput{ETag: v.ETag, Body: toDTO(v)} }

// ---- org surface

type orgPath struct {
	OrgID string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
}

type partyPath struct {
	OrgID   string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	PartyID string `path:"party_id" doc:"Legal party ID (an unknown, malformed or other organization's ID is 404)"`
}

type listOutput struct{ Body LegalPartyList }

func (h handler) list(ctx context.Context, in *orgPath) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	vs, err := h.svc.ListOrgParties(ctx, a, parseID(in.OrgID))
	if err != nil {
		return nil, err
	}
	out := &listOutput{Body: LegalPartyList{Items: make([]LegalPartyDTO, len(vs))}}
	for i, v := range vs {
		out.Body.Items[i] = toDTO(v)
	}
	return out, nil
}

func (h handler) get(ctx context.Context, in *partyPath) (*partyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetOrgParty(ctx, a, parseID(in.OrgID), parseID(in.PartyID))
	if err != nil {
		return nil, err
	}
	return partyOut(v), nil
}

type createPartyInput struct {
	idempotency.OptionalHeader
	OrgID string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	Body  struct {
		LegalType string      `json:"legal_type" enum:"company,sole_prop,association,foundation,public_body,other"`
		Title     string      `json:"title" minLength:"2" maxLength:"300" doc:"Invoice title (unvan)"`
		TaxOffice string      `json:"tax_office" maxLength:"100"`
		TaxNo     string      `json:"tax_no" maxLength:"20" doc:"Tax number (VKN): 10 digits with its check digit"`
		MersisNo  string      `json:"mersis_no,omitempty" maxLength:"30" doc:"MERSİS number: 16 digits"`
		Address   *AddressDTO `json:"address"`
		Email     string      `json:"email" maxLength:"254" doc:"Invoice contact address"`
		Phone     string      `json:"phone,omitempty" maxLength:"30" doc:"+<country><number>, or a Turkish number"`
	}
}

func (h handler) create(ctx context.Context, in *createPartyInput) (*partyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	v, err := h.svc.CreateBuyerParty(ctx, a, parseID(in.OrgID), PartyInput{
		LegalType: LegalType(b.LegalType), Title: b.Title, TaxOffice: b.TaxOffice, TaxNo: b.TaxNo, MersisNo: b.MersisNo,
		Address: b.Address.model(), Email: b.Email, Phone: b.Phone,
	})
	if err != nil {
		return nil, err
	}
	return partyOut(v), nil
}

type updatePartyInput struct {
	OrgID   string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	PartyID string `path:"party_id" doc:"Legal party ID (an unknown, malformed or other organization's ID is 404)"`
	IfMatch string `header:"If-Match" doc:"ETag of getOrgLegalParty (required: missing → 428, stale → 412)"`
	Body    struct {
		LegalType *string     `json:"legal_type,omitempty" enum:"company,sole_prop,association,foundation,public_body,other"`
		Title     *string     `json:"title,omitempty" minLength:"2" maxLength:"300"`
		TaxOffice *string     `json:"tax_office,omitempty" maxLength:"100"`
		TaxNo     *string     `json:"tax_no,omitempty" maxLength:"20" doc:"VKN"`
		MersisNo  *string     `json:"mersis_no,omitempty" maxLength:"30" doc:"Empty string clears it"`
		Address   *AddressDTO `json:"address,omitempty" doc:"Replaces the whole address"`
		Email     *string     `json:"email,omitempty" maxLength:"254"`
		Phone     *string     `json:"phone,omitempty" maxLength:"30" doc:"Empty string clears it"`
	}
}

func (h handler) update(ctx context.Context, in *updatePartyInput) (*partyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	v, err := h.svc.UpdateOrgParty(ctx, a, parseID(in.OrgID), parseID(in.PartyID), in.IfMatch, PartyPatch{
		LegalType: legalTypePtr(b.LegalType), Title: b.Title, TaxOffice: b.TaxOffice, TaxNo: b.TaxNo, MersisNo: b.MersisNo,
		Address: b.Address.model(), Email: b.Email, Phone: b.Phone,
	})
	if err != nil {
		return nil, err
	}
	return partyOut(v), nil
}

func legalTypePtr(s *string) *LegalType {
	if s == nil {
		return nil
	}
	t := LegalType(*s)
	return &t
}

type uploadDocumentInput struct {
	idempotency.OptionalHeader
	OrgID   string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	PartyID string `path:"party_id" doc:"Legal party ID (an unknown, malformed or other organization's ID is 404)"`
	Body    struct {
		Kind   string `json:"kind" enum:"tax_certificate,signature_circular,foundation_deed,university_protocol,authorization_letter,other"`
		FileID string `json:"file_id" format:"uuid" doc:"A party_document upload (createUpload, completeUpload)"`
	}
}

type documentOutput struct{ Body DocumentDTO }

func (h handler) upload(ctx context.Context, in *uploadDocumentInput) (*documentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fileID, err := uuid.Parse(in.Body.FileID)
	if err != nil {
		return nil, fieldErr("body.file_id", "must be a UUID")
	}
	d, err := h.svc.UploadDocument(ctx, a, parseID(in.OrgID), parseID(in.PartyID), DocumentInput{
		Kind: DocumentKind(in.Body.Kind), FileID: fileID,
	})
	if err != nil {
		return nil, err
	}
	return &documentOutput{Body: documentDTO(d, fullTaxIDs(a))}, nil
}

type downloadInput struct {
	OrgID      string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
	PartyID    string `path:"party_id" doc:"Legal party ID (an unknown, malformed or other organization's ID is 404)"`
	DocumentID string `path:"document_id" doc:"Document ID (an unknown, malformed or other party's ID is 404)"`
}

type downloadOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         DownloadDTO
}

func (h handler) download(ctx context.Context, in *downloadInput) (*downloadOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	l, err := h.svc.DownloadDocument(ctx, a, parseID(in.OrgID), parseID(in.PartyID), parseID(in.DocumentID))
	if err != nil {
		return nil, err
	}
	return &downloadOutput{CacheControl: "no-store", Body: DownloadDTO{URL: l.URL, ExpiresAt: l.ExpiresAt.UTC()}}, nil
}

// ---- admin surface

const sortNewest = "-id"

type adminListInput struct {
	page.Params
	Kind   string `query:"kind" enum:"platform,buyer_company,payee"`
	Status string `query:"status" enum:"draft,pending_verification,verified,rejected,inactive"`
	Q      string `query:"q" maxLength:"100" doc:"Part of the title (2-100 characters), or a 10-digit tax number"`
}

func (h handler) adminList(ctx context.Context, in *adminListInput) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListLegalParties", in.Kind, in.Status, in.Q)
	var after *uuid.UUID
	c, err := page.Decode(in.Cursor, sortNewest, fp, 1)
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
	p, err := h.svc.AdminListParties(ctx, a, AdminQuery{Kind: Kind(in.Kind), Status: Status(in.Status), Q: in.Q, Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &listOutput{Body: LegalPartyList{Items: make([]LegalPartyDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortNewest, fp, page.UUIDVal(*p.Next))
	}
	return out, nil
}

type adminCreatePartyInput struct {
	idempotency.OptionalHeader
	Body struct {
		Kind          string      `json:"kind" enum:"payee,platform" doc:"Buyer parties are created by their company (createBuyerLegalParty)"`
		LegalType     string      `json:"legal_type" enum:"company,university,foundation,association,sole_prop,individual,public_body,other"`
		OrgID         string      `json:"org_id,omitempty" format:"uuid" doc:"payee only: the university organization the party is"`
		Title         string      `json:"title" minLength:"2" maxLength:"300" doc:"Invoice title (unvan)"`
		TaxOffice     string      `json:"tax_office,omitempty" maxLength:"100"`
		TaxNo         string      `json:"tax_no,omitempty" maxLength:"20" doc:"Tax number (VKN): 10 digits with its check digit"`
		TCKN          string      `json:"tckn,omitempty" maxLength:"20" doc:"individual only: national ID (11 digits); stored encrypted, never returned"`
		MersisNo      string      `json:"mersis_no,omitempty" maxLength:"30" doc:"MERSİS number: 16 digits"`
		Address       *AddressDTO `json:"address,omitempty"`
		Email         string      `json:"email,omitempty" maxLength:"254" doc:"Invoice / payout contact address"`
		Phone         string      `json:"phone,omitempty" maxLength:"30"`
		VATStatus     string      `json:"vat_status,omitempty" enum:"vat_payer,vat_exempt,not_vat_payer,unknown" doc:"unknown when omitted"`
		EInvoiceAlias string      `json:"e_invoice_alias,omitempty" maxLength:"200"`
	}
}

func (h handler) adminCreate(ctx context.Context, in *adminCreatePartyInput) (*partyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	pi := PartyInput{
		Kind: Kind(b.Kind), LegalType: LegalType(b.LegalType), Title: b.Title, TaxOffice: b.TaxOffice, TaxNo: b.TaxNo,
		TCKN: b.TCKN, MersisNo: b.MersisNo, Address: b.Address.model(), Email: b.Email, Phone: b.Phone,
		VATStatus: VATStatus(b.VATStatus), EInvoiceAlias: b.EInvoiceAlias,
	}
	if b.OrgID != "" {
		org, err := uuid.Parse(b.OrgID)
		if err != nil {
			return nil, fieldErr("body.org_id", "must be a UUID")
		}
		pi.OrgID = &org
	}
	v, err := h.svc.AdminCreateParty(ctx, a, pi)
	if err != nil {
		return nil, err
	}
	return partyOut(v), nil
}

// AdminPartyPath is the party_id path parameter of the admin operations
// (exported: huma reads the fields of an embedded struct only when its
// type is exported).
type AdminPartyPath struct {
	PartyID string `path:"party_id" doc:"Legal party ID (an unknown or malformed ID is 404)"`
}

type adminUpdatePartyInput struct {
	AdminPartyPath
	IfMatch string `header:"If-Match" doc:"ETag of the party (required: missing → 428, stale → 412)"`
	Body    struct {
		LegalType     *string     `json:"legal_type,omitempty" enum:"company,university,foundation,association,sole_prop,individual,public_body,other"`
		Title         *string     `json:"title,omitempty" minLength:"2" maxLength:"300"`
		TaxOffice     *string     `json:"tax_office,omitempty" maxLength:"100" doc:"Empty string clears it"`
		TaxNo         *string     `json:"tax_no,omitempty" maxLength:"20" doc:"VKN; empty string clears it"`
		TCKN          *string     `json:"tckn,omitempty" maxLength:"20" doc:"individual only: replaces the national ID (never returned)"`
		MersisNo      *string     `json:"mersis_no,omitempty" maxLength:"30" doc:"Empty string clears it"`
		Address       *AddressDTO `json:"address,omitempty" doc:"Replaces the whole address"`
		Email         *string     `json:"email,omitempty" maxLength:"254" doc:"Empty string clears it"`
		Phone         *string     `json:"phone,omitempty" maxLength:"30" doc:"Empty string clears it"`
		VATStatus     *string     `json:"vat_status,omitempty" enum:"vat_payer,vat_exempt,not_vat_payer,unknown"`
		EInvoiceAlias *string     `json:"e_invoice_alias,omitempty" maxLength:"200" doc:"Empty string clears it"`
		Status        string      `json:"status,omitempty" enum:"inactive" doc:"inactive retires the party (not the platform party)"`
	}
}

func (h handler) adminUpdate(ctx context.Context, in *adminUpdatePartyInput) (*partyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := PartyPatch{
		LegalType: legalTypePtr(b.LegalType), Title: b.Title, TaxOffice: b.TaxOffice, TaxNo: b.TaxNo, TCKN: b.TCKN,
		MersisNo: b.MersisNo, Address: b.Address.model(), Email: b.Email, Phone: b.Phone, EInvoiceAlias: b.EInvoiceAlias,
		Deactivate: b.Status == string(StatusInactive),
	}
	if b.VATStatus != nil {
		v := VATStatus(*b.VATStatus)
		p.VATStatus = &v
	}
	v, err := h.svc.AdminUpdateParty(ctx, a, parseID(in.PartyID), in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return partyOut(v), nil
}

type adminVerifyPartyInput struct {
	idempotency.OptionalHeader
	AdminPartyPath
	Body struct {
		Decision string `json:"decision" enum:"verified,rejected"`
		Note     string `json:"note,omitempty" maxLength:"500" doc:"Internal note (audited; no personal data)"`
	}
}

func (h handler) adminVerify(ctx context.Context, in *adminVerifyPartyInput) (*partyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminVerifyParty(ctx, a, parseID(in.PartyID), Decision{Decision: Status(in.Body.Decision), Note: in.Body.Note})
	if err != nil {
		return nil, err
	}
	return partyOut(v), nil
}

type adminVerifyDocumentInput struct {
	idempotency.OptionalHeader
	DocumentID string `path:"document_id" doc:"Document ID (an unknown or malformed ID is 404)"`
}

func (h handler) adminVerifyDocument(ctx context.Context, in *adminVerifyDocumentInput) (*documentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.AdminVerifyDocument(ctx, a, parseID(in.DocumentID))
	if err != nil {
		return nil, err
	}
	return &documentOutput{Body: documentDTO(d, true)}, nil
}
