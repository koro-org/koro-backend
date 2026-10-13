package catalog

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/platform/money"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Field limits (02 §18; BE-038).
const (
	minTitle          = 3
	maxTitle          = 160
	maxServiceName    = 160
	maxServiceDesc    = 2000
	maxInvoiceDesc    = 300
	maxCodeText       = 40 // exemption / withholding codes, accounting reference
	maxReason         = 500
	maxNotes          = 1000
	maxDeliverables   = 20
	maxDeliverableLbl = 120
	maxQuantity       = 100000
	// maxUnitPrice caps a unit price (kuruş): 1 billion TRY, so price ×
	// quantity and every VAT/commission product stay far from int64 limits.
	maxUnitPrice = 100_000_000_000
	maxQueryLen  = 100
)

var (
	serviceCodeRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{2,40}$`)
	deliverableKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,40}$`)
)

func fieldErr(location, msg string) error {
	return problem.ErrValidation.WithFields(problem.FieldError{Location: location, Message: msg})
}

func normalized(loc, s string) (string, error) {
	n, err := textx.NormalizeText(s)
	if err != nil {
		return "", fieldErr(loc, "contains control or bidi characters")
	}
	return n, nil
}

// cleanText normalises a required single-line text of minRunes–maxRunes.
func cleanText(loc, s string, minRunes, maxRunes int) (string, error) {
	n, err := normalized(loc, s)
	if err != nil {
		return "", err
	}
	if c := utf8.RuneCountInString(n); c < minRunes || c > maxRunes {
		return "", fieldErr(loc, "must be "+strconv.Itoa(minRunes)+"-"+strconv.Itoa(maxRunes)+" characters")
	}
	return n, nil
}

// cleanOptional normalises an optional text; "" clears it (nil).
func cleanOptional(loc, s string, maxRunes int) (*string, error) {
	n, err := normalized(loc, s)
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(n) > maxRunes {
		return nil, fieldErr(loc, "must be at most "+strconv.Itoa(maxRunes)+" characters")
	}
	if n == "" {
		return nil, nil
	}
	return &n, nil
}

func ptr[T any](v T) *T { return &v }

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC().Truncate(time.Microsecond)
	return &u
}

// ---- services

// ServiceInput is the body of adminCreateService (02 §18.1 columns).
type ServiceInput struct {
	Code                  string
	NameTR                string
	DescriptionTR         string
	Category              Category
	FulfillmentType       FulfillmentType
	EntitlementTemplate   map[string]any // platform_native only
	DefaultVATRateBP      *int           // 2000 (20 %) when nil
	VATClass              VATClass       // standard when ""
	VATExemptionCode      string
	VATWithholdingCode    string
	VATWithholdingNum     *int
	VATWithholdingDen     *int
	InvoiceDescriptionTR  string
	AccountingReviewed    bool   // k23: finance only; stamps accounting_reviewed_at
	AccountingReviewedRef string // the accountant's reference (k23)
	Active                *bool  // true when nil
}

// ServicePatch is the body of adminUpdateService: nil leaves a field as it
// is; Clear empties optional fields.
type ServicePatch struct {
	NameTR                *string
	DescriptionTR         *string
	Category              *Category
	EntitlementTemplate   map[string]any
	DefaultVATRateBP      *int
	VATClass              *VATClass
	VATExemptionCode      *string
	VATWithholdingCode    *string
	VATWithholdingNum     *int
	VATWithholdingDen     *int
	InvoiceDescriptionTR  *string
	AccountingReviewed    *bool // k23: true stamps a new review, false withdraws it
	AccountingReviewedRef *string
	Active                *bool
	Clear                 []string
}

// accountingWrite reports whether the patch touches the accountant's review (03 k23).
func (p ServicePatch) accountingWrite() bool {
	return p.AccountingReviewed != nil || p.AccountingReviewedRef != nil || slices.Contains(p.Clear, "accounting_reviewed_ref")
}

func (p ServicePatch) empty() bool {
	return p.NameTR == nil && p.DescriptionTR == nil && p.Category == nil && p.EntitlementTemplate == nil &&
		p.DefaultVATRateBP == nil && p.VATClass == nil && p.VATExemptionCode == nil && p.VATWithholdingCode == nil &&
		p.VATWithholdingNum == nil && p.VATWithholdingDen == nil && p.InvoiceDescriptionTR == nil &&
		p.AccountingReviewed == nil && p.AccountingReviewedRef == nil && p.Active == nil && len(p.Clear) == 0
}

