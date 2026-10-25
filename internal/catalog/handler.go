package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the catalog operations (04 §2.14) to api. svc may be nil
// (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	op.Register(api, "catalog", huma.Operation{
		OperationID: "listServices", Method: http.MethodGet, Path: "/services",
		Summary: "List the Koro services",
		Description: "The services an organization may offer: active and reviewed by the accountant. Finance and " +
			"platform admins see every service with its VAT profile.",
	}, op.Meta{Action: string(authz.CatalogServiceList), Surface: op.SurfaceResource}, h.listServices)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "adminCreateService", Method: http.MethodPost, Path: "/admin/services",
		Summary: "Create a service", DefaultStatus: http.StatusCreated,
		Description: "The invoice wording and VAT profile of what may be sold. Offerings of a service go on sale only " +
			"once the accountant has reviewed it (accounting_reviewed, finance admins only: 03 k23). A duplicate code is " +
			"409 catalog.service_code_taken.",
	}, op.Meta{Action: string(authz.CatalogServiceManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.adminCreateService)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "adminUpdateService", Method: http.MethodPatch, Path: "/admin/services/{service_id}",
		Summary: "Update a service",
		Description: "Partial update; clear empties optional fields; active false withdraws the service. The " +
			"accountant's review (accounting_reviewed, accounting_reviewed_ref) is finance admins' only (03 k23); a " +
			"change to the VAT profile withdraws the review unless the same request reviews it again.",
	}, op.Meta{Action: string(authz.CatalogServiceManage), Surface: op.SurfaceAdmin}, h.adminUpdateService)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "browseOfferings", Method: http.MethodGet, Path: "/offerings",
		Summary: "Browse the marketplace",
		Description: "Active offerings whose sales have not ended. Prices are kuruş excluding VAT; the payee and the " +
			"commission are never shown.",
	}, op.Meta{Action: string(authz.CatalogOfferingBrowse), Surface: op.SurfaceResource, RateLimit: op.RateSearch}, h.browse)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "getOffering", Method: http.MethodGet, Path: "/offerings/{offering_id}",
		Summary: "Get an offering",
		Description: "A published offering for every signed-in user; a draft or an offering under review only for its " +
			"seller and the platform (404 otherwise). The seller's managers and platform staff also get the approval " +
			"block and the ETag that updateOffering takes as If-Match. buyer_fee_amount estimates the fee of one unit " +
			"when the commission is charged to the buyer.",
	}, op.Meta{Action: string(authz.CatalogOfferingRead), Surface: op.SurfaceResource}, h.get)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "listOrgOfferings", Method: http.MethodGet, Path: "/orgs/{org_id}/offerings",
		Summary:     "List the offerings of an organization",
		Description: "Drafts included, newest first, with the approval block.",
	}, op.Meta{Action: string(authz.CatalogOfferingListOrg), Surface: op.SurfaceOrg}, h.listOrg)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "createOffering", Method: http.MethodPost, Path: "/orgs/{org_id}/offerings",
		Summary: "Create an offering (package)", DefaultStatus: http.StatusCreated,
		Description: "A draft of a club or a university (for its payee: the default payee when omitted) or of the " +
			"platform org. The VAT rate is copied from the service; the event and the payee must be the organization's " +
			"(422 catalog.parent_mismatch). A platform-native service is the platform's only.",
	}, op.Meta{Action: string(authz.CatalogOfferingCreate), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.create)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "updateOffering", Method: http.MethodPatch, Path: "/offerings/{offering_id}",
		Summary: "Update a draft offering",
		Description: "Drafts only (409 catalog.invalid_state). If-Match with the ETag of getOffering is required: " +
			"missing → 428, stale → 412. clear empties optional fields.",
	}, op.Meta{Action: string(authz.CatalogOfferingUpdate), Surface: op.SurfaceResource}, h.update)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "submitOffering", Method: http.MethodPost, Path: "/offerings/{offering_id}/submit",
		Summary: "Submit an offering for approval",
		Description: "draft → pending_approval. The service must be reviewed (409 catalog.service_not_sellable), an " +
			"organization's payee active (409 catalog.payee_not_active) with an approved bank account (409 " +
			"parties.no_approved_account); what is missing is 422 catalog.offering_incomplete.",
	}, op.Meta{Action: string(authz.CatalogOfferingSubmit), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.submit)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "cancelOffering", Method: http.MethodPost, Path: "/offerings/{offering_id}/cancel",
		Summary: "Cancel an offering",
		Description: "Any offering that is not cancelled. Unpaid orders of it are cancelled and paid ones queued for " +
			"refund (offering.cancelled).",
	}, op.Meta{Action: string(authz.CatalogOfferingCancel), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.cancel)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "adminListOfferingReviewQueue", Method: http.MethodGet, Path: "/admin/offerings",
		Summary:     "List the offerings waiting for the platform decision",
		Description: "Oldest submission first.",
	}, op.Meta{Action: string(authz.CatalogOfferingReviewQueue), Surface: op.SurfaceAdmin}, h.adminQueue)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "adminDecideOffering", Method: http.MethodPost, Path: "/admin/offerings/{offering_id}/decision",
		Summary: "Approve or reject an offering",
		Description: "Its creator never decides it (403 authz.four_eyes_violation). An approval puts it on sale — " +
			"after the payee, its bank account and a commerce policy are checked again — unless its university still " +
			"has to approve it (it waits). A rejection sends it back to draft with the reason.",
	}, op.Meta{Action: string(authz.CatalogOfferingDecide), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.adminDecide)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "listUniversityOfferingApprovals", Method: http.MethodGet, Path: "/orgs/{org_id}/offering-approvals",
		Summary:     "List the offerings waiting for a university's approval",
		Description: "org_id is a university; the offerings of its clubs that require its approval, oldest first.",
	}, op.Meta{Action: string(authz.CatalogOfferingReviewQueue), Surface: op.SurfaceOrg}, h.uniQueue)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "decideUniversityOfferingApproval", Method: http.MethodPost,
		Path:    "/orgs/{org_id}/offering-approvals/{offering_id}/decision",
		Summary: "Approve or reject an offering as its university",
		Description: "An approval puts the offering on sale when the platform has approved it too; a rejection sends " +
			"it back to draft.",
	}, op.Meta{Action: string(authz.CatalogOfferingUniversityDecide), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional},
		h.uniDecide)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "adminListCommercePolicies", Method: http.MethodGet, Path: "/admin/commerce-policies",
		Summary:     "List commerce policies",
		Description: "Newest valid_from first; at keeps the policies in effect at that time.",
	}, op.Meta{Action: string(authz.CatalogPolicyList), Surface: op.SurfaceAdmin}, h.listPolicies)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "adminCreateCommercePolicy", Method: http.MethodPost, Path: "/admin/commerce-policies",
		Summary: "Create a commerce policy", DefaultStatus: http.StatusCreated,
		Description: "Requires re-authentication. A policy never changes: a change is \"close + new row\". Periods of " +
			"one scope never overlap (409 catalog.policy_overlap); none starts in the past. commission_rate_bp has no " +
			"default. Every platform and finance admin is notified; orders copy the policy in effect onto their lines.",
	}, op.Meta{Action: string(authz.CatalogPolicyManage), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.createPolicy)

	op.Register(api, "catalog", huma.Operation{
		OperationID: "adminCloseCommercePolicy", Method: http.MethodPost, Path: "/admin/commerce-policies/{policy_id}/close",
		Summary: "Close a commerce policy",
		Description: "Requires re-authentication. Sets valid_to (now when omitted, never in the past); the row " +
			"changes no other way; a closed policy is 409 catalog.invalid_state. Every platform and finance admin is notified.",
	}, op.Meta{Action: string(authz.CatalogPolicyManage), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.closePolicy)
}

