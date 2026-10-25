package catalog

import (
	"context"
	"slices"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// serviceClearable are the optional service fields Clear may empty.
var serviceClearable = []string{"description_tr", "vat_exemption_code", "vat_withholding_code", "invoice_description_tr",
	"accounting_reviewed_ref"}

// ListServices implements Service: organizations see the active, reviewed
// services (what they may offer); finance and platform admins see every
// service.
func (s *service) ListServices(ctx context.Context, a authz.Actor, category Category) ([]ServiceItem, error) {
	if err := authz.Can(ctx, a, authz.CatalogServiceList, serviceResource(uuid.Nil(), false)); err != nil {
		return nil, err
	}
	if category != "" && !category.Valid() {
		return nil, problem.Field("query.category", "is not a service category")
	}
	return s.d.Store.ListServices(ctx, ServiceFilter{Category: category, SellableOnly: !staff(a)})
}

// AdminCreateService implements Service. Setting the accountant's review
// (accounting_reviewed) is finance's only (03 k23).
func (s *service) AdminCreateService(ctx context.Context, a authz.Actor, in ServiceInput) (ServiceItem, error) {
	accounting := in.AccountingReviewed || in.AccountingReviewedRef != ""
	if err := authz.Can(ctx, a, authz.CatalogServiceManage, serviceResource(uuid.Nil(), accounting)); err != nil {
		return ServiceItem{}, err
	}
	row, err := s.newService(in)
	if err != nil {
		return ServiceItem{}, err
	}
	if in.AccountingReviewed {
		row.AccountingReviewedAt = ptr(s.now())
	}
	hash, err := idempotency.Hash(struct {
		Op string
		In ServiceInput
	}{"create_service", in})
	if err != nil {
		return ServiceItem{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		stored, err := s.d.Store.InsertService(ctx, row)
		if err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.d.Audit.Log(ctx, serviceEvent(a, stored, map[string]any{
			"code": stored.Code, "accounting_reviewed": stored.AccountingReviewedAt != nil,
			"default_vat_rate_bp": stored.DefaultVATRateBP, "vat_class": string(stored.VATClass),
		}))
	})
	if err != nil {
		return ServiceItem{}, err
	}
	return s.d.Store.GetService(ctx, id, false)
}

// newService validates the input of adminCreateService.
func (s *service) newService(in ServiceInput) (ServiceItem, error) {
	if !serviceCodeRe.MatchString(in.Code) {
		return ServiceItem{}, problem.Field("body.code", "must be 3-41 lower-case letters, digits and _, starting with a letter")
	}
	row := ServiceItem{ID: uuid.NewV7(), Code: in.Code, Category: in.Category, FulfillmentType: in.FulfillmentType,
		DefaultVATRateBP: 2000, VATClass: VATStandard, Active: true}
	var err error
	if row.NameTR, err = cleanText("body.name_tr", in.NameTR, 2, maxServiceName); err != nil {
		return ServiceItem{}, err
	}
	if row.DescriptionTR, err = cleanOptional("body.description_tr", in.DescriptionTR, maxServiceDesc); err != nil {
		return ServiceItem{}, err
	}
	if row.InvoiceDescriptionTR, err = cleanOptional("body.invoice_description_tr", in.InvoiceDescriptionTR, maxInvoiceDesc); err != nil {
		return ServiceItem{}, err
	}
	if row.VATExemptionCode, err = cleanOptional("body.vat_exemption_code", in.VATExemptionCode, maxCodeText); err != nil {
		return ServiceItem{}, err
	}
	if row.VATWithholdingCode, err = cleanOptional("body.vat_withholding_code", in.VATWithholdingCode, maxCodeText); err != nil {
		return ServiceItem{}, err
	}
	if row.AccountingReviewedRef, err = cleanOptional("body.accounting_reviewed_ref", in.AccountingReviewedRef, maxCodeText); err != nil {
		return ServiceItem{}, err
	}
	if row.EntitlementTemplate, err = templateJSON("body.entitlement_template", in.EntitlementTemplate); err != nil {
		return ServiceItem{}, err
	}
	if row.VATWithholdingNum, err = smallint("body.vat_withholding_num", in.VATWithholdingNum); err != nil {
		return ServiceItem{}, err
	}
	if row.VATWithholdingDen, err = smallint("body.vat_withholding_den", in.VATWithholdingDen); err != nil {
		return ServiceItem{}, err
	}
	if in.DefaultVATRateBP != nil {
		row.DefaultVATRateBP = *in.DefaultVATRateBP
	}
	if in.VATClass != "" {
		row.VATClass = in.VATClass
	}
	if in.Active != nil {
		row.Active = *in.Active
	}
	return row, checkService(row)
}

// AdminUpdateService implements Service: a partial update; the accountant's
// review (03 k23) is finance's. A change to the VAT profile withdraws the
// review unless the same request (by finance) reviews it again, so no
// unreviewed profile is ever sold.
func (s *service) AdminUpdateService(ctx context.Context, a authz.Actor, id uuid.UUID, p ServicePatch) (ServiceItem, error) {
	if err := authz.Can(ctx, a, authz.CatalogServiceManage, serviceResource(id, p.accountingWrite())); err != nil {
		return ServiceItem{}, err
	}
	if p.empty() {
		return ServiceItem{}, ErrEmptyPatch
	}
	for _, c := range p.Clear {
		if !slices.Contains(serviceClearable, c) {
			return ServiceItem{}, problem.Field("body.clear", "names a field that cannot be cleared")
		}
	}
	var out ServiceItem
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.GetService(ctx, id, true)
		if err != nil {
			return err
		}
		next, fields, err := s.patchService(cur, p)
		if err != nil {
			return err
		}
		stored, err := s.d.Store.UpdateService(ctx, next)
		if err != nil {
			return err
		}
		out = stored
		return s.d.Audit.Log(ctx, serviceEvent(a, stored, map[string]any{
			"fields": fields, "accounting_reviewed": stored.AccountingReviewedAt != nil, "active": stored.Active,
		}))
	})
	return out, err
}