// checkService mirrors the CHECKs of 000098 with field errors: the VAT
// rate, the class and its exemption code or withholding fraction, the
// entitlement template of a platform-native service.
func checkService(s ServiceItem) error {
	switch {
	case !s.Category.Valid():
		return fieldErr("body.category", "is not a service category")
	case !s.FulfillmentType.Valid():
		return fieldErr("body.fulfillment_type", "is not a fulfilment type")
	case !money.ValidBP(int64(s.DefaultVATRateBP)):
		return fieldErr("body.default_vat_rate_bp", "must be 0-10000 basis points")
	case !s.VATClass.Valid():
		return fieldErr("body.vat_class", "is not a VAT class")
	case (s.FulfillmentType == FulfillPlatformNative) != (s.EntitlementTemplate != nil):
		return fieldErr("body.entitlement_template", "is required for a platform-native service and only there")
	case (s.VATWithholdingNum == nil) != (s.VATWithholdingDen == nil):
		return fieldErr("body.vat_withholding_den", "numerator and denominator go together")
	case s.VATWithholdingNum != nil && (*s.VATWithholdingNum <= 0 || *s.VATWithholdingNum > *s.VATWithholdingDen):
		return fieldErr("body.vat_withholding_num", "must be 1..denominator")
	case (s.VATClass == VATWithheld) != (s.VATWithholdingNum != nil):
		return fieldErr("body.vat_class", "withheld goes with a withholding fraction and only it")
	case s.VATClass == VATExempt && s.VATExemptionCode == nil:
		return fieldErr("body.vat_exemption_code", "is required for an exempt service")
	}
	return nil
}

// templateJSON validates an entitlement template (a JSON object with a
// feature, a kind and a positive quantity_per_unit; BE-040 reads it).
func templateJSON(loc string, m map[string]any) (JSON, error) {
	if m == nil {
		return nil, nil
	}
	feature, _ := m["feature"].(string)
	kind, _ := m["kind"].(string)
	qty, _ := m["quantity_per_unit"].(float64)
	if !deliverableKeyRe.MatchString(feature) || !deliverableKeyRe.MatchString(kind) || qty < 1 || qty != float64(int(qty)) {
		return nil, fieldErr(loc, `must be {"feature", "kind", "quantity_per_unit" ≥ 1, "valid_days"?}`)
	}
	if v, ok := m["valid_days"]; ok {
		d, isNum := v.(float64)
		if !isNum || d < 1 || d > 3660 || d != float64(int(d)) {
			return nil, fieldErr(loc+".valid_days", "must be 1-3660 days")
		}
	}
	b, err := json.Marshal(m)
	if err != nil || len(b) > 4096 {
		return nil, fieldErr(loc, "must be a small JSON object")
	}
	return JSON(b), nil
}

func smallint(loc string, v *int) (*int16, error) {
	if v == nil {
		return nil, nil
	}
	if *v < 1 || *v > 1000 {
		return nil, fieldErr(loc, "must be 1-1000")
	}
	n := int16(*v)
	return &n, nil
}

// ---- offerings

// OfferingInput is the body of createOffering (04 §2.14).
type OfferingInput struct {
	ServiceID                  uuid.UUID
	EventID                    *uuid.UUID
	DefaultPayeeID             *uuid.UUID // the seller's default payee when nil
	Title                      string
	DescriptionJSON            map[string]any
	Deliverables               []Deliverable
	UnitPriceAmount            int64
	QuantityTotal              *int
	PerBuyerLimit              *int
	SalesStartAt               *time.Time
	SalesEndAt                 *time.Time
	FulfilBy                   *time.Time
	InvoiceTrigger             InvoiceTrigger // on_fulfillment when ""
	BlockedSponsorFlags        []SponsorFlag
	RequiresUniversityApproval bool // T-21 (clubs only)
}

// OfferingPatch is the body of updateOffering: nil leaves a field as it is;
// Clear empties optional fields (offeringClearable).
type OfferingPatch struct {
	ServiceID                  *uuid.UUID
	EventID                    *uuid.UUID
	DefaultPayeeID             *uuid.UUID
	Title                      *string
	DescriptionJSON            map[string]any
	Deliverables               *[]Deliverable
	UnitPriceAmount            *int64
	QuantityTotal              *int
	PerBuyerLimit              *int
	SalesStartAt               *time.Time
	SalesEndAt                 *time.Time
	FulfilBy                   *time.Time
	InvoiceTrigger             *InvoiceTrigger
	BlockedSponsorFlags        *[]SponsorFlag
	RequiresUniversityApproval *bool
	Clear                      []string
}

