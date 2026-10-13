package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"uuid"

	"koro/internal/catalog"
)

// matrixFixtures seeds fixtures/catalog.yaml (03 §7.3) into the in-memory
// catalog store installed by useMemStore. Every row is written in its final
// state at matrixOrgTime (2026-10-01T00:00:00Z, before the matrix clock),
// so the ETag of every offering is "1hmhuo3hji000":
//
//	kind service   code, category, fulfillment (org_bound | platform_native), reviewed
//	kind policy    the default policy (model A, commission rate) valid from matrixOrgTime,
//	               created by finance_admin_2
//	kind offering  org, service, status (draft | pending_approval | active), created_by;
//	               payee and event (fixtures of parties and events) when given; university:
//	               true requires the university's approval (pending); approved_by when the
//	               platform approved it (active, or pending with university)
func (w *catalogWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	mem, ok := w.built.(*catalog.MemStore)
	if !ok {
		return fmt.Errorf("catalog: the matrix needs the in-memory store, got %T", w.built)
	}
	all := fx.Of("catalog")
	rank := map[string]int{"service": 0, "policy": 1, "offering": 1}
	slices.SortStableFunc(all, func(a, b fixture) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
	t := matrixOrgTime
	for _, f := range all {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		str := func(k string) string { s, _ := f.Fields[k].(string); return s }
		flag := func(k string) bool { b, _ := f.Fields[k].(bool); return b }
		ref := func(k string) (*uuid.UUID, error) {
			if _, ok := f.Fields[k]; !ok {
				return nil, nil
			}
			v, err := fx.Ref(f, k)
			return &v, err
		}
		switch f.Kind {
		case "service":
			v := catalog.ServiceItem{ID: id, Code: str("code"), NameTR: "Matris " + str("code"), Category: catalog.Category(str("category")),
				FulfillmentType: catalog.FulfillmentType(str("fulfillment")), DefaultVATRateBP: 2000, VATClass: catalog.VATStandard,
				Active: true, CreatedAt: t, UpdatedAt: t}
			if v.FulfillmentType == catalog.FulfillPlatformNative {
				v.EntitlementTemplate = catalog.JSON(`{"feature":"targeted_post","kind":"quota","quantity_per_unit":5,"valid_days":90}`)
			}
			if flag("reviewed") {
				v.AccountingReviewedAt = &t
			}
			if _, err := mem.InsertService(ctx, v); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "policy":
			by, err := fx.ID("finance_admin_2")
			if err != nil {
				return err
			}
			rate, _ := f.Fields["rate"].(int)
			p := catalog.DefaultPolicy(rate)
			row := catalog.CommercePolicy{ID: id, Scope: p.Scope, ModelCode: p.ModelCode, CollectionMethod: p.CollectionMethod,
				CommissionRateBP: rate, CommissionBasis: p.CommissionBasis, CommissionPayer: p.CommissionPayer, CommissionVATRateBP: 2000,
				InvoiceAutomation: p.InvoiceAutomation, DisputeWindowDays: 7, ValidFrom: t, CreatedBy: by, CreatedAt: t}
			if _, err := mem.InsertPolicy(ctx, row); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "offering":
			if err := w.matrixOffering(ctx, fx, mem, f, id, ref); err != nil {
				return err
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}

func (w *catalogWiring) matrixOffering(ctx context.Context, fx *matrixFixtures, mem *catalog.MemStore, f fixture, id uuid.UUID,
	ref func(string) (*uuid.UUID, error)) error {
	t := matrixOrgTime
	org, err := fx.Ref(f, "org")
	if err != nil {
		return err
	}
	svc, err := fx.Ref(f, "service")
	if err != nil {
		return err
	}
	by, err := fx.Ref(f, "created_by")
	if err != nil {
		return err
	}
	status, _ := f.Fields["status"].(string)
	platform, _ := f.Fields["platform"].(bool)
	uni, _ := f.Fields["university"].(bool)
	o := catalog.Offering{ID: id, ServiceID: svc, SellerKind: catalog.SellerOrg, OrgID: org, Title: "Matris " + f.Name,
		Deliverables: catalog.Deliverables{{Key: "booth_3x3", Label: "3x3 stant alanı"}}, UnitPriceAmount: 1_000_000,
		Currency: "TRY", VATRateBP: 2000, CompensationType: "cash", QuantityTotal: ptrInt(5), PerBuyerLimit: ptrInt(1),
		InvoiceTrigger: catalog.InvoiceOnFulfillment, UniversityApprovalStatus: catalog.UniNotRequired,
		Status: catalog.OfferingStatus(status), CreatedBy: by, CreatedAt: t, UpdatedAt: t}
	if platform {
		o.SellerKind = catalog.SellerPlatform
	}
	if uni {
		o.RequiresUniversityApproval, o.UniversityApprovalStatus = true, catalog.UniPending
	}
	if o.DefaultPayeeID, err = ref("payee"); err != nil {
		return err
	}
	if o.EventID, err = ref("event"); err != nil {
		return err
	}
	if o.Status != catalog.OfferingDraft {
		o.SubmittedAt = &t
	}
	approver, err := ref("approved_by")
	if err != nil {
		return err
	}
	if approver != nil {
		o.ApprovedBy, o.ApprovedAt = approver, &t
	}
	if _, err := mem.InsertOffering(ctx, o); err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	return nil
}

func ptrInt(v int) *int { return &v }