type handler struct{ svc Service }

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---- services

// CatalogServiceDTO is 04 §2.14 CatalogServiceDTO; the VAT profile and the review are set
// for finance and platform admins only.
type CatalogServiceDTO struct {
	ServiceID             string         `json:"service_id" format:"uuid"`
	Code                  string         `json:"code"`
	Name                  string         `json:"name"`
	Description           string         `json:"description,omitempty"`
	Category              string         `json:"category"`
	FulfillmentType       string         `json:"fulfillment_type" enum:"event_bound,org_bound,platform_native"`
	DefaultVATRateBP      int            `json:"default_vat_rate_bp" doc:"Basis points: 2000 = 20 %"`
	VATClass              string         `json:"vat_class,omitempty" enum:"standard,exempt,outside_scope,withheld"`
	VATExemptionCode      string         `json:"vat_exemption_code,omitempty"`
	VATWithholdingCode    string         `json:"vat_withholding_code,omitempty"`
	VATWithholdingNum     *int16         `json:"vat_withholding_num,omitempty"`
	VATWithholdingDen     *int16         `json:"vat_withholding_den,omitempty"`
	InvoiceDescription    string         `json:"invoice_description,omitempty"`
	EntitlementTemplate   map[string]any `json:"entitlement_template,omitempty"`
	AccountingReviewedAt  *time.Time     `json:"accounting_reviewed_at,omitempty"`
	AccountingReviewedRef string         `json:"accounting_reviewed_ref,omitempty"`
	Active                *bool          `json:"active,omitempty"`
}

func serviceDTO(v ServiceItem, admin bool) CatalogServiceDTO {
	out := CatalogServiceDTO{
		ServiceID: v.ID.String(), Code: v.Code, Name: v.NameTR, Description: str(v.DescriptionTR),
		Category: string(v.Category), FulfillmentType: string(v.FulfillmentType), DefaultVATRateBP: v.DefaultVATRateBP,
	}
	if admin {
		out.VATClass, out.VATExemptionCode, out.VATWithholdingCode = string(v.VATClass), str(v.VATExemptionCode), str(v.VATWithholdingCode)
		out.VATWithholdingNum, out.VATWithholdingDen = v.VATWithholdingNum, v.VATWithholdingDen
		out.InvoiceDescription, out.AccountingReviewedRef = str(v.InvoiceDescriptionTR), str(v.AccountingReviewedRef)
		out.AccountingReviewedAt, out.Active = db.UTCPtr(v.AccountingReviewedAt), &v.Active
		if v.EntitlementTemplate != nil {
			_ = json.Unmarshal(v.EntitlementTemplate, &out.EntitlementTemplate)
		}
	}
	return out
}

type listServicesInput struct {
	Category string `query:"category" enum:"sponsorship,booth,logo_placement,talk_slot,social_media_post,workshop,targeted_post_quota,contact_request_quota,discovery_access,other"`
}

type catalogServiceListOutput struct {
	Body struct {
		Items []CatalogServiceDTO `json:"items"`
	}
}

func (h handler) listServices(ctx context.Context, in *listServicesInput) (*catalogServiceListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	vs, err := h.svc.ListServices(ctx, a, Category(in.Category))
	if err != nil {
		return nil, err
	}
	out := &catalogServiceListOutput{}
	out.Body.Items = make([]CatalogServiceDTO, len(vs))
	for i, v := range vs {
		out.Body.Items[i] = serviceDTO(v, staff(a))
	}
	return out, nil
}

// CatalogServiceBody is the body of adminCreateService (02 §18.1).
type CatalogServiceBody struct {
	Code                  string         `json:"code" minLength:"3" maxLength:"41" doc:"[a-z][a-z0-9_]{2,40}"`
	Name                  string         `json:"name" minLength:"2" maxLength:"160"`
	Description           string         `json:"description,omitempty" maxLength:"2000"`
	Category              string         `json:"category" enum:"sponsorship,booth,logo_placement,talk_slot,social_media_post,workshop,targeted_post_quota,contact_request_quota,discovery_access,other"`
	FulfillmentType       string         `json:"fulfillment_type" enum:"event_bound,org_bound,platform_native"`
	EntitlementTemplate   map[string]any `json:"entitlement_template,omitempty" doc:"platform_native only: {feature, kind, quantity_per_unit, valid_days?}"`
	DefaultVATRateBP      *int           `json:"default_vat_rate_bp,omitempty" minimum:"0" maximum:"10000" doc:"2000 when omitted"`
	VATClass              string         `json:"vat_class,omitempty" enum:"standard,exempt,outside_scope,withheld" doc:"standard when omitted"`
	VATExemptionCode      string         `json:"vat_exemption_code,omitempty" maxLength:"40"`
	VATWithholdingCode    string         `json:"vat_withholding_code,omitempty" maxLength:"40"`
	VATWithholdingNum     *int           `json:"vat_withholding_num,omitempty" minimum:"1" maximum:"1000"`
	VATWithholdingDen     *int           `json:"vat_withholding_den,omitempty" minimum:"1" maximum:"1000"`
	InvoiceDescription    string         `json:"invoice_description,omitempty" maxLength:"300"`
	AccountingReviewed    bool           `json:"accounting_reviewed,omitempty" doc:"The accountant reviewed the VAT profile (finance admins only)"`
	AccountingReviewedRef string         `json:"accounting_reviewed_ref,omitempty" maxLength:"40" doc:"The accountant's reference (finance admins only)"`
	Active                *bool          `json:"active,omitempty" doc:"true when omitted"`
}