// offeringClearable are the optional offering fields Clear may empty.
var offeringClearable = []string{"event_id", "default_payee_id", "description_json", "quantity_total", "per_buyer_limit",
	"sales_start_at", "sales_end_at", "fulfil_by"}

func (p OfferingPatch) empty() bool {
	return p.ServiceID == nil && p.EventID == nil && p.DefaultPayeeID == nil && p.Title == nil && p.DescriptionJSON == nil &&
		p.Deliverables == nil && p.UnitPriceAmount == nil && p.QuantityTotal == nil && p.PerBuyerLimit == nil &&
		p.SalesStartAt == nil && p.SalesEndAt == nil && p.FulfilBy == nil && p.InvoiceTrigger == nil &&
		p.BlockedSponsorFlags == nil && p.RequiresUniversityApproval == nil && len(p.Clear) == 0
}

// cleanDeliverables checks the deliverables: at most 20, unique keys of
// [a-z][a-z0-9_]{1,40}, labels of 1–120 characters.
func cleanDeliverables(in []Deliverable) (Deliverables, error) {
	if len(in) > maxDeliverables {
		return nil, fieldErr("body.deliverables", "must have at most 20 items")
	}
	out := make(Deliverables, 0, len(in))
	seen := map[string]bool{}
	for i, d := range in {
		loc := "body.deliverables[" + strconv.Itoa(i) + "]"
		if !deliverableKeyRe.MatchString(d.Key) || seen[d.Key] {
			return nil, fieldErr(loc+".key", "must be a unique key of lower-case letters, digits and _")
		}
		seen[d.Key] = true
		label, err := cleanText(loc+".label", d.Label, 1, maxDeliverableLbl)
		if err != nil {
			return nil, err
		}
		out = append(out, Deliverable{Key: d.Key, Label: label})
	}
	return out, nil
}

func cleanFlags(in []SponsorFlag) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, f := range in {
		if !slices.Contains(SponsorFlags(), f) || slices.Contains(out, string(f)) {
			return nil, fieldErr("body.blocked_sponsor_flags", "must be distinct values of alcohol, tobacco, gambling, political")
		}
		out = append(out, string(f))
	}
	slices.Sort(out)
	return out, nil
}

// checkOffering checks an offering's own fields (the price, the stock,
// the sales window) at now; the parents are checked by the service.
func checkOffering(o Offering, now time.Time) error {
	switch {
	case o.UnitPriceAmount <= 0 || o.UnitPriceAmount > maxUnitPrice:
		return fieldErr("body.unit_price_amount", "must be 1-100000000000 kuruş")
	case o.QuantityTotal != nil && (*o.QuantityTotal < 1 || *o.QuantityTotal > maxQuantity):
		return fieldErr("body.quantity_total", "must be 1-100000")
	case o.PerBuyerLimit != nil && (*o.PerBuyerLimit < 1 || *o.PerBuyerLimit > maxQuantity):
		return fieldErr("body.per_buyer_limit", "must be 1-100000")
	case o.PerBuyerLimit != nil && o.QuantityTotal != nil && *o.PerBuyerLimit > *o.QuantityTotal:
		return fieldErr("body.per_buyer_limit", "must not exceed quantity_total")
	case o.SalesStartAt != nil && o.SalesEndAt != nil && !o.SalesEndAt.After(*o.SalesStartAt):
		return fieldErr("body.sales_end_at", "must be after sales_start_at")
	case o.SalesEndAt != nil && !o.SalesEndAt.After(now):
		return fieldErr("body.sales_end_at", "must be in the future")
	case o.FulfilBy != nil && o.SalesStartAt != nil && o.FulfilBy.Before(*o.SalesStartAt):
		return fieldErr("body.fulfil_by", "must not be before sales_start_at")
	case !o.InvoiceTrigger.Valid():
		return fieldErr("body.invoice_trigger", "must be on_payment or on_fulfillment")
	}
	return nil
}