// patchService applies p to cur and returns the changed field names.
func (s *service) patchService(cur ServiceItem, p ServicePatch) (ServiceItem, []string, error) {
	next, fields := cur, []string{}
	var err error
	set := func(name string) { fields = append(fields, name) }
	if p.NameTR != nil {
		if next.NameTR, err = cleanText("body.name_tr", *p.NameTR, 2, maxServiceName); err != nil {
			return ServiceItem{}, nil, err
		}
		set("name_tr")
	}
	opt := func(name string, v *string, maxRunes int, dst **string) error {
		if v == nil {
			return nil
		}
		var e error
		if *dst, e = cleanOptional("body."+name, *v, maxRunes); e != nil {
			return e
		}
		set(name)
		return nil
	}
	for _, f := range []struct {
		name string
		v    *string
		max  int
		dst  **string
	}{
		{"description_tr", p.DescriptionTR, maxServiceDesc, &next.DescriptionTR},
		{"vat_exemption_code", p.VATExemptionCode, maxCodeText, &next.VATExemptionCode},
		{"vat_withholding_code", p.VATWithholdingCode, maxCodeText, &next.VATWithholdingCode},
		{"invoice_description_tr", p.InvoiceDescriptionTR, maxInvoiceDesc, &next.InvoiceDescriptionTR},
		{"accounting_reviewed_ref", p.AccountingReviewedRef, maxCodeText, &next.AccountingReviewedRef},
	} {
		if err := opt(f.name, f.v, f.max, f.dst); err != nil {
			return ServiceItem{}, nil, err
		}
	}
	if p.Category != nil {
		next.Category = *p.Category
		set("category")
	}
	if p.EntitlementTemplate != nil {
		if next.EntitlementTemplate, err = templateJSON("body.entitlement_template", p.EntitlementTemplate); err != nil {
			return ServiceItem{}, nil, err
		}
		set("entitlement_template")
	}
	vatProfile := false
	if p.DefaultVATRateBP != nil {
		next.DefaultVATRateBP = *p.DefaultVATRateBP
		set("default_vat_rate_bp")
		vatProfile = true
	}
	if p.VATClass != nil {
		next.VATClass = *p.VATClass
		set("vat_class")
		vatProfile = true
	}
	if p.VATWithholdingNum != nil {
		if next.VATWithholdingNum, err = smallint("body.vat_withholding_num", p.VATWithholdingNum); err != nil {
			return ServiceItem{}, nil, err
		}
		set("vat_withholding_num")
		vatProfile = true
	}
	if p.VATWithholdingDen != nil {
		if next.VATWithholdingDen, err = smallint("body.vat_withholding_den", p.VATWithholdingDen); err != nil {
			return ServiceItem{}, nil, err
		}
		set("vat_withholding_den")
		vatProfile = true
	}
	vatProfile = vatProfile || p.VATExemptionCode != nil || p.VATWithholdingCode != nil
	if p.Active != nil {
		next.Active = *p.Active
		set("active")
	}
	for _, c := range p.Clear {
		switch c {
		case "description_tr":
			next.DescriptionTR = nil
		case "vat_exemption_code":
			next.VATExemptionCode, vatProfile = nil, true
		case "vat_withholding_code":
			next.VATWithholdingCode, vatProfile = nil, true
		case "invoice_description_tr":
			next.InvoiceDescriptionTR = nil
		case "accounting_reviewed_ref":
			next.AccountingReviewedRef = nil
		}
		set(c)
	}
	switch {
	case p.AccountingReviewed != nil && *p.AccountingReviewed:
		next.AccountingReviewedAt = ptr(s.now())
		set("accounting_reviewed_at")
	case p.AccountingReviewed != nil || vatProfile && cur.AccountingReviewedAt != nil:
		// Withdrawn by finance, or the VAT profile changed without a new review.
		next.AccountingReviewedAt = nil
		set("accounting_reviewed_at")
	}
	return next, fields, checkService(next)
}

func serviceEvent(a authz.Actor, sv ServiceItem, meta map[string]any) audit.Event {
	id := sv.ID
	return audit.Event{Action: string(authz.CatalogServiceManage), Outcome: audit.Success, ResourceType: auditService,
		ResourceID: &id, Metadata: meta}.By(a)
}