type adminCreateServiceInput struct {
	idempotency.OptionalHeader
	Body CatalogServiceBody
}

type catalogServiceOutput struct{ Body CatalogServiceDTO }

func (h handler) adminCreateService(ctx context.Context, in *adminCreateServiceInput) (*catalogServiceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	v, err := h.svc.AdminCreateService(ctx, a, ServiceInput{
		Code: b.Code, NameTR: b.Name, DescriptionTR: b.Description, Category: Category(b.Category),
		FulfillmentType: FulfillmentType(b.FulfillmentType), EntitlementTemplate: b.EntitlementTemplate,
		DefaultVATRateBP: b.DefaultVATRateBP, VATClass: VATClass(b.VATClass), VATExemptionCode: b.VATExemptionCode,
		VATWithholdingCode: b.VATWithholdingCode, VATWithholdingNum: b.VATWithholdingNum, VATWithholdingDen: b.VATWithholdingDen,
		InvoiceDescriptionTR: b.InvoiceDescription, AccountingReviewed: b.AccountingReviewed,
		AccountingReviewedRef: b.AccountingReviewedRef, Active: b.Active,
	})
	if err != nil {
		return nil, err
	}
	return &catalogServiceOutput{Body: serviceDTO(v, true)}, nil
}

// ServicePath is the service_id path parameter.
type ServicePath struct {
	ServiceID string `path:"service_id" doc:"Service ID (an unknown or malformed ID is 404)"`
}

type adminUpdateServiceInput struct {
	ServicePath
	Body struct {
		Name                  *string        `json:"name,omitempty" minLength:"2" maxLength:"160"`
		Description           *string        `json:"description,omitempty" maxLength:"2000"`
		Category              *string        `json:"category,omitempty" enum:"sponsorship,booth,logo_placement,talk_slot,social_media_post,workshop,targeted_post_quota,contact_request_quota,discovery_access,other"`
		EntitlementTemplate   map[string]any `json:"entitlement_template,omitempty"`
		DefaultVATRateBP      *int           `json:"default_vat_rate_bp,omitempty" minimum:"0" maximum:"10000"`
		VATClass              *string        `json:"vat_class,omitempty" enum:"standard,exempt,outside_scope,withheld"`
		VATExemptionCode      *string        `json:"vat_exemption_code,omitempty" maxLength:"40"`
		VATWithholdingCode    *string        `json:"vat_withholding_code,omitempty" maxLength:"40"`
		VATWithholdingNum     *int           `json:"vat_withholding_num,omitempty" minimum:"1" maximum:"1000"`
		VATWithholdingDen     *int           `json:"vat_withholding_den,omitempty" minimum:"1" maximum:"1000"`
		InvoiceDescription    *string        `json:"invoice_description,omitempty" maxLength:"300"`
		AccountingReviewed    *bool          `json:"accounting_reviewed,omitempty" doc:"true: reviewed now; false: review withdrawn (finance admins only)"`
		AccountingReviewedRef *string        `json:"accounting_reviewed_ref,omitempty" maxLength:"40" doc:"Finance admins only"`
		Active                *bool          `json:"active,omitempty"`
		Clear                 []string       `json:"clear,omitempty" maxItems:"5" enum:"description_tr,vat_exemption_code,vat_withholding_code,invoice_description_tr,accounting_reviewed_ref" doc:"Optional fields to empty"`
	}
}

func (h handler) adminUpdateService(ctx context.Context, in *adminUpdateServiceInput) (*catalogServiceOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := ServicePatch{
		NameTR: b.Name, DescriptionTR: b.Description, EntitlementTemplate: b.EntitlementTemplate, DefaultVATRateBP: b.DefaultVATRateBP,
		VATExemptionCode: b.VATExemptionCode, VATWithholdingCode: b.VATWithholdingCode, VATWithholdingNum: b.VATWithholdingNum,
		VATWithholdingDen: b.VATWithholdingDen, InvoiceDescriptionTR: b.InvoiceDescription, AccountingReviewed: b.AccountingReviewed,
		AccountingReviewedRef: b.AccountingReviewedRef, Active: b.Active, Clear: b.Clear,
	}
	if b.Category != nil {
		p.Category = ptr(Category(*b.Category))
	}
	if b.VATClass != nil {
		p.VATClass = ptr(VATClass(*b.VATClass))
	}
	v, err := h.svc.AdminUpdateService(ctx, a, httpx.ParseID(in.ServiceID), p)
	if err != nil {
		return nil, err
	}
	return &catalogServiceOutput{Body: serviceDTO(v, true)}, nil
}

// ---- offering DTOs

// OfferingServiceDTO is the service of an offering.
type OfferingServiceDTO struct {
	ServiceID       string `json:"service_id" format:"uuid"`
	Code            string `json:"code"`
	Name            string `json:"name"`
	Category        string `json:"category"`
	FulfillmentType string `json:"fulfillment_type" enum:"event_bound,org_bound,platform_native"`
}

// OfferingSellerDTO is the seller org of an offering.
type OfferingSellerDTO struct {
	OrgID string `json:"org_id" format:"uuid"`
	Name  string `json:"name"`
	Kind  string `json:"kind" enum:"club,university,platform"`
}

// OfferingEventDTO is the event an offering is bound to.
type OfferingEventDTO struct {
	EventID  string    `json:"event_id" format:"uuid"`
	Title    string    `json:"title"`
	StartsAt time.Time `json:"starts_at"`
}