// sellerKindOf maps the seller org's kind: clubs and universities sell
// for their payee, the platform org for Koro; anything else sells nothing.
func sellerKindOf(k orgs.Kind) (SellerKind, bool) {
	switch k {
	case orgs.KindClub, orgs.KindUniversity:
		return SellerOrg, true
	case orgs.KindPlatform:
		return SellerPlatform, true
	}
	return "", false
}

// openEvent reports whether an offering may be bound to (or submitted
// with) the event: a draft or published event.
func openEvent(e events.EventSummary) bool {
	return e.Status == events.StatusDraft || e.Status == events.StatusPublished
}

// Decision is the body of adminDecideOffering and
// decideUniversityOfferingApproval.
type Decision struct {
	Approve bool
	Reason  string // the rejection reason (platform) or the note (university); ≤ 500
}

// cleanReason checks a reason or note of at most 500 characters (required
// when required is set).
func cleanReason(loc, s string, required bool) (*string, error) {
	n := strings.TrimSpace(s)
	if required && n == "" {
		return nil, fieldErr(loc, "is required")
	}
	return cleanOptional(loc, n, maxReason)
}

// ---- queries

// BrowseQuery is the query of browseOfferings (04 §2.14).
type BrowseQuery struct {
	Q            string
	OrgID        *uuid.UUID
	EventID      *uuid.UUID
	UniversityID *uuid.UUID // directory universities.id
	Category     Category
	PriceMax     *int64
	Sort         string
	Limit        int
	After        *BrowseKey
}

// OrgQuery is the query of listOrgOfferings.
type OrgQuery struct {
	Status OfferingStatus
	Limit  int
	After  *TimeKey
}

// QueueQuery is the query of the review queues.
type QueueQuery struct {
	Limit int
	After *TimeKey
}

// ---- commerce policies

// PolicyInput is the body of adminCreateCommercePolicy (02 §18.3 columns).
// Defaults: collection_method bank_transfer (direct_to_payee for model C),
// commission_basis net_excl_vat, commission_payer seller (buyer for model
// C), commission_vat_rate_bp 2000, invoice_automation manual,
// dispute_window_days 7, payout_hold_days 0, valid_from now. There is no
// default commission rate (T-10).
type PolicyInput struct {
	Scope               PolicyScope
	ScopeID             *uuid.UUID
	ModelCode           ModelCode
	CollectionMethod    CollectionMethod
	CommissionRateBP    *int
	CommissionMinAmount *int64
	CommissionMaxAmount *int64
	CommissionBasis     CommissionBasis
	CommissionPayer     CommissionPayer
	CommissionVATRateBP *int
	InvoiceAutomation   InvoiceAutomation
	DisputeWindowDays   *int
	PayoutHoldDays      *int
	ValidFrom           *time.Time
	ValidTo             *time.Time
	Notes               string
}