// OfferingDeliverableDTO is one deliverable of an offering.
type OfferingDeliverableDTO struct {
	Key   string `json:"key" pattern:"^[a-z][a-z0-9_]{1,40}$"`
	Label string `json:"label" minLength:"1" maxLength:"120"`
}

// OfferingCardDTO is one marketplace item (browseOfferings).
type OfferingCardDTO struct {
	OfferingID        string             `json:"offering_id" format:"uuid"`
	Title             string             `json:"title"`
	Service           OfferingServiceDTO `json:"service"`
	Seller            OfferingSellerDTO  `json:"seller"`
	Event             *OfferingEventDTO  `json:"event,omitempty"`
	UnitPriceAmount   int64              `json:"unit_price_amount" doc:"Kuruş, excluding VAT"`
	VATRateBP         int                `json:"vat_rate_bp"`
	Currency          string             `json:"currency"`
	QuantityRemaining *int               `json:"quantity_remaining" doc:"null: unlimited"`
	SalesStartAt      *time.Time         `json:"sales_start_at,omitempty"`
	SalesEndAt        *time.Time         `json:"sales_end_at,omitempty"`
	Status            string             `json:"status" enum:"draft,pending_approval,active,sold_out,closed,cancelled"`
}

// OfferingApprovalDTO is the management block of an offering (its seller's
// managers and platform staff only: the buyer never sees the payee).
type OfferingApprovalDTO struct {
	DefaultPayeeID             string     `json:"default_payee_id,omitempty" format:"uuid"`
	QuantityTotal              *int       `json:"quantity_total" doc:"null: unlimited"`
	QuantitySold               int        `json:"quantity_sold"`
	InvoiceTrigger             string     `json:"invoice_trigger" enum:"on_payment,on_fulfillment"`
	BlockedSponsorFlags        []string   `json:"blocked_sponsor_flags"`
	RequiresUniversityApproval bool       `json:"requires_university_approval"`
	UniversityApprovalStatus   string     `json:"university_approval_status" enum:"not_required,pending,approved,rejected"`
	UniversityDecidedAt        *time.Time `json:"university_decided_at,omitempty"`
	SubmittedAt                *time.Time `json:"submitted_at,omitempty"`
	ApprovedAt                 *time.Time `json:"approved_at,omitempty"`
	RejectionReason            string     `json:"rejection_reason,omitempty"`
	CancelledAt                *time.Time `json:"cancelled_at,omitempty"`
	CancelReason               string     `json:"cancel_reason,omitempty"`
	CreatedAt                  time.Time  `json:"created_at"`
	UpdatedAt                  time.Time  `json:"updated_at"`
}

// OfferingDTO is 04 §2.14 OfferingDTO (+ approval for the seller and staff).
type OfferingDTO struct {
	OfferingCardDTO
	DescriptionJSON map[string]any           `json:"description_json,omitempty" doc:"Tiptap JSON"`
	Deliverables    []OfferingDeliverableDTO `json:"deliverables"`
	PerBuyerLimit   *int                     `json:"per_buyer_limit,omitempty"`
	FulfilBy        *time.Time               `json:"fulfil_by,omitempty"`
	BuyerFeeAmount  *int64                   `json:"buyer_fee_amount,omitempty" doc:"Estimated fee of one unit (kuruş, VAT included) when the buyer pays the commission"`
	Approval        *OfferingApprovalDTO     `json:"approval,omitempty"`
}

func cardDTO(v OfferingView) OfferingCardDTO {
	o := v.Offering
	out := OfferingCardDTO{
		OfferingID: o.ID.String(), Title: o.Title,
		Service: OfferingServiceDTO{ServiceID: v.Service.ID.String(), Code: v.Service.Code, Name: v.Service.NameTR,
			Category: string(v.Service.Category), FulfillmentType: string(v.Service.FulfillmentType)},
		Seller:          OfferingSellerDTO{OrgID: o.OrgID.String(), Name: v.Seller.Name, Kind: string(v.Seller.Kind)},
		UnitPriceAmount: o.UnitPriceAmount, VATRateBP: o.VATRateBP, Currency: o.Currency, QuantityRemaining: o.Remaining(),
		SalesStartAt: db.UTCPtr(o.SalesStartAt), SalesEndAt: db.UTCPtr(o.SalesEndAt), Status: string(o.Status),
	}
	if e := v.Event; e != nil {
		out.Event = &OfferingEventDTO{EventID: e.ID.String(), Title: e.Title, StartsAt: e.StartsAt.UTC()}
	}
	return out
}

func offeringDTO(v OfferingView) OfferingDTO {
	o := v.Offering
	out := OfferingDTO{OfferingCardDTO: cardDTO(v), PerBuyerLimit: o.PerBuyerLimit, FulfilBy: db.UTCPtr(o.FulfilBy),
		BuyerFeeAmount: v.BuyerFee, Deliverables: make([]OfferingDeliverableDTO, len(o.Deliverables))}
	for i, d := range o.Deliverables {
		out.Deliverables[i] = OfferingDeliverableDTO(d)
	}
	if o.DescriptionJSON != nil {
		_ = json.Unmarshal(o.DescriptionJSON, &out.DescriptionJSON)
	}
	if v.Management {
		flags := []string(o.BlockedSponsorFlags)
		if flags == nil {
			flags = []string{}
		}
		out.Approval = &OfferingApprovalDTO{
			DefaultPayeeID: httpx.IDStr(o.DefaultPayeeID), QuantityTotal: o.QuantityTotal, QuantitySold: o.QuantitySold,
			InvoiceTrigger: string(o.InvoiceTrigger), BlockedSponsorFlags: flags,
			RequiresUniversityApproval: o.RequiresUniversityApproval, UniversityApprovalStatus: string(o.UniversityApprovalStatus),
			UniversityDecidedAt: db.UTCPtr(o.UniversityApprovedAt), SubmittedAt: db.UTCPtr(o.SubmittedAt), ApprovedAt: db.UTCPtr(o.ApprovedAt),
			RejectionReason: str(o.RejectionReason), CancelledAt: db.UTCPtr(o.CancelledAt), CancelReason: str(o.CancelReason),
			CreatedAt: o.CreatedAt.UTC(), UpdatedAt: o.UpdatedAt.UTC(),
		}
	}
	return out
}