// policyOf validates in and builds the row (the scope's existence is the
// service's). now is the earliest valid_from: no policy starts in the past.
func policyOf(in PolicyInput, now time.Time) (CommercePolicy, error) {
	p := CommercePolicy{
		Scope: in.Scope, ScopeID: in.ScopeID, ModelCode: in.ModelCode, CollectionMethod: in.CollectionMethod,
		CommissionMinAmount: in.CommissionMinAmount, CommissionMaxAmount: in.CommissionMaxAmount,
		CommissionBasis: in.CommissionBasis, CommissionPayer: in.CommissionPayer, CommissionVATRateBP: 2000,
		InvoiceAutomation: in.InvoiceAutomation, DisputeWindowDays: 7, ValidFrom: now,
	}
	switch {
	case !p.Scope.Valid():
		return CommercePolicy{}, fieldErr("body.scope", "must be default, seller_org, service or offering")
	case (p.Scope == ScopeDefault) != (p.ScopeID == nil):
		return CommercePolicy{}, fieldErr("body.scope_id", "is required for every scope but default, and only there")
	case !p.ModelCode.Valid():
		return CommercePolicy{}, fieldErr("body.model_code", "must be A, A2, B or C")
	case in.CommissionRateBP == nil:
		return CommercePolicy{}, fieldErr("body.commission_rate_bp", "is required (no default rate: T-10)")
	case !money.ValidBP(int64(*in.CommissionRateBP)):
		return CommercePolicy{}, fieldErr("body.commission_rate_bp", "must be 0-10000 basis points")
	}
	p.CommissionRateBP = *in.CommissionRateBP
	if p.CollectionMethod == "" {
		p.CollectionMethod = CollectBankTransfer
		if p.ModelCode == ModelC {
			p.CollectionMethod = CollectDirectToPayee
		}
	}
	if p.CommissionPayer == "" {
		p.CommissionPayer = PayerSeller
		if p.ModelCode == ModelC {
			p.CommissionPayer = PayerBuyer
		}
	}
	if p.CommissionBasis == "" {
		p.CommissionBasis = BasisNetExclVAT
	}
	if p.InvoiceAutomation == "" {
		p.InvoiceAutomation = InvoiceManual
	}
	if in.CommissionVATRateBP != nil {
		p.CommissionVATRateBP = *in.CommissionVATRateBP
	}
	if in.DisputeWindowDays != nil {
		p.DisputeWindowDays = int16(min(max(*in.DisputeWindowDays, -1), 1000))
	}
	if in.PayoutHoldDays != nil {
		p.PayoutHoldDays = int16(min(max(*in.PayoutHoldDays, -1), 1000))
	}
	if in.ValidFrom != nil {
		p.ValidFrom = in.ValidFrom.UTC().Truncate(time.Microsecond)
	}
	p.ValidTo = utc(in.ValidTo)
	switch {
	case !p.CollectionMethod.Valid():
		return CommercePolicy{}, fieldErr("body.collection_method", "must be bank_transfer or direct_to_payee")
	case p.CollectionMethod == CollectIyzico:
		return CommercePolicy{}, fieldErr("body.collection_method", "iyzico is not available before phase 2")
	case (p.ModelCode == ModelC) != (p.CollectionMethod == CollectDirectToPayee):
		return CommercePolicy{}, fieldErr("body.collection_method", "direct_to_payee goes with model C and only it")
	case p.CommissionPayer != PayerSeller && p.CommissionPayer != PayerBuyer:
		return CommercePolicy{}, fieldErr("body.commission_payer", "must be seller or buyer")
	case p.ModelCode == ModelC && p.CommissionPayer != PayerBuyer:
		return CommercePolicy{}, fieldErr("body.commission_payer", "model C charges the buyer")
	case p.CommissionBasis != BasisNetExclVAT && p.CommissionBasis != BasisGrossInclVAT:
		return CommercePolicy{}, fieldErr("body.commission_basis", "must be net_excl_vat or gross_incl_vat")
	case p.CommissionMinAmount != nil && (*p.CommissionMinAmount < 0 || *p.CommissionMinAmount > maxUnitPrice):
		return CommercePolicy{}, fieldErr("body.commission_min_amount", "must be 0-100000000000 kuruş")
	case p.CommissionMaxAmount != nil && (*p.CommissionMaxAmount < 0 || *p.CommissionMaxAmount > maxUnitPrice):
		return CommercePolicy{}, fieldErr("body.commission_max_amount", "must be 0-100000000000 kuruş")
	case p.CommissionMinAmount != nil && p.CommissionMaxAmount != nil && *p.CommissionMinAmount > *p.CommissionMaxAmount:
		return CommercePolicy{}, fieldErr("body.commission_max_amount", "must not be below commission_min_amount")
	case !money.ValidBP(int64(p.CommissionVATRateBP)):
		return CommercePolicy{}, fieldErr("body.commission_vat_rate_bp", "must be 0-10000 basis points")
	case p.InvoiceAutomation != InvoiceManual && p.InvoiceAutomation != InvoiceProvider:
		return CommercePolicy{}, fieldErr("body.invoice_automation", "must be manual or provider")
	case p.DisputeWindowDays < 0 || p.DisputeWindowDays > 60:
		return CommercePolicy{}, fieldErr("body.dispute_window_days", "must be 0-60")
	case p.PayoutHoldDays < 0 || p.PayoutHoldDays > 90:
		return CommercePolicy{}, fieldErr("body.payout_hold_days", "must be 0-90")
	case p.ValidFrom.Before(now):
		return CommercePolicy{}, fieldErr("body.valid_from", "must not be in the past")
	case p.ValidTo != nil && !p.ValidTo.After(p.ValidFrom):
		return CommercePolicy{}, fieldErr("body.valid_to", "must be after valid_from")
	}
	notes, err := cleanOptional("body.notes", in.Notes, maxNotes)
	if err != nil {
		return CommercePolicy{}, err
	}
	p.Notes = notes
	return p, nil
}