// OfferingPath is the offering_id path parameter.
type OfferingPath struct {
	OfferingID string `path:"offering_id" doc:"Offering ID (an unknown, malformed or invisible ID is 404)"`
}

// OrgPath is the org_id path parameter (exported: huma reads the fields of
// an embedded struct only when its type is exported).
type OrgPath struct {
	OrgID string `path:"org_id" doc:"Organization ID (an unknown or malformed ID is 404)"`
}

type offeringOutput struct {
	ETag string `header:"ETag"`
	Body OfferingDTO
}

func offeringOut(v OfferingView) *offeringOutput {
	out := &offeringOutput{Body: offeringDTO(v)}
	if v.Management {
		out.ETag = v.ETag
	}
	return out
}

type offeringListOutput struct {
	Body struct {
		Items      []OfferingDTO `json:"items"`
		NextCursor string        `json:"next_cursor,omitempty"`
	}
}

// ---- browse

type browseInput struct {
	page.Params
	Q              string `query:"q" maxLength:"100" doc:"Words of the title"`
	OrgID          string `query:"org_id" doc:"One seller"`
	EventID        string `query:"event_id"`
	UniversityID   string `query:"university_id" doc:"The sellers of a university (its org and clubs)"`
	Category       string `query:"service_category" enum:"sponsorship,booth,logo_placement,talk_slot,social_media_post,workshop,targeted_post_quota,contact_request_quota,discovery_access,other"`
	PriceMaxAmount int64  `query:"price_max_amount" minimum:"0" doc:"Kuruş, excluding VAT (0: no limit)"`
	Sort           string `query:"sort" enum:"-created_at,unit_price_amount,sales_end_at" default:"-created_at"`
}

type browseOfferingsOutput struct {
	Body struct {
		Items      []OfferingCardDTO `json:"items"`
		NextCursor string            `json:"next_cursor,omitempty"`
	}
}

func (h handler) browse(ctx context.Context, in *browseInput) (*browseOfferingsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q := BrowseQuery{Q: in.Q, Category: Category(in.Category), Sort: in.Sort, Limit: in.Limit}
	if q.Sort == "" {
		q.Sort = SortNewest
	}
	if q.OrgID, err = httpx.OptID("query.org_id", in.OrgID); err != nil {
		return nil, err
	}
	if q.EventID, err = httpx.OptID("query.event_id", in.EventID); err != nil {
		return nil, err
	}
	if q.UniversityID, err = httpx.OptID("query.university_id", in.UniversityID); err != nil {
		return nil, err
	}
	if in.PriceMaxAmount > 0 {
		q.PriceMax = &in.PriceMaxAmount
	}
	fp := page.Fingerprint("browseOfferings", in.Q, in.OrgID, in.EventID, in.UniversityID, in.Category,
		strconv.FormatInt(in.PriceMaxAmount, 10))
	c, err := page.Decode(in.Cursor, q.Sort, fp, 2)
	if err != nil {
		return nil, err
	}
	if c != nil {
		id, err := c.UUID(1)
		if err != nil {
			return nil, err
		}
		k := BrowseKey{ID: id}
		if q.Sort == SortPrice {
			if k.Price, err = strconv.ParseInt(c.Vals[0], 10, 64); err != nil {
				return nil, page.ErrInvalidCursor
			}
		} else if k.At, err = c.Time(0); err != nil {
			return nil, err
		}
		q.After = &k
	}
	p, err := h.svc.BrowseOfferings(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &browseOfferingsOutput{}
	out.Body.Items = make([]OfferingCardDTO, len(p.Items))
	for i, v := range p.Items {
		out.Body.Items[i] = cardDTO(v)
	}
	if k := p.NextBrowse; k != nil {
		first := page.TimeVal(k.At)
		if q.Sort == SortPrice {
			first = strconv.FormatInt(k.Price, 10)
		}
		out.Body.NextCursor = page.Next(q.Sort, fp, first, page.UUIDVal(k.ID))
	}
	return out, nil
}

func (h handler) get(ctx context.Context, in *OfferingPath) (*offeringOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetOffering(ctx, a, httpx.ParseID(in.OfferingID))
	if err != nil {
		return nil, err
	}
	return offeringOut(v), nil
}

// timeCursor decodes a (time, id) cursor of sort key sort.
func timeCursor(cursor, sort, fp string) (*TimeKey, error) {
	c, err := page.Decode(cursor, sort, fp, 2)
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
	return &TimeKey{At: at, ID: id}, nil
}

func listOut(p OfferingPage, sort, fp string) *offeringListOutput {
	out := &offeringListOutput{}
	out.Body.Items = make([]OfferingDTO, len(p.Items))
	for i, v := range p.Items {
		out.Body.Items[i] = offeringDTO(v)
	}
	if k := p.NextTime; k != nil {
		out.Body.NextCursor = page.Next(sort, fp, page.TimeVal(k.At), page.UUIDVal(k.ID))
	}
	return out
}

const (
	sortCreated   = "-created_at"
	sortSubmitted = "submitted_at"
)

type listOrgOfferingsInput struct {
	OrgPath
	page.Params
	Status string `query:"status" enum:"draft,pending_approval,active,sold_out,closed,cancelled"`
}

func (h handler) listOrg(ctx context.Context, in *listOrgOfferingsInput) (*offeringListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listOrgOfferings", in.OrgID, in.Status)
	after, err := timeCursor(in.Cursor, sortCreated, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListOrgOfferings(ctx, a, httpx.ParseID(in.OrgID), OrgQuery{Status: OfferingStatus(in.Status), Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	return listOut(p, sortCreated, fp), nil
}

// ---- create / update

// OfferingBody is the body of createOffering (04 §2.14).
type OfferingBody struct {
	ServiceID                  string                   `json:"service_id" format:"uuid"`
	EventID                    string                   `json:"event_id,omitempty" format:"uuid" doc:"An event of the organization (event-bound services need one)"`
	DefaultPayeeID             string                   `json:"default_payee_id,omitempty" format:"uuid" doc:"A payee of the organization; its default payee when omitted"`
	Title                      string                   `json:"title" minLength:"3" maxLength:"160"`
	DescriptionJSON            map[string]any           `json:"description_json,omitempty" doc:"Tiptap JSON: paragraphs, headings 2-3, lists, quotes, bold, italic, https links"`
	Deliverables               []OfferingDeliverableDTO `json:"deliverables" maxItems:"20" doc:"What the buyer gets (the items of the package)"`
	UnitPriceAmount            int64                    `json:"unit_price_amount" minimum:"1" maximum:"100000000000" doc:"Kuruş, excluding VAT"`
	QuantityTotal              *int                     `json:"quantity_total,omitempty" minimum:"1" maximum:"100000" doc:"Stock; unlimited when omitted"`
	PerBuyerLimit              *int                     `json:"per_buyer_limit,omitempty" minimum:"1" maximum:"100000"`
	SalesStartAt               *time.Time               `json:"sales_start_at,omitempty"`
	SalesEndAt                 *time.Time               `json:"sales_end_at,omitempty"`
	FulfilBy                   *time.Time               `json:"fulfil_by,omitempty"`
	InvoiceTrigger             string                   `json:"invoice_trigger,omitempty" enum:"on_payment,on_fulfillment" doc:"on_fulfillment when omitted"`
	BlockedSponsorFlags        []string                 `json:"blocked_sponsor_flags,omitempty" maxItems:"4" doc:"Sponsor categories the seller refuses: alcohol, tobacco, gambling, political"`
	RequiresUniversityApproval bool                     `json:"requires_university_approval,omitempty" doc:"A club's offering its university must approve too (T-21)"`
}

func deliverablesOf(in []OfferingDeliverableDTO) []Deliverable {
	out := make([]Deliverable, len(in))
	for i, d := range in {
		out[i] = Deliverable(d)
	}
	return out
}

func flagsOf(in []string) []SponsorFlag {
	out := make([]SponsorFlag, len(in))
	for i, f := range in {
		out[i] = SponsorFlag(f)
	}
	return out
}

type createInput struct {
	idempotency.OptionalHeader
	OrgPath
	Body OfferingBody
}

func (h handler) create(ctx context.Context, in *createInput) (*offeringOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	svc, err := uuid.Parse(b.ServiceID)
	if err != nil {
		return nil, problem.Field("body.service_id", "must be a UUID")
	}
	oi := OfferingInput{
		ServiceID: svc, Title: b.Title, DescriptionJSON: b.DescriptionJSON, Deliverables: deliverablesOf(b.Deliverables),
		UnitPriceAmount: b.UnitPriceAmount, QuantityTotal: b.QuantityTotal, PerBuyerLimit: b.PerBuyerLimit,
		SalesStartAt: b.SalesStartAt, SalesEndAt: b.SalesEndAt, FulfilBy: b.FulfilBy, InvoiceTrigger: InvoiceTrigger(b.InvoiceTrigger),
		BlockedSponsorFlags: flagsOf(b.BlockedSponsorFlags), RequiresUniversityApproval: b.RequiresUniversityApproval,
	}
	if oi.EventID, err = httpx.OptID("body.event_id", b.EventID); err != nil {
		return nil, err
	}
	if oi.DefaultPayeeID, err = httpx.OptID("body.default_payee_id", b.DefaultPayeeID); err != nil {
		return nil, err
	}
	v, err := h.svc.CreateOffering(ctx, a, httpx.ParseID(in.OrgID), oi)
	if err != nil {
		return nil, err
	}
	return offeringOut(v), nil
}

type updateOfferingInput struct {
	OfferingPath
	IfMatch string `header:"If-Match" doc:"ETag of getOffering (required: missing → 428, stale → 412)"`
	Body    struct {
		ServiceID                  *string                   `json:"service_id,omitempty" format:"uuid"`
		EventID                    *string                   `json:"event_id,omitempty" format:"uuid"`
		DefaultPayeeID             *string                   `json:"default_payee_id,omitempty" format:"uuid"`
		Title                      *string                   `json:"title,omitempty" minLength:"3" maxLength:"160"`
		DescriptionJSON            map[string]any            `json:"description_json,omitempty" doc:"Tiptap JSON"`
		Deliverables               *[]OfferingDeliverableDTO `json:"deliverables,omitempty" maxItems:"20"`
		UnitPriceAmount            *int64                    `json:"unit_price_amount,omitempty" minimum:"1" maximum:"100000000000"`
		QuantityTotal              *int                      `json:"quantity_total,omitempty" minimum:"1" maximum:"100000"`
		PerBuyerLimit              *int                      `json:"per_buyer_limit,omitempty" minimum:"1" maximum:"100000"`
		SalesStartAt               *time.Time                `json:"sales_start_at,omitempty"`
		SalesEndAt                 *time.Time                `json:"sales_end_at,omitempty"`
		FulfilBy                   *time.Time                `json:"fulfil_by,omitempty"`
		InvoiceTrigger             *string                   `json:"invoice_trigger,omitempty" enum:"on_payment,on_fulfillment"`
		BlockedSponsorFlags        *[]string                 `json:"blocked_sponsor_flags,omitempty" maxItems:"4"`
		RequiresUniversityApproval *bool                     `json:"requires_university_approval,omitempty"`
		Clear                      []string                  `json:"clear,omitempty" maxItems:"8" enum:"event_id,default_payee_id,description_json,quantity_total,per_buyer_limit,sales_start_at,sales_end_at,fulfil_by" doc:"Optional fields to empty"`
	}
}

func (h handler) update(ctx context.Context, in *updateOfferingInput) (*offeringOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := OfferingPatch{
		Title: b.Title, DescriptionJSON: b.DescriptionJSON, UnitPriceAmount: b.UnitPriceAmount, QuantityTotal: b.QuantityTotal,
		PerBuyerLimit: b.PerBuyerLimit, SalesStartAt: b.SalesStartAt, SalesEndAt: b.SalesEndAt, FulfilBy: b.FulfilBy,
		RequiresUniversityApproval: b.RequiresUniversityApproval, Clear: b.Clear,
	}
	for _, f := range []struct {
		loc string
		v   *string
		dst **uuid.UUID
	}{{"body.service_id", b.ServiceID, &p.ServiceID}, {"body.event_id", b.EventID, &p.EventID},
		{"body.default_payee_id", b.DefaultPayeeID, &p.DefaultPayeeID}} {
		if f.v == nil {
			continue
		}
		id, err := uuid.Parse(*f.v)
		if err != nil {
			return nil, problem.Field(f.loc, "must be a UUID (use clear to remove it)")
		}
		*f.dst = &id
	}
	if b.Deliverables != nil {
		ds := deliverablesOf(*b.Deliverables)
		p.Deliverables = &ds
	}
	if b.InvoiceTrigger != nil {
		p.InvoiceTrigger = ptr(InvoiceTrigger(*b.InvoiceTrigger))
	}
	if b.BlockedSponsorFlags != nil {
		fs := flagsOf(*b.BlockedSponsorFlags)
		p.BlockedSponsorFlags = &fs
	}
	v, err := h.svc.UpdateOffering(ctx, a, httpx.ParseID(in.OfferingID), in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return offeringOut(v), nil
}

type offeringActionInput struct {
	idempotency.OptionalHeader
	OfferingPath
}

func (h handler) submit(ctx context.Context, in *offeringActionInput) (*offeringOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.SubmitOffering(ctx, a, httpx.ParseID(in.OfferingID))
	if err != nil {
		return nil, err
	}
	return offeringOut(v), nil
}

type cancelOfferingInput struct {
	idempotency.OptionalHeader
	OfferingPath
	Body struct {
		CancelReason string `json:"cancel_reason" minLength:"1" maxLength:"500" doc:"Shown to the seller (no personal data)"`
	}
}

func (h handler) cancel(ctx context.Context, in *cancelOfferingInput) (*offeringOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CancelOffering(ctx, a, httpx.ParseID(in.OfferingID), in.Body.CancelReason)
	if err != nil {
		return nil, err
	}
	return offeringOut(v), nil
}

// ---- review queues and decisions

type queueInput struct {
	page.Params
	Status string `query:"status" enum:"pending_approval" default:"pending_approval" doc:"The offerings waiting for the platform decision"`
}

func (h handler) adminQueue(ctx context.Context, in *queueInput) (*offeringListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListOfferingReviewQueue")
	after, err := timeCursor(in.Cursor, sortSubmitted, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminReviewQueue(ctx, a, QueueQuery{Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	return listOut(p, sortSubmitted, fp), nil
}

// OfferingDecisionBody is the body of adminDecideOffering.
type OfferingDecisionBody struct {
	Decision        string `json:"decision" enum:"approved,rejected"`
	RejectionReason string `json:"rejection_reason,omitempty" maxLength:"500" doc:"Required for a rejection; shown to the seller (no personal data)"`
}

type adminDecideInput struct {
	idempotency.OptionalHeader
	OfferingPath
	Body OfferingDecisionBody
}

func (h handler) adminDecide(ctx context.Context, in *adminDecideInput) (*offeringOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.AdminDecideOffering(ctx, a, httpx.ParseID(in.OfferingID),
		Decision{Approve: in.Body.Decision == "approved", Reason: in.Body.RejectionReason})
	if err != nil {
		return nil, err
	}
	return offeringOut(v), nil
}

type uniQueueInput struct {
	OrgPath
	page.Params
	Status string `query:"status" enum:"pending" default:"pending" doc:"The offerings waiting for the university's decision"`
}

func (h handler) uniQueue(ctx context.Context, in *uniQueueInput) (*offeringListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listUniversityOfferingApprovals", in.OrgID)
	after, err := timeCursor(in.Cursor, sortSubmitted, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListUniversityApprovals(ctx, a, httpx.ParseID(in.OrgID), QueueQuery{Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	return listOut(p, sortSubmitted, fp), nil
}

type universityDecisionInput struct {
	idempotency.OptionalHeader
	OrgPath
	OfferingPath
	Body struct {
		Decision string `json:"decision" enum:"approved,rejected"`
		Note     string `json:"note,omitempty" maxLength:"500" doc:"Shown to the seller (no personal data)"`
	}
}

func (h handler) uniDecide(ctx context.Context, in *universityDecisionInput) (*offeringOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.DecideUniversityApproval(ctx, a, httpx.ParseID(in.OrgID), httpx.ParseID(in.OfferingID),
		Decision{Approve: in.Body.Decision == "approved", Reason: in.Body.Note})
	if err != nil {
		return nil, err
	}
	return offeringOut(v), nil
}

// ---- commerce policies

// CommercePolicyDTO is a commerce_policies row (02 §18.3).
type CommercePolicyDTO struct {
	PolicyID            string     `json:"policy_id" format:"uuid"`
	Scope               string     `json:"scope" enum:"default,seller_org,service,offering"`
	ScopeID             string     `json:"scope_id,omitempty" format:"uuid"`
	ModelCode           string     `json:"model_code" enum:"A,A2,B,C"`
	CollectionMethod    string     `json:"collection_method" enum:"bank_transfer,iyzico,direct_to_payee"`
	CommissionRateBP    int        `json:"commission_rate_bp" doc:"Basis points: 1000 = 10 %"`
	CommissionMinAmount *int64     `json:"commission_min_amount,omitempty"`
	CommissionMaxAmount *int64     `json:"commission_max_amount,omitempty"`
	CommissionBasis     string     `json:"commission_basis" enum:"net_excl_vat,gross_incl_vat"`
	CommissionPayer     string     `json:"commission_payer" enum:"seller,buyer"`
	CommissionVATRateBP int        `json:"commission_vat_rate_bp"`
	InvoiceAutomation   string     `json:"invoice_automation" enum:"manual,provider"`
	DisputeWindowDays   int16      `json:"dispute_window_days"`
	PayoutHoldDays      int16      `json:"payout_hold_days"`
	ValidFrom           time.Time  `json:"valid_from"`
	ValidTo             *time.Time `json:"valid_to,omitempty"`
	Notes               string     `json:"notes,omitempty"`
	CreatedBy           string     `json:"created_by" format:"uuid"`
	CreatedAt           time.Time  `json:"created_at"`
}

func policyDTO(p CommercePolicy) CommercePolicyDTO {
	return CommercePolicyDTO{
		PolicyID: p.ID.String(), Scope: string(p.Scope), ScopeID: httpx.IDStr(p.ScopeID), ModelCode: string(p.ModelCode),
		CollectionMethod: string(p.CollectionMethod), CommissionRateBP: p.CommissionRateBP,
		CommissionMinAmount: p.CommissionMinAmount, CommissionMaxAmount: p.CommissionMaxAmount,
		CommissionBasis: string(p.CommissionBasis), CommissionPayer: string(p.CommissionPayer),
		CommissionVATRateBP: p.CommissionVATRateBP, InvoiceAutomation: string(p.InvoiceAutomation),
		DisputeWindowDays: p.DisputeWindowDays, PayoutHoldDays: p.PayoutHoldDays, ValidFrom: p.ValidFrom.UTC(),
		ValidTo: db.UTCPtr(p.ValidTo), Notes: str(p.Notes), CreatedBy: p.CreatedBy.String(), CreatedAt: p.CreatedAt.UTC(),
	}
}

type listPoliciesInput struct {
	Scope string `query:"scope" enum:"default,seller_org,service,offering"`
	At    string `query:"at" format:"date-time" doc:"Only the policies in effect at this instant"`
}

type commercePolicyListOutput struct {
	Body struct {
		Items []CommercePolicyDTO `json:"items"`
	}
}

func (h handler) listPolicies(ctx context.Context, in *listPoliciesInput) (*commercePolicyListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	f := PolicyFilter{Scope: PolicyScope(in.Scope)}
	if in.At != "" {
		at, err := time.Parse(time.RFC3339Nano, in.At)
		if err != nil {
			return nil, problem.Field("query.at", "must be an RFC 3339 instant")
		}
		at = at.UTC()
		f.At = &at
	}
	ps, err := h.svc.AdminListPolicies(ctx, a, f)
	if err != nil {
		return nil, err
	}
	out := &commercePolicyListOutput{}
	out.Body.Items = make([]CommercePolicyDTO, len(ps))
	for i, p := range ps {
		out.Body.Items[i] = policyDTO(p)
	}
	return out, nil
}

// proofOf turns the reauth body part into identity's proof.
func proofOf(r *consent.ReauthDTO) identity.ReauthProof {
	if r == nil {
		return identity.ReauthProof{}
	}
	return identity.ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
}

type createCommercePolicyInput struct {
	idempotency.OptionalHeader
	Body struct {
		Scope               string             `json:"scope" enum:"default,seller_org,service,offering"`
		ScopeID             string             `json:"scope_id,omitempty" format:"uuid" doc:"The org, service or offering (not for default)"`
		ModelCode           string             `json:"model_code" enum:"A,A2,B,C"`
		CollectionMethod    string             `json:"collection_method,omitempty" enum:"bank_transfer,iyzico,direct_to_payee" doc:"bank_transfer (model C: direct_to_payee) when omitted; iyzico is phase 2"`
		CommissionRateBP    *int               `json:"commission_rate_bp" minimum:"0" maximum:"10000" doc:"Required, no default (T-10)"`
		CommissionMinAmount *int64             `json:"commission_min_amount,omitempty" minimum:"0"`
		CommissionMaxAmount *int64             `json:"commission_max_amount,omitempty" minimum:"0"`
		CommissionBasis     string             `json:"commission_basis,omitempty" enum:"net_excl_vat,gross_incl_vat" doc:"net_excl_vat when omitted"`
		CommissionPayer     string             `json:"commission_payer,omitempty" enum:"seller,buyer" doc:"seller (model C: buyer) when omitted"`
		CommissionVATRateBP *int               `json:"commission_vat_rate_bp,omitempty" minimum:"0" maximum:"10000" doc:"2000 when omitted"`
		InvoiceAutomation   string             `json:"invoice_automation,omitempty" enum:"manual,provider" doc:"manual when omitted"`
		DisputeWindowDays   *int               `json:"dispute_window_days,omitempty" minimum:"0" maximum:"60" doc:"7 when omitted"`
		PayoutHoldDays      *int               `json:"payout_hold_days,omitempty" minimum:"0" maximum:"90" doc:"0 when omitted"`
		ValidFrom           *time.Time         `json:"valid_from,omitempty" doc:"Now when omitted; never in the past"`
		ValidTo             *time.Time         `json:"valid_to,omitempty"`
		Notes               string             `json:"notes,omitempty" maxLength:"1000"`
		Reauth              *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

type commercePolicyOutput struct{ Body CommercePolicyDTO }

func (h handler) createPolicy(ctx context.Context, in *createCommercePolicyInput) (*commercePolicyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	pi := PolicyInput{
		Scope: PolicyScope(b.Scope), ModelCode: ModelCode(b.ModelCode), CollectionMethod: CollectionMethod(b.CollectionMethod),
		CommissionRateBP: b.CommissionRateBP, CommissionMinAmount: b.CommissionMinAmount, CommissionMaxAmount: b.CommissionMaxAmount,
		CommissionBasis: CommissionBasis(b.CommissionBasis), CommissionPayer: CommissionPayer(b.CommissionPayer),
		CommissionVATRateBP: b.CommissionVATRateBP, InvoiceAutomation: InvoiceAutomation(b.InvoiceAutomation),
		DisputeWindowDays: b.DisputeWindowDays, PayoutHoldDays: b.PayoutHoldDays, ValidFrom: b.ValidFrom, ValidTo: b.ValidTo,
		Notes: b.Notes,
	}
	if pi.ScopeID, err = httpx.OptID("body.scope_id", b.ScopeID); err != nil {
		return nil, err
	}
	p, err := h.svc.AdminCreatePolicy(ctx, a, pi, proofOf(b.Reauth))
	if err != nil {
		return nil, err
	}
	return &commercePolicyOutput{Body: policyDTO(p)}, nil
}

type closeCommercePolicyInput struct {
	idempotency.OptionalHeader
	PolicyID string `path:"policy_id" doc:"Policy ID (an unknown or malformed ID is 404)"`
	Body     struct {
		ValidTo *time.Time         `json:"valid_to,omitempty" doc:"Now when omitted; never in the past"`
		Reauth  *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) closePolicy(ctx context.Context, in *closeCommercePolicyInput) (*commercePolicyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminClosePolicy(ctx, a, httpx.ParseID(in.PolicyID), in.Body.ValidTo, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &commercePolicyOutput{Body: policyDTO(p)}, nil
}
